package snapshot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
)

// insertStmt places rendered statement text relative to a handle and
// re-parses the declaration so subsequent ops see fresh handles.
// where: "before"|"after"|"first"|"last" ("first"/"last" need a block-owning handle).
//
// insertStmt re-parses ctx.src[ctx.file] fresh on every call rather than
// reusing a cached table: each prior op may have shifted every later
// statement's handle number, and the only source of truth for current
// handle assignment is handleWalk over the current bytes.
func (ctx *patchCtx) insertStmt(at, where, stmtText string) (string, *Reject) {
	if h, ok := ctx.handles[at]; ok {
		at = h
	}
	src := ctx.src[ctx.file]
	fset := token.NewFileSet()
	decl, err := declInFile(fset, ctx.file, src, ctx.sym)
	if err != nil {
		return "", &Reject{Reason: "declaration does not parse", Detail: err.Error()}
	}
	nt := buildNodeTable(decl)
	node, ok := nt.nodes[at]
	if !ok {
		return "", &Reject{Reason: "unknown handle",
			Detail:     fmt.Sprintf("op %d: %s", ctx.opIndex, at),
			DidYouMean: nearestHandles(at, nt.order)}
	}

	var offset int
	switch where {
	case "before":
		offset = lineStart(src, fset.Position(node.Pos()).Offset)
	case "after":
		offset = lineEndNL(src, fset.Position(node.End()).Offset)
	case "first", "last":
		block, ok := blockOf(node)
		if !ok {
			return "", &Reject{Reason: "handle does not own a block", Detail: at}
		}
		if where == "first" {
			offset = fset.Position(block.Lbrace).Offset + 1
		} else {
			offset = fset.Position(block.Rbrace).Offset
		}
	default:
		return "", &Reject{Reason: "unknown insertion point", Detail: where,
			DidYouMean: []string{"before", "after", "first", "last"}}
	}

	text := stmtText + "\n"
	if where == "last" && offset > 0 && src[offset-1] != '\n' {
		// The block's last statement and the closing brace share a line
		// (e.g. a one-line body); separate our insertion from whatever
		// token precedes the brace so it doesn't glue onto it.
		text = "\n" + text
	}
	out := make([]byte, 0, len(src)+len(text))
	out = append(out, src[:offset]...)
	out = append(out, text...)
	out = append(out, src[offset:]...)

	fset2 := token.NewFileSet()
	decl2, err := declInFile(fset2, ctx.file, out, ctx.sym)
	if err != nil {
		return "", &Reject{Reason: "insertion does not parse", Detail: err.Error()}
	}
	nt2 := buildNodeTable(decl2)
	insertedLine := bytes.Count(out[:offset], []byte("\n")) + 1
	newHandle := ""
	for _, h := range nt2.order {
		if fset2.Position(nt2.nodes[h].Pos()).Line == insertedLine {
			newHandle = h
			break
		}
	}
	if newHandle == "" {
		return "", &Reject{Reason: "inserted statement not found after reparse", Detail: stmtText}
	}

	ctx.src[ctx.file] = out
	ctx.fileLastOp[ctx.file] = ctx.opIndex
	return newHandle, nil
}

// exprAtom parses and returns gofmt'd expression text or a Reject with
// in-scope did_you_mean candidates.
//
// exprAtom only validates syntax (parser.ParseExpr); it has no type
// information at this point in the pipeline (ctx.src is not yet
// typechecked), so a malformed expression rejects with the parse error and
// no did_you_mean — undefined identifiers and other semantic problems
// surface at end-of-list validation instead, attributed back to this op by
// ctx.fileLastOp.
func (ctx *patchCtx) exprAtom(src string) (string, *Reject) {
	expr, err := parser.ParseExpr(src)
	if err != nil {
		return "", &Reject{Reason: "expression does not parse", Detail: err.Error()}
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, token.NewFileSet(), expr); err != nil {
		return "", &Reject{Reason: "expression does not parse", Detail: err.Error()}
	}
	return buf.String(), nil
}

