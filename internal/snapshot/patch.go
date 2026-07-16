package snapshot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/imports"
)

// patchCtx carries the working state of one patch application: the
// pkg/sym defaults for ops that omit them, the in-memory working copy of
// file contents touched by the op list, and the handle table assigned by
// earlier ops for later ops to reference. Every op registered in opRegistry
// applies against ctx.src only; nothing reaches disk until the whole list
// has applied without a Reject.
type patchCtx struct {
	s        *Snapshot
	pkg, sym string            // defaults for ops that omit them
	src      map[string][]byte // working copy of file contents
	handles  map[string]string // "$1" -> handle assigned by op 1

	file       string         // file holding the target pkg.sym declaration (statement ops only)
	opIndex    int            // 1-based index of the op currently applying
	fileLastOp map[string]int // file -> op index that last edited it, for
	// attributing end-of-list typecheck diagnostics to the nearest op.

	// createdFiles lists every file an op created outright this patch
	// (add_test's and upsert_decl's new-file paths) rather than editing
	// through ctx.src, and deletedFiles maps every file an op deleted
	// (delete_file) to its original bytes. Both bypass the ctx.src/originals
	// rollback entirely, so patchComposable's own cleanupFileOps is the only
	// thing that undoes them on any path that doesn't end in a commit.
	createdFiles []string
	deletedFiles map[string][]byte

	// declOrig/declEdits back the decl ops' (rename, set_body, add_param,
	// upsert_decl, delete_decl, set_doc, add_field, remove_field) shared
	// splice ledger: each op computes its edits' byte offsets against the
	// live, pre-patch snapshot (the same positions its legacy single-op
	// method has always used), then applyDeclEdits folds them into a
	// per-file ledger replayed against that file's pristine (first-touch)
	// bytes. Replaying the full ledger on every new edit, rather than
	// splicing incrementally into ctx.src, means a second decl op touching
	// the same file composes correctly regardless of application order —
	// exactly what atomic multi-rename needs.
	declOrig  map[string][]byte
	declEdits map[string][]edit

	// affectedPkgs accumulates every decl op's own target package, unioned
	// into the dirty set alongside dirtyByFiles(touched) and
	// affected(env.Pkg): a rename or add_param elsewhere in the workspace
	// can break a reverse importer that no edited file itself touches.
	affectedPkgs map[string]bool

	// touched records, in op order and deduped by post-op address, each
	// declaration this patch's ops reshaped: statement ops all address the
	// envelope's own function (noted once in patchComposable), decl ops note
	// their own effective pkg/sym (rename under its NEW name — the old
	// address no longer resolves), delete_decl with gone set. The accept
	// response embeds a fresh view only when this holds exactly one entry;
	// see the ceiling comment on patchComposable's accept path.
	touched []touchedDecl

	// baseline accumulates every op's preflight capture of pre-existing
	// diagnostics (pos|msg keys) across the whole op list. Pre-existing rot
	// in the dirty set no longer refuses a patch; patchComposable filters
	// the end-of-list retypecheck against this set and rejects only when
	// NEW diagnostics appear.
	baseline map[string]bool

	// postChecks run once, after the shared end-of-list retypecheck
	// succeeds, against the now-spliced live snapshot: rename's
	// verifyResolution (reference-capture detection) and add_param's
	// "parameter actually landed" sanity check both need the POST-retypecheck
	// state, and a multi-rename patch must prove itself as one unit against
	// the FINAL state rather than after each individual rename.
	postChecks []func() *Reject
}

// touchedDecl is one declaration a patch op reshaped, addressed post-op
// (rename records the new name). gone marks a declaration that no longer
// exists after the patch (delete_decl), so no view can render for it.
type touchedDecl struct {
	pkg, sym string
	gone     bool
}

