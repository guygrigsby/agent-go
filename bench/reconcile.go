package bench

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// reconcilePlan is the decl-level expression of a ground-truth commit's
// authored rewrites, split by where each op must sit relative to the
// batched moves. delete_file reloads the workspace from disk mid-patch and
// cannot see the ledger, so preOps must run before anything else touches
// the ledger. dropMovers names the movers the batch must skip: a mover the
// commit rewrote cannot travel by move_decl (the landed original would
// need a ledger-aware replace no mid-patch op has, and may drag imports
// the target cannot hold, 687dd1bd's session -> common -> session), so
// its source decl deletes and its post-state upserts into the target; the
// task predicate still verifies the relocation.
type reconcilePlan struct {
	preOps     []map[string]any // delete_file, before any ledger op
	ops        []map[string]any // upserts then delete_decls, after the moves
	dropMovers map[string]bool  // "pkg|sym" excluded from the move batch
	notes      []string
}

// fileState is one touched file's parsed decls at one rev.
type fileState struct {
	file  string
	pkg   string // import path
	decls *fileDecls
}

// reconcileOps turns a ground-truth commit's authored rewrites into
// decl-level patch ops, so the oracle can express a move whose commit also
// rewrote code (a dropped parameter, a new registration helper): for every
// .go file the commit touched, a decl whose text changed or is new becomes
// an upsert_decl carrying the post-state text (with the post-file imports
// it references, so aliases survive), and removed decls become one batched
// delete_decl per package (intra-set references delete together). Movers
// with identical pre and post text stay in the move batch and get no
// source-side deletes; rewritten movers leave the batch (dropMovers) and
// travel as delete + upsert instead. Commit-deleted _test.go files become
// a leading delete_file (they carry anonymous interface assertions no decl
// op can address); commit-deleted non-test files delete per decl and leave
// an import-pruned shell behind (delete_file cannot follow ledger ops).
//
// v1 ceilings, each skipped WITH a note: grouped multi-spec const/var/type
// blocks (upsert_decl replaces single decls), external test packages
// (package foo_test addresses a different import path), and anonymous
// declarations (var _ = ...).
// applied names movers ("pkg|sym") earlier patches already relocated:
// their source declarations are gone from the live workspace, so the
// git-derived delete would reject on "symbol not found"; their target
// upserts still run (a compound mover's post-state can differ beyond the
// name).
func reconcileOps(gitDir, sha, modPath string, moves []MoveSpec, applied map[string]bool) (*reconcilePlan, error) {
	statusOut, err := exec.Command("git", "-C", gitDir, "show", "--no-renames", "--name-status", "--format=", sha).Output()
	if err != nil {
		return nil, fmt.Errorf("git show --name-status %s: %w", sha[:8], err)
	}
	plan := &reconcilePlan{dropMovers: map[string]bool{}}
	notes := &plan.notes

	// Pass 1: parse every touched file at both revs, indexed by package,
	// so mover rewrites can be detected across the source/target split.
	var pres, posts []fileState
	preByPkg := map[string]map[string]declInfo{}
	postByPkg := map[string]map[string]declInfo{}
	deletedFiles := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(statusOut)), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 || !strings.HasSuffix(parts[1], ".go") {
			continue
		}
		status, file := parts[0], parts[1]
		// Vendored copies and nested modules are not workspace members the
		// ops can address: the vendor tree follows its module, and a nested
		// module's packages resolve from the module cache. Skip with the
		// blocker named; a main-module decl needing their new surface fails
		// the end-of-list typecheck loudly, never silently.
		if strings.HasPrefix(file, "vendor/") {
			*notes = append(*notes, "skip "+file+": vendored copy of another module")
			continue
		}
		if mod := nestedModuleOf(gitDir, sha, file); mod != "" {
			*notes = append(*notes, "skip "+file+": inside nested module "+mod)
			continue
		}
		pkgPath := modPath
		if dir := path.Dir(file); dir != "." {
			pkgPath = modPath + "/" + dir
		}
		if pre := gitFileDecls(gitDir, sha+"^", file); pre != nil {
			pres = append(pres, fileState{file, pkgPath, pre})
			if preByPkg[pkgPath] == nil {
				preByPkg[pkgPath] = map[string]declInfo{}
			}
			for k, d := range pre.decls {
				preByPkg[pkgPath][k] = d
			}
		}
		if status == "D" {
			deletedFiles[file] = true
			continue
		}
		post := gitFileDecls(gitDir, sha, file)
		if post == nil {
			continue
		}
		if strings.HasSuffix(post.pkgName, "_test") {
			*notes = append(*notes, "skip "+file+": external test package "+post.pkgName)
			continue
		}
		posts = append(posts, fileState{file, pkgPath, post})
		if postByPkg[pkgPath] == nil {
			postByPkg[pkgPath] = map[string]declInfo{}
		}
		for k, d := range post.decls {
			postByPkg[pkgPath][k] = d
		}
	}

	// Movers whose text survives the commit unchanged travel by move_decl;
	// the rest are dropped to the delete+upsert path. Methods follow their
	// receiver type's verdict via the base-name check in the delete pass.
	moverInBatch := map[string]bool{}
	for _, m := range moves {
		if m.ToName != "" {
			// A rename changes the text by definition; the new name upserts,
			// the old one deletes.
			plan.dropMovers[m.Pkg+"|"+m.Sym] = true
			*notes = append(*notes, "compound mover "+m.Sym+" -> "+m.ToName+": rename travels as delete+upsert")
			continue
		}
		pre, okPre := preByPkg[m.Pkg][m.Sym]
		post, okPost := postByPkg[m.ToPkg][m.Sym]
		if okPre && okPost && pre.text == post.text {
			moverInBatch[m.Pkg+"|"+m.Sym] = true
			continue
		}
		plan.dropMovers[m.Pkg+"|"+m.Sym] = true
		if okPre && okPost {
			*notes = append(*notes, "mover "+m.Sym+" rewritten by the commit: delete+upsert instead of move")
		} else {
			*notes = append(*notes, "mover "+m.Sym+" not present at both revs: delete+upsert instead of move")
		}
	}

	// Pass 2: emit ops. Upserts from post files (changed or new decls),
	// batched deletes from pre files (decls gone at post), file deletes
	// for commit-deleted test files.
	var upserts, deletes []map[string]any
	deleteSyms := map[string][]string{}
	// A file and its vendored copy diff to identical ops; emit each
	// (pkg, decl) once.
	seenUpsert, seenDelete := map[string]bool{}, map[string]bool{}
	for _, ps := range posts {
		var keys []string
		for k := range ps.decls.decls {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			d := ps.decls.decls[k]
			if d.grouped {
				*notes = append(*notes, "skip "+ps.file+" "+k+": group semantics pin it (iota, inherited value, or multi-name spec)")
				continue
			}
			if base := baseOf(k); moverInBatch[moverKeyFor(moves, ps.pkg, k)] || moverInBatch[moverKeyFor(moves, ps.pkg, base)] {
				continue // move_decl carries it verbatim
			}
			if old, ok := preByPkg[ps.pkg][k]; ok && old.text == d.text {
				continue
			}
			if seenUpsert[ps.pkg+"|"+k] {
				continue
			}
			seenUpsert[ps.pkg+"|"+k] = true
			op := map[string]any{"op": "upsert_decl", "pkg": ps.pkg, "text": d.text}
			if imps := ps.decls.importsUsedBy(d); len(imps) > 0 {
				op["imports"] = imps
			}
			upserts = append(upserts, op)
		}
	}
	for _, ps := range pres {
		if deletedFiles[ps.file] && strings.HasSuffix(ps.file, "_test.go") {
			// Delete before any ledger op; anonymous assertions go with it.
			plan.preOps = append(plan.preOps, map[string]any{"op": "delete_file", "path": ps.file})
			continue
		}
		var keys []string
		for k := range ps.decls.decls {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := postByPkg[ps.pkg][k]; ok {
				continue
			}
			base := baseOf(k)
			if moverInBatch[ps.pkg+"|"+k] || moverInBatch[ps.pkg+"|"+base] ||
				applied[ps.pkg+"|"+k] || applied[ps.pkg+"|"+base] {
				continue // move_decl excises (or already excised) it
			}
			if ps.decls.decls[k].grouped {
				*notes = append(*notes, "skip "+ps.file+" "+k+": group semantics pin it (iota, inherited value, or multi-name spec)")
				continue
			}
			if seenDelete[ps.pkg+"|"+k] {
				continue
			}
			seenDelete[ps.pkg+"|"+k] = true
			deleteSyms[ps.pkg] = append(deleteSyms[ps.pkg], k)
		}
		if deletedFiles[ps.file] {
			*notes = append(*notes, "keep "+ps.file+": deleted per decl, an import-pruned shell remains (delete_file cannot follow ledger ops)")
		}
	}
	var delPkgs []string
	for p := range deleteSyms {
		delPkgs = append(delPkgs, p)
	}
	sort.Strings(delPkgs)
	for _, p := range delPkgs {
		syms := deleteSyms[p]
		sort.Strings(syms)
		deletes = append(deletes, map[string]any{"op": "delete_decl", "pkg": p, "syms": syms})
	}
	plan.ops = append(upserts, deletes...)
	return plan, nil
}