// bindResult records op's own result handle at ctx.handles["$N"], where N is
// the 1-based index of the currently-applying op, so a later op in the same
// patch can address this op's output as "$N" instead of recomputing the
// handle name after the shift an insertion causes.
func (ctx *patchCtx) bindResult(h string) {
	ctx.handles[fmt.Sprintf("$%d", ctx.opIndex)] = h
}

// declInFile parses src as a Go file and returns the FuncDecl matching sym
// (receiver-qualified for methods), for building a fresh nodeTable against
// ctx.src bytes rather than the live typechecked snapshot.
func declInFile(fset *token.FileSet, filename string, src []byte, sym string) (*ast.FuncDecl, error) {
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	recv, name, isMethod := strings.Cut(sym, ".")
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if isMethod {
			if fd.Name.Name == name && recvTypeName(fd) == recv {
				return fd, nil
			}
		} else if fd.Recv == nil && fd.Name.Name == sym {
			return fd, nil
		}
	}
	return nil, fmt.Errorf("declaration for %s not found", sym)
}

// blockOf returns the block a handle's node owns, for "first"/"last"
// insertion, or false if the node does not own one. Switch/case bodies are
// deliberately excluded: a case clause has no brace pair to anchor on.
func blockOf(n ast.Node) (*ast.BlockStmt, bool) {
	switch s := n.(type) {
	case *ast.BlockStmt:
		return s, true
	case *ast.IfStmt:
		return s.Body, true
	case *ast.ForStmt:
		return s.Body, true
	case *ast.RangeStmt:
		return s.Body, true
	}
	return nil, false
}

// lineStart returns the offset of the start of the line containing offset.
func lineStart(src []byte, offset int) int {
	for offset > 0 && src[offset-1] != '\n' {
		offset--
	}
	return offset
}

// lineEndNL returns the offset just past the next newline at or after
// offset — the start of the following line, or len(src) if there is none.
func lineEndNL(src []byte, offset int) int {
	for offset < len(src) && src[offset] != '\n' {
		offset++
	}
	if offset < len(src) {
		offset++
	}
	return offset
}

// nearestHandles suggests existing handles close to a miss: substring match
// either way, falling back to the whole (capped) handle order when nothing
// is close — same shape as nearestOps for unknown op names.
func nearestHandles(miss string, order []string) []string {
	lower := strings.ToLower(miss)
	var hits []string
	for _, h := range order {
		lh := strings.ToLower(h)
		if lh == lower || strings.Contains(lh, lower) || strings.Contains(lower, lh) {
			hits = append(hits, h)
		}
	}
	if len(hits) == 0 {
		hits = append([]string(nil), order...)
	}
	if len(hits) > 3 {
		hits = hits[:3]
	}
	return hits
}

// addAssignOp inserts `lhs := rhs` or `lhs = rhs` relative to a handle.
type addAssignOp struct{}

func (addAssignOp) name() string { return "add_assign" }

func (addAssignOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At     string `json:"at"`
		Where  string `json:"where"`
		Lhs    string `json:"lhs"`
		Rhs    string `json:"rhs"`
		Define bool   `json:"define"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	if !token.IsIdentifier(a.Lhs) {
		return &Reject{Reason: "lhs is not a valid identifier", Detail: a.Lhs}
	}
	rhs, rej := ctx.exprAtom(a.Rhs)
	if rej != nil {
		return rej
	}
	assign := "="
	if a.Define {
		assign = ":="
	}
	h, rej := ctx.insertStmt(a.At, a.Where, a.Lhs+" "+assign+" "+rhs)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// addCallOp inserts a statement relative to a handle. Despite the field
// name, "expr" is spliced as a full simple statement rather than routed
// through exprAtom's expression-only grammar: callers use it for blank
// assignments (`_ = x`) as often as bare calls (`foo()`), and insertStmt's
// own re-parse after splicing already validates whatever lands there.
type addCallOp struct{}

func (addCallOp) name() string { return "add_call" }

func (addCallOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At    string `json:"at"`
		Where string `json:"where"`
		Expr  string `json:"expr"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	h, rej := ctx.insertStmt(a.At, a.Where, a.Expr)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

func init() {
	opRegistry["add_assign"] = func() patchOp { return addAssignOp{} }
	opRegistry["add_call"] = func() patchOp { return addCallOp{} }
}
