package main

import (
	"os"
	"strings"
	"testing"

	dsc "dsc-sdk"
)

func TestRuleNameForPath(t *testing.T) {
	cases := map[string]string{
		"a/b/main.go":      "go",
		"go.mod":           "go",
		"web/app.tsx":      "ts_js",
		"web/index.mjs":    "ts_js",
		"script.py":        "python",
		"pkg/Foo.kt":       "java",
		"src/lib.rs":       "rust",
		"native/x.cpp":     "c_cpp",
		"deploy.sh":        "shell",
		"Makefile":         "shell",
		"config/app.yaml":  "config",
		"README.md":        "default",
		"data/unknown.xyz": "default",
	}
	for path, want := range cases {
		if got := ruleNameForPath(path); got != want {
			t.Errorf("ruleNameForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestRuleResolverBuiltinAndOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dsc.PJoin(dir, "go.md"), []byte("自定义 Go 规则"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &ruleResolver{overrideDir: dir}

	doc := r.load("go")
	if doc.Source != dir || !strings.Contains(doc.Content, "自定义") {
		t.Fatalf("override not used: %+v", doc)
	}
	// 未覆盖的规则回退内置
	python := r.load("python")
	if python.Source != "builtin" || !strings.Contains(python.Content, "Python") {
		t.Fatalf("builtin fallback failed: source=%q", python.Source)
	}
	// 未知名字回退 default
	unknown := r.load("no-such-rule")
	if unknown.Name != "default" || unknown.Content == "" {
		t.Fatalf("default fallback failed: %+v", unknown)
	}
}

func TestRuleResolverForPathsDeterministic(t *testing.T) {
	r := &ruleResolver{}
	docs := r.forPaths([]string{"b.ts", "a.go", "c.py", "d.ts"})
	var names []string
	for _, d := range docs {
		names = append(names, d.Name)
	}
	got := strings.Join(names, ",")
	if got != "go,python,ts_js" {
		t.Fatalf("docs = %s, want go,python,ts_js (按名升序去重)", got)
	}
}
