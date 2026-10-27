package tui

import (
	"context"
	"strings"
	"testing"
)

// TestSortSlashItems 纯函数契约：按命令名字母序、/exit 固定最后、同族内保持声明顺序、
// 大小写不敏感；未知/大写命令也不能挤到 /exit 后面。
func TestSortSlashItems(t *testing.T) {
	in := []compItem{
		{label: "/sandbox workspace"},
		{label: "/help"},
		{label: "/exit"},
		{label: "/sandbox read-only"},
		{label: "/Custom"},
		{label: "/sandbox full-access"},
		{label: "/alpha"},
	}
	got := sortSlashItems(in)
	labels := make([]string, 0, len(got))
	for _, it := range got {
		labels = append(labels, it.label)
	}
	want := []string{
		// alpha < custom < help < sandbox（同族按声明顺序：workspace/read-only/full-access）
		"/alpha", "/Custom", "/help",
		"/sandbox workspace", "/sandbox read-only", "/sandbox full-access",
		"/exit",
	}
	if strings.Join(labels, " ") != strings.Join(want, " ") {
		t.Fatalf("排序结果 = %v, want %v", labels, want)
	}

	// 输入不得被就地修改（菜单每次重建，改到全局声明会累积错乱）
	if in[0].label != "/sandbox workspace" || in[2].label != "/exit" {
		t.Fatalf("sortSlashItems 就地改动了输入: %+v", in)
	}
}

// TestSlashMenuSortedWithExitLast 菜单本身：除 /exit 外按命令名字母序；/mode 各档
// 保持 availableModes 的次序（同族稳定）；/exit 永远最后。
func TestSlashMenuSortedWithExitLast(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	items := m.slashCommandItems()
	if len(items) < 5 {
		t.Fatalf("菜单项过少: %d", len(items))
	}
	if last := items[len(items)-1].label; last != "/exit" {
		t.Fatalf("最后一项应为 /exit, got %q", last)
	}

	name := func(label string) string {
		s := strings.TrimPrefix(label, "/")
		if i := strings.IndexAny(s, " \t"); i >= 0 {
			s = s[:i]
		}
		return strings.ToLower(s)
	}
	prev := ""
	modeOrder := make([]string, 0, 4)
	for _, it := range items[:len(items)-1] {
		n := name(it.label)
		if n < prev {
			t.Fatalf("菜单未按字母序: %q 出现在 %q 之后", n, prev)
		}
		prev = n
		if n == "mode" {
			modeOrder = append(modeOrder, it.label)
		}
	}

	// 同一命令族内的次序 = 声明次序（权限递增、生命周期顺序都不能被字母序打乱）
	var sandbox []string
	for _, it := range items {
		if strings.HasPrefix(it.label, "/sandbox ") {
			sandbox = append(sandbox, it.label)
		}
	}
	wantSandbox := []string{"/sandbox read-only", "/sandbox workspace", "/sandbox full-access"}
	if strings.Join(sandbox, "|") != strings.Join(wantSandbox, "|") {
		t.Fatalf("/sandbox 三档次序被打乱: %v", sandbox)
	}

	// /mode 档位顺序应跟随 availableModes（本身已排序），而不是被二次打散
	modes := m.availableModes()
	if len(modeOrder) != len(modes) {
		t.Fatalf("/mode 项 %d 个, want %d", len(modeOrder), len(modes))
	}
	for i, mode := range modes {
		if modeOrder[i] != "/mode "+mode {
			t.Fatalf("/mode 次序 %v, want 跟随 %v", modeOrder, modes)
		}
	}
}
