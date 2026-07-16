package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"

	"golang.org/x/tools/go/packages"
)

// resolveWorkspaceFile turns a module-relative (or absolute) path into the
// absolute path plus the loaded package that compiles it. Test-variant
// packages are skipped so the primary package's types answer the reference
// check.
func resolveWorkspaceFile(s *Snapshot, path string) (string, *packages.Package) {
	abs := path
	if !filepath.IsAbs(abs) && len(s.pkgs) > 0 && s.pkgs[0].Module != nil {
		abs = filepath.Join(s.pkgs[0].Module.Dir, path)
	}
	for _, p := range s.pkgs {
		if p.Types == nil {
			continue
		}
		for _, f := range p.CompiledGoFiles {
			if f == abs {
				return abs, p
			}
		}
	}
	return abs, nil
}

// deleteFileOp removes one file from the workspace. Rejects while any
// package-level symbol the file declares is referenced from outside it
// (listing the reference positions); references to methods declared in the
// file surface through the post-delete reload's diagnostics instead. A
// deleted file changes the package's file set, the same class as creating
// one, so it takes the delete-and-reload path with cleanupFileOps owning
// restoration on every failure and dry_run path.
type deleteFileOp struct{}

func (deleteFileOp) name() string { return "delete_file" }

func (deleteFileOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Path string `json:"path"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	if a.Path == "" {
		return &Reject{Reason: "path is required"}
	}
	s := ctx.s
	abs, owner := resolveWorkspaceFile(s, a.Path)
	if owner == nil {
		return &Reject{Reason: "file not found", Detail: a.Path}
	}

	orig, err := os.ReadFile(abs)
	if err != nil {
		return &Reject{Reason: "file not found", Detail: abs}
	}
	var refs []Diagnostic
	scope := owner.Types.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if obj == nil || s.fset.Position(obj.Pos()).Filename != abs {
			continue
		}
		refs = append(refs, referencePositions(s, obj, abs, 0, len(orig))...)
		if len(refs) >= 10 {
			refs = refs[:10]
			break
		}
	}
	if len(refs) > 0 {
		return &Reject{Reason: "file declares referenced symbols", Detail: a.Path,
			Diagnostics: refs}
	}

	before := errorSet(s.errors())
	ctx.addBaseline(before)
	if err := os.Remove(abs); err != nil {
		return &Reject{Reason: "failed to delete file", Detail: err.Error()}
	}
	if ctx.deletedFiles == nil {
		ctx.deletedFiles = map[string][]byte{}
	}
	ctx.deletedFiles[abs] = orig
	s.loaded = false
	if _, err := s.load(); err != nil {
		return &Reject{Reason: "workspace failed to reload", Detail: err.Error()}
	}
	if diags := filterNew(s.errors(), before); len(diags) > 0 {
		return diagnosticRepairs(&Reject{Reason: "delete does not typecheck", Diagnostics: diags})
	}
	ctx.addAffected(owner.PkgPath)
	ctx.noteTouched(owner.PkgPath, filepath.Base(abs), true)
	return nil
}

func init() {
	opRegistry["delete_file"] = func() patchOp { return deleteFileOp{} }
	declOps["delete_file"] = true
}
