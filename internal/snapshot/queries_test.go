package snapshot

import (
	"fmt"
	"strings"
	"testing"
)

func TestQueryExistingKinds(t *testing.T) {
	s := demo(t)
	if res, err := s.Query("search", "", "", "dou", 0); err != nil || res["count"].(int) != 1 {
		t.Fatalf("search via query: %v %v", res, err)
	}
	if res, err := s.Query("inspect", "demo/lib", "Double", "", 0); err != nil || res["kind"] != "func" {
		t.Fatalf("inspect via query: %v %v", res, err)
	}
	if res, err := s.Query("refs", "demo/lib", "Double", "", 0); err != nil || res["count"].(int) != 2 {
		t.Fatalf("refs via query: %v %v", res, err)
	}
}

func TestCallers(t *testing.T) {
	s := demo(t)
	res, err := s.Query("callers", "demo/lib", "Double", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	hits, ok := res["callers"].([]callerHit)
	if !ok || len(hits) != 1 || hits[0].Pkg != "demo" || hits[0].Sym != "main" {
		t.Fatalf("got %v", res)
	}
}

func TestCallees(t *testing.T) {
	s := demo(t)
	res, err := s.Query("callees", "demo/lib", "UseHelper", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	hits, ok := res["callees"].([]calleeHit)
	if !ok || len(hits) != 1 || hits[0].Pkg != "demo/lib" || hits[0].Sym != "helper" {
		t.Fatalf("got %v", res)
	}
}

func TestImplementations(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "type Putter interface {\n\tPut(int)\n}"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Query("implementations", "demo/lib", "Putter", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(res["types"]), "Store") {
		t.Fatalf("got %v", res)
	}

	// Reverse direction: concrete type -> interfaces satisfied.
	res2, err := s.Query("implementations", "demo/lib", "Store", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(res2["interfaces"]), "Putter") {
		t.Fatalf("got %v", res2)
	}
}

func TestDoc(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "// Double returns twice v.\nfunc Double(v int) int { return v * 2 }"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Query("doc", "demo/lib", "Double", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(res["doc"]), "Double returns twice v") {
		t.Fatalf("got %v", res)
	}
}
