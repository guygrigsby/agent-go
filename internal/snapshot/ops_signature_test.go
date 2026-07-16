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

// TestPlanArgsCarriesUnderscoreParams: underscore params cannot carry by
// name (planArgs indexes old params by name and skips "_"), so appending a
// parameter to func(ctx context.Context, _ DecryptFn) used to reject with
// "new parameter needs a default" for the untouched _ param. Same-type
// underscore params pair positionally and carry their call-site argument.
func TestPlanArgsCarriesUnderscoreParams(t *testing.T) {
	oldSig, _ := parseSignatureText("(a int, _ string, _ bool)")
	newSig, _ := parseSignatureText("(a int, _ string, _ bool, extra int)")
	plan, rej := planArgs(oldSig, newSig, map[string]string{"extra": "0"}, 2)
	if rej != nil {
		t.Fatal(rej)
	}
	got := plan.rewrite([]string{"1", `"x"`, "true"}, false)
	want := []string{"1", `"x"`, "true", "0"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
	// A type change breaks the pairing: the new _ takes its default and the
	// old argument is dropped rather than carried into the wrong slot.
	newSig, _ = parseSignatureText("(a int, _ string, _ int64)")
	plan, rej = planArgs(oldSig, newSig, map[string]string{"_": "9"}, 2)
	if rej != nil {
		t.Fatal(rej)
	}
	got = plan.rewrite([]string{"1", `"x"`, "true"}, false)
	want = []string{"1", `"x"`, "9"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("type-changed _: got %v, want %v", got, want)
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
