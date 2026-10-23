package main

import (
	"strings"
	"testing"
)

// mkFileDiff 构造一个含 n 行新增的 diff（路径用于筛选判断）。
func mkFileDiff(path string, added int) *fileDiff {
	f := &fileDiff{OldPath: path, NewPath: path}
	h := diffHunk{NewStart: 1, NewLines: added}
	for i := 1; i <= added; i++ {
		h.Lines = append(h.Lines, diffLine{Kind: '+', Text: "x", NewNo: i})
	}
	f.Hunks = []diffHunk{h}
	return f
}

func TestSelectDiffFilesFilters(t *testing.T) {
	diffs := []*fileDiff{
		mkFileDiff("main.go", 3),
		mkFileDiff("vendor/lib/a.go", 3),
		mkFileDiff("node_modules/x/index.js", 3),
		mkFileDiff("api/service.pb.go", 3),
		mkFileDiff("go.sum", 3),
		mkFileDiff("pkg/a_test.go", 3),
		mkFileDiff("pkg/huge.go", 200),
		{OldPath: "img.png", NewPath: "", IsBinary: true},
		{OldPath: "gone.go", NewPath: "", IsDeleted: true},
	}
	opts := selectOptions{MaxFiles: 10, MaxFileDiffLines: 50}
	files, skipped := selectDiffFiles(diffs, opts)
	if len(files) != 1 || files[0].path() != "main.go" {
		t.Fatalf("files = %v", files)
	}
	reasons := make(map[string]string)
	for _, s := range skipped {
		reasons[s.Path] = s.Reason
	}
	for _, p := range []string{"vendor/lib/a.go", "node_modules/x/index.js", "api/service.pb.go", "go.sum", "pkg/a_test.go", "pkg/huge.go", "img.png", "gone.go"} {
		if reasons[p] == "" {
			t.Errorf("%s should be skipped, got %v", p, reasons)
		}
	}
}

func TestSelectDiffFilesIncludeTests(t *testing.T) {
	diffs := []*fileDiff{mkFileDiff("pkg/a_test.go", 3)}
	files, _ := selectDiffFiles(diffs, selectOptions{MaxFiles: 10, IncludeTests: true})
	if len(files) != 1 {
		t.Fatalf("tests should be included: %v", files)
	}
}

func TestSelectDiffFilesMaxFilesKeepsSorted(t *testing.T) {
	diffs := []*fileDiff{
		mkFileDiff("c.go", 1),
		mkFileDiff("a.go", 1),
		mkFileDiff("b.go", 1),
	}
	files, skipped := selectDiffFiles(diffs, selectOptions{MaxFiles: 2})
	if len(files) != 2 || files[0].path() != "a.go" || files[1].path() != "b.go" {
		t.Fatalf("files = %v", files)
	}
	if len(skipped) != 1 || skipped[0].Path != "c.go" || skipped[0].Reason != "beyond max_files" {
		t.Fatalf("skipped = %+v", skipped)
	}
}

func TestBundleDiffFiles(t *testing.T) {
	var files []*fileDiff
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		files = append(files, mkFileDiff(name, 100))
	}
	bundles := bundleDiffFiles(files, 250, 10)
	if len(bundles) != 3 {
		t.Fatalf("bundles = %d, want 3", len(bundles))
	}
	if len(bundles[0].Files) != 2 || bundles[0].Lines != 200 {
		t.Fatalf("bundle0 = %+v", bundles[0])
	}
	if len(bundles[2].Files) != 1 {
		t.Fatalf("bundle2 = %+v", bundles[2])
	}

	// 文件数上限优先
	bundles = bundleDiffFiles(files, 100000, 2)
	if len(bundles) != 3 || len(bundles[0].Files) != 2 {
		t.Fatalf("max-files bundling = %+v", bundles)
	}
}

func TestBundleDiffFilesSingleOversize(t *testing.T) {
	files := []*fileDiff{mkFileDiff("big.go", 900), mkFileDiff("small.go", 10)}
	bundles := bundleDiffFiles(files, 300, 6)
	if len(bundles) != 2 {
		t.Fatalf("bundles = %d, want 2", len(bundles))
	}
	if len(bundles[0].Files) != 1 || bundles[0].Files[0].path() != "big.go" {
		t.Fatalf("oversize file should own a bundle: %+v", bundles[0])
	}
}

func TestBundleLabelStable(t *testing.T) {
	if got := bundleLabel([]*fileDiff{mkFileDiff("b.go", 1), mkFileDiff("a.go", 1)}); !strings.HasPrefix(got, "a.go") {
		t.Fatalf("label = %q", got)
	}
}
