package snapshot

import (
	"strings"
	"testing"
)

func TestParseSignatureText(t *testing.T) {
	sig, rej := parseSignatureText("(ctx context.Context, id string) (*Repo, error)")
	if rej != nil {
		t.Fatal(rej)
	}
	if len(sig.params) != 2 || sig.params[0].name != "ctx" || sig.params[1].typ != "string" {
		t.Fatalf("params: %+v", sig.params)
	}
	if sig.results != "(*Repo, error)" {
		t.Fatalf("results: %q", sig.results)
	}
	if _, rej := parseSignatureText("(ctx context.Context"); rej == nil {
		t.Fatal("unbalanced signature must reject")
	}
	sig, rej = parseSignatureText("(items ...string)")
	if rej != nil || !sig.params[0].variadic {
		t.Fatalf("variadic not detected: %+v, %v", sig, rej)
	}
}

func TestPlanArgs(t *testing.T) {
	oldSig, _ := parseSignatureText("(a int, b string, rest ...int)")
	newSig, _ := parseSignatureText("(ctx context.Context, b string, rest ...int)")
	plan, rej := planArgs(oldSig, newSig, map[string]string{"ctx": "context.Background()"}, 1)
	if rej != nil {
		t.Fatal(rej)
	}
	// Call f(1, "x", 2, 3): a dropped, ctx inserted, b carried, spread tail carried.
	got := plan.rewrite([]string{"1", `"x"`, "2", "3"}, false)
	want := []string{"context.Background()", `"x"`, "2", "3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Spread call f(1, "x", nums...): tail carried with the spread intact.
	got = plan.rewrite([]string{"1", `"x"`, "nums"}, true)
	want = []string{"context.Background()", `"x"`, "nums"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("spread: got %v, want %v", got, want)
	}
}

func TestPlanArgsRejects(t *testing.T) {
	oldSig, _ := parseSignatureText("(a int)")
	newSig, _ := parseSignatureText("(a int, ctx context.Context)")
	if _, rej := planArgs(oldSig, newSig, nil, 3); rej == nil {
		t.Fatal("new param without default must reject when call sites exist")
	}
	if _, rej := planArgs(oldSig, newSig, nil, 0); rej != nil {
		t.Fatalf("no call sites means no default needed: %v", rej)
	}
}
