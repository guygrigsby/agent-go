package snapshot

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"strings"
)

// sigText is a parsed signature argument: the op's "(params) results"
// text broken into named params and the raw results text.
type sigText struct {
	params  []sigParam
	results string // raw text, "" for none; parenthesized when multiple
	text    string // the original, normalized
}

type sigParam struct {
	name     string
	typ      string
	variadic bool
}

// parseSignatureText parses the op's signature argument by wrapping it in
// a throwaway func declaration. Grouped params (a, b int) expand to one
// sigParam each.
func parseSignatureText(text string) (*sigText, *Reject) {
	src := "package p\nfunc _x" + strings.TrimSpace(text) + "{}"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sig.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, &Reject{Reason: "signature does not parse",
			Detail: text + ": " + err.Error()}
	}
	fd, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok {
		return nil, &Reject{Reason: "signature does not parse", Detail: text}
	}
	sig := sigFromFuncType(fset, fd.Type)
	sig.text = strings.TrimSpace(text)
	return sig, nil
}

// sigFromFuncType builds the sigText view of any func type, parsed or from
// the live snapshot; parseSignatureText and setSignatureEdits share it.
func sigFromFuncType(fset *token.FileSet, ft *ast.FuncType) *sigText {
	sig := &sigText{}
	for _, field := range ft.Params.List {
		typ := renderNode(fset, field.Type)
		_, variadic := field.Type.(*ast.Ellipsis)
		if len(field.Names) == 0 {
			sig.params = append(sig.params, sigParam{name: "_", typ: typ, variadic: variadic})
			continue
		}
		for _, id := range field.Names {
			sig.params = append(sig.params, sigParam{name: id.Name, typ: typ, variadic: variadic})
		}
	}
	if ft.Results != nil {
		var parts []string
		for _, field := range ft.Results.List {
			t := renderNode(fset, field.Type)
			n := max(len(field.Names), 1)
			for range n {
				parts = append(parts, t)
			}
		}
		if len(parts) == 1 && len(ft.Results.List[0].Names) == 0 {
			sig.results = parts[0]
		} else {
			sig.results = "(" + strings.Join(parts, ", ") + ")"
		}
	}
	return sig
}

func renderNode(fset *token.FileSet, n ast.Node) string {
	var b strings.Builder
	printer.Fprint(&b, fset, n)
	return b.String()
}

// argPlan says how to build a call site's new argument list: for each new
// parameter, either carry the old argument at oldIndex or splice in the
// default text. A carried variadic param carries the whole argument tail.
type argPlan struct {
	slots []argSlot
}

type argSlot struct {
	oldIndex int    // >= 0: carry old argument(s) from this position
	text     string // oldIndex < 0: literal default expression
	variadic bool
}

// planArgs matches new params to old by name and produces the call-site
// rewrite plan. New params need a default only when call sites exist.
func planArgs(old, new_ *sigText, defaults map[string]string, callSites int) (*argPlan, *Reject) {
	oldIndex := map[string]int{}
	for i, p := range old.params {
		if p.name != "_" {
			oldIndex[p.name] = i
		}
	}
	plan := &argPlan{}
	for i, p := range new_.params {
		if p.variadic && i != len(new_.params)-1 {
			return nil, &Reject{Reason: "variadic parameter must be last", Detail: p.name}
		}
		if j, carried := oldIndex[p.name]; carried {
			plan.slots = append(plan.slots, argSlot{oldIndex: j, variadic: p.variadic})
			continue
		}
		def, ok := defaults[p.name]
		if !ok {
			if callSites > 0 {
				return nil, &Reject{Reason: "new parameter needs a default",
					Detail: fmt.Sprintf("%s %s: %d call sites need an argument; add it to defaults", p.name, p.typ, callSites)}
			}
			def = ""
		}
		plan.slots = append(plan.slots, argSlot{oldIndex: -1, text: def, variadic: p.variadic})
	}
	return plan, nil
}

