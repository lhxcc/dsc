package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	dsc "dsc-sdk"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

const (
	decayLambda  = 0.05
	archiveDays  = 30
	timeNodeDays = 30
	// defaultPageSize 列表查询的默认每页条数
	defaultPageSize = 20
	// maxPageSize 列表查询的最大每页条数
	maxPageSize = 100
)

// Memory 对应 memories 表
type Memory struct {
	ID          int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	Content     string `gorm:"type:text;not null" json:"content"`
	CreatedAt   int64  `gorm:"not null" json:"created_at"`
	LastAccess  int64  `gorm:"not null;index" json:"last_access"`
	AccessCount int    `gorm:"not null;default:0" json:"access_count"`
	Archived    bool   `gorm:"not null;default:false;index" json:"archived"`
	Source      string `gorm:"type:text;default:'user'" json:"source"`
	// Project 记忆归属：项目键（session.SessionKeyForProject 生成，与 sessions/ 同一命名）
	// 或全局层哨兵 globalProject。NOT NULL 且无默认值——写入必须显式给归属，
	// 库里不存在「未归属」这种中间状态。
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

// projectValue 把范围映射为写入 project 列的值（all 不能用于写入）。
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

// where 给查询加上归属过滤。
func (f scopeFilter) where(q *gorm.DB) *gorm.DB {
	switch f.kind {
	case scopeAll:
		return q
	case scopeGlobal:
		return q.Where("project = ?", globalProject)
	case "project":
		return q.Where("project = ?", f.key)
	default:
		return q.Where("project = ?", currentProjectKey())
	}
}

// sqlClause 返回拼进原生查询的过滤片段（FTS 分支用）。
func (f scopeFilter) sqlClause() (string, []any) {
	switch f.kind {
	case scopeAll:
		return "", nil
	case scopeGlobal:
		return " AND m.project = ?", []any{globalProject}
	case "project":
		return " AND m.project = ?", []any{f.key}
	default:
		return " AND m.project = ?", []any{currentProjectKey()}
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

// dbPath 返回记忆库文件路径：目录经 SDK 统一解析（DSC_MEMORY_DIR，
// 缺省 <ExecDir>/memory）。不按进程 cwd 定位——cwd 会随启动方式漂移，
// 同一份记忆会被写到不同目录。
func dbPath() string {
	return dsc.PJoin(dsc.MemoryDir(), "memory.db")
}

// initDB 初始化数据库：常规表自动迁移 + FTS5 虚拟表与同步触发器。
//
// gorm 默认 logger 写入 os.Stdout（log.New(os.Stdout, "\r\n", ...)），
// 而 go-plugin 握手协议用 stdout 传递 "1|1|tcp|addr|grpc|" 行——
// gorm 任何警告（schema drift / slow query / error）的 "\r\n" 前缀
// 都会让 host 的 scanner 读到空行作为握手信息，报 "Unrecognized remote
// core message: " 并强杀进程（exit status 1）。本插件是 go-plugin 子进程，
// stdout 在 plugin.Serve 打印握手行之前不得有任何输出。将 logger 重定向
// 到 os.Stderr，保留诊断信息同时不污染握手通道。
func initDB(path string) error {
	if err := dsc.MkdirAll(dsc.PDir(path)); err != nil {
		return err
	}
	var err error
	// WAL + synchronous=NORMAL：记忆写入是「一条一条」的小事务，默认回滚日志模式每条都要
	// fsync（实测 5000 条存量下单条 8.3ms，其中约 90% 是 fsync），而 WAL 下同口径只要
	// 0.8ms。记忆数据的价值远低于「每条都落盘」的代价：NORMAL 在最坏情况（掉电）下最多
	// 丢最近的少量事务，进程崩溃不丢（数据已在 WAL 文件里）。
	// busy_timeout 防止并发读写（后台批量写入 + 工具查询）直接返回 SQLITE_BUSY。
	DB, err = gorm.Open(sqlite.Open(path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"), &gorm.Config{
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
		return err
	}

	if err := DB.AutoMigrate(&Memory{}, &CallLog{}); err != nil {
		return err
	}

	ftsSQL := `
        CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
                content,
                content='memories',
                content_rowid='id',
                tokenize='unicode61'
        );`
	if err := DB.Exec(ftsSQL).Error; err != nil {
		return err
	}

	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
                        INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
                END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
                        INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.id, old.content);
                END;`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
                        INSERT INTO memories_fts(memories_fts, rowid, content) VALUES('delete', old.id, old.content);
                        INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
                END;`,
	}
	for _, trig := range triggers {
		if err := DB.Exec(trig).Error; err != nil {
			return err
		}
	}
	return nil
}

// timeFactor 计算时间衰减因子
func timeFactor(lastAccess int64, now time.Time) float64 {
	days := now.Sub(time.Unix(lastAccess, 0)).Hours() / 24
	if days <= timeNodeDays {
		return 1.0
	}
	return math.Exp(-decayLambda * (days - timeNodeDays))
}

// archiveIdleMemories 自动归档超过30天未访问的记忆
func archiveIdleMemories(now time.Time) {
	threshold := now.Add(-archiveDays * 24 * time.Hour).Unix()
	DB.Model(&Memory{}).
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

// searchMemories 执行记忆检索：FTS5 优先，LIKE 兜底；结果按相关度×时间衰减稳定排序。
func searchMemories(query string, now time.Time, scope scopeFilter) ([]resultItem, error) {
	keywords := strings.Fields(query)
	if len(keywords) == 0 {
		return []resultItem{}, nil
	}
	archiveIdleMemories(now)

	var results []resultItem

	matchQuery := joinMatchTerms(keywords)
	type ftsRow struct {
		ID          int64
		Content     string
		CreatedAt   int64
		LastAccess  int64
		AccessCount int
		Project     string
		RelScore    float64
	}
	// 归属过滤（scope 是检索范围，不是权限：all 表示全库）
	extraWhere, extraArgs := scope.sqlClause()
	var ftsRows []ftsRow
	args := append([]any{matchQuery}, extraArgs...)
	err := DB.Raw(`
        SELECT m.id, m.content, m.created_at, m.last_access, m.access_count, m.project,
               -bm25(memories_fts) AS rel_score
        FROM memories_fts
        JOIN memories m ON m.id = memories_fts.rowid
        WHERE memories_fts MATCH ? AND m.archived = 0`+extraWhere+`
        ORDER BY rel_score DESC
        LIMIT 50`, args...).Scan(&ftsRows).Error
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
	} else {
		likeConditions := make([]string, len(keywords))
		likeArgs := make([]interface{}, len(keywords))
		for i, kw := range keywords {
			likeConditions[i] = "content LIKE ?"
			likeArgs[i] = "%" + kw + "%"
		}
		likeWhere := strings.Join(likeConditions, " OR ")

		var memories []Memory
		q := DB.Model(&Memory{}).
			Where("archived = ?", false).
			Where(likeWhere, likeArgs...)
		err := scope.where(q).Limit(50).Find(&memories).Error
		if err != nil {
			return nil, err
		}

		for _, mem := range memories {
			score := timeFactor(mem.LastAccess, now)
			results = append(results, resultItem{Memory: mem, Score: score})
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].ID < results[j].ID
	})
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

	for _, item := range results {
		DB.Model(&Memory{}).Where("id = ?", item.ID).Updates(map[string]interface{}{
			"last_access":  now.Unix(),
			"access_count": gorm.Expr("access_count + 1"),
		})
	}

	DB.Create(&CallLog{
		Query:       req.Query,
		Keywords:    kwStr,
		ResultCount: len(results),
		CreatedAt:   now.Unix(),
	})

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

// loadMemory 取一条记忆（按 id）。不做归属限制：记忆插件的用户只有模型自己，
// 隔离是「默认检索范围」，不是权限——模型要整理哪个项目的记忆都应允许，
// 用 memory_list(scope=all) 就能看到 id 与归属。
func loadMemory(id int64) (*Memory, error) {
	var mem Memory
	if err := DB.First(&mem, id).Error; err != nil {
		return nil, fmt.Errorf("memory id %d not found", id)
	}
	return &mem, nil
}

// addMemory 写入一条记忆。project 为项目键（空串 = 全局）。
// 去重按「同项目 + 同内容」判定：同样的结论出现在两个项目里属两条独立记忆，
// 全局那条也不该顶掉项目内那条的写入。
func addMemory(content, source, project string) (id int64, dedup bool, err error) {
	return addMemoryTx(DB, content, source, project)
}

// addMemoryTx 在给定句柄（可为事务）上写入一条记忆，去重语义同 addMemory。
// 抽出来是为了让批量落盘（flushPendingMemories）把一次刷新的多条放进同一个事务：
// 一个事务只提交一次，比逐条提交少几个数量级的 fsync。
func addMemoryTx(tx *gorm.DB, content, source, project string) (id int64, dedup bool, err error) {
	if source == "" {
		source = "user"
	}
	var existing Memory
	if err := tx.Where("content = ? AND project = ?", content, project).First(&existing).Error; err == nil {
		return existing.ID, true, nil
	}
	now := time.Now().Unix()
	mem := Memory{
		Content:     content,
		CreatedAt:   now,
		LastAccess:  now,
		AccessCount: 0,
		Archived:    false,
		Source:      source,
		Project:     project,
	}
	if err := tx.Create(&mem).Error; err != nil {
		return 0, false, err
	}
	return mem.ID, false, nil
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
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if req.ID == 0 {
		return "", fmt.Errorf("id is required")
	}
	if _, err := loadMemory(req.ID); err != nil {
		return "", err
	}
	result := DB.Delete(&Memory{}, req.ID)
	if result.Error != nil {
		return "", result.Error
	}
	if result.RowsAffected == 0 {
		return "", fmt.Errorf("memory id %d not found", req.ID)
	}
	out, _ := json.Marshal(map[string]interface{}{
		"ok":      true,
		"id":      req.ID,
		"deleted": result.RowsAffected,
	})
	return string(out), nil
}

// ---- 改 (memory_update) ----

func handleUpdate(ctx context.Context, args json.RawMessage) (string, error) {
	flushPendingMemories()
	var req struct {
		ID      int64   `json:"id"`
		Content string  `json:"content"`
		Source  string  `json:"source"`
		Scope   *string `json:"scope"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if req.ID == 0 {
		return "", fmt.Errorf("id is required")
	}

	updates := map[string]interface{}{}
	if req.Content != "" {
		updates["content"] = req.Content
	}
	if req.Source != "" {
		updates["source"] = req.Source
	}
	if req.Scope != nil {
		// 允许改归属（current / global / project:<key>）
		scope, err := parseScope(*req.Scope, "")
		if err != nil {
			return "", err
		}
		project, err := scope.projectValue()
		if err != nil {
			return "", err
		}
		updates["project"] = project
	}
	if len(updates) == 0 {
		return "", fmt.Errorf("at least one of content, source or scope must be provided")
	}
	updates["last_access"] = time.Now().Unix()

	if _, err := loadMemory(req.ID); err != nil {
		return "", err
	}
	result := DB.Model(&Memory{}).Where("id = ?", req.ID).Updates(updates)
	if result.Error != nil {
		return "", result.Error
	}
	if result.RowsAffected == 0 {
		return "", fmt.Errorf("memory id %d not found", req.ID)
	}

	var updated Memory
	DB.First(&updated, req.ID)
	out, _ := json.Marshal(map[string]interface{}{
		"ok":      true,
		"id":      req.ID,
		"updated": result.RowsAffected,
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
	query := scope.where(DB.Model(&Memory{}))
	if req.Source != "" {
		query = query.Where("source = ?", req.Source)
	}
	if req.Archived != nil {
		query = query.Where("archived = ?", *req.Archived)
	} else {
		query = query.Where("archived = ?", false)
	}

	var total int64
	query.Count(&total)

	var memories []Memory
	query.Order("created_at DESC").
		Offset((req.Page - 1) * req.PageSize).
		Limit(req.PageSize).
		Find(&memories)

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
			ID      int64   `json:"id"`
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
			"id":      strconv.FormatInt(r.ID, 10),
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
		ID    int64 `json:"id"`
		Dedup bool  `json:"dedup"`
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
		{Key: "id", Value: strconv.FormatInt(out.ID, 10)},
		{Key: "status", Value: status, Tone: badge.Tone},
	}), nil
}

func memoryDeleteView(result string) (json.RawMessage, error) {
	var out struct {
		OK      bool  `json:"ok"`
		ID      int64 `json:"id"`
		Deleted int64 `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return nil, nil
	}
	return dsc.CardView("MemoryDelete", &dsc.ViewBadge{Text: "deleted", Tone: "yellow"}, []dsc.ViewField{
		{Key: "id", Value: strconv.FormatInt(out.ID, 10)},
		{Key: "rows", Value: strconv.FormatInt(out.Deleted, 10)},
	}), nil
}

func memoryUpdateView(result string) (json.RawMessage, error) {
	var out struct {
		OK     bool  `json:"ok"`
		ID     int64 `json:"id"`
		Memory struct {
			Content string `json:"content"`
			Source  string `json:"source"`
		} `json:"memory"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return nil, nil
	}
	return dsc.CardView("MemoryUpdate", &dsc.ViewBadge{Text: "updated", Tone: "green"}, []dsc.ViewField{
		{Key: "id", Value: strconv.FormatInt(out.ID, 10)},
		{Key: "content", Value: out.Memory.Content},
		{Key: "source", Value: out.Memory.Source},
	}), nil
}

func memoryListView(result string) (json.RawMessage, error) {
	var out struct {
		Memories []struct {
			ID      int64  `json:"id"`
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
			"id":      strconv.FormatInt(m.ID, 10),
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
	if err := initDB(dbPath()); err != nil {
		fmt.Fprintf(os.Stderr, "memory-service: init db failed: %v\n", err)
		os.Exit(2)
	}
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
                        "id": {"type": "integer", "description": "要删除的记忆 ID"}
                },
                "required": ["id"]
        }`)
	updateSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "id": {"type": "integer", "description": "要修改的记忆 ID"},
                        "content": {"type": "string", "description": "新的记忆内容（可选，至少提供 content/source/scope 之一）"},
                        "source": {"type": "string", "description": "新的来源标记（可选）"},
                        "scope": {"type": "string", "description": "改归属（可选）：current=当前项目；global=跨项目共享；project:<项目键>=指定项目"}
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
		Version: "1.1.0",
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
		// 只读查询：声明只读，供 agent 判定同批调用可并行（dsc.CapabilityReadOnly）。
		Capabilities: []string{dsc.CapabilityReadOnly},
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
		Description: "添加一条记忆到记忆库：把值得长期保留的信息（如用户偏好、项目约定、重要结论、本次任务的经验教训）写入记忆库，供后续 memory_search 检索。必须指定 scope：project 归当前项目，global 供跨项目共享（用户偏好、通用约定）。",
		Schema:      addSchema,
		Handler:     handleAdd,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return memoryAddView(result)
		},
	})
	// 删
	sdk.Tool(dsc.Tool{
		Name:        "memory_delete",
		Description: "按 ID 删除一条记忆。删除后不可恢复，FTS 索引同步清除。",
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
		Description: "分页列出记忆库中的记忆，按创建时间降序排列。默认列当前项目（scope=current）；scope=global 列全局层，scope=all 列全库（含 project 归属列，便于按 id 整理）。",
		Schema:      listSchema,
		Handler:     handleList,
		// 只读查询：声明只读，供 agent 判定同批调用可并行（dsc.CapabilityReadOnly）。
		Capabilities: []string{dsc.CapabilityReadOnly},
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
