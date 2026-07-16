package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// pagedDemo copies the demo fixture and generates lib/paging_gen.go into
// the copy: enough references, callers, callees, symbols, and interface
// implementations to overflow one 50-entry page. Generated here rather
// than committed — a few hundred lines of mechanical repetition would be
// pure noise in testdata.
func pagedDemo(t *testing.T) *Snapshot {
	t.Helper()
	s := demo(t)
	var b strings.Builder
	b.WriteString("package lib\n\nfunc UseDoubleLots() {\n")
	for i := range 60 {
		fmt.Fprintf(&b, "\t_ = Double(%d)\n", i)
	}
	b.WriteString("}\n\n")
	for i := range 60 {
		fmt.Fprintf(&b, "func PagedSym%02d() {}\n", i)
	}
	b.WriteString("\ntype ManyPutter interface{ Put(int) }\n\n")
	for i := range 60 {
		fmt.Fprintf(&b, "type PagedPutter%02d struct{}\n\nfunc (PagedPutter%02d) Put(int) {}\n\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "lib", "paging_gen.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

// hitList round-trips a response's hit slice through JSON so tests can read
// entries uniformly whatever the concrete hit struct is.
func hitList(t *testing.T, v any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("hit list is not a JSON array: %v (raw %s)", err, raw)
	}
	return out
}

// wantPage asserts the shared paging contract on one response: count is
// the total, the window under key holds wantLen entries, and truncated /
// next_offset appear exactly when the window stops short of the end.
func wantPage(t *testing.T, res map[string]any, key string, wantCount, wantLen, wantNext int) []map[string]any {
	t.Helper()
	if got := res["count"].(int); got != wantCount {
		t.Errorf("count = %d, want %d (count must stay the TOTAL, not the window length)", got, wantCount)
	}
	hits := hitList(t, res[key])
	if len(hits) != wantLen {
		t.Errorf("len(%s) = %d, want %d", key, len(hits), wantLen)
	}
	if wantNext > 0 {
		if res["truncated"] != true {
			t.Errorf("truncated = %v, want true", res["truncated"])
		}
		if got := res["next_offset"].(int); got != wantNext {
			t.Errorf("next_offset = %v, want %d", res["next_offset"], wantNext)
		}
	} else {
		if _, ok := res["truncated"]; ok {
			t.Errorf("final page carries truncated = %v", res["truncated"])
		}
		if _, ok := res["next_offset"]; ok {
			t.Errorf("final page carries next_offset = %v", res["next_offset"])
		}
	}
	return hits
}

// posLess orders "file:line:col" strings by (file, line, col) numerically.
func posLess(a, b string) bool {
	pa, pb := strings.Split(a, ":"), strings.Split(b, ":")
	if pa[0] != pb[0] {
		return pa[0] < pb[0]
	}
	la, _ := strconv.Atoi(pa[1])
	lb, _ := strconv.Atoi(pb[1])
	if la != lb {
		return la < lb
	}
	ca, _ := strconv.Atoi(pa[2])
	cb, _ := strconv.Atoi(pb[2])
	return ca < cb
}

// assertOrderedDisjoint checks paging is stable: consecutive pages hold
// strictly increasing positions with no entry repeated across the seam.
func assertOrderedDisjoint(t *testing.T, posField string, pages ...[]map[string]any) {
	t.Helper()
	var all []string
	for _, p := range pages {
		for _, h := range p {
			all = append(all, h[posField].(string))
		}
	}
	seen := map[string]bool{}
	for i, pos := range all {
		if seen[pos] {
			t.Errorf("position %s appears twice across pages", pos)
		}
		seen[pos] = true
		if i > 0 && !posLess(all[i-1], pos) {
			t.Errorf("positions out of order at %d: %s !< %s", i, all[i-1], pos)
		}
	}
}

func TestRefsPaging(t *testing.T) {
	s := pagedDemo(t)
	// def in lib.go + 60 generated uses + 1 use in main.go
	res, err := s.Refs("demo/lib", "Double", 0)
	if err != nil {
		t.Fatal(err)
	}
	page0 := wantPage(t, res, "refs", 62, 50, 50)
	res2, err := s.Refs("demo/lib", "Double", 50)
	if err != nil {
		t.Fatal(err)
	}
	page1 := wantPage(t, res2, "refs", 62, 12, 0)
	assertOrderedDisjoint(t, "pos", page0, page1)

	// Paging must be stable: the same request returns the same window.
	again, err := s.Refs("demo/lib", "Double", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hitList(t, again["refs"]), page0) {
		t.Error("same offset returned a different window on a second call")
	}
}

func TestCallersPaging(t *testing.T) {
	s := pagedDemo(t)
	// 1 call in main.go + 60 generated calls
	res, err := s.Callers("demo/lib", "Double", 0)
	if err != nil {
		t.Fatal(err)
	}
	page0 := wantPage(t, res, "callers", 61, 50, 50)
	res2, err := s.Callers("demo/lib", "Double", 50)
	if err != nil {
		t.Fatal(err)
	}
	page1 := wantPage(t, res2, "callers", 61, 11, 0)
	assertOrderedDisjoint(t, "call_pos", page0, page1)
}

func TestCalleesPaging(t *testing.T) {
	s := pagedDemo(t)
	res, err := s.Callees("demo/lib", "UseDoubleLots", 0)
	if err != nil {
		t.Fatal(err)
	}
	page0 := wantPage(t, res, "callees", 60, 50, 50)
	res2, err := s.Callees("demo/lib", "UseDoubleLots", 50)
	if err != nil {
		t.Fatal(err)
	}
	page1 := wantPage(t, res2, "callees", 60, 10, 0)
	assertOrderedDisjoint(t, "call_pos", page0, page1)
}

func TestSearchPaging(t *testing.T) {
	s := pagedDemo(t)
	res, err := s.Search("pagedsym", 0)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, res, "symbols", 60, 50, 50)
	res2, err := s.Search("pagedsym", 50)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, res2, "symbols", 60, 10, 0)
}

