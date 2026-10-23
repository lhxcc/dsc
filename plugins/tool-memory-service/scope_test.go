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
	addVia(t, "proj-a 使用 CMake 而非 Make", "current")

	asProject(t, "/w/proj-b")
	if got := searchVia(t, "CMake", "current"); len(got) != 0 {
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
	if got := searchVia(t, "CMake", "current"); len(got) != 1 {
		t.Fatalf("A 项目应看到自己的记忆，got %d 条", len(got))
	}
}

// TestGlobalScopeIsSeparate global 档只含全局共享层：任一项目都能显式查到，
// 但 project 档（默认）不会把它混进来——两档严格互斥，不做隐式合并。
func TestGlobalScopeIsSeparate(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	addVia(t, "用户偏好中文回复", "global")
	addVia(t, "proj-a 私有约定", "current")

	asProject(t, "/w/proj-b")
	if got := searchVia(t, "中文", "global"); len(got) != 1 {
		t.Fatalf("全局层在任意项目都应可查到，got %d 条", len(got))
	}
	if got := searchVia(t, "中文", "current"); len(got) != 0 {
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
	first := addVia(t, "构建用 ninja", "current")

	asProject(t, "/w/proj-b")
	second := addVia(t, "构建用 ninja", "current")
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
	again := addVia(t, "构建用 ninja", "current")
	if dedup, _ := again["dedup"].(bool); !dedup {
		t.Fatal("同项目同内容应去重")
	}
	if again["id"] != second["id"] {
		t.Fatalf("去重应返回既有 id，got %v want %v", again["id"], second["id"])
	}
}

// TestMemoryProjectColumnStamp 写入时把当前项目键落到 project 列（与会话键同源）。
func TestMemoryProjectColumnStamp(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	addVia(t, "记录一条", "current")

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
	addVia(t, "有归属", "current")
	DB.Model(&Memory{}).Count(&count)
	if count != 1 {
		t.Fatalf("合法写入应落 1 行，got %d", count)
	}
}

// TestUpdateScopeValidates 改归属时给非法值要报错；不传 scope 则不动归属。
func TestUpdateScopeValidates(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	res := addVia(t, "条目", "current")
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

// TestIDOpsRejectOtherProjects scope 只接受 current/global，无法指定别的项目键；
// 更要紧的是按 ID 操作也不能越界——ID 自增可猜，若无归属校验，模型能用
// delete/update 遍历删改其它项目的记忆（甚至把它们「搬」进自己项目）。
func TestIDOpsRejectOtherProjects(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	foreign := addVia(t, "A 项目的私密结论", "current")
	foreignID := int64(foreign["id"].(float64))

	asProject(t, "/w/proj-b")
	if _, err := handleDelete(context.Background(), mustJSON(map[string]any{"id": foreignID})); err == nil {
		t.Fatal("不应能删除其它项目的记忆")
	}
	if _, err := handleUpdate(context.Background(), mustJSON(map[string]any{"id": foreignID, "scope": "current"})); err == nil {
		t.Fatal("不应能把其它项目的记忆改到当前项目")
	}
	if _, err := handleUpdate(context.Background(), mustJSON(map[string]any{"id": foreignID, "content": "改写"})); err == nil {
		t.Fatal("不应能改其它项目的记忆内容")
	}

	var mem Memory
	if err := DB.First(&mem, foreignID).Error; err != nil {
		t.Fatalf("既不应被删: %v", err)
	}
	if mem.Project == currentProjectKey() || mem.Project == globalProject {
		t.Fatalf("归属不应被改到当前项目或全局，got %q", mem.Project)
	}
	if mem.Content != "A 项目的私密结论" {
		t.Fatalf("内容不应被改写，got %q", mem.Content)
	}

	// 全局条目对任何项目可见，可正常操作
	asProject(t, "/w/proj-a")
	g := addVia(t, "通用偏好", "global")
	gid := int64(g["id"].(float64))
	asProject(t, "/w/proj-b")
	if _, err := handleUpdate(context.Background(), mustJSON(map[string]any{"id": gid, "content": "通用偏好2"})); err != nil {
		t.Fatalf("全局条目应可从任意项目修改: %v", err)
	}
	if _, err := handleDelete(context.Background(), mustJSON(map[string]any{"id": gid})); err != nil {
		t.Fatalf("全局条目应可从任意项目删除: %v", err)
	}
}

// TestScopeRejectsForeignProjectKey scope 不接受任意项目键，只认 current/global。
func TestScopeRejectsForeignProjectKey(t *testing.T) {
	setupMemoryDB(t)
	asProject(t, "/w/proj-a")
	addVia(t, "A 的记忆", "current")
	foreignKey := currentProjectKey()

	asProject(t, "/w/proj-b")
	if _, err := handleSearch(context.Background(), mustJSON(map[string]string{"keywords": "A", "scope": foreignKey})); err == nil {
		t.Fatal("scope 传项目键应报错（不支持指定别的项目）")
	}
	if _, err := handleAdd(context.Background(), mustJSON(map[string]string{"content": "x", "scope": foreignKey})); err == nil {
		t.Fatal("写入时 scope 传项目键应报错")
	}
}
