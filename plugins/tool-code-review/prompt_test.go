package main

import (
	"strings"
	"testing"
)

func TestRenderAnnotatedDiffNumbers(t *testing.T) {
	f := &fileDiff{
		NewPath: "a.go",
		Hunks: []diffHunk{{
			OldStart: 10, OldLines: 3, NewStart: 10, NewLines: 4, Section: "func f()",
			Lines: []diffLine{
				{Kind: ' ', Text: "ctx", OldNo: 10, NewNo: 10},
				{Kind: '-', Text: "old", OldNo: 11},
				{Kind: '+', Text: "new1", NewNo: 11},
				{Kind: '+', Text: "new2", NewNo: 12},
				{Kind: ' ', Text: "ctx2", OldNo: 12, NewNo: 13},
			},
		}},
	}
	out := renderAnnotatedDiff(f)
	for _, want := range []string{
		"@@ -10,3 +10,4 @@ func f()",
		"   10| ctx",
		"   11|-old",
		"   11|+new1",
		"   12|+new2",
		"   13| ctx2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered diff missing %q:\n%s", want, out)
		}
	}
}

func TestBuildDiffPromptIncludesRulesAndSchema(t *testing.T) {
	files := []*fileDiff{
		{NewPath: "a.go", Hunks: []diffHunk{{NewStart: 1, NewLines: 1, Lines: []diffLine{{Kind: '+', Text: "x", NewNo: 1}}}}},
	}
	system, user := buildDiffPrompt(files, []ruleDoc{{Name: "go", Content: "Go 规则正文"}})

	if !strings.Contains(system, "只输出 JSON 数组") {
		t.Errorf("system prompt missing JSON contract:\n%s", system)
	}
	if !strings.Contains(system, "适用规则：go") || !strings.Contains(system, "Go 规则正文") {
		t.Errorf("system prompt missing rule doc:\n%s", system)
	}
	if !strings.Contains(user, "### a.go（规则：go）") {
		t.Errorf("user prompt missing file header:\n%s", user)
	}
	if !strings.Contains(user, "    1|+x") {
		t.Errorf("user prompt missing annotated line:\n%s", user)
	}
}

func TestBuildScanPromptNumbersFromStart(t *testing.T) {
	system, user := buildScanPrompt("pkg/x.py", []string{"line1", "line2"}, 41, 42, []ruleDoc{{Name: "python", Content: "Python 规则"}})
	if !strings.Contains(system, "整文件扫描") {
		t.Errorf("scan system prompt missing mode note:\n%s", system)
	}
	for _, want := range []string{"第 41-42 行", "   41|line1", "   42|line2"} {
		if !strings.Contains(user, want) {
			t.Errorf("scan prompt missing %q:\n%s", want, user)
		}
	}
}
