// rules.go：按文件类型匹配评审规则文档。
//
// 对齐 open-code-review 的「按文件特征做规则匹配，把模型注意力收窄」思路：
// 规则由 extension/文件名确定性地选出，而不是让模型自己决定该看什么规范。
// 内置规则用 go:embed 打进二进制；工作区（或 DSC_CODE_REVIEW_RULES_DIR）下的
// 同名 `<rule>.md` 覆盖内置版本，便于团队维护自己的评审口径。
package main

import (
	"embed"
	"os"
	"sort"
	"strings"

	dsc "dsc-sdk"
)

//go:embed rules/*.md
var builtinRules embed.FS

// extRuleNames 是扩展名 → 规则文档名。
var extRuleNames = map[string]string{
	".go":         "go",
	".ts":         "ts_js",
	".tsx":        "ts_js",
	".js":         "ts_js",
	".jsx":        "ts_js",
	".mjs":        "ts_js",
	".cjs":        "ts_js",
	".vue":        "ts_js",
	".svelte":     "ts_js",
	".py":         "python",
	".pyi":        "python",
	".java":       "java",
	".kt":         "java",
	".kts":        "java",
	".rs":         "rust",
	".c":          "c_cpp",
	".h":          "c_cpp",
	".cc":         "c_cpp",
	".cpp":        "c_cpp",
	".cxx":        "c_cpp",
	".hpp":        "c_cpp",
	".hh":         "c_cpp",
	".sh":         "shell",
	".bash":       "shell",
	".zsh":        "shell",
	".yaml":       "config",
	".yml":        "config",
	".json":       "config",
	".toml":       "config",
	".ini":        "config",
	".conf":       "config",
	".properties": "config",
}

// baseRuleNames 是特殊文件名 → 规则文档名。
var baseRuleNames = map[string]string{
	"go.mod":         "go",
	"Dockerfile":     "shell",
	"Makefile":       "shell",
	"CMakeLists.txt": "c_cpp",
	"package.json":   "config",
	"tsconfig.json":  "config",
}

// ruleDoc 是一份规则文档。
type ruleDoc struct {
	Name    string
	Content string
	Source  string // builtin 或覆盖目录
}

// ruleResolver 解析规则文档：覆盖目录优先，其次内置。
type ruleResolver struct {
	overrideDir string
}

// newRuleResolver 构造解析器：DSC_CODE_REVIEW_RULES_DIR 显式指定覆盖目录，
// 否则使用工作区下的 .dsc/review-rules。
func newRuleResolver() *ruleResolver {
	dir := strings.TrimSpace(os.Getenv("DSC_CODE_REVIEW_RULES_DIR"))
	if dir == "" {
		if root := workspaceRoot(); root != "" {
			dir = dsc.PJoin(root, ".dsc", "review-rules")
		}
	}
	return &ruleResolver{overrideDir: dir}
}

// ruleNameForPath 按扩展名/文件名确定规则文档名，未命中返回 default。
func ruleNameForPath(path string) string {
	base := baseName(path)
	if name, ok := baseRuleNames[base]; ok {
		return name
	}
	lower := strings.ToLower(base)
	if i := strings.LastIndex(lower, "."); i >= 0 {
		if name, ok := extRuleNames[lower[i:]]; ok {
			return name
		}
	}
	return "default"
}

// load 读取规则文档：覆盖目录 > 内置 > default 兜底。
func (r *ruleResolver) load(name string) ruleDoc {
	if name == "" {
		name = "default"
	}
	if r != nil && r.overrideDir != "" {
		if b, err := os.ReadFile(dsc.PJoin(r.overrideDir, name+".md")); err == nil {
			return ruleDoc{Name: name, Content: string(b), Source: r.overrideDir}
		}
	}
	if b, err := builtinRules.ReadFile("rules/" + name + ".md"); err == nil {
		return ruleDoc{Name: name, Content: string(b), Source: "builtin"}
	}
	if name != "default" {
		return r.load("default")
	}
	return ruleDoc{Name: "default", Content: "只报能确证的缺陷；不报格式/命名等由工具覆盖的琐事。", Source: "fallback"}
}

// forPaths 返回这批路径适用的规则文档（按规则名升序去重，保证提示词前缀稳定）。
func (r *ruleResolver) forPaths(paths []string) []ruleDoc {
	seen := make(map[string]bool)
	names := make([]string, 0, 4)
	for _, p := range paths {
		name := ruleNameForPath(p)
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	docs := make([]ruleDoc, 0, len(names))
	for _, n := range names {
		docs = append(docs, r.load(n))
	}
	return docs
}
