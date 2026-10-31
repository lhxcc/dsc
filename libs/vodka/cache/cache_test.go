package cache

import (
	"fmt"
	"testing"
)

func Test_TagCache(t *testing.T) {

	c, err := New(Options{Adapter: "memory"})
	if err != nil {
		t.Fatal(err)
	}

	// base use
	err = c.Set("da", "weisd", 300)
	if err != nil {
		t.Fatal(err)
	}

	res := ""
	c.Get("da", &res)

	if res != "weisd" {
		t.Fatal("base put faield")
	}

	t.Log("ok")

	// use tags/namespace
	err = c.Tags([]string{"insion"}).Set("da", "weisd", 300)
	if err != nil {
		t.Fatal(err)
	}
	res = ""
	c.Tags([]string{"insion"}).Get("da", &res)

	if res != "weisd" {
		t.Fatal("tags put faield")
	}

	t.Log("ok")

	err = c.Tags([]string{"dsc/libs/vodka"}).Set("dsc/libs/vodka", "dsc/libs/vodka_contrib", 300)
	if err != nil {
		t.Fatal(err)
	}

	res = ""
	c.Tags([]string{"dsc/libs/vodka"}).Get("dsc/libs/vodka", &res)

	if res != "dsc/libs/vodka_contrib" {
		t.Fatal("not vodka_contrib")
	}

	t.Log("ok")

	// flush namespace
	err = c.Tags([]string{"dsc/libs/vodka"}).Flush()
	if err != nil {
		t.Fatal(err)
	}

	res = ""
	c.Tags([]string{"dsc/libs/vodka"}).Get("dsc/libs/vodka", &res)
	if res != "" {
		t.Fatal("flush faield")
	}

	res = ""
	c.Tags([]string{"dsc/libs/vodka"}).Get("bb", &res)
	if res != "" {
		t.Fatal("flush faield")
	}

	// still store in
	res = ""
	c.Tags([]string{"insion"}).Get("da", &res)
	if res != "weisd" {
		t.Fatal("where")
	}

	t.Log("ok")

}

// 回归：tag id 必须唯一。旧实现「UnixNano + rand.Intn(9)」在时钟同一节拍内
// 只有 9 个可能值，重复即导致 Flush 换不掉命名空间。
func Test_TagSetGenerateIdUnique(t *testing.T) {
	ts := NewTagSet(NewMemoryCacher(), nil)

	seen := make(map[string]bool, 2000)
	for i := 0; i < 2000; i++ {
		id := ts.generateId()
		if id == "" {
			t.Fatalf("iteration %d: empty id", i)
		}
		if seen[id] {
			t.Fatalf("iteration %d: duplicate id %q", i, id)
		}
		seen[id] = true
	}
}

// 回归：连续 flush 必须每次都让旧值失效（此前的偶发失败即出在这里）。
func Test_TagCacheFlushIsRepeatable(t *testing.T) {
	c, err := New(Options{Adapter: "memory"})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 200; i++ {
		tag := fmt.Sprintf("insion-%d", i%3)

		if err := c.Tags([]string{tag}).Set("da", "weisd", 300); err != nil {
			t.Fatal(err)
		}

		if err := c.Tags([]string{tag}).Flush(); err != nil {
			t.Fatal(err)
		}

		res := ""
		c.Tags([]string{tag}).Get("da", &res)
		if res != "" {
			t.Fatalf("iteration %d (tag %s): flush left stale value %q", i, tag, res)
		}
	}
}

// 回归：多个 tag 的命名空间要稳定——既不能丢 tag，也不能因顺序随机而变。
func Test_TagSetAddNamesDeterministic(t *testing.T) {
	ts := NewTagSet(NewMemoryCacher(), []string{"a"})
	ts.AddNames([]string{"b", "c", "a"})

	want := []string{"a", "b", "c"}
	if len(ts.names) != len(want) {
		t.Fatalf("names = %v, want %v", ts.names, want)
	}
	for i, name := range want {
		if ts.names[i] != name {
			t.Fatalf("names = %v, want %v", ts.names, want)
		}
	}

	first := ts.GetNamespace()
	for i := 0; i < 50; i++ {
		if got := ts.GetNamespace(); got != first {
			t.Fatalf("namespace unstable: %q vs %q", got, first)
		}
	}
}
