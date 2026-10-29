package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dsc/libs/vodka"
)

// 通用管理操作端点：任何声明了管理面的插件，其操作都经这里执行，宿主不认识具体名字。
//
// 为什么不是「每个插件注册自己的路由」：管理是能力，端点/命令是呈现。若让 MCP 插件
// 拥有 /mcp/connect，换实现就得换端点，WEB 面板还要再认识一遍；而插件声明操作、
// 呈现层读目录，同一份能力和入口数量无关。参照 DSH host/webserver 的 register(route)
// 之前先想清楚这一点——它可以，是因为它的插件与宿主同进程（handler 闭包直接递过去），
// 我们跨进程只能选「代理字节」或「把路由降级成数据」，而数据这一侧恰好已经建好了
// （ManagementOperations）。
//
// 鉴权与其它 admin 路由一致（adminAuth）。通过鉴权即视作**人的授权**：请求上下文带上
// WithUserGrant，审批门不再反问（与 TUI 里用户手打 /mcp connect 同一语义）。
const (
	// operationsMaxBody admin 操作请求体上限（管理操作参数都是小 JSON；工具调用结果
	// 不受此限，那是响应侧）。
	operationsMaxBody = 1 << 20
	// operationsTimeout 单次管理操作超时。MCP 连接允许 60s 握手，故留出余量。
	operationsTimeout = 120 * time.Second
)

// registerOperationAdminRoutes 注册通用管理操作端点。
//
//   GET  /operations/list            管理操作目录（group / name / tool / summary / schema /
//                                    model_visible）
//   POST /operations/<group>/<name>  执行一条操作，body 即操作参数（JSON 对象）
//                                    （路径模板是 vodka 的 <name> 语法，不是 {name}）
func (m *Manager) registerOperationAdminRoutes(g *vodka.RouteGroup) {
	g.Get("/list", m.handleOperationList)
	g.Post("/<group>/<name>", m.handleOperationExecute)
}

// operationView 一条管理操作的线上面貌。
//
// ModelVisible 一并给出：呈现层要能区分「模型也能用」与「只给人用」，并据此决定是否
// 展示「仅人工」标记；schema 让面板能生成表单而不是让用户手写 JSON。
type operationView struct {
	Group        string          `json:"group"`
	Name         string          `json:"name"`
	Tool         string          `json:"tool"`
	Summary      string          `json:"summary"`
	Schema       json.RawMessage `json:"schema,omitempty"`
	ModelVisible bool            `json:"model_visible"`
}

func (m *Manager) handleOperationList(c *vodka.Context) error {
	ops := m.ManagementOperations()
	out := make([]operationView, 0, len(ops))
	for _, op := range ops {
		out = append(out, operationView{
			Group:        op.Group,
			Name:         op.Name,
			Tool:         op.Tool,
			Summary:      op.Summary,
			Schema:       op.Schema,
			ModelVisible: op.ModelVisible,
		})
	}
	return c.JSON(map[string]any{"operations": out})
}

func (m *Manager) handleOperationExecute(c *vodka.Context) error {
	group := c.Param("group").String()
	name := c.Param("name").String()

	tool := ""
	for _, op := range m.ManagementOperations() {
		if op.Group == group && op.Name == name {
			tool = op.Tool
			break
		}
	}
	if tool == "" {
		return vodka.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("no management operation %q in group %q", name, group))
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, operationsMaxBody))
	if err != nil {
		return vodka.NewHTTPError(http.StatusBadRequest, "read body: "+err.Error())
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte(`{}`) // 无参操作：不给 body 等价于空对象
	}
	if !json.Valid(body) {
		return vodka.NewHTTPError(http.StatusBadRequest, "body must be a JSON object")
	}

	// 鉴权已由中间件完成：这是人的请求，等同于用户在 TUI 里手打命令。
	// 带上 never 是因为「能校验令牌的人」已经满足了要求前置审批策略的那类工具能力；
	// 它与「拒绝执行」是两回事——真正的拒绝由审批策略与范围账本决定，不在这里。
	ctx, cancel := context.WithTimeout(
		WithUserGrant(WithApprovalPolicy(c.Request.Context(), "never")), operationsTimeout)
	defer cancel()

	result, err := m.ExecuteTool(ctx, tool, body)
	if err != nil {
		// 400 与 500 的区分对调用方有意义：参数错和内部失败要分别处理。
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "invalid arguments") {
			status = http.StatusBadRequest
		}
		return vodka.NewHTTPError(status, err.Error())
	}
	return c.JSON(map[string]any{"ok": true, "operation": group + "/" + name, "result": result})
}
