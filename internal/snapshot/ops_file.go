package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/imports"
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
	if refs := fileExternalRefs(s, owner, abs, len(orig)); len(refs) > 0 {
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

// fileExternalRefs lists (up to 10) references from outside the file to any
// package-level symbol it declares: the blocker shared by delete_file (the
// symbols would dangle) and cross-package move_file (every qualifier would
// be wrong).
func fileExternalRefs(s *Snapshot, owner *packages.Package, abs string, size int) []Diagnostic {
	var refs []Diagnostic
	scope := owner.Types.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if obj == nil || s.fset.Position(obj.Pos()).Filename != abs {
			continue
		}
		refs = append(refs, referencePositions(s, obj, abs, 0, size)...)
		if len(refs) >= 10 {
			return refs[:10]
		}
	}
	return refs
}

// moveFileOp relocates one file. Within its package it is a pure path
// rename; across packages the package clause is rewritten to the target
// package (which must already be loaded) and a now-self import dropped, but
// only while no package-level symbol the file declares is referenced from
// outside it — every external qualifier would be wrong, so that case
// rejects with the positions (move the declarations individually with
// move_decl instead). Either way the file set changes, so it takes the
// write-and-reload path with cleanupFileOps owning restoration.
type moveFileOp struct{}

func (moveFileOp) name() string { return "move_file" }

func (moveFileOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	if a.From == "" || a.To == "" {
		return &Reject{Reason: "from and to are required"}
	}
	s := ctx.s
	absFrom, owner := resolveWorkspaceFile(s, a.From)
	if owner == nil {
		return &Reject{Reason: "file not found", Detail: a.From}
	}
	absTo := a.To
	if !filepath.IsAbs(absTo) && len(s.pkgs) > 0 && s.pkgs[0].Module != nil {
		absTo = filepath.Join(s.pkgs[0].Module.Dir, a.To)
	}
	if _, err := os.Stat(absTo); err == nil {
		return &Reject{Reason: "target file exists", Detail: a.To}
	}
	orig, err := os.ReadFile(absFrom)
	if err != nil {
		return &Reject{Reason: "file not found", Detail: absFrom}
	}

	content := orig
	tgtPkg := owner.PkgPath
	if filepath.Dir(absFrom) != filepath.Dir(absTo) {
		var tgt *packages.Package
		for _, p := range s.pkgs {
			if p.Types == nil || strings.HasSuffix(p.ID, ".test") {
				continue
			}
			for _, f := range p.CompiledGoFiles {
				if filepath.Dir(f) == filepath.Dir(absTo) {
					tgt = p
					break
				}
			}
			if tgt != nil {
				break
			}
		}
		if tgt == nil {
			return &Reject{Reason: "target package not found", Detail: filepath.Dir(a.To),
				DidYouMean: s.suggestPackages(filepath.Dir(a.To))}
		}
		if refs := fileExternalRefs(s, owner, absFrom, len(orig)); len(refs) > 0 {
			return &Reject{Reason: "file declares referenced symbols", Detail: a.From,
				Diagnostics: refs}
		}
		var clauseStart, clauseEnd int
		for _, f := range owner.Syntax {
			if s.fset.Position(f.Pos()).Filename != absFrom {
				continue
			}
			clauseStart = s.fset.Position(f.Name.Pos()).Offset
			clauseEnd = s.fset.Position(f.Name.End()).Offset
			break
		}
		if clauseEnd == 0 {
			return &Reject{Reason: "file not found", Detail: a.From}
		}
		rewritten := append(append(append([]byte{}, orig[:clauseStart]...),
			tgt.Types.Name()...), orig[clauseEnd:]...)
		fixed, ferr := imports.Process(absTo, rewritten, nil)
		if ferr != nil {
			return &Reject{Reason: "moved file does not parse", Detail: ferr.Error()}
		}
		content = fixed
		tgtPkg = tgt.PkgPath
	}

	before := errorSet(s.errors())
	ctx.addBaseline(before)
	if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
		return &Reject{Reason: "failed to move file", Detail: err.Error()}
	}
	if err := os.WriteFile(absTo, content, 0o644); err != nil {
		return &Reject{Reason: "failed to move file", Detail: err.Error()}
	}
	if err := os.Remove(absFrom); err != nil {
		os.Remove(absTo)
		return &Reject{Reason: "failed to move file", Detail: err.Error()}
	}
	ctx.createdFiles = append(ctx.createdFiles, absTo)
	if ctx.deletedFiles == nil {
		ctx.deletedFiles = map[string][]byte{}
	}
	ctx.deletedFiles[absFrom] = orig
	s.loaded = false
	if _, err := s.load(); err != nil {
		return &Reject{Reason: "workspace failed to reload", Detail: err.Error()}
	}
	if diags := filterNew(s.errors(), before); len(diags) > 0 {
		return diagnosticRepairs(&Reject{Reason: "move does not typecheck", Diagnostics: diags})
	}
	ctx.addAffected(owner.PkgPath)
	ctx.addAffected(tgtPkg)
	ctx.noteTouched(tgtPkg, filepath.Base(absTo), true)
	return nil
}

func init() {
	opRegistry["delete_file"] = func() patchOp { return deleteFileOp{} }
	declOps["delete_file"] = true
	opRegistry["move_file"] = func() patchOp { return moveFileOp{} }
	declOps["move_file"] = true
}
