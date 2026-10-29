package core

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPluginsDoNotImportCore 分层守卫：插件只依赖 SDK（dsc-sdk），不直接 import dsc/core。
//
// 为什么要有这条：分层约定是 core ← sdk ← plugins。core 是宿主内核（事件总线、会话、
// 计量、协议实现），会持续重构；sdk 是插件的稳定面（类型别名 + 薄转发）。插件直接
// import core 的后果不是「编译不过」，而是**同一件事有了多份实现机会**——DSC 刚吃过
// 这个亏：宿主的 core.TokenMeter 是实测口径却无人调用，agent 插件自己用裸估算决定
// 裁剪/压缩，两边口径一偏（纯 ASCII 被高估 4~5 倍），1M 窗口里服务端实测 332K 的会话
// 被判每轮超窗。
//
// 迁移是渐进的：下列插件仍持有 core 依赖（多为协议实现边界，需要先在 SDK 补齐转发面），
// 登记在此以便只允许减少、不允许新增。补齐后把条目删掉即可。
var pluginsStillOnCore = map[string]string{
	// LUA/SQL 宿主的互通绑定：把宿主聚合服务（LLM/Tool/Notify）暴露给脚本与 SQL，
	// 绑定层需要具体的 *llmclient.Client / *toolclient.Client / notify API 形态，
	// 而不是 interface。迁移计划：在 sdk/ 补齐对应别名后删除本条。
	"tool-lua-host": "脚本绑定层需要 core/llmclient|toolclient|notify 的具体 API",
	"tool-sql-host": "SQL 绑定层需要 core/llmclient|toolclient 的具体 API",
}

// TestPluginsDoNotImportCore 扫描 plugins/ 下所有 Go 源文件（含测试），禁止 import "dsc/core"。
func TestPluginsDoNotImportCore(t *testing.T) {
	repoRoot := filepath.Dir(filepath.Dir(thisTestFile(t)))
	pluginsDir := filepath.Join(repoRoot, "plugins")

	var bad []string
	err := filepath.WalkDir(pluginsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 权限/不存在一律跳过，不因扫描问题阻断测试
		}
		if d.IsDir() {
			// 只扫直接子目录（一个插件一个目录），跳过 vendor 与隐藏目录
			if path != pluginsDir {
				base := d.Name()
				if base == "vendor" || strings.HasPrefix(base, ".") {
					return filepath.SkipDir
				}
				// 一层插件目录内部继续扫（不上溯到别的顶层）
				return nil
			}
			return nil
		}
		// 测试文件同样计入：测试里的耦合会诱导重构时又把 core 引回来
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		// 前缀匹配：`"dsc/core"` 与 `"dsc/core/llmclient"` 都是 core 依赖。
		// 只匹配精确串会漏掉子包——这里曾经漏过（三个文件用着 core/llmclient，
		// 守卫却报「全部插件已脱离 core」）。
		if !strings.Contains(string(data), `"dsc/core`) {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		bad = append(bad, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 plugins 失败: %v", err)
	}

	sort.Strings(bad)
	for _, rel := range bad {
		plugin := pluginDirOf(rel)
		why, exempt := pluginsStillOnCore[plugin]
		if exempt {
			t.Logf("%s 仍依赖 core（已登记：%s）", rel, why)
			continue
		}
		t.Errorf("%s 直接 import \"dsc/core\"：插件应只依赖 dsc-sdk（分层 core ← sdk ← plugins）。"+
			"需要的类型/函数请在 sdk/ 里加转发（类型用别名 type X = core.X），不要在插件里 import core", rel)
	}
	if len(bad) == 0 {
		t.Log("全部插件均已脱离 core")
	}
}

// thisTestFile 返回当前测试文件路径（用于定位仓库根）。
func thisTestFile(t *testing.T) string {
	t.Helper()
	// 测试工作目录即包目录（core/），文件名固定，据此回推仓库根
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(wd, "plugin_sdk_boundary_test.go")
}

// pluginDirOf 从 "plugins/<name>/x.go" 取出 <name>。
func pluginDirOf(relSlash string) string {
	parts := strings.Split(relSlash, "/")
	if len(parts) >= 3 && parts[0] == "plugins" {
		return parts[1]
	}
	return ""
}
