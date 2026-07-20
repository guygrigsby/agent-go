package snapshot

import (
	"encoding/json"
	"fmt"
	"go/format"
	"go/token"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// edit is one byte-range replacement in a file: replace length bytes at
// offset with text (length 0 for a pure insertion). This is the currency
// every decl op's edit-computation core speaks: the legacy single-op
// methods (Rename, AddParam, SetBody, UpsertDecl) apply a file's edits
// directly to disk in one descending-offset pass, exactly as they always
// did; the composable ops feed the same edits through
// patchCtx.applyDeclEdits, which folds them into a per-file ledger replayed
// against that file's pristine (pre-patch) bytes, so several decl ops
// touching the same file within one patch compose correctly regardless of
// application order.
type edit struct {
	file   string
	offset int
	length int
	text   string
}

// renameEdits computes a rename's edits without touching disk or the
// snapshot: identifier validation, symbol lookup, the reference scan, the
// workspace-boundary check, and the pre-existing-diagnostics baseline
// capture (the dirty set's current errors, keyed pos|msg, which the caller
// filters its post-splice retypecheck against — pre-existing rot must not
// block an edit, only NEW diagnostics reject), all
// exactly as Rename has always done inline. old is the symbol's current
// name, needed by both callers to verify a byte range still holds the
// expected text before splicing it. declPos is the target's pristine
// declaration position, which callers pass to renameExpected (alongside
// edits) to compute verifyResolution's expected/declKey once the ledger
// they're replaying against is known: Rename runs it immediately, against
// only its own edits (byFile) — unchanged from today. patchComposable
// defers it to a postCheck run after the full op list has applied, against
// the file's complete per-file ledger (ctx.declEdits), so a multi-rename
// patch accounts for every sibling decl op's shift in the same file, not
// just its own.
func renameEdits(s *Snapshot, pkgPath, sym, to string) (edits []edit, docEdit *edit, old string, declPos token.Position, newSym string, baseline map[string]bool, rej *Reject) {
	if !token.IsIdentifier(to) {
		rej = &Reject{Reason: "new name is not a valid identifier", Detail: to}
		if recv, name, ok := strings.Cut(to, "."); ok && token.IsIdentifier(recv) && token.IsIdentifier(name) {
			rej.Detail = to + ": pass only the new member name; the receiver stays"
			rej.DidYouMean = []string{name}
		}
		return nil, nil, "", token.Position{}, "", nil, rej
	}
	p, obj, rej0 := s.findObject(pkgPath, sym)
	if rej0 != nil {
		return nil, nil, "", token.Position{}, "", nil, rej0
	}
	old = obj.Name()
	if old == to {
		return nil, nil, "", token.Position{}, "", nil, &Reject{Reason: "symbol already has that name", Detail: to}
	}
	newSym = to
	if recv, _, isMethod := strings.Cut(sym, "."); isMethod {
		newSym = recv + "." + to
	}

	for _, r := range s.references(obj) {
		edits = append(edits, edit{r.pos.Filename, r.pos.Offset, len(old), to})
	}
	for _, e := range edits {
		if !strings.HasPrefix(e.file, s.dir+string(os.PathSeparator)) {
			return nil, nil, "", token.Position{}, "", nil, &Reject{Reason: "symbol is referenced outside the workspace", Detail: e.file}
		}
	}
	docEdit = renameDocEdit(s, p, old, sym, to)

	editedFiles := map[string]bool{}
	for _, e := range edits {
		editedFiles[e.file] = true
	}
	preDirty := append(s.dirtyByFiles(editedFiles), s.affected(pkgPath)...)
	baseline = errorSet(errorsIn(preDirty))

	declPos = s.fset.Position(obj.Pos())
	return edits, docEdit, old, declPos, newSym, baseline, nil
}

// renameDocEdit carries the doc comment's leading identifier with a
// rename: Go convention ties "// Greet returns..." to func Greet, so the
// convention-bound leading word moves and prose mentions stay. The edit
// splices like a reference but is excluded from resolution verification
// (a comment offset resolves to nothing). Nil when the decl has no doc
// or the doc does not open with the exact old name as a word.
func renameDocEdit(s *Snapshot, p *packages.Package, old, sym, to string) *edit {
	filename, _, doc := s.findDeclNode(p, old, sym)
	if filename == "" || doc == nil || len(doc.List) == 0 {
		return nil
	}
	first := doc.List[0].Text
	lead := "// " + old
	if !strings.HasPrefix(first, lead) {
		return nil
	}
	if rest := first[len(lead):]; rest != "" {
		c := rest[0]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			return nil // "// Greeter ...": a different word
		}
	}
	off := s.fset.Position(doc.List[0].Pos()).Offset + len("// ")
	return &edit{filename, off, len(old), to}
}

