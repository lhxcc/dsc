//go:build !windows

package main

// aliasStubHint Windows 的「应用执行别名」占位符在其它平台不存在，恒返回空串。
func aliasStubHint(string) string { return "" }
