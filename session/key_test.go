package session

import (
	"strings"
	"testing"
)

// TestSessionKeyForProject 校验项目根路径 → 会话文件名的转换：
// 同项目同名、跨项目隔离，绝不落入硬编码 default.jsonl（空路径除外）。
func TestSessionKeyForProject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"windows abs", `C:\Users\Administrator\Desktop\DeepClean`, "C=--Users--Administrator--Desktop--DeepClean"},
		{"git bash /mnt/c", `/mnt/c/Users/Administrator/Desktop/DeepClean`, "mnt--c--Users--Administrator--Desktop--DeepClean"},
		{"unix home", `/home/jor/DeepClean`, "home--jor--DeepClean"},
		{"unix root", `/`, "default"},
		{"empty", ``, "default"},
		{"illegal chars", `C:\Users\a\b?c*d`, "C=--Users--a--b~4c~5d"},
		// 名字里的 - 与 + 原样保留（分隔符是 "--"，单个 - 不构成歧义）
		{"dash in name", `/home/jor/my-project`, "home--jor--my-project"},
		{"plus in name", `/home/jor/Stra+da`, "home--jor--Stra+da"},
		{"dash and plus", `/w/dsc-for-windows-amd64`, "w--dsc-for-windows-amd64"},
		// 会与分隔符混淆的 "-" 才转义：段首、段尾、连写
		{"leading dash", `/w/-hidden`, "w--~dhidden"},
		{"trailing dash", `/w/hidden-`, "w--hidden~d"},
		{"double dash", `/w/a--b`, "w--a~d~db"},
		{"only dash", `/w/-`, "w--~d"},
		// 编码引导符自身、非盘符冒号、等号
		{"tilde in name", `/w/a~b`, "w--a~~b"},
		{"colon in name", `/home/jor/a:b`, "home--jor--a~cb"},
		{"equals in name", `/w/a=b`, "w--a~eb"},
	}
	for _, c := range cases {
		got := SessionKeyForProject(c.in)
		if c.want == "default" { // 空路径是特殊值，不带哈希
			if got != "default" {
				t.Errorf("%s: SessionKeyForProject(%q) = %q, want %q", c.name, c.in, got, c.want)
			}
			continue
		}
		if !strings.HasPrefix(got, c.want+"-") {
			t.Errorf("%s: SessionKeyForProject(%q) = %q, 应以 %q- 开头", c.name, c.in, got, c.want)
		}
		if h := hashSuffix(got); !isShortHash(h) {
			t.Errorf("%s: 末尾短哈希格式不对: %q（key=%q）", c.name, h, got)
		}
	}
}

// hashSuffix 取键末尾 "-<HASH>" 的哈希部分（无哈希返回空）。
func hashSuffix(key string) string {
	i := strings.LastIndex(key, "-")
	if i < 0 {
		return ""
	}
	return key[i+1:]
}