// renameExpected computes a rename's expected post-splice reference
// positions: each raw reference offset in edits (scanned against the
// pristine, pre-patch snapshot) shifted by the cumulative length delta of
// every edit in ledger that lands at an earlier offset in the same file.
// ledger is the file's full edit history — the rename's own edits only for
// the legacy single-op Rename path (byFile, so this reproduces exactly
// today's single-op math), or the whole per-file decl-op ledger
// (ctx.declEdits) at composable-patch postCheck time, when a sibling decl
// op with a different length delta (e.g. another rename in the same file)
// also shifts positions the naive same-file assumption misses.
func renameExpected(edits []edit, declPos token.Position, ledger map[string][]edit) (expected map[string]bool, declKey string) {
	expected = map[string]bool{}
	for _, e := range edits {
		shift := 0
		for _, le := range ledger[e.file] {
			if le.offset < e.offset {
				shift += len(le.text) - le.length
			}
		}
		key := fmt.Sprintf("%s:%d", e.file, e.offset+shift)
		expected[key] = true
		if e.file == declPos.Filename && e.offset == declPos.Offset {
			declKey = key
		}
	}
	return expected, declKey
}

// Rename renames a symbol at every reference, then proves the result: the
// affected packages must typecheck and every rewritten position must
// resolve to the renamed object. Reference capture (the new name silently
// binding to a shadowing declaration of the same type) fails the second
// check even when the compiler is happy. Any failure rolls back every file.
func (s *Snapshot) Rename(pkgPath, sym, to string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	edits, docEdit, old, declPos, newSym, baseline, rej := renameEdits(s, pkgPath, sym, to)
	if rej != nil {
		s.sugarRepairs(rej, "rename",
			map[string]any{"pkg": pkgPath, "sym": sym, "to": to}, s.resolves)
		return nil, rej
	}

	// The doc edit splices with everything else but stays out of the
	// resolution expectations below; expected keys come from edits alone,
	// shifted against the full ledger including the doc edit.
	splices := edits
	if docEdit != nil {
		splices = append(append([]edit{}, edits...), *docEdit)
	}
	byFile := map[string][]edit{}
	editedFiles := map[string]bool{}
	for _, e := range splices {
		byFile[e.file] = append(byFile[e.file], e)
		editedFiles[e.file] = true
	}
	// Ledger is this rename's own edits only — the single-op path never
	// shares a file-edit history with another op, so this is exactly
	// today's original per-op math.
	expected, declKey := renameExpected(edits, declPos, byFile)
	// Apply per file, descending offset so earlier offsets stay valid.
	originals := map[string][]byte{}
	for file, fedits := range byFile {
		src, err := os.ReadFile(file)
		if err != nil {
			s.rollback(originals)
			return nil, err
		}
		originals[file] = src
		sort.Slice(fedits, func(i, j int) bool { return fedits[i].offset > fedits[j].offset })
		out := src
		for _, e := range fedits {
			if e.offset+e.length > len(out) || string(out[e.offset:e.offset+e.length]) != old {
				s.rollback(originals)
				return nil, fmt.Errorf("stale offset in %s at %d", file, e.offset)
			}
			out = append(append(append([]byte{}, out[:e.offset]...), e.text...), out[e.offset+e.length:]...)
		}
		if err := os.WriteFile(file, out, 0o644); err != nil {
			s.rollback(originals)
			return nil, err
		}
	}

	// Dirty: packages compiling an edited file, plus every reverse importer
	// of the target's package — a method rename can break interface
	// satisfaction in a package that never names the method.
	dirty := append(s.dirtyByFiles(editedFiles), s.affected(pkgPath)...)
	diags, n, err := s.retypecheck(dirty, baseline)
	if err != nil {
		s.rollback(originals)
		return nil, err
	}
	if len(diags) > 0 {
		s.rollback(originals)
		return nil, diagnosticRepairs(&Reject{Reason: "rename does not typecheck", Diagnostics: diags})
	}
	if rej := s.verifyResolution(pkgPath, newSym, declKey, expected); rej != nil {
		s.rollback(originals)
		// Splices already landed; re-typecheck the same set against the
		// restored files to put the snapshot back.
		s.retypecheck(dirty, baseline)
		return nil, rej
	}
	for file := range editedFiles {
		s.noteWrite(file)
	}
	files := make([]string, 0, len(byFile))
	for file := range byFile {
		files = append(files, file)
	}
	sort.Strings(files)
	res := addPreExisting(map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym, "new_name": to,
		"references": len(edits), "files": files,
		"load_ms": ms, "packages_rechecked": n,
		"generation": s.generation(pkgPath, newSym),
	}, baseline)
	// The viewed symbol is the NEW name; the old address no longer resolves.
	s.attachView(res, pkgPath, newSym)
	return res, nil
}