// noteTouched records a declaration an op reshaped, deduped by address so
// several ops on the same declaration (set_body then set_doc) count as one
// touched declaration. Two ops addressing one declaration under different
// names (set_body on the old name, then rename) still count as two — a
// conservative miss that only costs the caller the view round-trip the
// embedded view would have saved.
func (ctx *patchCtx) noteTouched(pkg, sym string, gone bool) {
	for i, t := range ctx.touched {
		if t.pkg == pkg && t.sym == sym {
			ctx.touched[i].gone = ctx.touched[i].gone || gone
			return
		}
	}
	ctx.touched = append(ctx.touched, touchedDecl{pkg, sym, gone})
}

// declOps are ops whose args name their own pkg/sym (or, for upsert_decl,
// derive it from the declaration text) rather than address positions
// relative to the envelope's own target declaration. patchComposable does
// not require the envelope pkg/sym to resolve to a function before running
// a patch made up entirely of these — only the statement ops (ops_stmt.go)
// need that fixed function context.
var declOps = map[string]bool{
	"rename": true, "set_body": true, "add_param": true, "upsert_decl": true,
	"delete_decl": true, "set_doc": true, "add_field": true, "remove_field": true,
}

// opRegistry holds every composable op, keyed by wire name.
var opRegistry = map[string]func() patchOp{}

// patchOp is one composable operation in a patch's op list.
type patchOp interface {
	name() string
	apply(ctx *patchCtx, args json.RawMessage) *Reject
}

// patchEnvelope is the wire shape of a whole patch.
type patchEnvelope struct {
	Pkg        string            `json:"pkg"`
	Sym        string            `json:"sym"`
	Generation int64             `json:"generation"`
	DryRun     bool              `json:"dry_run"`
	Ops        []json.RawMessage `json:"ops"`
}

// opName is the discriminator shared by every op object; each op's own
// fields are unmarshaled a second time from the same raw message.
type opName struct {
	Op string `json:"op"`
}

// decodeOpArgs unmarshals an op's raw arguments strictly: an argument the
// op does not define — usually another op's vocabulary, like set_body sent
// with at/where/exprs — rejects at the shape layer instead of being
// silently dropped and surfacing later as a splice or typecheck failure.
// The "op" discriminator itself is scrubbed first; it is not an argument.
func decodeOpArgs(raw json.RawMessage, dst any) *Reject {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	delete(m, "op")
	clean, _ := json.Marshal(m)
	dec := json.NewDecoder(bytes.NewReader(clean))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &Reject{Reason: "op argument not in this op's schema",
			Detail: err.Error() + "; the catalog shows each op's arguments"}
	}
	return nil
}

// Patch applies a transaction envelope of edit operations: parse, check the
// generation the caller built the patch against, validate every op name,
// then run the whole list through the composable ctx.src pipeline. The
// envelope's own generation check applies to the envelope pkg only — a
// per-op pkg/sym override (rename/set_body/add_param/upsert_decl/
// delete_decl/set_doc/add_field/remove_field can each name a different
// declaration than the envelope) is not itself generation-checked in v1;
// staleness there still surfaces as a typecheck reject, just without the
// sharper "stale generation: re-view" message.
func (s *Snapshot) Patch(raw []byte) (map[string]any, error) {
	var env patchEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &Reject{Reason: "malformed patch", Detail: err.Error()}
	}
	if rej := s.checkGeneration(env.Pkg, env.Sym, env.Generation); rej != nil {
		s.mu.Lock()
		if env.Sym != "" && s.viewable(env.Pkg, env.Sym) {
			rej.PossibleRepairs = append(rej.PossibleRepairs,
				Repair{Why: "refreshes generation and handles", Call: viewCall(env.Pkg, env.Sym)})
		}
		s.mu.Unlock()
		return nil, rej
	}
	if len(env.Ops) == 0 {
		return nil, &Reject{Reason: "patch has no ops"}
	}
	names := make([]string, len(env.Ops))
	for i, raw := range env.Ops {
		var n opName
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, &Reject{Reason: "malformed op", Detail: err.Error()}
		}
		if opRegistry[n.Op] == nil {
			cands := nearestOps(n.Op)
			rej := &Reject{Reason: "unknown op", Detail: n.Op, DidYouMean: cands}
			// A full-catalog fallback is not a near miss; substituting
			// arbitrary ops into the patch would invent repairs.
			if len(cands) < len(opRegistry) {
				for _, c := range cands {
					call, ok := patchCall(env, i, "op", c)
					if !ok {
						break
					}
					rej.PossibleRepairs = append(rej.PossibleRepairs,
						Repair{Why: "op " + c + " exists", Call: call})
					if len(rej.PossibleRepairs) == maxRepairs {
						break
					}
				}
			}
			return nil, rej
		}
		names[i] = n.Op
	}
	return s.patchComposable(env, names)
}