// nestedModuleOf returns the nearest ancestor directory (not the repo
// root) holding a go.mod at sha, or "" when the file belongs to the root
// module.
func nestedModuleOf(gitDir, sha, file string) string {
	for dir := path.Dir(file); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if err := exec.Command("git", "-C", gitDir, "cat-file", "-e", sha+":"+dir+"/go.mod").Run(); err == nil {
			return dir
		}
	}
	return ""
}

// moverKeyFor maps a decl key in pkg to the mover key that owns it: for a
// decl in a move's TARGET package the batch membership was recorded under
// the SOURCE package.
func moverKeyFor(moves []MoveSpec, pkg, name string) string {
	for _, m := range moves {
		if m.Sym != name {
			continue
		}
		if m.Pkg == pkg || m.ToPkg == pkg {
			return m.Pkg + "|" + m.Sym
		}
	}
	return pkg + "|" + name
}

func baseOf(key string) string {
	if recv, _, ok := strings.Cut(key, "."); ok {
		return recv
	}
	return key
}

type declInfo struct {
	text    string
	grouped bool
	quals   map[string]bool // identifiers used as selector qualifiers
}

type fileDecls struct {
	pkgName string
	decls   map[string]declInfo
	imports []importSpec // in file order
}

type importSpec struct{ path, name string }

