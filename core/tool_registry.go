package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// ToolRegistry 管理所有可用工具
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]ToolDefinition
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]ToolDefinition),
	}
}

// Reset 清空全部已注册工具（宿主 Shutdown 时回收，避免复用 Manager 时残留）。
func (r *ToolRegistry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = make(map[string]ToolDefinition)
}

// Register 註冊工具（線程安全）
func (r *ToolRegistry) Register(t ToolDefinition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name()]; exists {
		return fmt.Errorf("tool %s already registered", t.Name())
	}
	r.tools[t.Name()] = t
	return nil
}

// Unregister 卸載工具（線程安全）
func (r *ToolRegistry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; exists {
		delete(r.tools, name)
		return true
	}
	return false
}

// Get 獲取工具
func (r *ToolRegistry) Get(name string) (ToolDefinition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List 返回所有工具名
func (r *ToolRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// HiddenTool 可选接口：工具声明自己「注册了，但不进模型可见目录」。
//
// 为什么要这个接口：目录每个请求都要发，一个工具在不在目录里直接等于成本。但
// 「隐藏」不等于「不可用」——隐藏的工具仍在注册表里，后台任务、子代理、按名调用
// 都照常工作，只是不随请求发给模型。
//
// 归属也在这里：**谁提供工具，谁决定它此刻可不可见**。以前这件事由目录装配处按
// 工具名前缀判断（谁能想到 mcp__ 前缀这种细节会写进唯一的装配点），结果是每加一个
// 「有曝光策略」的子系统，就要在装配点再加一段特例。
type HiddenTool interface {
	IsHidden() bool
}

// VisibleTools 返回要进模型目录的工具（跳过自报隐藏的，按名升序）。
//
// 排序在这里做：调用方（AllToolsProto）要把结果发给模型，顺序必须确定，否则请求
// 前缀随 map 迭代序漂移、命中不了缓存。
func (r *ToolRegistry) VisibleTools() []ToolDefinition {
	r.mu.RLock()
	out := make([]ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		if h, ok := t.(HiddenTool); ok && h.IsHidden() {
			continue
		}
		out = append(out, t)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// ToOpenAITools 轉換為 OpenAI 工具定義格式
// 用於發送給 LLM Provider
func (r *ToolRegistry) ToOpenAITools() []map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []map[string]interface{}
	for _, t := range r.tools {
		var params map[string]interface{}
		_ = json.Unmarshal(t.ParametersSchema(), &params)
		result = append(result, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name(),
				"description": t.Description(),
				"parameters":  params,
			},
		})
	}
	return result
}

// Execute 執行工具調用
func (r *ToolRegistry) Execute(ctx context.Context, call PluginToolCall) ToolResult {
	tool, ok := r.Get(call.Name)
	if !ok {
		return ToolResult{
			ToolCallID: call.ID,
			Error:      fmt.Sprintf("unknown tool: %s", call.Name),
		}
	}
	result, err := tool.Execute(ctx, call.Arguments)
	if err != nil {
		return ToolResult{
			ToolCallID: call.ID,
			Error:      err.Error(),
		}
	}
	return ToolResult{
		ToolCallID: call.ID,
		Content:    result,
	}
}