// patchComposable runs the ctx.src pipeline: every name in names is
// registered in opRegistry (Patch already ruled out unknown names). Ops
// mutate ctx.src only; nothing touches disk until the whole list has
// applied without a Reject, so a failure partway leaves the workspace
// untouched — no rollback bookkeeping is needed for that case.
//
// The envelope's own pkg/sym resolve to a fixed function context (nt.file)
// only when the op list contains a statement op (ops_stmt.go): those
// address positions by handle within that one function. A patch made up
// entirely of decl ops skips that resolution — each decl op resolves its
// own pkg/sym (defaulting from the envelope) independently, and the
// envelope's own sym need not even be a function (or be given at all, when
// every op supplies its own).
//
// dry_run runs the identical op-apply, format, write, and retypecheck steps
// a commit would, so its response carries the same accept/reject outcome a
// real commit would produce — including diagnostics on rejection. It then
// unconditionally restores the original bytes and re-typechecks the dirty
// set against that restored content — the same resync rename.go's
// verifyResolution failure path performs — before replying, so a preview
// never leaves disk changed. That resync's own retypecheck would otherwise
// bump the dirty packages' generation counters on a clean recheck exactly as
// a real splice does; dry_run snapshots and restores those counters around
// the resync so previewing a patch never invalidates a caller's held
// generation handle.
func (s *Snapshot) patchComposable(env patchEnvelope, names []string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}

	ctx := &patchCtx{
		s: s, pkg: env.Pkg, sym: env.Sym,
		src:        map[string][]byte{},
		handles:    map[string]string{},
		fileLastOp: map[string]int{},
	}

	needsFunc := false
	for _, n := range names {
		if !declOps[n] {
			needsFunc = true
			break
		}
	}
	if needsFunc {
		nt, rej := s.nodeTableFor(env.Pkg, env.Sym)
		if rej != nil {
			s.viewRepairs(rej, env.Pkg, env.Sym)
			return nil, rej
		}
		src, err := os.ReadFile(nt.file)
		if err != nil {
			return nil, err
		}
		preDirty := append(s.dirtyByFiles(map[string]bool{nt.file: true}), s.affected(env.Pkg)...)
		ctx.addBaseline(errorSet(errorsIn(preDirty)))
		ctx.file = nt.file
		ctx.src[nt.file] = append([]byte(nil), src...)
		// Every statement op addresses handles inside this one function, so
		// the whole group is a single touched declaration.
		ctx.noteTouched(env.Pkg, env.Sym, false)
	}

	for i, raw := range env.Ops {
		ctx.opIndex = i + 1
		resolved, rej := ctx.resolveArgRefs(raw)
		if rej != nil {
			ctx.cleanupFileOps()
			return nil, rej
		}
		op := opRegistry[names[i]]()
		if rej := op.apply(ctx, resolved); rej != nil {
			// The op that just ran (this one, or an earlier one in the same
			// list) may have created a file outright (add_test's new-file
			// path). No disk write from ctx.src has happened yet at this
			// point in the pipeline, so there is nothing else to roll back —
			// only a created file needs cleaning up.
			ctx.cleanupFileOps()
			s.patchOpRepairs(rej, env, i)
			return nil, rej
		}
	}

	// imports.Process (gofmt plus import fixup), not plain format.Source: any
	// op list can include wrap_error, whose "fmt.Errorf(...)" splice needs a
	// "fmt" import added if the file doesn't already have one, and it's a
	// strict superset of format.Source's formatting for op lists that don't
	// (matches UpsertDecl's existing use of imports.Process for the same
	// reason).
	touched := make([]string, 0, len(ctx.src))
	formatted := map[string][]byte{}
	for file, b := range ctx.src {
		out, ferr := imports.Process(file, b, nil)
		if ferr != nil {
			ctx.cleanupFileOps()
			return nil, &Reject{Reason: "patch result does not format", Detail: file + ": " + ferr.Error()}
		}
		formatted[file] = out
		touched = append(touched, file)
	}
	sort.Strings(touched)

	// Nothing has been written yet, so disk still holds every touched
	// file's pre-patch bytes — read them fresh here rather than tracking a
	// second original-bytes map through the op loop.
	originals := map[string][]byte{}
	for _, file := range touched {
		b, err := os.ReadFile(file)
		if err != nil {
			ctx.cleanupFileOps()
			return nil, err
		}
		originals[file] = b
	}
	for _, file := range touched {
		if err := os.WriteFile(file, formatted[file], 0o644); err != nil {
			s.rollback(originals)
			ctx.cleanupFileOps()
			return nil, err
		}
	}

	editedFiles := map[string]bool{}
	for _, f := range touched {
		editedFiles[f] = true
	}
	dirty := append(s.dirtyByFiles(editedFiles), s.affected(env.Pkg)...)
	for pkg := range ctx.affectedPkgs {
		dirty = append(dirty, s.affected(pkg)...)
	}
	// The final dirty set is a superset of every op's own preflight scope:
	// affected(env.Pkg) joins unconditionally, even when no op named env.Pkg
	// (a decl-only patch never runs the needsFunc preflight above). Capture
	// its pre-existing diagnostics too — p.Errors still reflects the
	// pre-splice snapshot here; only disk has changed — so rot in a reverse
	// importer no op touched doesn't read as new below.
	ctx.addBaseline(errorSet(errorsIn(dirty)))

	// dry_run previews the same retypecheck a commit would run, so it must
	// see the same accept/reject outcome. But the resync retypecheck below
	// (which puts the snapshot back once the proposed edit's bytes are
	// restored) still bumps generations on a clean recheck, exactly like a
	// real splice would — so for dry_run, snapshot every dirty package's
	// generation counter here and restore it after resync, leaving a
	// dry_run's held generation handles valid.
	var savedGens map[string]int64
	if env.DryRun {
		savedGens = map[string]int64{}
		for _, p := range dirty {
			if p != nil {
				savedGens[p.PkgPath] = s.gens[p.PkgPath]
			}
		}
	}
	diags, n, err := s.retypecheck(dirty, ctx.baseline)
	if err != nil {
		s.rollback(originals)
		ctx.cleanupFileOps()
		return nil, err
	}
	// postChecks (rename's verifyResolution, add_param's post-edit sanity
	// check) need the just-retypechecked live snapshot; run them once here,
	// against the FINAL state of every decl op in the patch, not per-op.
	var opRej *Reject
	if len(diags) == 0 {
		for _, chk := range ctx.postChecks {
			if r := chk(); r != nil {
				opRej = r
				break
			}
		}
	}
	if env.DryRun {
		s.rollback(originals)
		s.retypecheck(dirty, ctx.baseline)
		// cleanupFileOps must run before the gens restore below: its
		// forced full reload (when add_test created a file this patch) bumps
		// every workspace package's generation counter same as any reload,
		// which would clobber a restore done first. dry_run never leaves
		// artifacts, on the accept preview or the reject preview alike, so
		// this runs unconditionally rather than only in the reject arms.
		ctx.cleanupFileOps()
		for pkg, g := range savedGens {
			s.gens[pkg] = g
		}
		if len(diags) > 0 {
			return nil, diagnosticRepairs(&Reject{Reason: "patch does not typecheck", Diagnostics: annotateDiagnostics(diags, ctx.fileLastOp)})
		}
		if opRej != nil {
			return nil, opRej
		}
		return addPreExisting(map[string]any{"status": "ok", "dry_run": true, "would": "accepted"}, ctx.baseline), nil
	}
	if len(diags) > 0 {
		s.rollback(originals)
		ctx.cleanupFileOps()
		return nil, diagnosticRepairs(&Reject{Reason: "patch does not typecheck", Diagnostics: annotateDiagnostics(diags, ctx.fileLastOp)})
	}
	if opRej != nil {
		s.rollback(originals)
		// Splices already landed; re-typecheck the same set against the
		// restored files to put the snapshot back (mirrors rename.go's own
		// verifyResolution failure path).
		//
		// This resync restores files and the snapshot's types/syntax, but not
		// the generation counters retypecheck just bumped for the dirty set
		// (bumpGenerations runs on any clean recheck, including this one).
		// Accepted, same direction as the rest of the generation contract: a
		// stale view after a failed patch is safe (it just forces a re-view),
		// where restoring counters to un-bump them is not worth tracking.
		s.retypecheck(dirty, ctx.baseline)
		// Runs after the resync above for the same reason as the dry_run
		// arm: cleanupFileOps' own reload (when this patch's op list
		// created a file) would otherwise be redone by, or race with, the
		// resync's incremental splice.
		ctx.cleanupFileOps()
		return nil, opRej
	}
	for _, file := range touched {
		s.noteWrite(file)
	}
	res := addPreExisting(map[string]any{
		"status": "accepted", "symbol": env.Pkg + "." + env.Sym,
		"ops_applied": len(env.Ops), "files": touched,
		"load_ms": ms, "packages_rechecked": n,
		"generation": s.generation(env.Pkg, env.Sym),
	}, ctx.baseline)
	// Ceiling: the accept response embeds a fresh view only when exactly ONE
	// declaration was touched (however many ops touched it). A multi-decl
	// patch — several decl ops on different symbols, an atomic multi-rename —
	// has no single "the touched declaration", and embedding every view would
	// bloat each multi-op accept, so the key is omitted and views_omitted
	// says why; the caller views the declarations it still needs explicitly.
	switch {
	case len(ctx.touched) == 1 && ctx.touched[0].gone:
		res["views_omitted"] = "declaration no longer exists at its old address"
	case len(ctx.touched) == 1:
		s.attachView(res, ctx.touched[0].pkg, ctx.touched[0].sym)
	default:
		res["views_omitted"] = fmt.Sprintf(
			"%d declarations touched; view each explicitly", len(ctx.touched))
	}
	return res, nil
}

