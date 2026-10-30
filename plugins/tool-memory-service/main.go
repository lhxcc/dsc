package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dsc "dsc-sdk"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 记忆库按项目分文件：memory/<项目键>.db，全局层单独一份 memory/global.db。
//
// 为什么不是一个库加 project 列：DSC 常见的用法是同一份安装在多个项目上「并行多开」，
// 各实例共用 <ExecDir>/memory 下同一个文件。SQLite 有锁、不会把文件写坏（本地盘上），
// 但一个文件意味着所有项目排队等同一把写锁——搜索要更新热度（每次搜索都写），
// 后台还在批量落盘推理记录，跨项目的写请求就挤在一起。分文件之后，不同项目各写各的，
// 只有同一个项目多开时才排队（那本来就该排队）；附带好处是坏一个文件只坏一个项目、
// 单个项目能独立备份/清空、库体积不再无界增长。
const (
	// dbSuffix 项目库文件后缀。键直接做文件名：与会话文件同一套键规则，
	// 已经保证不含路径分隔符（见 session.SessionKeyForProject）。
	dbSuffix = ".db"
	// legacyDBKey 旧版单库的文件名（memory.db）。不再读取：按 AGENTS.md §4，
	// 不保留「新旧布局并存」的兼容分支；旧库在启动时改名留档。
	legacyDBKey = "memory"
	// maxOpenDBs 同时打开的库文件上限。项目数通常是个位数，留些余量即可；
	// 超限按 LRU 关闭最久未用的（Windows 上不关会被临时目录清理判为占用）。
	maxOpenDBs = 16
	// dbOpenRetries 打开/建表遇到 SQLITE_BUSY 的重试次数。多个实例同时首次访问
	// 同一个新项目库时会撞上建表事务，重试比直接失败划算（失败等于该实例记忆能力消失）。
	dbOpenRetries = 5
)

// dbs 项目库句柄池（键 → 已打开的库）。句柄按需打开，避免启动时把所有项目库
// 都建出来（那会让「只读盘点」也产生文件）。
var dbs = &dbPool{open: map[string]*gorm.DB{}}

// dbPool 是项目键 → gorm 句柄的缓存，带 LRU 关闭。
type dbPool struct {
	mu   sync.Mutex
	open map[string]*gorm.DB
	lru  []string // 末尾 = 最近使用
}

// db 取（必要时打开）某个项目键对应的库。
func (p *dbPool) db(key string) (*gorm.DB, error) {
	if !dsc.ValidSessionID(key) {
		return nil, fmt.Errorf("invalid project key %q: it must be usable as a file name", key)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if db := p.open[key]; db != nil {
		p.touchLocked(key)
		return db, nil
	}
	db, err := openMemoryDB(dbPathFor(key))
	if err != nil {
		return nil, err
	}
	if len(p.open) >= maxOpenDBs {
		p.evictLocked()
	}
	p.open[key] = db
	p.lru = append(p.lru, key)
	return db, nil
}

// existing 取一个**已存在**的项目库；文件不存在时返回 ok=false（不建库）。
//
// 读路径专用：一次检索/盘点不该在磁盘上留下空库——那会让「查一下」产生副作用，
// 项目多开时还会把历史项目的空壳文件重新堆出来。写路径（memory_add、批量落盘）
// 仍用 db()，缺库就建。
func (p *dbPool) existing(key string) (*gorm.DB, bool) {
	if !dsc.ValidSessionID(key) {
		return nil, false
	}
	p.mu.Lock()
	db := p.open[key]
	p.mu.Unlock()
	if db != nil {
		p.mu.Lock()
		p.touchLocked(key)
		p.mu.Unlock()
		return db, true
	}
	if _, err := os.Stat(dbPathFor(key)); err != nil {
		return nil, false
	}
	db, err := p.db(key)
	if err != nil {
		return nil, false
	}
	return db, true
}

// closeAll 关闭全部句柄（测试清理用；生产只在进程退出时随进程释放）。
func (p *dbPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, db := range p.open {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		delete(p.open, key)
	}
	p.lru = nil
}

func (p *dbPool) touchLocked(key string) {
	for i, k := range p.lru {
		if k == key {
			p.lru = append(p.lru[:i], p.lru[i+1:]...)
			break
		}
	}
	p.lru = append(p.lru, key)
}

// evictLocked 关闭最久未用的库句柄（调用方已持锁）。
func (p *dbPool) evictLocked() {
	if len(p.lru) == 0 {
		return
	}
	oldest := p.lru[0]
	p.lru = p.lru[1:]
	if db := p.open[oldest]; db != nil {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		delete(p.open, oldest)
	}
}

// dbPathFor 返回某个项目键的库文件路径。
func dbPathFor(key string) string {
	return dsc.PJoin(dsc.MemoryDir(), key+dbSuffix)
}

// retireLegacyDB 把旧版单库（memory.db）改名留档，之后不再读取。
//
// 为什么不做数据迁移：按 AGENTS.md §4，不为被替代的实现保留兼容分支——一段「读旧库、
// 按 project 列拆分、写新库」的代码会永久留在仓库里，只服务一次升级；本项目也有先例
// （更早那版「无 project 列的 memory.db」同样是不迁移、删掉重建）。
// 改名而不是删除，是为了用户还能自己从旧文件里捞内容；新布局从空开始，不读它。
func retireLegacyDB() {
	legacy := dsc.PJoin(dsc.MemoryDir(), legacyDBKey+dbSuffix)
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	backup := legacy + ".pre-project-split"
	if err := os.Rename(legacy, backup); err != nil {
		fmt.Fprintf(os.Stderr, "memory-service: legacy memory.db left in place (%v); it is not read — per-project databases live beside it\n", err)
		return
	}
	// WAL 边车跟着改名，否则会在目录里留下无主的 -wal/-shm。
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(legacy + suffix); err == nil {
			_ = os.Rename(legacy+suffix, backup+suffix)
		}
	}
	fmt.Fprintf(os.Stderr, "memory-service: retired legacy memory.db as %s (per-project databases do not import it)\n", filepath.Base(backup))
}

