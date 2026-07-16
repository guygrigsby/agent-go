package snapshot

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
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
	sig := &sigText{text: strings.TrimSpace(text)}
	for _, field := range fd.Type.Params.List {
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
	if fd.Type.Results != nil {
		var parts []string
		for _, field := range fd.Type.Results.List {
			t := renderNode(fset, field.Type)
			n := max(len(field.Names), 1)
			for range n {
				parts = append(parts, t)
			}
		}
		if len(parts) == 1 && len(fd.Type.Results.List[0].Names) == 0 {
			sig.results = parts[0]
		} else {
			sig.results = "(" + strings.Join(parts, ", ") + ")"
		}
	}
	return sig, nil
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