// applyDeclEdits folds edits into the per-file decl-op ledger and replays
// each touched file's full edit history against its pristine (first-touch)
// bytes: the first decl op to touch a file captures ctx.src's already-loaded
// bytes if present (so a decl op sharing a file with the envelope's
// statement-op target sees the same starting bytes those ops work from),
// else reads disk fresh. Every edit's offset must already be valid against
// that pristine baseline — each decl op computes its own edits from the
// live, pre-patch snapshot, so this always holds regardless of how many
// other decl ops have already touched the same file.
func (ctx *patchCtx) applyDeclEdits(edits []edit) *Reject {
	if len(edits) == 0 {
		return nil
	}
	if ctx.declOrig == nil {
		ctx.declOrig = map[string][]byte{}
		ctx.declEdits = map[string][]edit{}
	}
	byFile := map[string][]edit{}
	var files []string
	for _, e := range edits {
		if _, ok := byFile[e.file]; !ok {
			files = append(files, e.file)
		}
		byFile[e.file] = append(byFile[e.file], e)
	}
	for _, file := range files {
		// Ceiling: a decl op and a statement op may not both edit one file in
		// a single patch. Statement ops (ops_stmt.go) treat ctx.src[file] as
		// the source of truth, editing it incrementally and re-parsing it
		// fresh to reassign handles; decl ops treat their declOrig+declEdits
		// ledger as the truth and OVERWRITE ctx.src[file] with a full replay
		// on every applyDeclEdits. The two models only reconcile when they
		// touch different files. Same-file composition is sound only in one
		// fragile ordering (a lone decl edit, then a stmt op, with no further
		// decl op on that file) and silently corrupts otherwise — a stmt-op
		// insertion is either clobbered by a later ledger replay or shifts the
		// offsets a decl edit was computed against. ctx.file is set (in
		// patchComposable) exactly when the patch contains a statement op, and
		// stmt ops only ever edit ctx.file, so rejecting a decl edit that
		// lands on ctx.file forecloses the whole unsound class, order- and
		// count-independent. Split such work across separate patches.
		if ctx.file != "" && file == ctx.file {
			return &Reject{Reason: "cannot mix a decl op and a statement op on the same file",
				Detail: file + ": a statement op addresses handles by position that a same-file " +
					"decl op's reparse invalidates; run the decl op and the statement op as separate patches"}
		}
		if _, ok := ctx.declOrig[file]; !ok {
			if b, ok := ctx.src[file]; ok {
				ctx.declOrig[file] = append([]byte(nil), b...)
			} else {
				b, err := os.ReadFile(file)
				if err != nil {
					return &Reject{Reason: "file not found", Detail: file}
				}
				ctx.declOrig[file] = b
			}
		}
		ctx.declEdits[file] = append(ctx.declEdits[file], byFile[file]...)
		all := append([]edit(nil), ctx.declEdits[file]...)
		sort.Slice(all, func(i, j int) bool { return all[i].offset > all[j].offset })
		out := append([]byte(nil), ctx.declOrig[file]...)
		for _, e := range all {
			if e.offset < 0 || e.offset+e.length > len(out) {
				return &Reject{Reason: "stale offset", Detail: fmt.Sprintf("%s at %d", file, e.offset)}
			}
			out = append(append(append([]byte{}, out[:e.offset]...), e.text...), out[e.offset+e.length:]...)
		}
		ctx.src[file] = out
		ctx.fileLastOp[file] = ctx.opIndex
	}
	return nil
}