// projectDBKeys 列出磁盘上已有的项目库键（排序稳定，含 global，排除旧单库）。
//
// 只认已存在的文件：盘点是读操作，不该顺手把历史项目的空库重新建出来。
func projectDBKeys() ([]string, error) {
	entries, err := os.ReadDir(dsc.MemoryDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, dbSuffix) {
			continue
		}
		key := strings.TrimSuffix(name, dbSuffix)
		if key == legacyDBKey {
			continue // 旧单库：不读、不算作项目
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

const (
	decayLambda  = 0.05
	archiveDays  = 30
	timeNodeDays = 30
	// defaultPageSize 列表查询的默认每页条数
	defaultPageSize = 20
	// maxPageSize 列表查询的最大每页条数
	maxPageSize = 100
)

// Memory 对应 memories 表。
//
// ID 是**全局稳定**的字符串（`mem-<16 位十六进制>`，8 字节随机），不是自增整数：
// 记忆分散在多个项目库里，自增主键在每个库里都会从 1 重来，模型拿着 "3" 无法指认
// 是哪一条；用一个不依赖所在文件的随机 id，删/改/迁移都可以只凭 id 定位。
// 排序不看 id（创建时间有独立的 created_at 列），所以放弃自增的单调性没有代价。
type Memory struct {
	ID          string `gorm:"primaryKey;type:text" json:"id"`
	Content     string `gorm:"type:text;not null" json:"content"`
	CreatedAt   int64  `gorm:"not null" json:"created_at"`
	LastAccess  int64  `gorm:"not null;index" json:"last_access"`
	AccessCount int    `gorm:"not null;default:0" json:"access_count"`
	Archived    bool   `gorm:"not null;default:false;index" json:"archived"`
	Source      string `gorm:"type:text;default:'user'" json:"source"`
	// Project 记忆归属：该行所在库的项目键（或全局层哨兵 globalProject）。
	// 分库之后它与所在文件一一对应，保留这一列是为了「列表里能直接看出归属」
	// 以及跨库盘点时的自证；查询不再需要它做过滤（一个库只有一种归属）。
	Project string `gorm:"type:text;not null;index" json:"project"`
}

// 记忆作用域（scope）——注意这是**检索/写入范围**，不是权限。
// 记忆插件的用户只有模型自己，隔离的目的是「默认别把别的项目的细节灌进当前上下文」，
// 而不是禁止模型管理自己的数据。因此除两档常用范围外，还能显式点名某个项目。
//
//	current            当前项目（默认）
//	global             跨项目共享层（用户偏好、通用约定）
//	project:<项目键>    指定某个项目（用于跨项目管理/整理）
//	all                全库（仅检索与列表；用于盘点有哪些项目、查看某条 id 属于谁）
const (
	scopeCurrent = "current"
	scopeGlobal  = "global"
	scopeAll     = "all"
	scopePrefix  = "project:"
)

// globalProject 是全局层在 project 列的存值：直接复用 scope 取值 "global"。
//
// 不另造哨兵（如 "*"）：库里的值与工具参数用同一套词，看数据就能对上语义。
// 与项目键不会撞——项目键由 SessionKeyForProject 生成，形如
// "O=--Agents--dsc-49C81869"，永远带 "-<8位哈希>" 后缀，不可能等于 "global"。
const globalProject = scopeGlobal

// currentProjectKey 返回当前工作区的项目键（与会话文件名同一套规则）。
func currentProjectKey() string {
	return dsc.ProjectSessionKey()
}

// scopeFilter 是解析后的检索/写入范围。
type scopeFilter struct {
	kind string // current | global | all | project
	key  string // kind==project 时的项目键
}

// parseScope 解析 scope 参数。def 为空值时使用的默认档（写入侧传 ""，
// 即「必填」；读取侧传 scopeCurrent）。
func parseScope(raw, def string) (scopeFilter, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		if def == "" {
			return scopeFilter{}, fmt.Errorf("scope is required: current, global, or project:<key>")
		}
		return scopeFilter{kind: def}, nil
	}
	switch strings.ToLower(s) {
	case scopeCurrent:
		return scopeFilter{kind: scopeCurrent}, nil
	case scopeGlobal:
		return scopeFilter{kind: scopeGlobal}, nil
	case scopeAll:
		return scopeFilter{kind: scopeAll}, nil
	}
	if len(s) > len(scopePrefix) && strings.EqualFold(s[:len(scopePrefix)], scopePrefix) {
		key := strings.TrimSpace(s[len(scopePrefix):])
		if key == "" {
			return scopeFilter{}, fmt.Errorf("scope %q is missing the project key", s)
		}
		if key == globalProject {
			return scopeFilter{}, fmt.Errorf("use scope=global for the shared layer, not project:%s", key)
		}
		return scopeFilter{kind: "project", key: key}, nil
	}
	return scopeFilter{}, fmt.Errorf("invalid scope %q: want current, global, all, or project:<key>", raw)
}

// projectValue 把范围映射为写入目标（即库文件的键；all 不能用于写入）。
func (f scopeFilter) projectValue() (string, error) {
	switch f.kind {
	case scopeCurrent:
		return currentProjectKey(), nil
	case scopeGlobal:
		return globalProject, nil
	case "project":
		return f.key, nil
	}
	return "", fmt.Errorf("scope=all cannot be used for writing: choose current, global, or project:<key>")
}

// keys 返回该范围要碰的库键。
//
// all 只枚举**磁盘上已有**的库（盘点是读操作，不该把历史项目的空库重建出来）；
// 其余档位返回单键，读写都在那一个库里完成。
func (f scopeFilter) keys() ([]string, error) {
	switch f.kind {
	case scopeGlobal:
		return []string{globalProject}, nil
	case "project":
		return []string{f.key}, nil
	case scopeAll:
		return projectDBKeys()
	default:
		return []string{currentProjectKey()}, nil
	}
}

// CallLog 对应 call_logs 表
type CallLog struct {
	ID          int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	Query       string `gorm:"type:text;not null" json:"query"`
	Keywords    string `gorm:"type:text;not null" json:"keywords"`
	ResultCount int    `gorm:"not null" json:"result_count"`
	CreatedAt   int64  `gorm:"not null" json:"created_at"`
}

// resultItem 是搜索返回的结果项，包含分数
type resultItem struct {
	Memory
	Score float64 `json:"score"`
}

// openMemoryDB 打开（必要时创建）一个记忆库文件并确保表结构就绪。
//
// 每个项目库都是独立文件、独立 FTS 索引与触发器；initSchema 对同一个文件重复执行
// 是幂等的（CREATE ... IF NOT EXISTS），所以句柄被 LRU 关掉后再打开不需要特殊处理。
//
// 多实例同时首次访问同一个新项目库时，建表事务会撞 SQLITE_BUSY（busy_timeout 兜住
// 大部分，但首次建 schema 的锁可能超过它）——退避重试几次。这里失败等于该实例的
// 记忆能力整体消失，代价远大于多等 200ms。
func openMemoryDB(path string) (*gorm.DB, error) {
	var lastErr error
	for attempt := 0; attempt < dbOpenRetries; attempt++ {
		db, err := initDB(path)
		if err == nil {
			return db, nil
		}
		lastErr = err
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	return nil, fmt.Errorf("open memory db %s: %w", path, lastErr)
}

// initDB 初始化一个库文件：常规表自动迁移 + FTS5 虚拟表与同步触发器。
//
// gorm 默认 logger 写入 os.Stdout（log.New(os.Stdout, "\r\n", ...)），
// 而 go-plugin 握手协议用 stdout 传递 "1|1|tcp|addr|grpc|" 行——
// gorm 任何警告（schema drift / slow query / error）的 "\r\n" 前缀
// 都会让 host 的 scanner 读到空行作为握手信息，报 "Unrecognized remote
// core message: " 并强杀进程（exit status 1）。本插件是 go-plugin 子进程，
// stdout 在 plugin.Serve 打印握手行之前不得有任何输出。将 logger 重定向
// 到 os.Stderr，保留诊断信息同时不污染握手通道。
func initDB(path string) (*gorm.DB, error) {
	if err := dsc.MkdirAll(dsc.PDir(path)); err != nil {
		return nil, err
	}
	// WAL + synchronous=NORMAL：记忆写入是「一条一条」的小事务，默认回滚日志模式每条都要
	// fsync（实测 5000 条存量下单条 8.3ms，其中约 90% 是 fsync），而 WAL 下同口径只要
	// 0.8ms。记忆数据的价值远低于「每条都落盘」的代价：NORMAL 在最坏情况（掉电）下最多
	// 丢最近的少量事务，进程崩溃不丢（数据已在 WAL 文件里）。
	// busy_timeout 防止并发读写（后台批量写入 + 工具查询）直接返回 SQLITE_BUSY。
	db, err := gorm.Open(sqlite.Open(path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"), &gorm.Config{
		Logger: logger.New(
			log.New(os.Stderr, "\r\n", log.LstdFlags),
			logger.Config{
				SlowThreshold: 200 * time.Millisecond,
				LogLevel:      logger.Warn,
				Colorful:      false,
			},
		),
	})
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(&Memory{}, &CallLog{}); err != nil {
		return nil, err
	}

	// FTS5 的外部内容表只能按**整数 rowid** 关联内容表（content_rowid 必须是
	// 一个 int64 列）。记忆的对外 id 是字符串，所以索引键用 SQLite 的隐式 rowid：
	// 两者共存——rowid 只在文件内部用来对索引，id 才是模型看到、跨库指认时用的值。
	ftsSQL := `
        CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
                content,
                content='memories',
                content_rowid='rowid',
                tokenize='unicode61'
        );`
	if err := db.Exec(ftsSQL).Error; err != nil {
		return nil, err
	}

	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
                        INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
                END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
                        INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
                END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
                        INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.rowid, old.content);
                        INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
                END;`,
	}
	for _, trig := range triggers {
		if err := db.Exec(trig).Error; err != nil {
			return nil, err
		}
	}
	return db, nil
}

// timeFactor 计算时间衰减因子
func timeFactor(lastAccess int64, now time.Time) float64 {
	days := now.Sub(time.Unix(lastAccess, 0)).Hours() / 24
	if days <= timeNodeDays {
		return 1.0
	}
	return math.Exp(-decayLambda * (days - timeNodeDays))
}

// archiveIdleMemories 自动归档超过30天未访问的记忆（按库执行：一次搜索只碰
// 本次检索涉及的项目库，归档范围也随之从「全库」降到「本项目」）。
func archiveIdleMemories(db *gorm.DB, now time.Time) {
	threshold := now.Add(-archiveDays * 24 * time.Hour).Unix()
	db.Model(&Memory{}).
		Where("archived = ? AND last_access < ?", false, threshold).
		Update("archived", true)
}

// joinMatchTerms 把搜索关键词拼成 FTS5 MATCH 查询串
func joinMatchTerms(keywords []string) string {
	quoted := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		quoted = append(quoted, `"`+strings.ReplaceAll(kw, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// searchInDB 在单个项目库里检索：FTS5 优先，LIKE 兜底。
func searchInDB(db *gorm.DB, keywords []string, now time.Time) ([]resultItem, error) {
	if len(keywords) == 0 {
		return nil, nil
	}
	var results []resultItem

	matchQuery := joinMatchTerms(keywords)
	type ftsRow struct {
		ID          string
		Content     string
		CreatedAt   int64
		LastAccess  int64
		AccessCount int
		Project     string
		RelScore    float64
	}
	var ftsRows []ftsRow
	err := db.Raw(`
        SELECT m.id, m.content, m.created_at, m.last_access, m.access_count, m.project,
               -bm25(memories_fts) AS rel_score
        FROM memories_fts
        JOIN memories m ON m.rowid = memories_fts.rowid
        WHERE memories_fts MATCH ? AND m.archived = 0
        ORDER BY rel_score DESC
        LIMIT 50`, matchQuery).Scan(&ftsRows).Error
	if err != nil {
		ftsRows = nil
	}

	if len(ftsRows) > 0 {
		for _, row := range ftsRows {
			mem := Memory{
				ID:          row.ID,
				Content:     row.Content,
				CreatedAt:   row.CreatedAt,
				LastAccess:  row.LastAccess,
				AccessCount: row.AccessCount,
				Archived:    false,
				Project:     row.Project,
			}
			score := row.RelScore * timeFactor(row.LastAccess, now)
			results = append(results, resultItem{Memory: mem, Score: score})
		}
		return results, nil
	}

	likeConditions := make([]string, len(keywords))
	likeArgs := make([]interface{}, len(keywords))
	for i, kw := range keywords {
		likeConditions[i] = "content LIKE ?"
		likeArgs[i] = "%" + kw + "%"
	}
	var memories []Memory
	if err := db.Model(&Memory{}).
		Where("archived = ?", false).
		Where(strings.Join(likeConditions, " OR "), likeArgs...).
		Limit(50).Find(&memories).Error; err != nil {
		return nil, err
	}
	for _, mem := range memories {
		results = append(results, resultItem{Memory: mem, Score: timeFactor(mem.LastAccess, now)})
	}
	return results, nil
}

// touchHits 刷新本次命中的热度（上次访问时间 + 访问次数）。
//
// 这是打分口径的一部分：检索结果按「相关度 × 时间衰减」排序，不记录访问就等于所有
// 条目按创建时间老化，常用结论会沉下去。分库之后它是同一条语句、作用在一个库上，
// 一次搜索一次写事务（不是每条一次提交），语义不变、提交次数从 N 降到 1。
func touchHits(db *gorm.DB, items []resultItem, now time.Time) {
	if len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	db.Model(&Memory{}).Where("id IN ?", ids).Updates(map[string]interface{}{
		"last_access":  now.Unix(),
		"access_count": gorm.Expr("access_count + 1"),
	})
}

// searchMemories 跨库检索：逐个目标库查询后按分数归并（每个库内部仍是 FTS5 优先）。
func searchMemories(query string, now time.Time, scope scopeFilter) ([]resultItem, error) {
	keywords := strings.Fields(query)
	if len(keywords) == 0 {
		return []resultItem{}, nil
	}
	keys, err := scope.keys()
	if err != nil {
		return nil, err
	}
	var results []resultItem
	for _, key := range keys {
		db, ok := dbs.existing(key)
		if !ok {
			continue // 该项目的库还不存在 = 没有记忆，不必为它建文件
		}
		archiveIdleMemories(db, now)
		items, err := searchInDB(db, keywords, now)
		if err != nil {
			return nil, err
		}
		touchHits(db, items, now)
		results = append(results, items...)
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].ID < results[j].ID
	})
	if len(results) > 50 {
		results = results[:50] // 与原单库口径一致：一次最多 50 条
	}
	return results, nil
}

