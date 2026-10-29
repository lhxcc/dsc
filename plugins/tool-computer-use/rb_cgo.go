//go:build cgo

package main

import "github.com/go-vgo/robotgo"

// inputBackendReady 报告本构建是否带真实输入/截图后端（cgo 版恒为 true）。
const inputBackendReady = true

// robotgo 薄封装：var 间接层便于单测注入替身（不触达真实 X11）。
//
// 这是 cgo 构建变体；无 C 工具链的机器上看 rb_nocgo.go（同名同型的报错替身）。
// 两个文件必须保持签名一致——调用点在 main.go 里，任一变体编译不过都会立刻暴露。
var (
	rbCapture     = robotgo.Capture
	rbScreenSize  = robotgo.GetScreenSize
	rbSysScale    = robotgo.SysScale
	rbDisplaysNum = robotgo.DisplaysNum
	rbMousePos    = robotgo.GetMousePos
	rbMoveClick   = robotgo.MoveClick
	rbClick       = robotgo.Click
	rbMoveMouse   = robotgo.MoveMouse
	rbMoveSmooth  = robotgo.MoveMouseSmooth
	rbScrollDir   = robotgo.ScrollDir
	rbTypeStr     = robotgo.TypeStr
	rbKeyTap      = robotgo.KeyTap
	rbMouseDown   = robotgo.MouseDown
	rbMouseUp     = robotgo.MouseUp
	rbWriteAll    = robotgo.WriteAll
	rbReadAll     = robotgo.ReadAll
	rbMilliSleep  = robotgo.MilliSleep
)