// importsUsedBy returns the post-file imports whose local name appears as
// a qualifier in the decl, aliased entries included, as the wire shape
// upsert_decl's imports arg takes.
func (fd *fileDecls) importsUsedBy(d declInfo) []map[string]string {
	var out []map[string]string
	for _, imp := range fd.imports {
		local := imp.name
		if local == "" {
			local = path.Base(imp.path)
		}
		if d.quals[local] {
			out = append(out, map[string]string{"path": imp.path, "name": imp.name})
		}
	}
	return out
}

// gitFileDecls parses one file at one rev into keyed top-level decls; nil
// when the file does not exist at that rev.
func gitFileDecls(gitDir, rev, file string) *fileDecls {
	src, err := exec.Command("git", "-C", gitDir, "show", rev+":"+file).Output()
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Base(file), src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	fd := &fileDecls{pkgName: f.Name.Name, decls: map[string]declInfo{}}
	for _, d := range f.Decls {
		var key string
		grouped := false
		start, end := d.Pos(), d.End()
		switch d := d.(type) {
		case *ast.FuncDecl:
			key = d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				key = recvBase(d.Recv.List[0].Type) + "." + d.Name.Name
			}
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				for _, sp := range d.Specs {
					if is, ok := sp.(*ast.ImportSpec); ok {
						name := ""
						if is.Name != nil {
							name = is.Name.Name
						}
						fd.imports = append(fd.imports, importSpec{strings.Trim(is.Path.Value, `"`), name})
					}
				}
				continue
			}
			if len(d.Specs) > 1 {
				// Grouped block: index each member as its own standalone
				// decl (the engine replaces or excises single specs in
				// place). Members the group semantics pin stay grouped=true
				// so the caller notes the skip: multi-name specs, iota
				// anywhere in the group, and inherited or inherited-from
				// expressions.
				groupIota := groupUsesIota(d)
				for i, sp := range d.Specs {
					name, text, ok := memberDecl(fset, src, d, i)
					if name == "" || name == "_" {
						continue
					}
					pinned := !ok || groupIota || memberInherits(d, i) || followerInherits(d, i)
					quals := map[string]bool{}
					ast.Inspect(sp, func(n ast.Node) bool {
						if sel, isSel := n.(*ast.SelectorExpr); isSel {
							if id, isID := sel.X.(*ast.Ident); isID {
								quals[id.Name] = true
							}
						}
						return true
					})
					fd.decls[name] = declInfo{text: text, grouped: pinned, quals: quals}
				}
				continue
			}
			switch sp := d.Specs[0].(type) {
			case *ast.TypeSpec:
				key = sp.Name.Name
			case *ast.ValueSpec:
				if len(sp.Names) != 1 {
					grouped = true
				}
				key = sp.Names[0].Name
			}
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
		default:
			continue
		}
		if key == "" || key == "_" {
			continue
		}
		quals := map[string]bool{}
		ast.Inspect(d, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					quals[id.Name] = true
				}
			}
			return true
		})
		so := fset.Position(start).Offset
		eo := fset.Position(end).Offset
		fd.decls[key] = declInfo{text: string(src[so:eo]), grouped: grouped, quals: quals}
	}
	return fd
}

