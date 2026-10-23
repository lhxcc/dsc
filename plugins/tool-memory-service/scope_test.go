package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	dsc "dsc-sdk"
	"dsc/core"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupMemoryDB 把全局 DB 指到临时库，测试结束恢复（DB 是包级单例）。
func setupMemoryDB(t *testing.T) {
	t.Helper()
	old := DB
	if err := initDB(filepath.Join(t.TempDir(), "memory.db")); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		DB = old
	})
}

// asProject 切换当前工作区（项目键由它派生，与会话文件名同一套规则）。
// 插件进程里工作区根是 core.WorkspaceRoot（宿主启动时按 DSC_WORKSPACE_ROOT 设定），
// 故测试直接改该变量，而不是设环境变量（已加载的包级变量不会跟随 env 变化）。
func asProject(t *testing.T, root string) {
	t.Helper()
	old := core.WorkspaceRoot
	core.WorkspaceRoot = root
	t.Cleanup(func() { core.WorkspaceRoot = old })
}

func addVia(t *testing.T, content, scope string) map[string]any {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"content": content, "scope": scope})
	out, err := handleAdd(context.Background(), args)
	if err != nil {
		t.Fatalf("memory_add(%q): %v", content, err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse add result: %v", err)
	}
	return res
}

func searchVia(t *testing.T, keyword, scope string) []map[string]any {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"keywords": keyword, "scope": scope})
	out, err := handleSearch(context.Background(), args)
	if err != nil {
		t.Fatalf("memory_search(%q, scope=%s): %v", keyword, scope, err)
	}
	var res struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse search result: %v", err)
	}
	return res.Results
}

// TestProjectScopeIsolation 默认只搜当前项目 + 全局：A 项目的记忆不得出现在 B 项目，
// 需要跨项目翻查时显式 scope=all。
func TestProjectScopeIsolation(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	addVia(t, "proj-a 使用 CMake 而非 Make", "project")

	asProject(t, "/w/proj-b")
	if got := searchVia(t, "CMake", "project"); len(got) != 0 {
		t.Fatalf("B 项目默认搜索不应看到 A 项目记忆: %+v", got)
	}
	if got := searchVia(t, "CMake", ""); len(got) != 0 {
		t.Fatalf("scope 缺省应按 project 处理: %+v", got)
	}
	if got := searchVia(t, "CMake", "all"); len(got) != 1 {
		t.Fatalf("scope=all 应可跨项目检索，got %d 条", len(got))
	}

	// A 项目自己当然看得到
	asProject(t, "/w/proj-a")
	if got := searchVia(t, "CMake", "project"); len(got) != 1 {
		t.Fatalf("A 项目应看到自己的记忆，got %d 条", len(got))
	}
}

// TestGlobalScopeVisibleEverywhere scope=global 的记忆（用户偏好、通用约定）
// 在所有项目可见；scope=global 检索则只返回这些。
func TestGlobalScopeVisibleEverywhere(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	addVia(t, "用户偏好中文回复", "global")
	addVia(t, "proj-a 私有约定", "project")

	asProject(t, "/w/proj-b")
	if got := searchVia(t, "中文", "project"); len(got) != 1 {
		t.Fatalf("全局记忆应在任意项目可见，got %d 条", len(got))
	}
	if got := searchVia(t, "私有约定", "global"); len(got) != 0 {
		t.Fatalf("scope=global 不应返回项目记忆，got %d 条", len(got))
	}
	if got := searchVia(t, "私有约定", "all"); len(got) != 1 {
		t.Fatalf("scope=all 应返回项目记忆，got %d 条", len(got))
	}

	// memory_list 默认同样按 project 过滤（含全局）
	args, _ := json.Marshal(map[string]any{"scope": "project"})
	out, err := handleList(context.Background(), args)
	if err != nil {
		t.Fatalf("memory_list: %v", err)
	}
	var list struct {
		Memories []struct {
			Content string `json:"content"`
			Project string `json:"project"`
		} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("parse list: %v", err)
	}
	if len(list.Memories) != 1 || list.Memories[0].Content != "用户偏好中文回复" {
		t.Fatalf("B 项目列表应只含全局条目，got %+v", list.Memories)
	}
	if list.Memories[0].Project != "" {
		t.Fatalf("全局条目的 project 应为空串，got %q", list.Memories[0].Project)
	}
}

// TestDedupPerProject 去重按「项目 + 内容」：两个项目各自记同一结论是两条独立记忆，
// 否则 A 项目写入会导致 B 项目误以为「已有」而拿不到自己的那条。
func TestDedupPerProject(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	first := addVia(t, "构建用 ninja", "project")

	asProject(t, "/w/proj-b")
	second := addVia(t, "构建用 ninja", "project")
	if first["id"] == second["id"] {
		t.Fatalf("不同项目应各自成条，got 同 id %v", first["id"])
	}
	if dedup, _ := second["dedup"].(bool); dedup {
		t.Fatal("跨项目不应判为重复")
	}

	// 同项目重复写入 → 去重
	again := addVia(t, "构建用 ninja", "project")
	if dedup, _ := again["dedup"].(bool); !dedup {
		t.Fatal("同项目同内容应去重")
	}
	if again["id"] != second["id"] {
		t.Fatalf("去重应返回既有 id，got %v want %v", again["id"], second["id"])
	}
}

// TestProjectLabel 空项目键渲染为「全局」，其余原样展示。
func TestProjectLabel(t *testing.T) {
	if got := projectLabel(""); got != "全局" {
		t.Fatalf("空键应显示全局，got %q", got)
	}
	if got := projectLabel("w--proj-a-1234ABCD"); got != "w--proj-a-1234ABCD" {
		t.Fatalf("项目键应原样展示，got %q", got)
	}
}

// TestMemoryProjectColumnStamp 写入时把当前项目键落到 project 列（与会话键同源）。
func TestMemoryProjectColumnStamp(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	addVia(t, "记录一条", "project")

	var mem Memory
	if err := DB.Where("content = ?", "记录一条").First(&mem).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if want := dsc.ProjectSessionKey(); mem.Project != want {
		t.Fatalf("project = %q, want %q", mem.Project, want)
	}
}

// TestAutoMigrateAddsProjectColumn 旧的单库（no project column）升级时自动补列，
// 历史条目归为空项目键，按「全局」语义继续可检索——不需要单独的数据迁移脚本。
func TestAutoMigrateAddsProjectColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if err := legacy.Exec(`CREATE TABLE memories (
		id integer primary key autoincrement,
		content text not null,
		created_at integer not null,
		last_access integer not null,
		access_count integer not null default 0,
		archived numeric not null default false,
		source text default 'user')`).Error; err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if err := legacy.Exec(`INSERT INTO memories (content, created_at, last_access) VALUES ('旧条目', 1, 1)`).Error; err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if sqlDB, err := legacy.DB(); err == nil {
		_ = sqlDB.Close()
	}

	old := DB
	if err := initDB(path); err != nil {
		t.Fatalf("initDB on legacy db: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		DB = old
	})

	var mem Memory
	if err := DB.Where("content = ?", "旧条目").First(&mem).Error; err != nil {
		t.Fatalf("升级后应仍能读到旧条目: %v", err)
	}
	if mem.Project != "" {
		t.Fatalf("历史条目应归为空项目键（全局），got %q", mem.Project)
	}
}
