package main

import (
	"regexp"
	"strings"
	"testing"
)

// 本文件覆盖 `dsc help`（本轮补的缺漏）与「未知参数 fail-loud」。
//
// 背景：此前 -h/-help/--help/help 全部落空——参数解析只认少数几个旗标，未识别的
// 参数被静默忽略，`dsc -h` 会直接启动 TUI，`dsc hepl` 拼接错误也一样，用户只看到
// 「命令没按预期做」。现在帮助请求一致退出 0，其余未知参数明确报错。

// TestIsHelpCommand 帮助请求的各种写法都要识别，且不误伤「取值恰好是 help」的情况。
func TestIsHelpCommand(t *testing.T) {
	yes := [][]string{
		{"-h"},
		{"-help"},
		{"--help"},
		{"-?"},
		{"help"},
		{"-log", "-h"},            // 与其他旗标共存
		{"-log", "x.log", "-h"},   // -log 取文件值后再 -h
		{"-mode", "thin", "help"}, // 位置参数形式的 help
	}
	for _, args := range yes {
		if !isHelpCommand(args) {
			t.Errorf("%v 应识别为帮助请求", args)
		}
	}

	no := [][]string{
		{},
		{"-input", "help"}, // help 是 -input 的取值，不是子命令
		{"-mode", "help"},  // 同上，是取值
		{"-headless"},      // 无值旗标
		{"version"},
		{"setup"},
		{"helper"}, // 不是 help
		{"-hx"},    // 不是 -h
	}
	for _, args := range no {
		if isHelpCommand(args) {
			t.Errorf("%v 不应识别为帮助请求", args)
		}
	}
}

// TestHelpTextCoversFlags 帮助文本必须把真实可用的旗标与子命令都写进去，
// 否则「有帮助」等于没有——这是本函数的唯一目的（旗标增删时两边一起改）。
func TestHelpTextCoversFlags(t *testing.T) {
	for _, f := range helpFlags {
		if !strings.Contains(helpText, f) {
			t.Errorf("帮助文本缺少旗标 %s", f)
		}
	}
	for _, c := range helpSubcommands {
		if !strings.Contains(helpText, c) {
			t.Errorf("帮助文本缺少子命令 %s", c)
		}
	}
	// 帮助文本里提到的每个「旗标形状」的词都应当真被解析（防止文档写了个不存在的旗标）。
	// 用正则而非按空白切分：中文正文里旗标后面常常直接跟标点（如「-input，否则…」）。
	re := regexp.MustCompile(`-[a-zA-Z][a-zA-Z0-9-]*`)
	for _, name := range re.FindAllString(helpText, -1) {
		if !knownFlag(name) {
			t.Errorf("帮助文本提到未实现的旗标 %s", name)
		}
	}
}

// TestCheckArgs 未知旗标/未知位置参数/缺少取值都 fail-loud；合法组合一律放行。
func TestCheckArgs(t *testing.T) {
	ok := [][]string{
		{},
		{"-input", "任务文本"},
		{"-headless", "-input", "任务"},
		{"-log"}, // -log 取值可选
		{"-log", "dsc.log"},
		{"-mode", "thin", "-input", "x"},
		{"-admin", "127.0.0.1:9999"},
		{"-hooks", "hooks.json", "-patch", "p.yaml", "-patch", "q.yaml"},
		{"version"},
		{"setup"},
		{"help"},
	}
	for _, args := range ok {
		if err := checkArgs(args); err != nil {
			t.Errorf("%v 应通过校验，却报错: %v", args, err)
		}
	}

	bad := map[string]string{
		"-imput x": "未知选项",
		"-hx":      "未知选项",
		"hepl":     "未知参数",
		"-mode":    "缺少取值",
		"-input":   "缺少取值",
		"-hooks":   "缺少取值",
		"任务文本":     "未知参数", // 裸文本必须用 -input
	}
	for args, want := range bad {
		fields := strings.Fields(args)
		err := checkArgs(fields)
		if err == nil {
			t.Errorf("%q 应报错", args)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q 的错误应含 %q，实际 %q", args, want, err.Error())
		}
		if !strings.Contains(err.Error(), "dsc help") {
			t.Errorf("%q 的错误应提示 dsc help，实际 %q", args, err.Error())
		}
	}
}
