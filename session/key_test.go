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
		{"windows abs", `C:\Users\Administrator\Desktop\DeepClean`, "C--Users-Administrator-Desktop-DeepClean"},
		{"git bash /mnt/c", `/mnt/c/Users/Administrator/Desktop/DeepClean`, "mnt-c-Users-Administrator-Desktop-DeepClean"},
		{"unix home", `/home/jor/DeepClean`, "home-jor-DeepClean"},
		{"unix root", `/`, "default"},
		{"empty", ``, "default"},
		{"illegal chars", `C:\Users\a\b?c*d`, "C--Users-a-b+4c+5d"},
		// 名字里的 - 必须与分隔符区分（否则 my-project 与 my/project 撞键）
		{"dash in name", `/home/jor/my-project`, "home-jor-my+-project"},
		{"leading dash name", `/home/jor/-hidden`, "home-jor-+-hidden"},
		{"plus in name", `/home/jor/a+b`, "home-jor-a++b"},
		// 名字里的冒号也属名字，只有盘符冒号才算分隔
		{"colon in name", `/home/jor/a:b`, "home-jor-a+=b"},
		{"drive colon plus sep", `C:\a-b`, "C--a+-b"},
	}
	for _, c := range cases {
		if got := SessionKeyForProject(c.in); got != c.want {
			t.Errorf("%s: SessionKeyForProject(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// TestSessionKeyDistinctPaths 不同路径必须得到不同 key：'-' 曾被同时用作分隔符与
// 名字字符，导致 my-project 与 my/project（以及 a+b 与 a-b 等）撞成同一个会话
// 存档，两个项目的会话/压缩状态会互相串。此处逐个断言两两不相等。
func TestSessionKeyDistinctPaths(t *testing.T) {
	paths := []string{
		`/w/my-project`,
		`/w/my/project`,
		`/w/my+project`, // 转义引导符本身出现在名字里
		`/w/my+-project`,
		`/w/my--project`,
		`/w/a:b/c`,
		`/w/a/b/c`,
		`/w/a?b`,
		`/w/a-b`,
		`/w/a*b`,
		`/w/a+b`,
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