func TestImplementationsPaging(t *testing.T) {
	s := pagedDemo(t)
	// 60 generated PagedPutterNN + Store
	res, err := s.Implementations("demo/lib", "ManyPutter", 0)
	if err != nil {
		t.Fatal(err)
	}
	page0 := wantPage(t, res, "types", 61, 50, 50)
	res2, err := s.Implementations("demo/lib", "ManyPutter", 50)
	if err != nil {
		t.Fatal(err)
	}
	page1 := wantPage(t, res2, "types", 61, 11, 0)
	// Stable across calls: same first window again.
	again, err := s.Implementations("demo/lib", "ManyPutter", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hitList(t, again["types"]), page0) {
		t.Error("same offset returned a different window on a second call")
	}
	if len(page0)+len(page1) != 61 {
		t.Errorf("pages cover %d entries, want 61", len(page0)+len(page1))
	}
}

func TestPagingOffsetPastEnd(t *testing.T) {
	s := pagedDemo(t)
	res, err := s.Refs("demo/lib", "Double", 1000)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, res, "refs", 62, 0, 0)
	// Negative offsets clamp to the start rather than reject.
	res2, err := s.Refs("demo/lib", "Double", -5)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, res2, "refs", 62, 50, 50)
}

// Query must thread offset through its kind dispatch to every paging kind.
func TestQueryThreadsOffset(t *testing.T) {
	s := pagedDemo(t)
	res, err := s.Query("refs", "demo/lib", "Double", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, res, "refs", 62, 12, 0)
	res, err = s.Query("search", "", "", "pagedsym", 50)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, res, "symbols", 60, 10, 0)
	res, err = s.Query("callers", "demo/lib", "Double", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, res, "callers", 61, 11, 0)
}
