package session

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store 会话存储：按会话 ID 在目录下各存一个 JSONL 文件（<id>.jsonl），
// 提供多会话的创建、加载、保存、删除与列表（事件数、最后活动时间、首条
// user 内容预览）。对齐 DSH SessionStore 的存储 seam；会话内容本身仍是
// 事件溯源日志（见 persist.go）。

// SessionKeyForProject 把项目根路径转换为默认会话文件名（key）：
// 统一 / 分隔 → 去掉根斜杆 → 各级目录以 "--" 连接。
// 保证「同一项目同名、不同项目隔离」——默认会话按当前工作区命名，不再使用
// 硬编码的 default.jsonl，避免跨项目历史被混入注入上下文。
//
// 为什么要转义：早先分隔符与名字共用 "-"，于是 /w/my-project 与 /w/my/project
// 得到同一个 key，两个不相干的项目共用一份会话存档与压缩状态。现在分隔符是
// 双字符 "--"，名字里单个 "-"（以及 "+"）可原样保留，只在会产生歧义处转义：
//
//	键末尾追加 -<HASH>           → 规范化前全路径的短哈希（8 位大写十六进制）
//	路径分隔 /                   → "--"
//	盘符冒号 C: 的 ":"            → "="
//	名字里的 "-"（段首/段尾，或与相邻 "-" 连写） → "~d"
//	名字里的 "~"                 → "~~"
//	名字里的 ":"（非盘符）        → "~c"
//	名字里的 "="                 → "~e"
//	Windows 文件名非法字符 < > " | ? * → "~0" … "~5"
//
// 末尾的短哈希是最后一道防撞保险：即使编码规则将来演进出现遗漏，两个不同路径
// 也不会落到同一个文件上（编码相同但路径不同时，哈希必然不同）。
//
// "~" 是引导符且其后固定再跟一个字符，故解码无歧义；每段的首尾都不会是 "-"
// （歧义处的 "-" 一律写成 "~d"），故段内既不会出现连写的 "-"，也不会与分隔符
// "--" 拼成 "---"。名字里的 "-"/"+" 绝大多数情况原样保留（可读性），
// 只有 "~" 会被写成 "~~"（路径里极少出现）。
//
// 例：
//
//	C:\Users\Administrator\Desktop\DeepClean → C=--Users--Administrator--Desktop--DeepClean
//	/mnt/c/Users/Administrator/Desktop/DeepClean → mnt--c--Users--Administrator--Desktop--DeepClean
//	/home/jor/my-project → home--jor--my-project（"-" 原样保留，与分隔符区分）
//	/w/a--b → w--a~d~db（名字里连写的 "-" 才转义）
func SessionKeyForProject(projectRoot string) string {
	// 统一反斜杆为正斜杆，必须按规则 10 写**两行连续替换**、顺序不可颠倒：
	// 先处理双反斜杆（网络路径 `\\server\share` 与 JSON 转义残留都可能出现），再处理
	// 单个。只写一行单反斜杆替换会把 `\\server` 转成 `//server`——多出一个分隔符，
	// 虽然后面的归一能吃掉，但那是靠下游兜底，不是这行该有的语义。
	// 也不能用平台相关转换函数：在 Linux 上 `\` 不是分隔符，Windows 风格输入不会被
	// 转换（AGENTS.md §10）。
	p := strings.ReplaceAll(projectRoot, `\\`, "/")
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "/")
	p = normalizeProjectPath(p)
	if p == "" {
		return "default"
	}
	segs := strings.Split(p, "/")
	var b strings.Builder
	b.Grow(len(p) + 16)
	for i, seg := range segs {
		if i > 0 {
			b.WriteString("--")
		}
		// 盘符：段首单个字母 + ":"（C:），冒号编码成 "="
		if i == 0 && len(seg) >= 2 && seg[1] == ':' {
			b.WriteByte(seg[0])
			b.WriteByte('=')
			seg = seg[2:]
		}
		writeKeySegment(&b, seg)
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String() + "-" + shortPathHash(p)
}

