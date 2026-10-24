package core

// 用户提问事件的宿主广播（对齐 agent/status 的做法：语义事件由宿主统一发，
// 订阅插件各自响应，不让 TUI 直接调某个插件的 API）。
//
// 为什么需要它：ask_user_question / 计划评审会让 agent **阻塞等待用户回答**，
// 此时没有回合结束事件、也没有错误事件——用户离开屏幕就完全不知道模型卡住了。
// 通知类插件（声明 notify 能力者）订阅本事件即可播放提示音，而不必关心提问来自哪条路径。

// EventUserQuestionAsked 宿主即将向用户展示提问（ask_user_question、计划评审等）时广播。
// 载荷：UserQuestionAskedEvent（JSON）。
const EventUserQuestionAsked EventName = "userquestion/asked"

// UserQuestionAskedEvent 提问事件载荷。只带「让用户判断要不要过来看」所需的最小信息：
// 题目数量与首个标题，不含选项正文（避免把大段内容塞进事件总线）。
type UserQuestionAskedEvent struct {
	// Questions 本批问题数量（模型可一次问多个）。
	Questions int `json:"questions"`
	// Header 首个问题的标题（未设置时为空）。
	Header string `json:"header,omitempty"`
}