// memberDecl renders group member i as a standalone declaration ("const
// Name = ...", doc comment kept); ok is false for shapes the engine's
// grouped ops cannot address (multi-name specs).
func memberDecl(fset *token.FileSet, src []byte, d *ast.GenDecl, i int) (name, text string, ok bool) {
	render := func(name string, specStart, specEnd token.Pos, doc *ast.CommentGroup) (string, string, bool) {
		spec := string(src[fset.Position(specStart).Offset:fset.Position(specEnd).Offset])
		text := d.Tok.String() + " " + spec
		if doc != nil {
			// Doc comment precedes the token in the standalone form.
			text = string(src[fset.Position(doc.Pos()).Offset:fset.Position(doc.End()).Offset]) + "\n" + text
		}
		return name, text, true
	}
	switch sp := d.Specs[i].(type) {
	case *ast.TypeSpec:
		return render(sp.Name.Name, sp.Pos(), sp.End(), sp.Doc)
	case *ast.ValueSpec:
		if len(sp.Names) != 1 {
			return sp.Names[0].Name, "", false
		}
		return render(sp.Names[0].Name, sp.Pos(), sp.End(), sp.Doc)
	}
	return "", "", false
}

// groupUsesIota reports whether any spec in the group references iota:
// member positions then define values, and no single-spec op is safe.
func groupUsesIota(d *ast.GenDecl) bool {
	uses := false
	for _, sp := range d.Specs {
		vs, ok := sp.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, v := range vs.Values {
			ast.Inspect(v, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "iota" {
					uses = true
				}
				return true
			})
		}
	}
	return uses
}

// memberInherits reports whether spec i repeats the previous member's
// expression (a bare const spec); followerInherits whether spec i+1 does.
func memberInherits(d *ast.GenDecl, i int) bool {
	vs, ok := d.Specs[i].(*ast.ValueSpec)
	return ok && len(vs.Values) == 0 && vs.Type == nil
}

func followerInherits(d *ast.GenDecl, i int) bool {
	return i+1 < len(d.Specs) && memberInherits(d, i+1)
}

func recvBase(t ast.Expr) string {
	for {
		switch tt := t.(type) {
		case *ast.StarExpr:
			t = tt.X
		case *ast.IndexExpr:
			t = tt.X
		case *ast.IndexListExpr:
			t = tt.X
		case *ast.Ident:
			return tt.Name
		default:
			return ""
		}
	}
}