// setSignatureEdits computes the full rewrite: the declaration's signature
// text and every call site's argument list per the plan. Value uses of the
// function are deliberately not rewritten — they must be repaired by
// sibling ops in the same patch, or the end-of-list typecheck rejects with
// their positions in the diagnostics.
func setSignatureEdits(s *Snapshot, pkgPath, sym, sigStr string, defaults map[string]string) ([]edit, *Reject) {
	newSig, rej := parseSignatureText(sigStr)
	if rej != nil {
		return nil, rej
	}
	p, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, &Reject{Reason: "symbol is not a function", Detail: objKind(obj)}
	}
	decl, declFile := findFuncDecl(p, fn)
	if decl == nil {
		return nil, &Reject{Reason: "function declaration not found", Detail: sym}
	}
	oldSig := sigFromFuncType(s.fset, decl.Type)
	calls, _ := s.callSites(fn)
	plan, rej := planArgs(oldSig, newSig, defaults, len(calls))
	if rej != nil {
		return nil, rej
	}

	var edits []edit
	start := s.fset.Position(decl.Type.Params.Opening).Offset
	end := s.fset.Position(decl.Type.Params.Closing).Offset + 1
	if decl.Type.Results != nil {
		end = s.fset.Position(decl.Type.Results.End()).Offset
	}
	edits = append(edits, edit{declFile, start, end - start, newSig.text})

	src := map[string][]byte{}
	for _, c := range calls {
		file := c.pos.Filename
		if src[file] == nil {
			b, err := os.ReadFile(file)
			if err != nil {
				return nil, &Reject{Reason: "call site unreadable", Detail: file + ": " + err.Error()}
			}
			src[file] = b
		}
		b := src[file]
		spread := c.call.Ellipsis.IsValid()
		var argTexts []string
		for _, arg := range c.call.Args {
			from := s.fset.Position(arg.Pos()).Offset
			to := s.fset.Position(arg.End()).Offset
			argTexts = append(argTexts, string(b[from:to]))
		}
		newArgs := plan.rewrite(argTexts, spread)
		if spread && len(plan.slots) > 0 && plan.slots[len(plan.slots)-1].variadic &&
			plan.slots[len(plan.slots)-1].oldIndex >= 0 && len(newArgs) > 0 {
			newArgs[len(newArgs)-1] += "..."
		}
		lp := s.fset.Position(c.call.Lparen).Offset + 1
		rp := s.fset.Position(c.call.Rparen).Offset
		edits = append(edits, edit{file, lp, rp - lp, strings.Join(newArgs, ", ")})
	}
	return edits, nil
}

// setSignatureOp is the composable form; there is no single-op sugar —
// the cases that need this op (interface plus implementors, value-use
// repairs) are multi-op patches by nature.
type setSignatureOp struct{}

func (setSignatureOp) name() string { return "set_signature" }

func (setSignatureOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg       string            `json:"pkg"`
		Sym       string            `json:"sym"`
		Signature string            `json:"signature"`
		Defaults  map[string]string `json:"defaults"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	if a.Signature == "" {
		return &Reject{Reason: "signature is required"}
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	edits, rej := setSignatureEdits(ctx.s, pkg, sym, a.Signature, a.Defaults)
	if rej != nil {
		return rej
	}
	if rej := ctx.applyDeclEdits(edits); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	return nil
}

func init() {
	opRegistry["set_signature"] = func() patchOp { return setSignatureOp{} }
	declOps["set_signature"] = true
}

// rewrite builds a site's new argument texts from its old ones. spread
// marks a site calling the old variadic with tail... — the tail argument
// is carried as-is (the caller re-attaches the ellipsis).
func (p *argPlan) rewrite(oldArgs []string, spread bool) []string {
	var out []string
	for _, s := range p.slots {
		switch {
		case s.oldIndex < 0:
			out = append(out, s.text)
		case s.variadic:
			// Carry the whole tail: every old arg from the variadic's
			// position onward (one spread expr, or zero or more values).
			for i := s.oldIndex; i < len(oldArgs); i++ {
				out = append(out, oldArgs[i])
			}
		default:
			if s.oldIndex < len(oldArgs) {
				out = append(out, oldArgs[s.oldIndex])
			}
		}
	}
	return out
}