// isShortHash 校验短哈希形态：8 位大写十六进制。
func isShortHash(h string) bool {
	if len(h) != 8 {
		return false
	}
	for _, r := range h {
		if !(r >= '0' && r <= '9') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// TestSessionKeyShortHash 末尾短哈希的作用与稳定性：
//   - 同一路径必得同一键（跨进程/跨轮稳定，否则会话会「换文件」）；
//   - 哈希取自规范化后的全路径，与文件名编码无关；
//   - 值固定（锁定算法：sha256 前 4 字节，大写十六进制）。
func TestSessionKeyShortHash(t *testing.T) {
	a := SessionKeyForProject(`/home/jor/DeepClean`)
	if b := SessionKeyForProject(`/home/jor/DeepClean`); a != b {
		t.Fatalf("同一路径应稳定: %q vs %q", a, b)
	}
	// Windows 反斜杆与 POSIX 正斜杆指同一路径时必须同键，否则同一项目会被拆成两份会话
	if w := SessionKeyForProject(`C:\Users\jor\proj`); w != SessionKeyForProject(`C:/Users/jor/proj`) {
		t.Fatalf("分隔符写法不同不应拆键: %q vs %q", w, SessionKeyForProject(`C:/Users/jor/proj`))
	}
	if got, want := shortPathHash("home/jor/DeepClean"), "7B7E5C28"; got != want {
		t.Fatalf("短哈希算法变更需同步历史文件: got %q want %q", got, want)
	}
	// 空路径是特殊值，不带哈希
	if got := SessionKeyForProject(""); got != "default" {
		t.Fatalf("空路径应为 default，got %q", got)
	}
}

// TestSessionKeyDistinctPaths 不同路径必须得到不同 key：'-' 曾被同时用作分隔符与
// 名字字符，导致 my-project 与 my/project 撞成同一个会话存档，两个项目的会话/
// 压缩状态会互相串。此处逐个断言两两不相等，含段首/段尾"-"的边界情形。
func TestSessionKeyDistinctPaths(t *testing.T) {
	paths := []string{
		`/w/my-project`,
		`/w/my/project`,
		`/w/my+project`,
		`/w/my-/x`,
		`/w/my/-x`,
		`/w/a--b`,
		`/w/a/b`,
		`/w/a~db`,
		`/w/a:b/c`,
		`/w/a/b/c`,
		`/w/a?b`,
		`/w/a-b`,
		`/w/a*b`,
		`/w/a+b`,
		`/w/a=b`,
	}
	seen := map[string]string{}
	for _, p := range paths {
		key := SessionKeyForProject(p)
		if prev, ok := seen[key]; ok {
			t.Fatalf("键冲突：%q 与 %q 都映射为 %q", prev, p, key)
		}
		seen[key] = p
	}
}

// TestSessionKeyIsFilenameSafe 转换结果必须能直接作为文件名（七端一致，含 Windows）：
// 不含任何路径分隔符与 Windows 非法字符。
func TestSessionKeyIsFilenameSafe(t *testing.T) {
	for _, p := range []string{
		`C:\Users\a\my-project`, `/home/json/x:y`, `/w/a?b*c<d>e"f|g`, `\\server\share\dir`,
		`/w/a--b`, `/w/-lead`, `/w/trail-`, `/w/a~b`,
	} {
		key := SessionKeyForProject(p)
		if key == "" {
			t.Fatalf("空 key: %q", p)
		}
		if strings.ContainsAny(key, `/\\<>:"|?*`) {
			t.Errorf("key 含非法文件名字符: %q → %q", p, key)
		}
	}
}

// TestSessionKeySeparatorUnambiguous 段内不得出现连写 "--"（那是分隔符），
// 否则人工排查时无法区分「名字里的连写 -」与「目录边界」。
func TestSessionKeySeparatorUnambiguous(t *testing.T) {
	key := SessionKeyForProject(`/w/a--b-c-d--e`)
	if strings.Count(key, "--") != 1 {
		t.Fatalf("key=%q 只应有 1 处分隔符 \"--\"", key)
	}
}

// TestSessionKeyPathSpellingNormalized 同一目录的「不同写法」必须映射到同一会话键：
// 尾随分隔符、重复分隔符都归一。否则只是工作区根多打了个斜杆，就会切到一个空会话，
// 现场表现成「历史突然加载不出来了」。
func TestSessionKeyPathSpellingNormalized(t *testing.T) {
	canonical := SessionKeyForProject("O:/Agents/Strada")
	// 锁定规范写法的键值：归一化不得改动既有会话文件名，否则现存会话集体「丢历史」
	if canonical != "O=--Agents--Strada-CD976A24" {
		t.Fatalf("规范路径键值变了（会让现存会话找不到）：%q", canonical)
	}
	for _, variant := range []string{
		"O:/Agents/Strada/",
		"O:/Agents/Strada//",
		"O://Agents//Strada",
		`O:/Agents/Strada`,
		`O:/Agents/Strada/`,
	} {
		if got := SessionKeyForProject(variant); got != canonical {
			t.Errorf("%q 应归一到 %q，实际 %q", variant, canonical, got)
		}
	}
	// UNC 不影响归一：反斜杆/正斜杆两种写法同键，且尾随斜杆同样被吃掉
	unc := SessionKeyForProject(`\\server\share\dir`)
	if alt := SessionKeyForProject(`//server/share/dir/`); alt != unc {
		t.Errorf("UNC 写法应同键: %q vs %q", unc, alt)
	}
	if strings.ContainsAny(unc, `/\\`) {
		t.Errorf("UNC 键含路径分隔符（不能做文件名）: %q", unc)
	}
}
