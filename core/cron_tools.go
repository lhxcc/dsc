package core

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"dsc/cron"
)

// 宿主内置 cron 管理工具族：把宿主侧的 cron 定时任务调度器（core/cron.go）以模型
// 工具形式暴露给 agent —— 让模型在评测（如 bench）或日常会话中能真实调用 DSC 的
// CRON 机制（增/删/列/启停），而非仅经 TUI 斜杆命令或管理 API。
//
// 对齐 DSH 的「工具优先」哲学：模型按工具调用即可编排定时任务，宿主侧负责校验、
// 持久化（cron.json）与调度（robfig/cron）。
//
// 安全：cron_add / cron_remove / cron_set_enabled 属「不可逆高影响变更」（修改
// 持久化任务集、可能触发无人值守执行任意 prompt），声明 ApprovalRequester 让审批
// 闸门在非 never 时阻止；与 install_dsc_plugin 同族。

// cronNameRe 与 dscPluginNameRe 一致：任务名仅允许 [A-Za-z0-9_-]，防止注入。
var cronNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ============================================================
// cron_add
// ============================================================

type cronAddTool struct{ m *Manager }

func (t *cronAddTool) Name() string { return "cron_add" }

// ApprovalReason 实现 ApprovalRequester：新增定时任务会持久化到 cron.json 并按
// schedule 触发执行任意 prompt（无人值守时高影响），须经用户审批。
func (t *cronAddTool) ApprovalReason(_ string) string {
	return "Adds a persistent scheduled cron job that may auto-run arbitrary prompts."
}

func (t *cronAddTool) Description() string {
	return "Add a DSC cron scheduled job. The cron expression is the standard 5-field form (minute hour day-of-month month day-of-week), " +
		"e.g. \"0 8 * * *\" runs daily at 08:00. The prompt is what the cron runner executes (a subagent prompt) at each trigger; " +
		"the job is persisted to cron.json and starts firing on schedule immediately (enabled=true by default). " +
		"Set notify=true only when the output needs attention as soon as it lands; leave it false (default) for periodic housekeeping, since a notification costs a waking model turn on every run. " +
		"Returns the job id and full record on success. Requires approval=never to actually execute (consistent with install/upgrade_dsc_plugin)."
}

func (t *cronAddTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"name":{"type":"string","description":"Human-readable job name; [A-Za-z0-9_-] only."},
"cron":{"type":"string","description":"Standard 5-field cron expression, e.g. \"0 8 * * *\" (daily at 08:00)."},
"prompt":{"type":"string","description":"The prompt to execute at each trigger (a subagent prompt)."},
"enabled":{"type":"boolean","description":"Whether the job is enabled immediately. Default true.","default":true},
"notify":{"type":"boolean","description":"Whether to notify the model the moment each run settles (default false). Set true only for jobs whose result someone must act on soon (daily report, patrol finding); leave false for high-frequency housekeeping, because true wakes the session (one LLM turn) on every run.","default":false},
"max_iterations":{"type":"integer","description":"Optional cap on subagent iterations per trigger. 0 means unlimited (default)."}
},
"required":["name","cron","prompt"],"additionalProperties":false}`)
}

func (t *cronAddTool) TimeoutMs() int { return 30000 }

func (t *cronAddTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Name          string `json:"name"`
		Cron          string `json:"cron"`
		Prompt        string `json:"prompt"`
		Enabled       *bool  `json:"enabled"`
		Notify        bool   `json:"notify"`
		MaxIterations int    `json:"max_iterations"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if !cronNameRe.MatchString(p.Name) || p.Name == "" {
		return "", fmt.Errorf("invalid name %q: use [A-Za-z0-9_-] only", p.Name)
	}
	if strings.TrimSpace(p.Cron) == "" {
		return "", fmt.Errorf("cron expression is required")
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return "", fmt.Errorf("prompt is required")
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	j := &cron.Job{
		Name:          p.Name,
		Cron:          p.Cron,
		Prompt:        p.Prompt,
		Enabled:       enabled,
		Notify:        p.Notify,
		MaxIterations: p.MaxIterations,
	}
	if err := t.m.AddCronJob(j); err != nil {
		return "", fmt.Errorf("add cron job: %w", err)
	}
	b, _ := json.Marshal(map[string]any{
		"ok":      true,
		"id":      j.ID,
		"name":    j.Name,
		"cron":    j.Cron,
		"enabled": j.Enabled,
		"notify":  j.Notify,
		"note":    "已添加定时任务，按 cron 表达式自动触发；持久化于 cron.json",
	})
	return string(b), nil
}