// ---- 查 (memory_search) ----

func handleSearch(ctx context.Context, args json.RawMessage) (string, error) {
	flushPendingMemories() // 先落盘待写队列：模型刚写的必须立刻搜得到
	var req struct {
		Query    string `json:"query"`
		Keywords string `json:"keywords"`
		Scope    string `json:"scope"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	kwStr := req.Keywords
	if kwStr == "" {
		kwStr = req.Query
	}
	now := time.Now()
	scope, err := parseScope(req.Scope, scopeCurrent)
	if err != nil {
		return "", err
	}
	results, err := searchMemories(kwStr, now, scope)
	if err != nil {
		return "", err
	}

	// 检索日志记在「当前项目」库里：它是会话自己的诊断记录，跨库盘点不该在
	// 别人的库上留痕；本项目还没写过记忆时不建库（读操作不产生文件）。
	if cur, ok := dbs.existing(currentProjectKey()); ok {
		cur.Create(&CallLog{
			Query:       req.Query,
			Keywords:    kwStr,
			ResultCount: len(results),
			CreatedAt:   now.Unix(),
		})
	}

	out, err := json.Marshal(map[string]interface{}{
		"results": results,
		"total":   len(results),
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---- 增 (memory_add) ----

// findMemory 按 id 在所有项目库里定位一条记忆，并返回它所在的库。
//
// id 是全局稳定的随机串（见 Memory.ID），所以「只凭 id 就能指认」这条契约在分库
// 之后仍然成立：查询顺序是「当前项目 → global → 其余项目（按键排序）」，命中即返回。
// 每跳都是一次主键查找，项目数十来个的量级可以忽略。
func findMemory(id string) (*Memory, *gorm.DB, error) {
	cur := currentProjectKey()
	keys, err := projectDBKeys()
	if err != nil {
		return nil, nil, err
	}
	// 当前项目可能还没有库文件（首次使用），也要试一次：那条路径会建库并按 id 查空。
	ordered := make([]string, 0, len(keys)+2)
	ordered = append(ordered, cur)
	if cur != globalProject {
		ordered = append(ordered, globalProject)
	}
	ordered = append(ordered, keys...)

	seen := map[string]bool{}
	for _, key := range ordered {
		if seen[key] {
			continue
		}
		seen[key] = true
		db, err := dbs.db(key)
		if err != nil {
			continue
		}
		var mem Memory
		if err := db.Where("id = ?", id).First(&mem).Error; err == nil {
			return &mem, db, nil
		}
	}
	return nil, nil, fmt.Errorf("memory id %s not found", id)
}

// addMemory 写入一条记忆到指定归属的库。project 为项目键或全局层哨兵。
// 去重按「同库 + 同内容」判定：同样的结论出现在两个项目里属两条独立记忆。
func addMemory(content, source, project string) (id string, dedup bool, err error) {
	db, err := dbs.db(project)
	if err != nil {
		return "", false, err
	}
	return addMemoryTx(db, content, source, project)
}

// addMemoryTx 在给定句柄（可为事务）上写入一条记忆，去重语义同 addMemory。
// 抽出来是为了让批量落盘（flushPendingMemories）把一次刷新的多条放进同一个事务：
// 一个事务只提交一次，比逐条提交少几个数量级的 fsync。
func addMemoryTx(tx *gorm.DB, content, source, project string) (id string, dedup bool, err error) {
	if source == "" {
		source = "user"
	}
	var existing Memory
	if err := tx.Where("content = ? AND project = ?", content, project).First(&existing).Error; err == nil {
		return existing.ID, true, nil
	}
	now := time.Now().Unix()
	mem := Memory{
		ID:          newMemoryID(tx),
		Content:     content,
		CreatedAt:   now,
		LastAccess:  now,
		AccessCount: 0,
		Archived:    false,
		Source:      source,
		Project:     project,
	}
	if err := tx.Create(&mem).Error; err != nil {
		return "", false, err
	}
	return mem.ID, false, nil
}

// newMemoryID 生成一个在本库内不重复的记忆 id。
//
// 熵留得很足（8 字节）：id 要跨库唯一，没有「同一张表里查重后重生成」的机会——
// 这里的查重只是最后一道保险，撞的机会本身可以忽略。
func newMemoryID(tx *gorm.DB) string {
	for i := 0; i < 4; i++ {
		id := dsc.NewID("mem")
		var n int64
		tx.Model(&Memory{}).Where("id = ?", id).Count(&n)
		if n == 0 {
			return id
		}
	}
	return dsc.NewID("mem")
}

func handleAdd(ctx context.Context, args json.RawMessage) (string, error) {
	flushPendingMemories()
	var req struct {
		Content string `json:"content"`
		Source  string `json:"source"`
		Scope   string `json:"scope"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if req.Content == "" {
		return "", fmt.Errorf("content is required")
	}
	scope, err := parseScope(req.Scope, "")
	if err != nil {
		return "", err
	}
	project, err := scope.projectValue()
	if err != nil {
		return "", err
	}
	id, dedup, err := addMemory(req.Content, req.Source, project)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(map[string]interface{}{
		"id":      id,
		"dedup":   dedup,
		"project": project,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---- 删 (memory_delete) ----

func handleDelete(ctx context.Context, args json.RawMessage) (string, error) {
	flushPendingMemories()
	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		return "", fmt.Errorf("id is required")
	}
	mem, db, err := findMemory(req.ID)
	if err != nil {
		return "", err
	}
	result := db.Delete(&Memory{}, "id = ?", mem.ID)
	if result.Error != nil {
		return "", result.Error
	}
	if result.RowsAffected == 0 {
		return "", fmt.Errorf("memory id %s not found", req.ID)
	}
	out, _ := json.Marshal(map[string]interface{}{
		"ok":      true,
		"id":      req.ID,
		"deleted": result.RowsAffected,
	})
	return string(out), nil
}

// ---- 改 (memory_update) ----

// updateMemory 改写一条记忆；scope 变化时**跨库搬移**（删旧库里的那条、按同一 id
// 写进目标库）。id 保持不变，模型之前拿到的引用因此不会失效。
func handleUpdate(ctx context.Context, args json.RawMessage) (string, error) {
	flushPendingMemories()
	var req struct {
		ID      string  `json:"id"`
		Content string  `json:"content"`
		Source  string  `json:"source"`
		Scope   *string `json:"scope"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		return "", fmt.Errorf("id is required")
	}

	mem, db, err := findMemory(req.ID)
	if err != nil {
		return "", err
	}

	target := mem.Project
	if req.Scope != nil {
		// 允许改归属（current / global / project:<key>）
		scope, err := parseScope(*req.Scope, "")
		if err != nil {
			return "", err
		}
		if target, err = scope.projectValue(); err != nil {
			return "", err
		}
	}
	if req.Content == "" && req.Source == "" && req.Scope == nil {
		return "", fmt.Errorf("at least one of content, source or scope must be provided")
	}

	now := time.Now().Unix()
	if target != mem.Project {
		// 跨库搬移：同一 id 从旧库删除、按新归属写进目标库。整条记录连同热度一起
		// 带过去，否则「整理」会把一条常用结论变回冷门。
		moved := *mem
		moved.Project = target
		moved.LastAccess = now
		if req.Content != "" {
			moved.Content = req.Content
		}
		if req.Source != "" {
			moved.Source = req.Source
		}
		targetDB, err := dbs.db(target)
		if err != nil {
			return "", err
		}
		if err := targetDB.Create(&moved).Error; err != nil {
			return "", err
		}
		if err := db.Delete(&Memory{}, "id = ?", mem.ID).Error; err != nil {
			return "", err
		}
		mem = &moved
		db = targetDB
	} else {
		updates := map[string]interface{}{"last_access": now}
		if req.Content != "" {
			updates["content"] = req.Content
		}
		if req.Source != "" {
			updates["source"] = req.Source
		}
		result := db.Model(&Memory{}).Where("id = ?", mem.ID).Updates(updates)
		if result.Error != nil {
			return "", result.Error
		}
		if result.RowsAffected == 0 {
			return "", fmt.Errorf("memory id %s not found", req.ID)
		}
	}

	var updated Memory
	if err := db.Where("id = ?", mem.ID).First(&updated).Error; err != nil {
		return "", err
	}
	out, _ := json.Marshal(map[string]interface{}{
		"ok":      true,
		"id":      mem.ID,
		"updated": 1,
		"memory":  updated,
	})
	return string(out), nil
}

// ---- 查列表 (memory_list) ----

func handleList(ctx context.Context, args json.RawMessage) (string, error) {
	flushPendingMemories()
	var req struct {
		Page     int    `json:"page"`
		PageSize int    `json:"page_size"`
		Source   string `json:"source"`
		Scope    string `json:"scope"`
		Archived *bool  `json:"archived"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if req.Page <= 0 {
		req.Page = 1
	}
	if req.PageSize <= 0 {
		req.PageSize = defaultPageSize
	}
	if req.PageSize > maxPageSize {
		req.PageSize = maxPageSize
	}

	scope, err := parseScope(req.Scope, scopeCurrent)
	if err != nil {
		return "", err
	}
	keys, err := scope.keys()
	if err != nil {
		return "", err
	}

	// 分页是「跨库归并后的分页」：每个库先各自取前 page*pageSize 条（它一定覆盖
	// 全局前 page*pageSize 名），归并排序后再切这一页。这样每页结果与「假想中的
	// 一个合并库」一致，代价是每库一次有界查询。
	need := req.Page * req.PageSize
	var (
		all   []Memory
		total int64
	)
	for _, key := range keys {
		db, ok := dbs.existing(key)
		if !ok {
			continue // 没有库就没有条目；列表是读操作，不建文件
		}
		query := db.Model(&Memory{})
		if req.Source != "" {
			query = query.Where("source = ?", req.Source)
		}
		if req.Archived != nil {
			query = query.Where("archived = ?", *req.Archived)
		} else {
			query = query.Where("archived = ?", false)
		}
		var n int64
		query.Count(&n)
		total += n

		var rows []Memory
		if err := query.Order("created_at DESC").Limit(need).Find(&rows).Error; err != nil {
			return "", err
		}
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].CreatedAt != all[j].CreatedAt {
			return all[i].CreatedAt > all[j].CreatedAt
		}
		return all[i].ID < all[j].ID // 同秒创建时按 id 稳定排序，保证分页不重复/不遗漏
	})
	start := (req.Page - 1) * req.PageSize
	memories := []Memory{}
	if start < len(all) {
		end := start + req.PageSize
		if end > len(all) {
			end = len(all)
		}
		memories = all[start:end]
	}

	out, _ := json.Marshal(map[string]interface{}{
		"memories":  memories,
		"total":     total,
		"page":      req.Page,
		"page_size": req.PageSize,
	})
	return string(out), nil
}

// ---- 视图 ----

func memorySearchView(result string) (json.RawMessage, error) {
	var out struct {
		Results []struct {
			ID      string  `json:"id"`
			Content string  `json:"content"`
			Source  string  `json:"source"`
			Project string  `json:"project"`
			Score   float64 `json:"score"`
		} `json:"results"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return nil, nil
	}
	rows := make([]dsc.ViewRow, 0, len(out.Results))
	for _, r := range out.Results {
		rows = append(rows, dsc.ViewRow{
			"id":      r.ID,
			"content": r.Content,
			"score":   strconv.FormatFloat(r.Score, 'f', 2, 64),
			"source":  r.Source,
			"project": r.Project,
		})
	}
	return dsc.TableView("Memory", &dsc.ViewBadge{Text: fmt.Sprintf("%d hit(s)", out.Total), Tone: "teal"}, []dsc.ViewColumn{
		{Key: "id", Title: "id"},
		{Key: "content", Title: "content"},
		{Key: "score", Title: "score", Tone: "green"},
		{Key: "source", Title: "source"},
		{Key: "project", Title: "project"},
	}, rows), nil
}

func memoryAddView(result string) (json.RawMessage, error) {
	var out struct {
		ID    string `json:"id"`
		Dedup bool   `json:"dedup"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return nil, nil
	}
	badge := &dsc.ViewBadge{Text: "saved", Tone: "green"}
	status := "saved"
	if out.Dedup {
		badge = &dsc.ViewBadge{Text: "duplicate", Tone: "yellow"}
		status = "skipped (duplicate content)"
	}
	return dsc.CardView("Memory", badge, []dsc.ViewField{
		{Key: "id", Value: out.ID},
		{Key: "status", Value: status, Tone: badge.Tone},
	}), nil
}

func memoryDeleteView(result string) (json.RawMessage, error) {
	var out struct {
		OK      bool   `json:"ok"`
		ID      string `json:"id"`
		Deleted int64  `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return nil, nil
	}
	return dsc.CardView("MemoryDelete", &dsc.ViewBadge{Text: "deleted", Tone: "yellow"}, []dsc.ViewField{
		{Key: "id", Value: out.ID},
		{Key: "rows", Value: strconv.FormatInt(out.Deleted, 10)},
	}), nil
}

func memoryUpdateView(result string) (json.RawMessage, error) {
	var out struct {
		OK     bool   `json:"ok"`
		ID     string `json:"id"`
		Memory struct {
			Content string `json:"content"`
			Source  string `json:"source"`
		} `json:"memory"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return nil, nil
	}
	return dsc.CardView("MemoryUpdate", &dsc.ViewBadge{Text: "updated", Tone: "green"}, []dsc.ViewField{
		{Key: "id", Value: out.ID},
		{Key: "content", Value: out.Memory.Content},
		{Key: "source", Value: out.Memory.Source},
	}), nil
}

func memoryListView(result string) (json.RawMessage, error) {
	var out struct {
		Memories []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
			Source  string `json:"source"`
			Project string `json:"project"`
		} `json:"memories"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return nil, nil
	}
	rows := make([]dsc.ViewRow, 0, len(out.Memories))
	for _, m := range out.Memories {
		rows = append(rows, dsc.ViewRow{
			"id":      m.ID,
			"content": m.Content,
			"source":  m.Source,
			"project": m.Project,
		})
	}
	return dsc.TableView("Memory", &dsc.ViewBadge{Text: fmt.Sprintf("%d items", out.Total), Tone: "teal"}, []dsc.ViewColumn{
		{Key: "id", Title: "id"},
		{Key: "content", Title: "content"},
		{Key: "source", Title: "source"},
		{Key: "project", Title: "project"},
	}, rows), nil
}

func main() {
	retireLegacyDB()
	startMemoryWriter() // 后台批量落盘（pre-step 只入队，不碰磁盘）

	searchSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "query": {"type": "string", "description": "自然语言查询语句"},
                        "keywords": {"type": "string", "description": "空格分隔的搜索关键词，优先于 query；缺省时取 query"},
                        "scope": {"type": "string", "description": "检索范围（缺省 current）：current=当前项目；global=全局共享层；project:<项目键>=指定项目；all=全库（跨项目盘点/整理时用）"}
                },
                "description": "query 与 keywords 至少提供一个"
        }`)
	addSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "content": {"type": "string", "description": "要保存的记忆内容"},
                        "source": {"type": "string", "description": "记忆来源标记，缺省 user"},
                        "scope": {"type": "string", "description": "必填归属：current=当前项目；global=跨项目共享（用户偏好、通用约定）；project:<项目键>=指定某个项目（如 project:O=--Agents--dsc-49C81869）"}
                },
                "required": ["content", "scope"]
        }`)
	deleteSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "id": {"type": "string", "description": "要删除的记忆 ID（形如 mem-1a2b3c4d5e6f7a8b，从搜索结果或列表里取原值）"}
                },
                "required": ["id"]
        }`)
	updateSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "id": {"type": "string", "description": "要修改的记忆 ID（形如 mem-1a2b3c4d5e6f7a8b，原值传入）"},
                        "content": {"type": "string", "description": "新的记忆内容（可选，至少提供 content/source/scope 之一）"},
                        "source": {"type": "string", "description": "新的来源标记（可选）"},
                        "scope": {"type": "string", "description": "改归属（可选）：current=当前项目；global=跨项目共享；project:<项目键>=指定项目。跨归属修改会把该条搬到对应的库里，ID 不变"}
                },
                "required": ["id"]
        }`)
	listSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "page": {"type": "integer", "description": "页码，从 1 开始，默认 1"},
                        "page_size": {"type": "integer", "description": "每页条数，默认 20，最大 100"},
                        "source": {"type": "string", "description": "按来源筛选（可选）"},
                        "scope": {"type": "string", "description": "范围（缺省 current）：current=当前项目；global=全局共享层；project:<项目键>=指定项目；all=全库（含各项目，用于盘点）"},
                        "archived": {"type": "boolean", "description": "是否只看已归档记忆，默认 false"}
                }
        }`)

	sdk := dsc.New(dsc.Config{
		Name:    "memory-service",
		Version: "1.2.0",
		Type:    dsc.TypeTool,
		Provides: map[string]string{
			"memory": "true",
		},
	})

	// 查（搜索）
	sdk.Tool(dsc.Tool{
		Name:        "memory_search",
		Description: "搜索记忆库：按关键词检索历史记忆（用户偏好、项目约定、本次任务的经验教训等），默认只搜当前项目（scope=current）；用户偏好、跨项目约定等全局记忆需显式 scope=global 检索。返回按相关度与时间衰减排序的结果。参数 query 或 keywords 至少提供一个。",
		Schema:      searchSchema,
		Handler:     handleSearch,
		// ContextFn 动态贡献 system prompt（每次 ListContext 求值，对齐 DSH
		// ctx.systemPrompt.section）：记忆库规模/容量等运行态变化时即时反映。
		ContextFn: func() string {
			return "记忆服务：可用 memory_search 检索、memory_add 新增、memory_update 修改、memory_delete 删除、memory_list 列表。" +
				"记忆归属必填（memory_add 的 scope）：current=当前项目，global=跨项目共享层，project:<项目键>=指定项目；检索默认只查当前项目，查全局用 scope=global，整理其它项目用 scope=all 盘点后再按 id 增删改（隔离只作用于默认检索范围，不限制你管理这些数据）。模型的推理过程会被动记录，任务完成时会提醒写一条经验教训。"
		},
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return memorySearchView(result)
		},
	})
	// 增
	sdk.Tool(dsc.Tool{
		Name:        "memory_add",
		Description: "添加一条记忆到记忆库：把值得长期保留的信息（如用户偏好、项目约定、重要结论、本次任务的经验教训）写入记忆库，供后续 memory_search 检索。必须指定 scope：current 归当前项目（各自的库文件），global 供跨项目共享（用户偏好、通用约定）。",
		Schema:      addSchema,
		Handler:     handleAdd,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return memoryAddView(result)
		},
	})
	// 删
	sdk.Tool(dsc.Tool{
		Name:        "memory_delete",
		Description: "按 ID 删除一条记忆（ID 是记忆所属库里的稳定标识，可直接用搜索结果里的原值）。删除后不可恢复，FTS 索引同步清除。",
		Schema:      deleteSchema,
		Handler:     handleDelete,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return memoryDeleteView(result)
		},
	})
	// 改
	sdk.Tool(dsc.Tool{
		Name:        "memory_update",
		Description: "按 ID 修改记忆内容、来源标记或归属（scope=current/global）。至少提供 content、source、scope 之一。修改后 last_access 自动刷新。",
		Schema:      updateSchema,
		Handler:     handleUpdate,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return memoryUpdateView(result)
		},
	})
	// 查列表（分页）
	sdk.Tool(dsc.Tool{
		Name:        "memory_list",
		Description: "分页列出记忆库中的记忆，按创建时间降序排列。默认列当前项目（scope=current）；scope=global 列全局层，scope=all 列全部项目的记忆（跨库归并，project 列标明归属，便于盘点后按 id 整理）。",
		Schema:      listSchema,
		Handler:     handleList,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return memoryListView(result)
		},
	})

	sdk.Hook(dsc.Hook{
		// 记忆写入成功后作废收尾请求（本轮任务已留下记录）
		AfterTool: func(ctx context.Context, toolName, argumentsJSON, result, toolErr string) (string, string) {
			if toolErr == "" && strings.HasPrefix(toolName, "memory_") {
				clearTaskRecord()
			}
			// 每次工具结束顺手把待写队列落盘：此刻模型本就在等工具结果，这笔开销免费，
			// 而队列清空后下一次 pre-step（LLM 请求前）不必再碰磁盘。
			flushPendingMemories()
			return result, toolErr
		},
		// pre-step：跟随会话日志，被动记录模型推理；任务收尾时以「一次性插播消息」
		// 请求写记忆（详见 capture.go takeTaskRecordNotice）。
		OnEvent: func(ctx context.Context, eventType, dataJSON string) (string, error) {
			if eventType != string(dsc.EventAgentPreStep) {
				return "", nil
			}
			capturePreStep(dataJSON)
			notice := takeTaskRecordNotice()
			if notice == "" {
				return "", nil
			}
			// 宿主会把 append 里的消息追加到本轮消息末尾（role=system），
			// 不改 system prompt，故不会破坏其缓存前缀。
			return taskNoticePayload(notice), nil
		},
	})
	sdk.Serve()
}
