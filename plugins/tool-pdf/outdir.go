package main

import (
	"path/filepath"
	"strings"

	dsc "dsc-sdk"
)

// defaultOutDir 返回默认输出目录：**处理目标旁边的具名子目录**。
//
// 旧行为把产物堆到 <工作区根>/pdf-images/<文件名>/，与目标不在同一处：
// 处理工作区外的 PDF 时产物落到无关位置，不同目录下的同名 PDF 还会互相覆盖。
// 现在遵循「目标在哪，产物就在它旁边」：<源文件目录>/<源文件主名>-images[/sub]。
//
// srcAbs 须为已解析的真实绝对路径（调用方经 dsc.ResolveWorkspacePath 得到）。
func defaultOutDir(srcAbs, sub string) string {
	base := filepath.Base(srcAbs)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	dir := dsc.PJoin(dsc.PDir(srcAbs), base+"-images")
	if sub != "" {
		dir = dsc.PJoin(dir, sub)
	}
	return dir
}

// ensureOutDir 在 outDir 为空时按「目标旁的具名子目录」补默认值，并创建目录。
// 返回最终使用的目录（调用方把它写进工具结果，便于模型引用产物路径）。
func ensureOutDir(outDir, srcAbs, sub string) (string, error) {
	if outDir == "" {
		outDir = defaultOutDir(srcAbs, sub)
	}
	if err := dsc.MkdirAll(outDir); err != nil {
		return "", err
	}
	return outDir, nil
}