func (t *cronAddTool) ExecuteWithView(ctx context.Context, args json.RawMessage, result string) (string, string, error) {
	v, verr := json.Marshal(ToolView{
		Kind:  "card",
		Title: "CronAdd",
		Badge: &ViewBadge{Text: "added", Tone: "green"},
		Fields: []ViewField{
			{Key: "id", Value: extractField(result, "id")},
			{Key: "name", Value: extractField(result, "name")},
			{Key: "cron", Value: extractField(result, "cron")},
			{Key: "notify", Value: extractField(result, "notify")},
		},
	})
	if verr != nil {
		return result, "", nil
	}
	return result, string(v), nil
}

// ============================================================
// cron_list
// ============================================================

type cronListTool struct{ m *Manager }

func (t *cronListTool) Name() string { return "cron_list" }

func (t *cronListTool) Description() string {
	return "List all DSC cron scheduled jobs (id, name, cron expression, enabled, notify, last_run_at, last_status, last_output). " +
		"Use this to discover existing scheduled tasks before adding/removing; never guess an id by name."
}

func (t *cronListTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (t *cronListTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	jobs := t.m.ListCronJobs()
	type row struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Cron       string `json:"cron"`
		Enabled    bool   `json:"enabled"`
		Notify     bool   `json:"notify"`
		LastRunAt  int64  `json:"last_run_at"`
		LastStatus string `json:"last_status"`
		LastOutput string `json:"last_output"`
	}
	rows := make([]row, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, row{
			ID: j.ID, Name: j.Name, Cron: j.Cron,
			Enabled: j.Enabled, Notify: j.Notify, LastRunAt: j.LastRunAt,
			LastStatus: j.LastStatus, LastOutput: j.LastOutput,
		})
	}
	b, _ := json.Marshal(map[string]any{
		"jobs":  rows,
		"count": len(rows),
	})
	return string(b), nil
}

func (t *cronListTool) ExecuteWithView(ctx context.Context, args json.RawMessage, result string) (string, string, error) {
	var r struct {
		Jobs []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Cron       string `json:"cron"`
			Enabled    bool   `json:"enabled"`
			Notify     bool   `json:"notify"`
			LastStatus string `json:"last_status"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return result, "", nil
	}
	rows := make([]ViewRow, 0, len(r.Jobs))
	for _, j := range r.Jobs {
		rows = append(rows, ViewRow{
			"id":      j.ID,
			"name":    j.Name,
			"cron":    j.Cron,
			"enabled": fmt.Sprint(j.Enabled),
			"notify":  fmt.Sprint(j.Notify),
			"last":    j.LastStatus,
		})
	}
	v, verr := json.Marshal(ToolView{
		Kind:    "table",
		Title:   "CronJobs",
		Columns: []ViewColumn{{Key: "id", Title: "ID"}, {Key: "name", Title: "名称"}, {Key: "cron", Title: "表达式"}, {Key: "enabled", Title: "启用"}, {Key: "notify", Title: "通知"}, {Key: "last", Title: "上次状态"}},
		Rows:    rows,
	})
	if verr != nil {
		return result, "", nil
	}
	return result, string(v), nil
}

// ============================================================
// cron_remove
// ============================================================

type cronRemoveTool struct{ m *Manager }

func (t *cronRemoveTool) Name() string { return "cron_remove" }

// ApprovalReason 实现 ApprovalRequester：删除定时任务（不可逆），须经用户审批。
func (t *cronRemoveTool) ApprovalReason(_ string) string {
	return "Removes a cron job (irreversible)."
}

func (t *cronRemoveTool) Description() string {
	return "Remove a DSC cron job by its id (use cron_list to discover ids). The job is unscheduled and deleted from cron.json."
}

func (t *cronRemoveTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"id":{"type":"string","description":"Job id (returned by cron_add / cron_list)."}},
"required":["id"],"additionalProperties":false}`)
}

