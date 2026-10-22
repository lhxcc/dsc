package benchreport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiffReportsClassificationAndOrder(t *testing.T) {
	base := &Report{Score: "75.00", Cases: []ReportCase{
		{ID: "a", Status: "pass"},
		{ID: "b", Status: "fail"},
		{ID: "c", Status: "skip"},
		{ID: "d", Status: "pass"},
		{ID: "e", Status: "fail"},
	}}
	cur := &Report{Score: "60.00", Cases: []ReportCase{
		{ID: "a", Status: "fail"}, // 回归
		{ID: "b", Status: "pass"}, // 改进
		{ID: "c", Status: "pass"}, // skip → pass 视为改进
		{ID: "d", Status: "skip"}, // pass → skip 视为回归（没跑完）
		{ID: "f", Status: "pass"}, // 新增
	}}

	d := DiffReports(base, cur)
	if d.ScoreDelta != "-15.00" {
		t.Fatalf("score delta = %q, want -15.00", d.ScoreDelta)
	}
	if len(d.Regressions) != 2 || d.Regressions[0] != "a" || d.Regressions[1] != "d" {
		t.Fatalf("regressions = %v, want [a d]", d.Regressions)
	}
	if len(d.Improvements) != 2 || d.Improvements[0] != "b" || d.Improvements[1] != "c" {
		t.Fatalf("improvements = %v, want [b c]", d.Improvements)
	}
	if d.Unchanged != 0 {
		t.Fatalf("unchanged = %d, want 0", d.Unchanged)
	}
	wantKinds := map[string]string{
		"a": KindRegression,
		"b": KindImprovement,
		"c": KindImprovement,
		"d": KindRegression,
		"e": KindMissing,
		"f": KindNew,
	}
	var ids []string
	for _, c := range d.Cases {
		ids = append(ids, c.ID)
		if got := wantKinds[c.ID]; got != c.Kind {
			t.Fatalf("case %s kind = %q, want %q", c.ID, c.Kind, got)
		}
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Fatalf("diff cases not sorted by id: %v", ids)
		}
	}
}

func TestDiffReportsUnchangedAndChanged(t *testing.T) {
	base := &Report{Cases: []ReportCase{
		{ID: "same-pass", Status: "pass"},
		{ID: "same-fail", Status: "fail"},
		{ID: "fail-to-skip", Status: "fail"},
	}}
	cur := &Report{Cases: []ReportCase{
		{ID: "same-pass", Status: "pass"},
		{ID: "same-fail", Status: "fail"},
		{ID: "fail-to-skip", Status: "skip"}, // 无明确优劣 → changed
	}}
	d := DiffReports(base, cur)
	if d.Unchanged != 2 {
		t.Fatalf("unchanged = %d, want 2", d.Unchanged)
	}
	if len(d.Regressions) != 0 || len(d.Improvements) != 0 {
		t.Fatalf("regressions=%v improvements=%v, want none", d.Regressions, d.Improvements)
	}
	for _, c := range d.Cases {
		if c.ID == "fail-to-skip" && c.Kind != KindChanged {
			t.Fatalf("fail→skip kind = %q, want %q", c.Kind, KindChanged)
		}
	}
}

func TestHasNewFail(t *testing.T) {
	// pass→fail：既是回归也是新增失败。
	d := DiffReports(
		&Report{Cases: []ReportCase{{ID: "a", Status: "pass"}}},
		&Report{Cases: []ReportCase{{ID: "a", Status: "fail"}}},
	)
	if !d.HasRegression() || !d.HasNewFail() {
		t.Fatal("pass→fail should count as both regression and new fail")
	}
	// fail→fail：基线同样失败，不计。
	d = DiffReports(
		&Report{Cases: []ReportCase{{ID: "a", Status: "fail"}}},
		&Report{Cases: []ReportCase{{ID: "a", Status: "fail"}}},
	)
	if d.HasRegression() || d.HasNewFail() {
		t.Fatal("fail→fail must not be counted")
	}
	// 新增用例失败：不是回归，但是新增失败。
	d = DiffReports(
		&Report{Cases: []ReportCase{{ID: "a", Status: "pass"}}},
		&Report{Cases: []ReportCase{{ID: "a", Status: "pass"}, {ID: "b", Status: "fail"}}},
	)
	if d.HasRegression() {
		t.Fatal("new case must not be a regression")
	}
	if !d.HasNewFail() {
		t.Fatal("new case failing should be a new fail")
	}
}

func TestLoadRejectsBadReports(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	cases := map[string]string{
		"empty":     `{"score":"0.00","cases":[]}`,
		"duplicate": `{"cases":[{"id":"a","status":"pass"},{"id":"a","status":"fail"}]}`,
		"no-id":     `{"cases":[{"status":"pass"}]}`,
		"broken":    `{not json`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(name+".json", body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}

	valid := write("valid.json", `{"score":"50.00","cases":[{"id":"a","status":"pass"}]}`)
	r, err := Load(valid)
	if err != nil {
		t.Fatalf("load valid report: %v", err)
	}
	if r.Score != "50.00" || len(r.Cases) != 1 {
		t.Fatalf("unexpected report: %+v", r)
	}
}