// renameOp is rename's composable form: same renameEdits core, applied to
// ctx.src through the decl-op ledger instead of straight to disk, with
// verifyResolution deferred to a postCheck run once at end-of-list.
type renameOp struct{}

func (renameOp) name() string { return "rename" }

func (renameOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg string `json:"pkg"`
		Sym string `json:"sym"`
		To  string `json:"to"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	edits, docEdit, _, declPos, newSym, baseline, rej := renameEdits(ctx.s, pkg, sym, a.To)
	if rej != nil {
		return rej
	}
	ctx.addBaseline(baseline)
	splices := edits
	if docEdit != nil {
		splices = append(append([]edit{}, edits...), *docEdit)
	}
	if rej := ctx.applyDeclEdits(splices); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	// The viewed symbol is the NEW name; the old address no longer resolves
	// once the patch commits.
	ctx.noteTouched(pkg, newSym, false)
	ctx.postChecks = append(ctx.postChecks, func() *Reject {
		// ctx.declEdits now holds the FULL per-file ledger — every decl op
		// in the patch has applied by the time postChecks run — so a
		// sibling rename touching this same file with a different
		// name-length delta is accounted for here, not just this rename's
		// own edits.
		expected, declKey := renameExpected(edits, declPos, ctx.declEdits)
		return ctx.s.verifyResolution(pkg, newSym, declKey, expected)
	})
	return nil
}

// verifyResolution checks that after revalidation the renamed object is
// declared where the rewrite put it and is referenced from exactly the
// rewritten positions. A missing position means a reference was captured by
// a shadowing declaration; a moved declaration means a collision.
func (s *Snapshot) verifyResolution(pkgPath, newSym, declKey string, expected map[string]bool) *Reject {
	_, obj, rej := s.findObject(pkgPath, newSym)
	if rej != nil {
		return &Reject{Reason: "renamed symbol not found after revalidation", Detail: pkgPath + "." + newSym}
	}
	p := s.fset.Position(obj.Pos())
	if declKey != "" && fmt.Sprintf("%s:%d", p.Filename, p.Offset) != declKey {
		return &Reject{Reason: "rename collides with existing symbol",
			Detail: pkgPath + "." + newSym + " resolves to declaration at " + p.String()}
	}
	got := map[string]bool{}
	for _, r := range s.references(obj) {
		got[fmt.Sprintf("%s:%d", r.pos.Filename, r.pos.Offset)] = true
	}
	for k := range expected {
		if !got[k] {
			return &Reject{Reason: "reference captured by another declaration",
				Detail: "rewritten reference at " + k + " no longer resolves to the renamed symbol"}
		}
	}
	return nil
}

// rollback restores file contents and keeps mtime bookkeeping consistent so
// the restore is not mistaken for an external edit.
func (s *Snapshot) rollback(originals map[string][]byte) {
	for file, src := range originals {
		os.WriteFile(file, src, 0o644)
		s.noteWrite(file)
	}
}

// spliceBody replaces the byte range of a function body and reformats.
func spliceBody(src []byte, lbrace, rbrace int, body string) ([]byte, error) {
	var buf strings.Builder
	buf.Write(src[:lbrace])
	buf.WriteString("{\n")
	buf.WriteString(body)
	buf.WriteString("\n}")
	buf.Write(src[rbrace+1:])
	return format.Source([]byte(buf.String()))
}