// normalizeProjectPath 把「同一目录的不同写法」归一到同一形态：折叠重复分隔符、
// 去掉尾随分隔符。会话键由此路径编码 + 短哈希而来，写法不同就等于换了一个会话——
// 现场表现是「历史突然加载不出来」，实际只是工作区根多了个分隔符：
//
//	O:/Agents/Strada   → O=--Agents--Strada-CD976A24
//	O:/Agents/Strada/  → O=--Agents--Strada---93781973   ← 同一目录，却是另一个会话
//
// 归一不用 core.PClean：core 已经 import session（manager.go / session_search.go），
// 反向 import 会成环。规则 11 对此有明文安排——session 包用本地等价实现
// session/posix.go（守卫白名单里的唯一 filepath 使用点），此处即调它的 posixClean。
//
// 不用标准库 path 包的 Clean：它只认 `/` 当分隔符，反斜杆会被当成普通字符原样保留
// （实测对 `O:\a\b` 调用后结果仍含反斜杆），那样会话键会混进文件名非法字符。
//
// 前导分隔符先全部去掉，UNC 与绝对路径同等对待：Windows 的 filepath 清理函数保留
// `\\server\share` 的双前导、POSIX 压成单个，不去掉的话同一台网络共享在两类平台上
// 会算出不同的键（代价是相对路径 server/share 与之同键——工作区根恒为绝对路径，无实害）。
// 盘符大小写不折叠（Windows 上 os.Getwd 已是规范大小写，折叠会
// 改掉现存所有键）；大小写不一致由启动日志的 session identity 行暴露。
func normalizeProjectPath(p string) string {
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return ""
	}
	return posixClean(p)
}

// keyHashLen 是键末尾短哈希的字符数（8 位大写十六进制，等同 32 位）。
const keyHashLen = 8

// shortPathHash 返回规范化路径的短哈希（大写十六进制，git 短哈希风格）。
// 追加在键末尾，用于兜住编码规则可能存在的一切遗漏：路径不同则哈希必不同。
func shortPathHash(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return strings.ToUpper(hex.EncodeToString(sum[:keyHashLen/2]))
}

// writeKeySegment 按 SessionKeyForProject 的转义规则写入单个路径段。
func writeKeySegment(b *strings.Builder, seg string) {
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch c {
		case '-':
			// 只在会产生歧义处转义：段首/段尾，或与相邻 "-" 连写（否则会被读成分隔符 "--"）
			if i == 0 || i == len(seg)-1 || seg[i-1] == '-' || seg[i+1] == '-' {
				b.WriteString("~d")
			} else {
				b.WriteByte('-')
			}
		case '~':
			b.WriteString("~~")
		case ':':
			b.WriteString("~c")
		case '=':
			b.WriteString("~e")
		case '<', '>', '"', '|', '?', '*':
			// 这些字符在 Windows 文件名里非法，按固定码替换（保证仍是单射）
			b.WriteByte('~')
			b.WriteByte(illegalCharCode(c))
		default:
			b.WriteByte(c)
		}
	}
}

// illegalCharCode 返回 Windows 文件名非法字符的固定单字符码（'0'–'5'）。
func illegalCharCode(c byte) byte {
	switch c {
	case '<':
		return '0'
	case '>':
		return '1'
	case '"':
		return '2'
	case '|':
		return '3'
	case '?':
		return '4'
	default: // '*'
		return '5'
	}
}

// SessionInfo 会话列表条目（轻量元数据，不加载全部事件）。
type SessionInfo struct {
	ID       string    `json:"id"`
	Events   int       `json:"events"`
	Turns    int       `json:"turns"`
	LastTime time.Time `json:"last_time"`
	// Preview 是最近一条真实用户消息（Source 为 user/空）的截断预览。
	// 不取首条：长会话里首条往往只是早期的探索命令（如 "ls -l"），
	// 无法说明当前在做什么。
	Preview string `json:"preview"`
}

// 会话存储的文件命名与目录约定（全仓唯一实现，宿主与各插件一律引用，
// 不得自行拼字符串——多方实现会在分隔符/后缀/校验上产生分歧）。
const (
	// DirName 是会话数据目录名（相对 DSC ExecDir）。
	DirName = "sessions"
	// FileSuffix 是会话事件日志后缀：<id>.jsonl。
	FileSuffix = ".jsonl"
)

// Dir 返回会话数据目录（ExecDir 由宿主经 DSC 环境注入，各进程一致）。
func Dir(execDir string) string {
	return posixJoin(execDir, DirName)
}

// ValidID 校验会话 id 是否为安全的单组件文件名：非空、不含路径分隔符、
// 不含路径穿越段（..）或绝对路径特征，避免 <id>.<suffix> 逃逸会话目录。
func ValidID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return false
	}
	return true
}

