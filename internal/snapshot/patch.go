package snapshot

import (
	"encoding/json"
	"sort"
	"strings"
)

// patchCtx carries the working state of one patch application: the
// pkg/sym defaults for ops that omit them, the in-memory working copy of
// file contents touched by the op list, and the handle table assigned by
// earlier ops for later ops to reference. Ops registered in opRegistry
// (Task 4 onward) apply against ctx.src only; the legacy ops dispatched in
// this task still write straight through their existing single-op methods.
type patchCtx struct {
	s        *Snapshot
	pkg, sym string            // defaults for ops that omit them
	src      map[string][]byte // working copy of file contents
	handles  map[string]string // "$1" -> handle assigned by op 1
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
// then dispatch. v1 supports exactly one legacy op per patch — multi-op
// composition and dry_run arrive with the handle-based ops in Tasks 4-8.
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
	for i, raw := range env.Ops {
		var n opName
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, &Reject{Reason: "malformed op", Detail: err.Error()}
		}
		if !legacyOps[n.Op] {
			if _, ok := opRegistry[n.Op]; !ok {
				return nil, &Reject{Reason: "unknown op", Detail: n.Op, DidYouMean: nearestOps(n.Op)}
			}
		}
		names[i] = n.Op
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
