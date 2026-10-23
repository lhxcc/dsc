package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	dsc "dsc-sdk"
	"dsc/core"
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

// TestProjectScopeIsolation project 档严格等于「当前项目」：A 项目的记忆不得出现在
// B 项目，也没有跨项目翻查的档位（别的项目的私有细节不该被顺手带进上下文）。
func TestProjectScopeIsolation(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	addVia(t, "proj-a 使用 CMake 而非 Make", "project")

	asProject(t, "/w/proj-b")
	if got := searchVia(t, "CMake", "project"); len(got) != 0 {
		t.Fatalf("B 项目不应看到 A 项目记忆: %+v", got)
	}
	if got := searchVia(t, "CMake", ""); len(got) != 0 {
		t.Fatalf("scope 缺省应按 project 处理: %+v", got)
	}
	// 未知档位直接报错，不会静默换成「全库」或别的范围
	if _, err := handleSearch(context.Background(), mustJSON(map[string]string{"keywords": "CMake", "scope": "all"})); err == nil {
		t.Fatal("未知 scope 应报错")
	}

	// A 项目自己看得到
	asProject(t, "/w/proj-a")
	if got := searchVia(t, "CMake", "project"); len(got) != 1 {
		t.Fatalf("A 项目应看到自己的记忆，got %d 条", len(got))
	}
}

// TestGlobalScopeIsSeparate global 档只含全局共享层：任一项目都能显式查到，
// 但 project 档（默认）不会把它混进来——两档严格互斥，不做隐式合并。
func TestGlobalScopeIsSeparate(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	addVia(t, "用户偏好中文回复", "global")
	addVia(t, "proj-a 私有约定", "project")

	asProject(t, "/w/proj-b")
	if got := searchVia(t, "中文", "global"); len(got) != 1 {
		t.Fatalf("全局层在任意项目都应可查到，got %d 条", len(got))
	}
	if got := searchVia(t, "中文", "project"); len(got) != 0 {
		t.Fatalf("project 档不应混入全局条目，got %d 条", len(got))
	}
	if got := searchVia(t, "私有约定", "global"); len(got) != 0 {
		t.Fatalf("global 档不应返回项目记忆，got %d 条", len(got))
	}

	// memory_list 各自只列自己那一档
	args, _ := json.Marshal(map[string]any{"scope": "global"})
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
		t.Fatalf("global 列表应只含全局条目，got %+v", list.Memories)
	}
	if list.Memories[0].Project != globalProject {
		t.Fatalf("全局条目的 project 应为 %q，got %q", globalProject, list.Memories[0].Project)
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

	// 全局层是同内容第三份，独立成条（不顶掉项目里的）
	if g := addVia(t, "构建用 ninja", "global"); g["dedup"] == true {
		t.Fatal("全局条目不应与项目条目判为重复")
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

// TestProjectLabel 全局哨兵显示「全局」，空值（缺 project 列的旧库）显示「未归属」，
// 其余原样展示。
func TestProjectLabel(t *testing.T) {
	if got := projectLabel(globalProject); got != "全局" {
		t.Fatalf("全局哨兵应显示全局，got %q", got)
	}
	if got := projectLabel("w--proj-1A2B3C4D"); got != "w--proj-1A2B3C4D" {
		t.Fatalf("项目键应原样展示，got %q", got)
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

// mustJSON 测试辅助：把任意值编码为 json.RawMessage。
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestAddRequiresScope scope 必填且只接受两档：缺省或乱填都应报错，
// 不允许写出「没有归属」的记忆（那种数据无法管理）。
func TestAddRequiresScope(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")

	if _, err := handleAdd(context.Background(), mustJSON(map[string]string{"content": "缺归属"})); err == nil {
		t.Fatal("缺 scope 应报错")
	}
	if _, err := handleAdd(context.Background(), mustJSON(map[string]string{"content": "乱填", "scope": "all"})); err == nil {
		t.Fatal("非法 scope 应报错")
	}
	if _, err := handleAdd(context.Background(), mustJSON(map[string]string{"content": "空串", "scope": " "})); err == nil {
		t.Fatal("空白 scope 应报错")
	}

	var count int64
	DB.Model(&Memory{}).Count(&count)
	if count != 0 {
		t.Fatalf("报错的写入不应落库，got %d 行", count)
	}

	// 合法写入仍正常
	addVia(t, "有归属", "project")
	DB.Model(&Memory{}).Count(&count)
	if count != 1 {
		t.Fatalf("合法写入应落 1 行，got %d", count)
	}
}

// TestUpdateScopeValidates 改归属时给非法值要报错；不传 scope 则不动归属。
func TestUpdateScopeValidates(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	res := addVia(t, "条目", "project")
	id := int64(res["id"].(float64))

	if _, err := handleUpdate(context.Background(), mustJSON(map[string]any{"id": id, "scope": "all"})); err == nil {
		t.Fatal("非法 scope 应报错")
	}
	var mem Memory
	if err := DB.First(&mem, id).Error; err != nil {
		t.Fatal(err)
	}
	if mem.Project != currentProjectKey() {
		t.Fatalf("报错不应改归属，got %q", mem.Project)
	}

	// 不传 scope、只改内容 → 归属不变
	if _, err := handleUpdate(context.Background(), mustJSON(map[string]any{"id": id, "content": "条目2"})); err != nil {
		t.Fatalf("改内容应成功: %v", err)
	}
	DB.First(&mem, id)
	if mem.Project != currentProjectKey() {
		t.Fatalf("未传 scope 时归属应保持，got %q", mem.Project)
	}

	// 显式改到全局层
	if _, err := handleUpdate(context.Background(), mustJSON(map[string]any{"id": id, "scope": "global"})); err != nil {
		t.Fatalf("改到全局应成功: %v", err)
	}
	DB.First(&mem, id)
	if mem.Project != globalProject {
		t.Fatalf("应改到全局哨兵，got %q", mem.Project)
	}
}