// Store 按目录管理多会话。
type Store struct {
	dir    string
	mu     sync.Mutex
	nextID int // 下一个会话编号（NewStore 时按目录现有最大编号初始化）
}

// NewStore 创建/打开会话存储目录（自动创建）。
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("session store: mkdir %s: %w", dir, err)
	}
	s := &Store{dir: dir, nextID: 1}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("session store: read dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), FileSuffix) {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(e.Name(), "session-%d"+FileSuffix, &n); err == nil && n >= s.nextID {
			s.nextID = n + 1
		}
	}
	return s, nil
}

// path 返回指定会话的文件路径；id 须为安全的单组件文件名，拒绝路径穿越。
func (s *Store) path(id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("session store: invalid session id %q", id)
	}
	return posixJoin(s.dir, id+FileSuffix), nil
}

// Create 创建新会话，id 为 session-<n>（按内存计数器递增，避免未落盘会话重复编号）。
func (s *Store) Create() (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := New()
	sess.id = fmt.Sprintf("session-%d", s.nextID)
	s.nextID++
	return sess, nil
}

// Load 加载指定会话；不存在返回 (nil, nil)。
func (s *Store) Load(id string) (*Session, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	sess, err := Load(p)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		sess.id = id
	}
	return sess, nil
}

// Ensure 加载指定会话，不存在则新建并绑定该 id（供固定会话 id 的场景复用）。
func (s *Store) Ensure(id string) (*Session, error) {
	sess, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		sess = New()
		sess.id = id
	}
	return sess, nil
}

// Save 原子落盘指定会话（按会话 ID）。
func (s *Store) Save(sess *Session) error {
	if sess.id == "" {
		return fmt.Errorf("session store: cannot save session without id")
	}
	p, err := s.path(sess.id)
	if err != nil {
		return err
	}
	return sess.Save(p)
}

// Fork 从已落盘会话 srcID 复制到 boundary（含）创建新会话：分配新 id 并落盘。
// 父会话不存在或 boundary 落在开放轮次/步骤内时返回错误。
func (s *Store) Fork(srcID string, boundary int) (*Session, error) {
	parent, err := s.Load(srcID)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, fmt.Errorf("session store: session %q not found", srcID)
	}
	child, err := parent.Fork(boundary)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	child.id = fmt.Sprintf("session-%d", s.nextID)
	s.nextID++
	s.mu.Unlock()
	if err := s.Save(child); err != nil {
		return nil, err
	}
	return child, nil
}

// Delete 删除指定会话；不存在视为幂等成功。
func (s *Store) Delete(id string) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("session store: remove %s: %w", id, err)
	}
	return nil
}

// List 列出所有会话的轻量元数据（按最后活动时间倒序）。
func (s *Store) List() ([]SessionInfo, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("session store: read dir: %w", err)
	}
	var infos []SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), FileSuffix) {
			continue
		}
		info, err := scanInfo(posixJoin(s.dir, e.Name()))
		if err != nil {
			continue // 单会话损坏不影响列表
		}
		info.ID = strings.TrimSuffix(e.Name(), FileSuffix)
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].LastTime.Equal(infos[j].LastTime) {
			return infos[i].LastTime.After(infos[j].LastTime)
		}
		return infos[i].ID > infos[j].ID // 同毫秒按 id 倒序（编号大者较新）
	})
	return infos, nil
}

// scanInfo 轻量扫描单个 JSONL：事件数（行数）、轮次数、最后事件时间、
// 最近一条真实用户消息预览。
func scanInfo(path string) (SessionInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionInfo{}, err
	}
	defer f.Close()
	var info SessionInfo
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		info.Events++
		var ej eventJSON
		if err := json.Unmarshal([]byte(text), &ej); err != nil {
			continue
		}
		if ej.Time > 0 {
			info.LastTime = time.UnixMilli(ej.Time)
		}
		switch ej.Type {
		case TurnStart:
			info.Turns++
		case UserMessage:
			// 最近一条真实用户消息作为预览；goal/guard/截断续行等合成消息
			// 不代表用户意图，忽略（Source 为空视为旧格式的真实用户消息）。
			var d UserMessageData
			if err := json.Unmarshal(ej.Data, &d); err != nil {
				continue
			}
			if src := strings.TrimSpace(d.Source); src != "" && src != "user" {
				continue
			}
			if content := strings.Join(strings.Fields(d.Content), " "); content != "" {
				info.Preview = truncate(content, 40)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return SessionInfo{}, err
	}
	return info, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
