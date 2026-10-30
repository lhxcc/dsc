package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	dsc "dsc-sdk"
)

// TestEachProjectOwnsItsDBFile 分库的核心契约：不同项目写各自的文件，
// 一个项目的记忆不会落进另一个项目（线上多开时这才是「互不影响」）。
func TestEachProjectOwnsItsDBFile(t *testing.T) {
	dir := setupMemoryDir(t)
	asProject(t, "/w/proj-a")
	keyA := dsc.ProjectSessionKey()
	addVia(t, "A 的私有约定", "current")

	asProject(t, "/w/proj-b")
	keyB := dsc.ProjectSessionKey()
	addVia(t, "B 的私有约定", "current")

	if keyA == keyB {
		t.Fatal("测试前提：两个项目的键必须不同")
	}
	for _, tc := range []struct {
		key   string
		want  int64
		other string
	}{
		{keyA, 1, "B 的私有约定"},
		{keyB, 1, "A 的私有约定"},
	} {
		path := filepath.Join(dir, tc.key+".db")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("项目 %s 应有独立库文件 %s: %v", tc.key, tc.key+".db", err)
		}
		var n int64
		dbFor(t, tc.key).Model(&Memory{}).Count(&n)
		if n != tc.want {
			t.Fatalf("%s 库应有 %d 条，实际 %d", tc.key, tc.want, n)
		}
		var mem Memory
		if err := dbFor(t, tc.key).Where("content = ?", tc.other).First(&mem).Error; err == nil {
			t.Fatalf("%s 库里不该出现别的项目的记忆：%+v", tc.key, mem)
		}
	}
}

// TestIDsAreGloballyUnique 记忆 id 是跨库稳定的字符串：两个项目里同样的内容
// 各自成条、id 不同，且只凭 id 就能定位到它所在的库（跨库管理的前提）。
func TestIDsAreGloballyUnique(t *testing.T) {
	setupMemoryDir(t)
	asProject(t, "/w/proj-a")
	a := addVia(t, "构建用 ninja", "current")

	asProject(t, "/w/proj-b")
	b := addVia(t, "构建用 ninja", "current")

	idA, _ := a["id"].(string)
	idB, _ := b["id"].(string)
	if idA == "" || idB == "" {
		t.Fatalf("id 应为非空字符串，got %v / %v", a["id"], b["id"])
	}
	if idA == idB {
		t.Fatalf("不同项目中的两条记忆不应共享 id：%s", idA)
	}

	// 当前项目是 B，但 A 的那条也要能按 id 找到，并报出它自己的归属
	mem, _, err := findMemory(idA)
	if err != nil {
		t.Fatalf("findMemory(%s): %v", idA, err)
	}
	if mem.Project != keyOf(t, "/w/proj-a") {
		t.Fatalf("A 的条目归属应是 A 的键，got %q", mem.Project)
	}
	if mem.Content != "构建用 ninja" {
		t.Fatalf("内容不符: %q", mem.Content)
	}
}

// TestListAcrossProjectsPaginates 跨库列表的分页是「归并后再切页」：
// 两个项目各两条，page_size=2 时第 1 页应是全局最新的两条，且两页不重不漏。
func TestListAcrossProjectsPaginates(t *testing.T) {
	setupMemoryDir(t)
	asProject(t, "/w/proj-a")
	addVia(t, "A 第一条", "current")
	addVia(t, "A 第二条", "current")
	asProject(t, "/w/proj-b")
	addVia(t, "B 第一条", "current")
	addVia(t, "B 第二条", "current")

	seen := map[string]bool{}
	for page := 1; page <= 2; page++ {
		out, err := handleList(context.Background(), mustJSON(map[string]any{"scope": "all", "page": page, "page_size": 2}))
		if err != nil {
			t.Fatalf("memory_list: %v", err)
		}
		var res struct {
			Memories []struct {
				ID      string `json:"id"`
				Content string `json:"content"`
			} `json:"memories"`
			Total int64 `json:"total"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("parse list: %v", err)
		}
		if res.Total != 4 {
			t.Fatalf("跨库 total 应为 4，got %d", res.Total)
		}
		if len(res.Memories) != 2 {
			t.Fatalf("每页应 2 条，第 %d 页 got %d", page, len(res.Memories))
		}
		for _, m := range res.Memories {
			if seen[m.ID] {
				t.Fatalf("分页出现重复条目 %s（第 %d 页）", m.ID, page)
			}
			seen[m.ID] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("两页合计应覆盖 4 条，got %d", len(seen))
	}
}

// TestLegacySingleDBIsRetired 旧单库不再参与：启动时改名留档，任何项目都不读它。
// 按 AGENTS.md §4 不做数据迁移，但也不静默删除——用户还能自己从旧文件里捞。
func TestLegacySingleDBIsRetired(t *testing.T) {
	dir := setupMemoryDir(t)
	legacy := filepath.Join(dir, "memory.db")
	if err := os.WriteFile(legacy, []byte("not a real db"), 0o644); err != nil {
		t.Fatal(err)
	}
	retireLegacyDB()

	if _, err := os.Stat(legacy); err == nil {
		t.Fatal("旧 memory.db 应已改名")
	}
	if _, err := os.Stat(legacy + ".pre-project-split"); err != nil {
		t.Fatalf("旧库应留档为 memory.db.pre-project-split: %v", err)
	}
	keys, err := projectDBKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k == legacyDBKey {
			t.Fatalf("旧库不应被当成项目库: %v", keys)
		}
	}
}