func (t *cronRemoveTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if p.ID == "" {
		return "", fmt.Errorf("id is required")
	}
	if err := t.m.RemoveCronJob(p.ID); err != nil {
		return "", fmt.Errorf("remove cron job: %w", err)
	}
	b, _ := json.Marshal(map[string]any{
		"ok":   true,
		"id":   p.ID,
		"note": "已删除定时任务",
	})
	return string(b), nil
}

func (t *cronRemoveTool) ExecuteWithView(ctx context.Context, args json.RawMessage, result string) (string, string, error) {
	v, verr := json.Marshal(ToolView{
		Kind:  "card",
		Title: "CronRemove",
		Badge: &ViewBadge{Text: "removed", Tone: "yellow"},
		Fields: []ViewField{
			{Key: "id", Value: extractField(result, "id")},
		},
	})
	if verr != nil {
		return result, "", nil
	}
	return result, string(v), nil
}

// ============================================================
// cron_set_enabled
// ============================================================

type cronSetEnabledTool struct{ m *Manager }

func (t *cronSetEnabledTool) Name() string { return "cron_set_enabled" }

// ApprovalReason 实现 ApprovalRequester：启停定时任务会改变调度状态，须经用户审批。
func (t *cronSetEnabledTool) ApprovalReason(_ string) string {
	return "Toggles a cron job's enabled state (changes scheduled execution)."
}

func (t *cronSetEnabledTool) Description() string {
	return "Update a DSC cron job by id: enable/disable it (disabled jobs stay in cron.json but no longer fire), " +
		"and/or set whether each run notifies the model when it settles. Pass at least one of enabled/notify."
}

func (t *cronSetEnabledTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"id":{"type":"string","description":"Job id (returned by cron_add / cron_list)."},
"enabled":{"type":"boolean","description":"true to enable, false to disable (omit to leave unchanged)."},
"notify":{"type":"boolean","description":"true to notify the model when each run settles, false to stay silent (omit to leave unchanged)."}},
"required":["id"],"additionalProperties":false}`)
}

func (t *cronSetEnabledTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
		Notify  *bool  `json:"notify"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if p.ID == "" {
		return "", fmt.Errorf("id is required")
	}
	if p.Enabled == nil && p.Notify == nil {
		return "", fmt.Errorf("nothing to update: pass enabled and/or notify")
	}
	if p.Enabled != nil {
		if err := t.m.SetCronJobEnabled(p.ID, *p.Enabled); err != nil {
			return "", fmt.Errorf("set cron enabled: %w", err)
		}
	}
	if p.Notify != nil {
		if err := t.m.SetCronJobNotify(p.ID, *p.Notify); err != nil {
			return "", fmt.Errorf("set cron notify: %w", err)
		}
	}
	cur, _ := t.m.cronScheduler.Get(p.ID)
	out := map[string]any{"ok": true, "id": p.ID}
	if cur != nil {
		out["enabled"], out["notify"] = cur.Enabled, cur.Notify
	}
	out["note"] = "已更新任务设置（启停 / 完成通知）"
	b, _ := json.Marshal(out)
	return string(b), nil
}

func (t *cronSetEnabledTool) ExecuteWithView(ctx context.Context, args json.RawMessage, result string) (string, string, error) {
	tone := "green"
	if !strings.Contains(result, `"enabled":true`) && !strings.Contains(result, `"enabled": true`) {
		tone = "yellow"
	}
	v, verr := json.Marshal(ToolView{
		Kind:  "card",
		Title: "CronSetEnabled",
		Badge: &ViewBadge{Text: extractField(result, "enabled"), Tone: tone},
		Fields: []ViewField{
			{Key: "id", Value: extractField(result, "id")},
			{Key: "enabled", Value: extractField(result, "enabled")},
			{Key: "notify", Value: extractField(result, "notify")},
		},
	})
	if verr != nil {
		return result, "", nil
	}
	return result, string(v), nil
}

// ============================================================
// 工具集合
// ============================================================

// cronTools 返回宿主内置的 cron 管理工具族，供模型在评测或日常会话中真实调用
// DSC 的 CRON 机制。
func (m *Manager) cronTools() []ToolDefinition {
	return []ToolDefinition{
		&cronAddTool{m: m},
		&cronListTool{m: m},
		&cronRemoveTool{m: m},
		&cronSetEnabledTool{m: m},
	}
}