// addAffected records pkg as a decl op's own target package, unioned into
// patchComposable's post-loop dirty set via affected(pkg).
func (ctx *patchCtx) addAffected(pkg string) {
	if ctx.affectedPkgs == nil {
		ctx.affectedPkgs = map[string]bool{}
	}
	ctx.affectedPkgs[pkg] = true
}

// addBaseline folds one op's preflight capture of pre-existing diagnostics
// into the patch-wide baseline patchComposable filters against at
// end-of-list.
func (ctx *patchCtx) addBaseline(set map[string]bool) {
	if len(set) == 0 {
		return
	}
	if ctx.baseline == nil {
		ctx.baseline = map[string]bool{}
	}
	for k := range set {
		ctx.baseline[k] = true
	}
}

// cleanupFileOps undoes every file-set change this patch's ops made
// outright — deletes ctx.createdFiles, restores ctx.deletedFiles — and
// forces a full reload, so the snapshot forgets changes that must not
// survive: any rejection after the op that made them (whether that op's
// own failure or a later op's), and unconditionally on dry_run (which never
// leaves artifacts on disk regardless of accept/reject). A plain
// s.retypecheck resync is not enough here — that splices dirty *packages*
// in place, it never re-globs a package's CompiledGoFiles, so a created or
// deleted file would keep its pre-cleanup listing. Only a full s.load()
// re-globs. No-op when the file set never changed, so the common op list
// pays nothing extra.
func (ctx *patchCtx) cleanupFileOps() {
	if len(ctx.createdFiles) == 0 && len(ctx.deletedFiles) == 0 {
		return
	}
	for _, f := range ctx.createdFiles {
		os.Remove(f)
	}
	for f, b := range ctx.deletedFiles {
		os.WriteFile(f, b, 0o644)
	}
	ctx.s.loaded = false
	ctx.s.load()
}

