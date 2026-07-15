package snapshot

import (
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
// earlier ops for later ops to reference. Ops registered in opRegistry
// apply against ctx.src only; the legacy ops dispatched separately still
// write straight through their existing single-op methods.
type patchCtx struct {
	s        *Snapshot
	pkg, sym string            // defaults for ops that omit them
	src      map[string][]byte // working copy of file contents
	handles  map[string]string // "$1" -> handle assigned by op 1

	file       string         // file holding the target pkg.sym declaration
	opIndex    int            // 1-based index of the op currently applying
	fileLastOp map[string]int // file -> op index that last edited it, for
	// attributing end-of-list typecheck diagnostics to the nearest op.
}

// patchOp is one composable operation in a patch's op list. Implementations
// arrive with Task 4; this task only defines the shape so the registry name
// and signature are fixed for what builds on it.
type patchOp interface {
	name() string
	apply(ctx *patchCtx, args json.RawMessage) *Reject
}

// opRegistry holds composable ops that apply natively to patchCtx.src.
// Empty in this task — rename, set_body, add_param, and upsert_decl are
// still dispatched directly to their existing methods below.
var opRegistry = map[string]func() patchOp{}

// legacyOps are the pre-Task-4 mutations, each still a single-op fast path
// delegating to its existing implementation. Task 8 folds them into
// opRegistry once they operate on ctx.src and support multi-op sequencing.
var legacyOps = map[string]bool{
	"rename": true, "set_body": true, "add_param": true, "upsert_decl": true,
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

// Patch applies a transaction envelope of edit operations: parse, check the
// generation the caller built the patch against, validate every op name,
// then dispatch. Ops registered in opRegistry go down the composable
// ctx.src pipeline (multi-op and dry_run supported); legacy ops still
// dispatch one at a time through their existing single-op methods; mixing
// the two in one patch is rejected as not yet composable.
func (s *Snapshot) Patch(raw []byte) (map[string]any, error) {
	var env patchEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &Reject{Reason: "malformed patch", Detail: err.Error()}
	}
	if rej := s.checkGeneration(env.Pkg, env.Sym, env.Generation); rej != nil {
		return nil, rej
	}
	if len(env.Ops) == 0 {
		return nil, &Reject{Reason: "patch has no ops"}
	}
	names := make([]string, len(env.Ops))
	var hasLegacy, hasComposable bool
	for i, raw := range env.Ops {
		var n opName
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, &Reject{Reason: "malformed op", Detail: err.Error()}
		}
		switch {
		case legacyOps[n.Op]:
			hasLegacy = true
		case opRegistry[n.Op] != nil:
			hasComposable = true
		default:
			return nil, &Reject{Reason: "unknown op", Detail: n.Op, DidYouMean: nearestOps(n.Op)}
		}
		names[i] = n.Op
	}
	if hasLegacy && hasComposable {
		return nil, &Reject{Reason: "not yet composable", Detail: "patch mixes legacy and composable ops"}
	}
	if hasComposable {
		return s.patchComposable(env, names)
	}
	if len(env.Ops) > 1 {
		return nil, &Reject{Reason: "not yet composable", Detail: "patch has more than one op"}
	}
	if env.DryRun {
		return nil, &Reject{Reason: "dry_run requires composable ops"}
	}
	res, err := s.dispatchLegacy(names[0], env.Pkg, env.Sym, env.Ops[0])
	if err != nil {
		return nil, err
	}
	res["ops_applied"] = 1
	return res, nil
}

