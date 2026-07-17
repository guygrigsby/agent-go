package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitFixture builds a two-commit repo: write pre files, commit, apply post
// (nil content deletes), commit, and return the repo dir and post sha.
func gitFixture(t *testing.T, pre, post map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(files map[string]string) {
		for rel, src := range files {
			p := filepath.Join(dir, rel)
			if src == "" {
				os.Remove(p)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	run("init", "-q")
	write(pre)
	run("add", "-A")
	run("commit", "-qm", "pre")
	write(post)
	run("add", "-A")
	run("commit", "-qm", "post")
	return dir, run("rev-parse", "HEAD")
}

// The reconciler turns a commit's authored rewrites into decl-level ops:
// changed and new decls upsert with post-state text (aliased imports
// named), removed decls delete, commit-deleted files delete_file, and
// movers never get source-side deletes (move_decl already excised them).
func TestReconcileOps(t *testing.T) {
	pre := map[string]string{
		"go.mod": "module mod\n\ngo 1.24\n",
		"a/a.go": `package a

func Keep() int { return 1 }

func Change() int { return 1 }

func Gone() int { return 1 }

// Moved relocates to b in the commit.
func Moved() int { return 1 }

// Stay relocates to b unchanged.
func Stay() int { return 9 }
`,
		"a/dead.go": `package a

var _ = Keep()
`,
		"a/a_test.go": `package a

import "testing"

var _ = Moved

// TestMoved relocates to b in the commit.
func TestMoved(t *testing.T) {
	if Moved() != 1 {
		t.Fatal("nope")
	}
}
`,
	}
	post := map[string]string{
		"a/a.go": `package a

func Keep() int { return 1 }

func Change() int { return 2 }
`,
		"a/dead.go":   "",
		"a/a_test.go": "",
		"b/b_test.go": `package b

import "testing"

// TestMoved relocates to b in the commit.
func TestMoved(t *testing.T) {
	if Moved() != 2 {
		t.Fatal("nope")
	}
}
`,
		"b/b.go": `package b

import (
	stde "errors"
)

// Moved relocates to b in the commit.
func Moved() int { return 2 }

// Stay relocates to b unchanged.
func Stay() int { return 9 }

func Fresh() error { return stde.New("x") }
`,
	}
	dir, sha := gitFixture(t, pre, post)
	moves := []MoveSpec{
		{Pkg: "mod/a", Sym: "Moved", ToPkg: "mod/b"},
		{Pkg: "mod/a", Sym: "Stay", ToPkg: "mod/b"},
		{Pkg: "mod/a", Sym: "TestMoved", ToPkg: "mod/b"},
	}
	plan, err := reconcileOps(dir, sha, "mod", moves)
	if err != nil {
		t.Fatal(err)
	}
	ops, notes := append(append([]map[string]any{}, plan.preOps...), plan.ops...), plan.notes
	find := func(op, want string) map[string]any {
		for _, o := range ops {
			if o["op"] != op {
				continue
			}
			text, _ := o["text"].(string)
			sym, _ := o["sym"].(string)
			path, _ := o["path"].(string)
			if syms, ok := o["syms"].([]string); ok {
				sym += " " + strings.Join(syms, " ")
			}
			if strings.Contains(text+" "+sym+" "+path, want) {
				return o
			}
		}
		return nil
	}
	if o := find("upsert_decl", "func Change() int { return 2 }"); o == nil {
		t.Errorf("no upsert for changed decl; ops=%v notes=%v", ops, notes)
	} else if o["pkg"] != "mod/a" {
		t.Errorf("Change upsert pkg = %v", o["pkg"])
	}
	if o := find("upsert_decl", "func Moved() int { return 2 }"); o == nil {
		t.Error("no upsert for rewritten mover in target")
	} else if o["pkg"] != "mod/b" {
		t.Errorf("Moved upsert pkg = %v", o["pkg"])
	}
	fresh := find("upsert_decl", "Fresh")
	if fresh == nil {
		t.Fatal("no upsert for new decl Fresh")
	}
	imps, _ := fresh["imports"].([]map[string]string)
	if len(imps) != 1 || imps[0]["path"] != "errors" || imps[0]["name"] != "stde" {
		t.Errorf("Fresh imports = %v, want aliased errors", fresh["imports"])
	}
	if o := find("delete_decl", "Gone"); o == nil {
		t.Error("no delete for removed decl Gone")
	}
	if o := find("delete_decl", "Moved"); o == nil {
		t.Error("rewritten mover missing its source-side delete")
	}
	if o := find("delete_decl", "Stay"); o != nil {
		t.Errorf("unchanged mover got a source-side delete: %v", o)
	}
	if o := find("upsert_decl", "func Stay"); o != nil {
		t.Errorf("unchanged mover got an upsert: %v", o)
	}
	if o := find("delete_file", "a/dead.go"); o != nil {
		t.Errorf("non-test deleted file must not delete_file (ledger hazard): %v", o)
	}
	for _, o := range ops {
		if text, _ := o["text"].(string); strings.Contains(text, "func Keep") {
			t.Errorf("unchanged decl got an op: %v", o)
		}
	}
	// The commit-deleted test file holding a mover: delete_file leads, the
	// mover leaves the batch, and its rewritten post-state upserts instead.
	if o := find("delete_file", "a/a_test.go"); o == nil {
		t.Error("no leading delete_file for commit-deleted test file with mover")
	} else if found := func() bool {
		for _, p := range plan.preOps {
			if p["path"] == "a/a_test.go" {
				return true
			}
		}
		return false
	}(); !found {
		t.Error("test-file delete not in preOps")
	}
	if !plan.dropMovers["mod/a|TestMoved"] {
		t.Errorf("TestMoved not dropped from the batch: %v", plan.dropMovers)
	}
	if !plan.dropMovers["mod/a|Moved"] {
		t.Errorf("rewritten mover not dropped from the batch: %v", plan.dropMovers)
	}
	if plan.dropMovers["mod/a|Stay"] {
		t.Error("unchanged mover wrongly dropped")
	}
	if o := find("upsert_decl", "Moved() != 2"); o == nil {
		t.Error("no post-state upsert for the dropped test mover")
	}
}