// dollarRefPattern matches a bare "$N" intra-patch handle reference: N is
// the 1-based index of the op whose bound result it names.
var dollarRefPattern = regexp.MustCompile(`^\$(\d+)$`)

// resolveArgRefs rewrites any "$N" value under the at/from/to keys of one
// op's raw JSON args into the literal handle op N bound, before the op's own
// struct unmarshal ever sees it — op implementations never handle $ syntax
// themselves. N must name an op strictly earlier than the one currently
// applying and must have actually bound a handle (via bindResult); anything
// else — a typo, a forward reference to an op that hasn't run yet, self-
// reference, or an op that never binds one — rejects as "unknown $ref"
// naming the current op's index. from/to are unused by any op Tasks 5/6
// define; resolving them here anyway keeps this the single place future ops
// need to support $ addressing.
func (ctx *patchCtx) resolveArgRefs(raw json.RawMessage) (json.RawMessage, *Reject) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return raw, nil // malformed op args surfaces from the op's own unmarshal
	}
	changed := false
	for _, key := range []string{"at", "from", "to"} {
		rm, ok := args[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(rm, &s); err != nil {
			continue // not a plain string value; nothing to resolve
		}
		m := dollarRefPattern.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		h, ok := ctx.handles[s]
		if n >= ctx.opIndex || !ok {
			return nil, &Reject{Reason: "unknown $ref", Detail: fmt.Sprintf("op %d: %s", ctx.opIndex, s)}
		}
		b, err := json.Marshal(h)
		if err != nil {
			return nil, &Reject{Reason: "malformed op args", Detail: err.Error()}
		}
		args[key] = b
		changed = true
	}
	if !changed {
		return raw, nil
	}
	out, err := json.Marshal(args)
	if err != nil {
		return nil, &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	return out, nil
}

// annotateDiagnostics prefixes each diagnostic's message with the nearest
// op that touched its file, so a caller composing several ops in one patch
// can tell which one introduced a type error. Attribution is file-level and
// picks the most recently applied op that touched the file — nearest-op,
// not exact: it does not track how a later op's insertion shifts the line
// ranges an earlier op touched.
func annotateDiagnostics(diags []Diagnostic, fileLastOp map[string]int) []Diagnostic {
	out := make([]Diagnostic, len(diags))
	for i, d := range diags {
		file := strings.SplitN(d.Pos, ":", 2)[0]
		out[i] = d
		if op, ok := fileLastOp[file]; ok {
			out[i].Msg = fmt.Sprintf("op %d: %s", op, d.Msg)
		}
	}
	return out
}

// orDefault returns v if the op supplied it, else the envelope default.
func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// nearestOps suggests valid op names for an unrecognized one: the same
// substring match as suggestSymbols, falling back to the whole catalog when
// nothing is close. There are only a handful of ops, so listing them all is
// signal rather than the noise a full symbol dump would be.
func nearestOps(name string) []string {
	catalog := make([]string, 0, len(opRegistry))
	for n := range opRegistry {
		catalog = append(catalog, n)
	}
	sort.Strings(catalog)
	lower := strings.ToLower(name)
	var hits []string
	for _, n := range catalog {
		ln := strings.ToLower(n)
		if ln == lower || strings.Contains(ln, lower) || strings.Contains(lower, ln) {
			hits = append(hits, n)
		}
	}
	if len(hits) == 0 {
		return catalog
	}
	if len(hits) > 3 {
		hits = hits[:3]
	}
	return hits
}
