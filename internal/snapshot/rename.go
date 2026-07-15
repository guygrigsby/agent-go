package snapshot

import (
	"fmt"
	"go/format"
	"go/token"
	"os"
	"sort"
	"strings"
)

// edit is one identifier rewrite: replace length bytes at offset in file.
type edit struct {
	file   string
	offset int
	length int
}

// Rename renames a symbol at every reference, then proves the result: the
// affected packages must typecheck and every rewritten position must
// resolve to the renamed object. Reference capture (the new name silently
// binding to a shadowing declaration of the same type) fails the second
// check even when the compiler is happy. Any failure rolls back every file.
func (s *Snapshot) Rename(pkgPath, sym, to string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !token.IsIdentifier(to) {
		rej := &Reject{Reason: "new name is not a valid identifier", Detail: to}
		if recv, name, ok := strings.Cut(to, "."); ok && token.IsIdentifier(recv) && token.IsIdentifier(name) {
			rej.Detail = to + ": pass only the new member name; the receiver stays"
			rej.DidYouMean = []string{name}
		}
		return nil, rej
	}
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	_, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	old := obj.Name()
	if old == to {
		return nil, &Reject{Reason: "symbol already has that name", Detail: to}
	}
	newSym := to
	if recv, _, isMethod := strings.Cut(sym, "."); isMethod {
		newSym = recv + "." + to
	}

	var edits []edit
	for _, r := range s.references(obj) {
		edits = append(edits, edit{r.pos.Filename, r.pos.Offset, len(old)})
	}
	for _, e := range edits {
		if !strings.HasPrefix(e.file, s.dir+string(os.PathSeparator)) {
			return nil, &Reject{Reason: "symbol is referenced outside the workspace", Detail: e.file}
		}
	}

	byFile := map[string][]edit{}
	editedFiles := map[string]bool{}
	for _, e := range edits {
		byFile[e.file] = append(byFile[e.file], e)
		editedFiles[e.file] = true
	}
	preDirty := append(s.dirtyByFiles(editedFiles), s.affected(pkgPath)...)
	if diags := errorsIn(preDirty); len(diags) > 0 {
		return nil, &Reject{Reason: "affected packages have pre-existing errors", Diagnostics: diags}
	}
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
			out = append(append(append([]byte{}, out[:e.offset]...), to...), out[e.offset+e.length:]...)
		}
		if err := os.WriteFile(file, out, 0o644); err != nil {
			s.rollback(originals)
			return nil, err
		}
	}

	// Expected post-rename positions: each original offset shifted by the
	// size delta of the edits before it in the same file.
	delta := len(to) - len(old)
	declPos := s.fset.Position(obj.Pos())
	expected := map[string]bool{}
	declKey := ""
	for _, e := range edits {
		shift := 0
		for _, f := range byFile[e.file] {
			if f.offset < e.offset {
				shift += delta
			}
		}
		key := fmt.Sprintf("%s:%d", e.file, e.offset+shift)
		expected[key] = true
		if e.file == declPos.Filename && e.offset == declPos.Offset {
			declKey = key
		}
	}

	// Dirty: packages compiling an edited file, plus every reverse importer
	// of the target's package — a method rename can break interface
	// satisfaction in a package that never names the method.
	dirty := append(s.dirtyByFiles(editedFiles), s.affected(pkgPath)...)
	diags, n, err := s.retypecheck(dirty)
	if err != nil {
		s.rollback(originals)
		return nil, err
	}
	if len(diags) > 0 {
		s.rollback(originals)
		return nil, &Reject{Reason: "rename does not typecheck", Diagnostics: diags}
	}
	if rej := s.verifyResolution(pkgPath, newSym, declKey, expected); rej != nil {
		s.rollback(originals)
		// Splices already landed; re-typecheck the same set against the
		// restored files to put the snapshot back.
		s.retypecheck(dirty)
		return nil, rej
	}
	for file := range editedFiles {
		s.noteWrite(file)
	}
	return map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym, "new_name": to,
		"references": len(edits), "files": len(byFile),
		"load_ms": ms, "packages_rechecked": n,
	}, nil
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