// patchComposable runs the ctx.src pipeline: every name in names is
// registered in opRegistry (Patch already ruled out legacy ops and unknown
// names). Ops mutate ctx.src only; nothing touches disk until the whole
// list has applied without a Reject, so a failure partway leaves the
// workspace untouched — no rollback bookkeeping is needed for that case.
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
	nt, rej := s.nodeTableFor(env.Pkg, env.Sym)
	if rej != nil {
		return nil, rej
	}
	src, err := os.ReadFile(nt.file)
	if err != nil {
		return nil, err
	}
	editedFiles := map[string]bool{nt.file: true}
	preDirty := append(s.dirtyByFiles(editedFiles), s.affected(env.Pkg)...)
	if diags := errorsIn(preDirty); len(diags) > 0 {
		return nil, &Reject{Reason: "affected packages have pre-existing errors", Diagnostics: diags}
	}

	ctx := &patchCtx{
		s: s, pkg: env.Pkg, sym: env.Sym,
		src:        map[string][]byte{nt.file: append([]byte(nil), src...)},
		handles:    map[string]string{},
		file:       nt.file,
		fileLastOp: map[string]int{},
	}
	for i, raw := range env.Ops {
		ctx.opIndex = i + 1
		resolved, rej := ctx.resolveArgRefs(raw)
		if rej != nil {
			return nil, rej
		}
		op := opRegistry[names[i]]()
		if rej := op.apply(ctx, resolved); rej != nil {
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
			return nil, &Reject{Reason: "patch result does not format", Detail: file + ": " + ferr.Error()}
		}
		formatted[file] = out
		touched = append(touched, file)
	}
	sort.Strings(touched)

	originals := map[string][]byte{nt.file: src}
	for _, file := range touched {
		if err := os.WriteFile(file, formatted[file], 0o644); err != nil {
			s.rollback(originals)
			return nil, err
		}
	}

	dirty := append(s.dirtyByFiles(editedFiles), s.affected(env.Pkg)...)
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
	diags, n, err := s.retypecheck(dirty)
	if err != nil {
		s.rollback(originals)
		return nil, err
	}
	if env.DryRun {
		s.rollback(originals)
		s.retypecheck(dirty)
		for pkg, g := range savedGens {
			s.gens[pkg] = g
		}
		if len(diags) > 0 {
			return nil, &Reject{Reason: "patch does not typecheck", Diagnostics: annotateDiagnostics(diags, ctx.fileLastOp)}
		}
		return map[string]any{"status": "ok", "dry_run": true, "would": "accepted"}, nil
	}
	if len(diags) > 0 {
		s.rollback(originals)
		return nil, &Reject{Reason: "patch does not typecheck", Diagnostics: annotateDiagnostics(diags, ctx.fileLastOp)}
	}
	for _, file := range touched {
		s.noteWrite(file)
	}
	return map[string]any{
		"status": "accepted", "symbol": env.Pkg + "." + env.Sym,
		"ops_applied": len(env.Ops), "files": touched,
		"load_ms": ms, "packages_rechecked": n,
		"generation": s.generation(env.Pkg, env.Sym),
	}, nil
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

// dispatchLegacy translates one op's raw JSON into the existing single-op
// method's plain-string arguments, defaulting pkg/sym from the envelope but
// letting the op override either.
func (s *Snapshot) dispatchLegacy(op, pkg, sym string, raw json.RawMessage) (map[string]any, error) {
	switch op {
	case "rename":
		var args struct {
			Pkg string `json:"pkg"`
			Sym string `json:"sym"`
			To  string `json:"to"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, &Reject{Reason: "malformed op args", Detail: err.Error()}
		}
		return s.Rename(orDefault(args.Pkg, pkg), orDefault(args.Sym, sym), args.To)
	case "set_body":
		var args struct {
			Pkg  string `json:"pkg"`
			Sym  string `json:"sym"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, &Reject{Reason: "malformed op args", Detail: err.Error()}
		}
		return s.SetBody(orDefault(args.Pkg, pkg), orDefault(args.Sym, sym), args.Body)
	case "add_param":
		var args struct {
			Pkg     string `json:"pkg"`
			Sym     string `json:"sym"`
			Name    string `json:"name"`
			Type    string `json:"type"`
			Default string `json:"default"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, &Reject{Reason: "malformed op args", Detail: err.Error()}
		}
		return s.AddParam(orDefault(args.Pkg, pkg), orDefault(args.Sym, sym), args.Name, args.Type, args.Default)
	case "upsert_decl":
		var args struct {
			Pkg  string `json:"pkg"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, &Reject{Reason: "malformed op args", Detail: err.Error()}
		}
		return s.UpsertDecl(orDefault(args.Pkg, pkg), args.Text)
	}
	// Unreachable: Patch validates op names before calling dispatchLegacy.
	return nil, &Reject{Reason: "unknown op", Detail: op, DidYouMean: nearestOps(op)}
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
	catalog := make([]string, 0, len(legacyOps)+len(opRegistry))
	for n := range legacyOps {
		catalog = append(catalog, n)
	}
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
