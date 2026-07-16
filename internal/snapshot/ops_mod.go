package snapshot

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// workspaceModule returns the module the snapshot loaded, or nil outside
// module mode.
func workspaceModule(s *Snapshot) *packages.Module {
	if len(s.pkgs) == 0 {
		return nil
	}
	return s.pkgs[0].Module
}

// noteModFiles snapshots go.mod and go.sum bytes into ctx.modifiedFiles the
// first time a module op runs, so cleanupFileOps restores them on any
// failure or dry_run path. A go.sum that does not exist yet records a nil
// entry, restored as deletion.
func noteModFiles(ctx *patchCtx, mod *packages.Module) {
	if ctx.modifiedFiles == nil {
		ctx.modifiedFiles = map[string][]byte{}
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		p := filepath.Join(mod.Dir, name)
		if _, ok := ctx.modifiedFiles[p]; ok {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			b = nil
		}
		ctx.modifiedFiles[p] = b
	}
}

// runModOp is the shared core of the module ops: snapshot go.mod/go.sum for
// restoration, run one go command in the module directory, reload the
// workspace (the module graph changed), and reject on NEW diagnostics.
// ponytail: no command timeout; a proxy hang holds the patch open — add one
// when it bites.
func runModOp(ctx *patchCtx, reason string, args ...string) *Reject {
	s := ctx.s
	mod := workspaceModule(s)
	if mod == nil {
		return &Reject{Reason: "workspace has no module"}
	}
	noteModFiles(ctx, mod)
	before := errorSet(s.errors())
	ctx.addBaseline(before)
	cmd := exec.Command("go", args...)
	cmd.Dir = mod.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return &Reject{Reason: reason, Detail: strings.TrimSpace(string(out))}
	}
	s.loaded = false
	if _, err := s.load(); err != nil {
		return &Reject{Reason: "workspace failed to reload", Detail: err.Error()}
	}
	if diags := filterNew(s.errors(), before); len(diags) > 0 {
		return diagnosticRepairs(&Reject{Reason: reason, Diagnostics: diags})
	}
	return nil
}

// addDependencyOp runs go get module@version against the workspace module.
// The go tool owns requirement resolution; the op owns atomicity (go.mod
// and go.sum restore on any later rejection) and validation (the post-get
// reload must introduce no new diagnostics).
type addDependencyOp struct{}

func (addDependencyOp) name() string { return "add_dependency" }

func (addDependencyOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Module  string `json:"module"`
		Version string `json:"version"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	if a.Module == "" {
		return &Reject{Reason: "module is required"}
	}
	v := a.Version
	if v == "" {
		v = "latest"
	}
	return runModOp(ctx, "go get failed", "get", a.Module+"@"+v)
}

// removeDependencyOp drops a requirement via go get module@none, rejected
// while any workspace file still imports the module (listing the import
// positions).
type removeDependencyOp struct{}

func (removeDependencyOp) name() string { return "remove_dependency" }

func (removeDependencyOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Module string `json:"module"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	if a.Module == "" {
		return &Reject{Reason: "module is required"}
	}
	s := ctx.s
	var refs []Diagnostic
	seen := map[string]bool{}
	for _, p := range s.pkgs {
		for _, f := range p.Syntax {
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if path != a.Module && !strings.HasPrefix(path, a.Module+"/") {
					continue
				}
				pos := s.fset.Position(imp.Pos()).String()
				if seen[pos] {
					continue
				}
				seen[pos] = true
				refs = append(refs, Diagnostic{Pos: pos, Msg: "imports " + path})
				if len(refs) >= 10 {
					break
				}
			}
		}
	}
	if len(refs) > 0 {
		return &Reject{Reason: "module is still imported", Detail: a.Module,
			Diagnostics: refs}
	}
	return runModOp(ctx, "go get failed", "get", a.Module+"@none")
}

// modTidyOp runs go mod tidy with the same restore-and-validate wrapper.
type modTidyOp struct{}

func (modTidyOp) name() string { return "mod_tidy" }

func (modTidyOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct{}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	return runModOp(ctx, "go mod tidy failed", "mod", "tidy")
}

func init() {
	opRegistry["add_dependency"] = func() patchOp { return addDependencyOp{} }
	declOps["add_dependency"] = true
	opRegistry["remove_dependency"] = func() patchOp { return removeDependencyOp{} }
	declOps["remove_dependency"] = true
	opRegistry["mod_tidy"] = func() patchOp { return modTidyOp{} }
	declOps["mod_tidy"] = true
}
