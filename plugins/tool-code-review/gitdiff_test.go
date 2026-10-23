package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestParseUnifiedDiffBasic(t *testing.T) {
	text := `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -1,4 +1,5 @@ package main
 line1
-old
+new
+added2
 line3
`
	files := parseUnifiedDiff(text)
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	f := files[0]
	if f.OldPath != "main.go" || f.NewPath != "main.go" {
		t.Fatalf("paths = %q, %q", f.OldPath, f.NewPath)
	}
	if len(f.Hunks) != 1 || len(f.Hunks[0].Lines) != 5 {
		t.Fatalf("hunks = %+v", f.Hunks)
	}
	added := f.commentableLines()
	if len(added) != 2 || !added[2] || !added[3] {
		t.Fatalf("added = %v, want {2,3}", added)
	}
	if got := f.maxNewLine(); got != 4 {
		t.Fatalf("maxNewLine = %d, want 4", got)
	}
	if f.hunkLineCount() != 5 {
		t.Fatalf("hunkLineCount = %d, want 5", f.hunkLineCount())
	}
}

func TestParseUnifiedDiffNewDeletedBinary(t *testing.T) {
	text := `diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..abc1234
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+hello
+world
diff --git a/old.go b/old.go
deleted file mode 100644
index abc1234..0000000
--- a/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package main
-func main() {}
diff --git a/img.png b/img.png
index aaa..bbb 100644
Binary files a/img.png and b/img.png differ
`
	files := parseUnifiedDiff(text)
	if len(files) != 3 {
		t.Fatalf("files = %d, want 3", len(files))
	}
	if !files[0].IsNew || files[0].path() != "new.txt" {
		t.Fatalf("new file = %+v", files[0])
	}
	if !files[1].IsDeleted || files[1].path() != "old.go" {
		t.Fatalf("deleted file = %+v", files[1])
	}
	if !files[2].IsBinary {
		t.Fatalf("binary file not detected: %+v", files[2])
	}
}

func TestParseUnifiedDiffQuotedAndRenamed(t *testing.T) {
	text := `diff --git "a/old name.go" "b/new name.go"
similarity index 90%
rename from old name.go
rename to new name.go
--- "a/old name.go"
+++ "b/new name.go"
@@ -1 +1 @@
-a
+b
`
	files := parseUnifiedDiff(text)
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	if !files[0].IsRenamed || files[0].OldPath != "old name.go" || files[0].NewPath != "new name.go" {
		t.Fatalf("renamed = %+v", files[0])
	}
}

func TestParseUnifiedDiffIgnoresNoNewlineMarker(t *testing.T) {
	text := `diff --git a/x.txt b/x.txt
--- a/x.txt
+++ b/x.txt
@@ -1 +1 @@
-a
\ No newline at end of file
+b
\ No newline at end of file
`
	files := parseUnifiedDiff(text)
	if len(files) != 1 || len(files[0].Hunks) != 1 {
		t.Fatalf("files = %+v", files)
	}
	if got := len(files[0].Hunks[0].Lines); got != 2 {
		t.Fatalf("lines = %d, want 2 (marker lines skipped)", got)
	}
}

func TestParseUnifiedDiffDeletionAnchors(t *testing.T) {
	// 纯删除 hunk：删除代码没有新行号，锚定到删除位置前一行（NewStart-1）。
	text := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -3,2 +3,0 @@
-if err != nil {
-	return err
`
	files := parseUnifiedDiff(text)
	if len(files) != 1 {
		t.Fatalf("files = %d", len(files))
	}
	lines := files[0].commentableLines()
	if len(lines) != 1 || !lines[2] {
		t.Fatalf("deletion anchors = %v, want {2}", lines)
	}
}

func TestUnquoteGitPath(t *testing.T) {
	if got := unquoteGitPath(`"a/\346\226\207.md"`); got != "a/文.md" {
		t.Fatalf("unquote = %q", got)
	}
	if got := unquoteGitPath("plain.go"); got != "plain.go" {
		t.Fatalf("plain = %q", got)
	}
}

// TestCollectDiffWorkingTree 校验工作区改动（未暂存）能被采集并解析出新增行。
func TestCollectDiffWorkingTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newGitRepo(t)
	writeFileT(t, dir, "a.go", "package main\n\nfunc main() {}\n")
	gitCmdT(t, dir, "add", ".")
	gitCmdT(t, dir, "commit", "-q", "-m", "init")
	writeFileT(t, dir, "a.go", "package main\n\nfunc main() { println(\"hi\") }\n")
	setWorkspaceRootT(t, dir)

	text, err := collectDiff(context.Background(), diffOptions{})
	if err != nil {
		t.Fatalf("collectDiff: %v", err)
	}
	files := parseUnifiedDiff(text)
	if len(files) != 1 || files[0].path() != "a.go" {
		t.Fatalf("files = %+v", files)
	}
	if len(files[0].commentableLines()) == 0 {
		t.Fatalf("no added lines: %s", text)
	}
}

// TestCollectDiffPathspec 校验 paths 参数收窄评审范围。
func TestCollectDiffPathspec(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newGitRepo(t)
	writeFileT(t, dir, "a.go", "package a\n")
	writeFileT(t, dir, "b.go", "package b\n")
	gitCmdT(t, dir, "add", ".")
	gitCmdT(t, dir, "commit", "-q", "-m", "init")
	writeFileT(t, dir, "a.go", "package a\n// changed\n")
	writeFileT(t, dir, "b.go", "package b\n// changed\n")
	setWorkspaceRootT(t, dir)

	text, err := collectDiff(context.Background(), diffOptions{Paths: []string{"a.go"}})
	if err != nil {
		t.Fatalf("collectDiff: %v", err)
	}
	if strings.Contains(text, "b.go") {
		t.Fatalf("pathspec not applied:\n%s", text)
	}
}

// TestCollectDiffNotRepo 校验非 git 目录给出可理解的错误。
func TestCollectDiffNotRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	setWorkspaceRootT(t, dir)
	if _, err := collectDiff(context.Background(), diffOptions{}); err == nil {
		t.Fatal("expected error outside a git repository")
	}
}
