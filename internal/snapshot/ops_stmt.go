package snapshot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
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
//
// at arrives already resolved: patchComposable's resolveArgRefs rewrites any
// "$N" in an op's raw at/from/to args to the literal handle op N bound
// before the op's own struct unmarshal ever runs, so insertStmt (and every
// op that calls it) only ever sees plain handles like "n3".
func (ctx *patchCtx) insertStmt(at, where, stmtText string) (string, *Reject) {
	src := ctx.src[ctx.file]
	fset := token.NewFileSet()
	decl, err := declInFile(fset, ctx.file, src, ctx.sym)
	if err != nil {
		return "", &Reject{Reason: "declaration does not parse", Detail: err.Error()}
	}
	nt := buildNodeTable(decl)
	node, rej := ctx.lookupHandle(nt, at)
	if rej != nil {
		return "", rej
	}

	var offset int
	switch where {
	case "before":
		offset = lineStart(src, fset.Position(node.Pos()).Offset)
	case "after":
		offset = lineEndNL(src, fset.Position(node.End()).Offset)
	case "first", "last":
		switch nd := node.(type) {
		case *ast.CaseClause:
			// A case clause has no brace pair: "first" (and "last" against
			// an empty body) anchor right after the colon; "last" against a
			// populated body anchors after its final statement's line.
			if where == "first" || len(nd.Body) == 0 {
				offset = skipBlankLine(src, fset.Position(nd.Colon).Offset+1)
			} else {
				offset = lineEndNL(src, fset.Position(nd.Body[len(nd.Body)-1].End()).Offset)
			}
		default:
			block, ok := blockOf(node)
			if !ok {
				return "", &Reject{Reason: "handle does not own a block", Detail: at}
			}
			if where == "first" {
				offset = skipBlankLine(src, fset.Position(block.Lbrace).Offset+1)
			} else {
				offset = fset.Position(block.Rbrace).Offset
			}
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

// exprAtom parses src as a Go expression and returns its gofmt'd text, or a
// Reject carrying the parse error.
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

// unwrapParen strips any number of enclosing parens from an expression, so a
// shape check (call, receive, ...) sees through `(fallible())` the same as
// `fallible()`.
func unwrapParen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
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
// insertion or delete_node's emptiness check, or false if the node does not
// own one. A switch statement's own body (the list of its case clauses) is a
// real BlockStmt with brace positions, so add_case can append to it the same
// way as any other block; a single case clause's body is not (it has no
// brace pair to anchor on) and is handled separately, in insertStmt and
// childList.
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
	case *ast.SwitchStmt:
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

// skipBlankLine advances offset past one immediately-following newline, so a
// "first" insertion right after an opening brace or a case's colon lands at
// the start of the next line rather than gluing onto the brace/colon's own
// line — the common case is an empty block, whose body is just that one
// newline before the closing brace or next case. Only ever skips at most one
// newline: a truly blank line in the body (two newlines in a row) is left
// alone rather than swallowed.
func skipBlankLine(src []byte, offset int) int {
	if offset < len(src) && src[offset] == '\n' {
		return offset + 1
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

// lookupHandle resolves at in nt, or an "unknown handle" Reject naming the
// applying op's index and offering nearby handles — the one shape every op
// that addresses a handle uses, instead of each repeating the same
// map-lookup-plus-reject inline.
func (ctx *patchCtx) lookupHandle(nt *nodeTable, at string) (ast.Node, *Reject) {
	node, ok := nt.nodes[at]
	if !ok {
		return nil, &Reject{Reason: "unknown handle",
			Detail:     fmt.Sprintf("op %d: %s", ctx.opIndex, at),
			DidYouMean: nearestHandles(at, nt.order)}
	}
	return node, nil
}

// nodeKindName names an AST node's Go type for rejection details — the same
// %T-based shape objKind's default case uses for types.Object, so a caller
// gets "AssignStmt"/"IfStmt"/"CaseClause" rather than a raw Go type string.
func nodeKindName(n ast.Node) string {
	return strings.TrimPrefix(fmt.Sprintf("%T", n), "*ast.")
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
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
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

// addCallOp inserts a call-expression statement relative to a handle. "expr"
// must be a genuine call (`foo()`) or a channel receive (`<-ch`), the only
// two expression shapes Go allows as a standalone statement; anything else
// (an assignment in particular) belongs to add_assign instead.
type addCallOp struct{}

func (addCallOp) name() string { return "add_call" }

func (addCallOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At    string `json:"at"`
		Where string `json:"where"`
		Expr  string `json:"expr"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	expr, err := parser.ParseExpr(a.Expr)
	if err != nil {
		return &Reject{Reason: "expression does not parse", Detail: err.Error()}
	}
	expr = unwrapParen(expr)
	_, isCall := expr.(*ast.CallExpr)
	recv, isUnary := expr.(*ast.UnaryExpr)
	isReceive := isUnary && recv.Op == token.ARROW
	if !isCall && !isReceive {
		return &Reject{Reason: "add_call requires a call expression",
			Detail: "not a call or channel receive; use add_assign for assignments"}
	}
	h, rej := ctx.insertStmt(a.At, a.Where, a.Expr)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// addReturnOp inserts a return statement. Arity and result-type errors (too
// few/many values, wrong type) are not checked here; end-of-list typecheck
// catches them like any other op's mistakes, attributed back to this op's
// index by ctx.fileLastOp. No exprs renders a bare "return".
type addReturnOp struct{}

func (addReturnOp) name() string { return "add_return" }

func (addReturnOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At    string   `json:"at"`
		Where string   `json:"where"`
		Exprs []string `json:"exprs"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	parts := make([]string, len(a.Exprs))
	for i, e := range a.Exprs {
		v, rej := ctx.exprAtom(e)
		if rej != nil {
			return rej
		}
		parts[i] = v
	}
	stmt := "return"
	if len(parts) > 0 {
		stmt = "return " + strings.Join(parts, ", ")
	}
	h, rej := ctx.insertStmt(a.At, a.Where, stmt)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// addDeferOp inserts a defer statement. go/parser itself rejects a defer
// whose expression is not a call or method call (see parser.checkExpr for
// DeferStmt/GoStmt) — that is a syntax rule, not a typecheck one — so a
// non-call expr surfaces from insertStmt's re-parse after splicing as
// "insertion does not parse". add_call needs its own call-shape check
// because a plain ExprStmt parses for any expression; defer/go do not need
// the same special-casing.
type addDeferOp struct{}

func (addDeferOp) name() string { return "add_defer" }

func (addDeferOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At    string `json:"at"`
		Where string `json:"where"`
		Expr  string `json:"expr"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	expr, rej := ctx.exprAtom(a.Expr)
	if rej != nil {
		return rej
	}
	h, rej := ctx.insertStmt(a.At, a.Where, "defer "+expr)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// addGoOp inserts a go statement. Same parser-enforced call-shape guarantee
// as addDeferOp.
type addGoOp struct{}

func (addGoOp) name() string { return "add_go" }

func (addGoOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At    string `json:"at"`
		Where string `json:"where"`
		Expr  string `json:"expr"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	expr, rej := ctx.exprAtom(a.Expr)
	if rej != nil {
		return rej
	}
	h, rej := ctx.insertStmt(a.At, a.Where, "go "+expr)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// deleteNodeOp splices a statement, or an empty block/case-owning statement,
// out of the source. A block-owner with statements inside it rejects rather
// than silently discarding them; the caller deletes children first. An if
// with a populated else likewise rejects even when its then-block is empty,
// since deleting the whole statement would discard the else content too.
type deleteNodeOp struct{}

func (deleteNodeOp) name() string { return "delete_node" }

func (deleteNodeOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At string `json:"at"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	return ctx.deleteNode(a.At)
}

// deleteNode re-parses ctx.src fresh (same rationale as insertStmt: prior
// ops may have shifted every handle) and splices out at's whole line range.
func (ctx *patchCtx) deleteNode(at string) *Reject {
	src := ctx.src[ctx.file]
	fset := token.NewFileSet()
	decl, err := declInFile(fset, ctx.file, src, ctx.sym)
	if err != nil {
		return &Reject{Reason: "declaration does not parse", Detail: err.Error()}
	}
	nt := buildNodeTable(decl)
	node, rej := ctx.lookupHandle(nt, at)
	if rej != nil {
		return rej
	}
	if ifs, ok := node.(*ast.IfStmt); ok && ifs.Else != nil {
		return &Reject{Reason: "block is not empty", Detail: at + " has an else clause"}
	}
	if list, ok := childList(node); ok && len(list) > 0 {
		return &Reject{Reason: "block is not empty",
			Detail: at + " has children: " + strings.Join(directChildHandles(nt, list), ", ")}
	}

	start := lineStart(src, fset.Position(node.Pos()).Offset)
	end := lineEndNL(src, fset.Position(node.End()).Offset)
	out := make([]byte, 0, len(src)-(end-start))
	out = append(out, src[:start]...)
	out = append(out, src[end:]...)

	fset2 := token.NewFileSet()
	if _, err := declInFile(fset2, ctx.file, out, ctx.sym); err != nil {
		return &Reject{Reason: "deletion does not parse", Detail: err.Error()}
	}

	ctx.src[ctx.file] = out
	ctx.fileLastOp[ctx.file] = ctx.opIndex
	return nil
}

// childList returns the direct statement children a handle's node owns, for
// delete_node's emptiness check: a case clause's body (no brace pair to
// anchor on, so not reachable through blockOf), or a block-owning
// statement's block list. False means the node is a plain statement, owning
// neither.
func childList(n ast.Node) ([]ast.Stmt, bool) {
	if cc, ok := n.(*ast.CaseClause); ok {
		return cc.Body, true
	}
	if block, ok := blockOf(n); ok {
		return block.List, true
	}
	return nil, false
}

// directChildHandles names the handles of list's elements, for reporting
// which children block a non-empty deletion. list's elements are exactly the
// same AST node pointers handleWalk visited when nt was built, so identity
// (pointer) comparison finds them exactly.
func directChildHandles(nt *nodeTable, list []ast.Stmt) []string {
	set := make(map[ast.Stmt]bool, len(list))
	for _, st := range list {
		set[st] = true
	}
	var out []string
	for _, h := range nt.order {
		if st, ok := nt.nodes[h].(ast.Stmt); ok && set[st] {
			out = append(out, h)
		}
	}
	return out
}

// addIfOp inserts an if-statement, with an empty then-block and, when
// requested, an empty else block.
//
// bindResult records the IfStmt's own handle. blockOf already maps an
// IfStmt handle to its Body for "first"/"last" insertion, so $N addresses
// the then-block. There is currently no handle for a requested else block:
// reaching it needs a follow-up View call or structural addressing added
// later — v1's $N covers only the then-block a constructor creates.
type addIfOp struct{}

func (addIfOp) name() string { return "add_if" }

func (addIfOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At    string `json:"at"`
		Where string `json:"where"`
		Cond  string `json:"cond"`
		Else  bool   `json:"else"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	cond, rej := ctx.exprAtom(a.Cond)
	if rej != nil {
		return rej
	}
	stmt := "if " + cond + " {\n}"
	if a.Else {
		stmt += " else {\n}"
	}
	h, rej := ctx.insertStmt(a.At, a.Where, stmt)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// addForOp inserts a for-statement with an empty body: condition-only,
// infinite (neither cond nor range given), or range form. The range form
// splices "range" verbatim after "for " rather than through exprAtom — a
// range clause ("k, v := range coll") is not a standalone expression, so
// only a full syntax re-parse (insertStmt's, after splicing) can validate
// it.
type addForOp struct{}

func (addForOp) name() string { return "add_for" }

func (addForOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At    string `json:"at"`
		Where string `json:"where"`
		Cond  string `json:"cond"`
		Range string `json:"range"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	if a.Cond != "" && a.Range != "" {
		return &Reject{Reason: "add_for: cond and range are mutually exclusive"}
	}
	header := "for"
	switch {
	case a.Range != "":
		header = "for " + a.Range
	case a.Cond != "":
		cond, rej := ctx.exprAtom(a.Cond)
		if rej != nil {
			return rej
		}
		header = "for " + cond
	}
	h, rej := ctx.insertStmt(a.At, a.Where, header+" {\n}")
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// addSwitchOp inserts a switch-statement with an empty body: tagless when no
// tag is given.
type addSwitchOp struct{}

func (addSwitchOp) name() string { return "add_switch" }

func (addSwitchOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At    string `json:"at"`
		Where string `json:"where"`
		Tag   string `json:"tag"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	header := "switch"
	if a.Tag != "" {
		tag, rej := ctx.exprAtom(a.Tag)
		if rej != nil {
			return rej
		}
		header = "switch " + tag
	}
	h, rej := ctx.insertStmt(a.At, a.Where, header+" {\n}")
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// addCaseOp appends a case (or default) clause to an existing switch. "at"
// names the switch, not an insertion point around it: add_case always
// appends as the last clause, since v1 has no argument for placing a new
// case among existing ones. bindResult records the new CaseClause's own
// handle, which insertStmt's CaseClause branch treats as a block owner for
// "first"/"last" against the case's body.
type addCaseOp struct{}

func (addCaseOp) name() string { return "add_case" }

func (addCaseOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At      string   `json:"at"`
		Exprs   []string `json:"exprs"`
		Default bool     `json:"default"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	if a.Default && len(a.Exprs) > 0 {
		return &Reject{Reason: "add_case: default and exprs are mutually exclusive"}
	}
	if !a.Default && len(a.Exprs) == 0 {
		return &Reject{Reason: "add_case requires exprs or default"}
	}
	var header string
	if a.Default {
		header = "default:"
	} else {
		parts := make([]string, len(a.Exprs))
		for i, e := range a.Exprs {
			v, rej := ctx.exprAtom(e)
			if rej != nil {
				return rej
			}
			parts[i] = v
		}
		header = "case " + strings.Join(parts, ", ") + ":"
	}
	h, rej := ctx.insertStmt(a.At, "last", header)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// exprSlot returns the byte-range (as token.Pos, resolved to offsets by the
// caller's own FileSet) of the expression slot a handle names: an if/for's
// condition, a case clause's whole expression list read as one contiguous
// range spanning every expr (set_cond's single new expr replaces the whole
// list — v1 has no per-element case-expr addressing), or — only when
// allowExprStmt is set, replace_expr's wider v1 scope — a whole expression
// statement's expr. False means the node has no such slot (wrong kind, or an
// if/for with no condition, or a default case with an empty List).
func exprSlot(node ast.Node, allowExprStmt bool) (start, end token.Pos, ok bool) {
	switch n := node.(type) {
	case *ast.IfStmt:
		if n.Cond == nil {
			return 0, 0, false
		}
		return n.Cond.Pos(), n.Cond.End(), true
	case *ast.ForStmt:
		if n.Cond == nil {
			return 0, 0, false
		}
		return n.Cond.Pos(), n.Cond.End(), true
	case *ast.CaseClause:
		if len(n.List) == 0 {
			return 0, 0, false
		}
		return n.List[0].Pos(), n.List[len(n.List)-1].End(), true
	case *ast.ExprStmt:
		if allowExprStmt {
			return n.X.Pos(), n.X.End(), true
		}
	}
	return 0, 0, false
}

// replaceBytes splices newText over ctx.src[ctx.file][start:end) and
// re-parses to confirm the result is still syntactically valid Go, following
// insertStmt/deleteNode's re-parse-after-splice discipline.
func (ctx *patchCtx) replaceBytes(start, end int, newText string) *Reject {
	src := ctx.src[ctx.file]
	out := make([]byte, 0, len(src)-(end-start)+len(newText))
	out = append(out, src[:start]...)
	out = append(out, newText...)
	out = append(out, src[end:]...)
	fset2 := token.NewFileSet()
	if _, err := declInFile(fset2, ctx.file, out, ctx.sym); err != nil {
		return &Reject{Reason: "replacement does not parse", Detail: err.Error()}
	}
	ctx.src[ctx.file] = out
	ctx.fileLastOp[ctx.file] = ctx.opIndex
	return nil
}

// spliceExprSlot re-parses ctx.src fresh (same rationale as insertStmt: prior
// ops may have shifted every handle), looks up at's expression slot via
// exprSlot, and replaces it with exprText. wrongKindReason names the
// caller's own rejection message when at's node has no such slot — set_cond
// and replace_expr each word this differently, since their v1 scopes differ.
func (ctx *patchCtx) spliceExprSlot(at, exprText string, allowExprStmt bool, wrongKindReason string) *Reject {
	src := ctx.src[ctx.file]
	fset := token.NewFileSet()
	decl, err := declInFile(fset, ctx.file, src, ctx.sym)
	if err != nil {
		return &Reject{Reason: "declaration does not parse", Detail: err.Error()}
	}
	nt := buildNodeTable(decl)
	node, rej := ctx.lookupHandle(nt, at)
	if rej != nil {
		return rej
	}
	if cc, isCase := node.(*ast.CaseClause); isCase && len(cc.List) == 0 {
		return &Reject{Reason: wrongKindReason, Detail: "default clause has no condition to replace"}
	}
	startPos, endPos, ok := exprSlot(node, allowExprStmt)
	if !ok {
		return &Reject{Reason: wrongKindReason, Detail: nodeKindName(node)}
	}
	expr, rej := ctx.exprAtom(exprText)
	if rej != nil {
		return rej
	}
	return ctx.replaceBytes(fset.Position(startPos).Offset, fset.Position(endPos).Offset, expr)
}

// setCondOp replaces an if/for/case's condition (case: its whole expr list,
// with the one new expr) in place.
type setCondOp struct{}

func (setCondOp) name() string { return "set_cond" }

func (setCondOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At   string `json:"at"`
		Expr string `json:"expr"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	if rej := ctx.spliceExprSlot(a.At, a.Expr, false, "set_cond targets an if/for/case condition"); rej != nil {
		return rej
	}
	ctx.bindResult(a.At)
	return nil
}

// replaceExprOp is set_cond's wider v1 sibling: same condition-replacement
// plus a whole expression statement. Per-slot sub-expression handles (e.g.
// one argument of a call) are future work, not this op's v1 scope.
type replaceExprOp struct{}

func (replaceExprOp) name() string { return "replace_expr" }

func (replaceExprOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At   string `json:"at"`
		Expr string `json:"expr"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	if rej := ctx.spliceExprSlot(a.At, a.Expr, true,
		"replace_expr targets a condition or expression statement in v1"); rej != nil {
		return rej
	}
	ctx.bindResult(a.At)
	return nil
}

// indexOfStmt returns list's index of target by pointer identity (the same
// identity directChildHandles relies on — list's elements are exactly the
// ast.Stmt values handleWalk visited), or -1 if list does not contain it.
func indexOfStmt(list []ast.Stmt, target ast.Stmt) int {
	for i, st := range list {
		if st == target {
			return i
		}
	}
	return -1
}

// walkStmtLists visits every direct statement list reachable from decl's
// body that wrap_stmts' sibling check treats as a "block": the body itself,
// and every nested if/else/for/range/block body and case-clause body.
// Mirrors handleWalk's descent through the same node shapes (including its
// else-if-chain trick of wrapping a lone *ast.IfStmt in a synthetic
// single-element list to keep recursing) since it is the same "what counts
// as a block" question handleWalk already answers for handle assignment.
func walkStmtLists(body *ast.BlockStmt, visit func(list []ast.Stmt)) {
	var walk func(list []ast.Stmt)
	walk = func(list []ast.Stmt) {
		visit(list)
		for _, st := range list {
			switch st := st.(type) {
			case *ast.IfStmt:
				walk(st.Body.List)
				if els, ok := st.Else.(*ast.BlockStmt); ok {
					walk(els.List)
				} else if els, ok := st.Else.(*ast.IfStmt); ok {
					walk([]ast.Stmt{els})
				}
			case *ast.ForStmt:
				walk(st.Body.List)
			case *ast.RangeStmt:
				walk(st.Body.List)
			case *ast.SwitchStmt:
				for _, c := range st.Body.List {
					if cc, ok := c.(*ast.CaseClause); ok {
						walk(cc.Body)
					}
				}
			case *ast.BlockStmt:
				walk(st.List)
			}
		}
	}
	walk(body.List)
}

// wrapStmts re-parses ctx.src fresh (same rationale as insertStmt/deleteNode)
// and splices with's header plus an opening brace right before from's line
// start, and a closing brace right after to's line end, enclosing every
// statement between them inclusive. from/to must be direct members of the
// same statement list, in order — sibling containment is checked
// structurally via walkStmtLists, not by handle-number adjacency, since
// handle numbers also count statements nested inside from..to that are not
// themselves siblings at this level.
func (ctx *patchCtx) wrapStmts(fromH, toH, with, cond string) (string, *Reject) {
	src := ctx.src[ctx.file]
	fset := token.NewFileSet()
	decl, err := declInFile(fset, ctx.file, src, ctx.sym)
	if err != nil {
		return "", &Reject{Reason: "declaration does not parse", Detail: err.Error()}
	}
	nt := buildNodeTable(decl)
	fromNode, rej := ctx.lookupHandle(nt, fromH)
	if rej != nil {
		return "", rej
	}
	toNode, rej := ctx.lookupHandle(nt, toH)
	if rej != nil {
		return "", rej
	}
	// Every value buildNodeTable stores comes from handleWalk's own
	// func(ast.Stmt) callback, so this assertion cannot fail; kept ok-checked
	// only to match directChildHandles' existing style for the same lookup.
	fromStmt, _ := fromNode.(ast.Stmt)
	toStmt, _ := toNode.(ast.Stmt)

	var list []ast.Stmt
	walkStmtLists(decl.Body, func(l []ast.Stmt) {
		if list == nil && indexOfStmt(l, fromStmt) != -1 {
			list = l
		}
	})
	fromIdx, toIdx := -1, -1
	if list != nil {
		fromIdx = indexOfStmt(list, fromStmt)
		toIdx = indexOfStmt(list, toStmt)
	}
	if list == nil || toIdx == -1 || toIdx < fromIdx {
		detail := "from is not a direct statement of any block"
		if list != nil {
			detail = "enclosing block: " + strings.Join(directChildHandles(nt, list), ", ")
		}
		return "", &Reject{Reason: "from/to are not siblings in order", Detail: detail}
	}

	var header string
	switch with {
	case "if", "for":
		if cond == "" {
			return "", &Reject{Reason: "wrap_stmts: cond is required for with=" + with}
		}
		c, rej := ctx.exprAtom(cond)
		if rej != nil {
			return "", rej
		}
		header = with + " " + c
	case "block":
		if cond != "" {
			return "", &Reject{Reason: "wrap_stmts: cond is not allowed for with=block"}
		}
	default:
		return "", &Reject{Reason: "unknown wrap_stmts with value", Detail: with,
			DidYouMean: []string{"if", "for", "block"}}
	}

	fromOff := lineStart(src, fset.Position(fromStmt.Pos()).Offset)
	toEndOff := lineEndNL(src, fset.Position(toStmt.End()).Offset)
	out := make([]byte, 0, len(src)+len(header)+8)
	out = append(out, src[:fromOff]...)
	if header != "" {
		out = append(out, header...)
		out = append(out, ' ')
	}
	out = append(out, "{\n"...)
	out = append(out, src[fromOff:toEndOff]...)
	out = append(out, "}\n"...)
	out = append(out, src[toEndOff:]...)

	fset2 := token.NewFileSet()
	decl2, err := declInFile(fset2, ctx.file, out, ctx.sym)
	if err != nil {
		return "", &Reject{Reason: "wrap_stmts result does not parse", Detail: err.Error()}
	}
	nt2 := buildNodeTable(decl2)
	wrappedLine := bytes.Count(out[:fromOff], []byte("\n")) + 1
	newHandle := ""
	for _, h := range nt2.order {
		if fset2.Position(nt2.nodes[h].Pos()).Line == wrappedLine {
			newHandle = h
			break
		}
	}
	if newHandle == "" {
		return "", &Reject{Reason: "wrapped statement not found after reparse"}
	}

	ctx.src[ctx.file] = out
	ctx.fileLastOp[ctx.file] = ctx.opIndex
	return newHandle, nil
}

// wrapStmtsOp encloses a contiguous run of sibling statements (from..to,
// inclusive) in a new if/for/block.
type wrapStmtsOp struct{}

func (wrapStmtsOp) name() string { return "wrap_stmts" }

func (wrapStmtsOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		From string `json:"from"`
		To   string `json:"to"`
		With string `json:"with"`
		Cond string `json:"cond"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	h, rej := ctx.wrapStmts(a.From, a.To, a.With, a.Cond)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

// exprText renders an already-parsed expression node back to source text via
// go/format — the inverse of exprAtom, for reassembling LHS identifiers,
// result types, and call expressions pulled out of a statement already on
// hand rather than parsed fresh from a string.
func exprText(n ast.Expr) string {
	var buf bytes.Buffer
	// n is already a valid parsed AST node; format.Node on one can only fail
	// for shapes go/printer refuses to print at all (illegal ASTs), which
	// none of wrap_error's uses (identifiers, type exprs, call exprs pulled
	// straight from parsed source) can produce.
	_ = format.Node(&buf, token.NewFileSet(), n)
	return buf.String()
}

// unwrapCall unwraps parens and asserts the result is a call, for wrap_error's
// two node shapes (an assignment's RHS, or an expression statement's expr)
// that must both be genuine calls.
func unwrapCall(e ast.Expr) *ast.CallExpr {
	call, _ := unwrapParen(e).(*ast.CallExpr)
	return call
}

// errorReturningResults returns the enclosing declaration's result type
// expressions, rejecting "enclosing function does not return error" when
// there are none or the last one isn't literally the identifier "error" — a
// locally shadowed type named "error" is a known v1 ceiling, same trade-off
// as zeroValueFor's syntactic (not go/types-backed) approach below.
func errorReturningResults(decl *ast.FuncDecl) ([]ast.Expr, *Reject) {
	if decl.Type.Results == nil || len(decl.Type.Results.List) == 0 {
		return nil, &Reject{Reason: "enclosing function does not return error"}
	}
	var results []ast.Expr
	for _, f := range decl.Type.Results.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			results = append(results, f.Type)
		}
	}
	if last, ok := results[len(results)-1].(*ast.Ident); !ok || last.Name != "error" {
		return nil, &Reject{Reason: "enclosing function does not return error"}
	}
	return results, nil
}

// zeroValueFor renders a Go zero-value literal for a result's type by
// inspecting the type expression's own AST shape — no type-checking:
// pointer/slice/map/chan/func/interface types (plus the "error"/"any"
// identifiers) zero to nil, the predeclared numeric types zero to 0, string
// zeros to "", bool zeros to false, and anything else (a named type) is
// assumed struct-shaped and zeros to "T{}". A named non-struct type (e.g.
// "type Count int") is a known v1 ceiling: it renders as "Count{}", which
// does not compile — real zero-value inference needs the type's underlying
// kind from go/types, out of scope for this purely syntactic op.
func zeroValueFor(t ast.Expr) string {
	switch e := t.(type) {
	case *ast.StarExpr, *ast.ArrayType, *ast.MapType, *ast.ChanType,
		*ast.FuncType, *ast.InterfaceType:
		return "nil"
	case *ast.Ident:
		switch e.Name {
		case "bool":
			return "false"
		case "string":
			return `""`
		case "error", "any":
			return "nil"
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
			"float32", "float64", "complex64", "complex128",
			"byte", "rune":
			return "0"
		default:
			return e.Name + "{}"
		}
	default:
		return exprText(e) + "{}"
	}
}

// calleeResultCount resolves a call's function result count for wrap_error's
// bare-expression-statement case, which (unlike the assignment case) carries
// no textual arity of its own to read off. v1 only resolves a package-level
// function identifier in the target's own package (ctx.pkg) via the
// snapshot's already-typechecked scope — this is safe to read mid-patch
// because patchComposable never writes ctx.src to disk until every op has
// applied, so s.pkgs still reflects the pre-patch, on-disk state for the
// whole op loop. A selector call (an imported package's function, or a
// method call) rejects rather than guessing; proper resolution needs either
// import-alias tracking or receiver type inference, both out of scope here.
func (ctx *patchCtx) calleeResultCount(call *ast.CallExpr) (int, *Reject) {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return 0, &Reject{Reason: "wrap_error cannot resolve call target in v1",
			Detail: "only a same-package function identifier is supported"}
	}
	p := ctx.s.primary(ctx.pkg)
	if p == nil || p.Types == nil {
		return 0, &Reject{Reason: "wrap_error cannot resolve call target in v1", Detail: ident.Name}
	}
	fn, ok := p.Types.Scope().Lookup(ident.Name).(*types.Func)
	if !ok {
		return 0, &Reject{Reason: "wrap_error cannot resolve call target in v1", Detail: ident.Name}
	}
	results := fn.Signature().Results()
	errType := types.Universe.Lookup("error").Type()
	if results.Len() == 0 || !types.Identical(results.At(results.Len()-1).Type(), errType) {
		return 0, &Reject{Reason: "wrap_error target does not return error", Detail: ident.Name}
	}
	return results.Len(), nil
}

// wrapError re-parses ctx.src fresh and rewrites at (an assignment whose
// last LHS is "_"/"err" with a call RHS, or an expression-statement call) to
// bind "err", inserting an "if err != nil { return ..., fmt.Errorf(...) }"
// guard right after — the Go idiom this op automates end to end.
func (ctx *patchCtx) wrapError(at, message string) (string, *Reject) {
	src := ctx.src[ctx.file]
	fset := token.NewFileSet()
	decl, err := declInFile(fset, ctx.file, src, ctx.sym)
	if err != nil {
		return "", &Reject{Reason: "declaration does not parse", Detail: err.Error()}
	}
	results, rej := errorReturningResults(decl)
	if rej != nil {
		return "", rej
	}
	nt := buildNodeTable(decl)
	node, rej := ctx.lookupHandle(nt, at)
	if rej != nil {
		return "", rej
	}

	var newLineText string
	switch st := node.(type) {
	case *ast.AssignStmt:
		if len(st.Lhs) == 0 {
			return "", &Reject{Reason: "wrap_error target has no assigned values", Detail: at}
		}
		last, ok := st.Lhs[len(st.Lhs)-1].(*ast.Ident)
		if !ok || (last.Name != "_" && last.Name != "err") {
			return "", &Reject{Reason: "wrap_error requires the last assigned value be _ or err", Detail: at}
		}
		if len(st.Rhs) != 1 || unwrapCall(st.Rhs[0]) == nil {
			return "", &Reject{Reason: "wrap_error requires a single call on the right-hand side", Detail: at}
		}
		if st.Tok != token.DEFINE && st.Tok != token.ASSIGN {
			// A compound assignment (+=, &&=, ...) can't be rewritten to swap
			// its last value for err by just changing the token text — Go
			// doesn't have a compound-assign form for an error-shaped target
			// this way, so this is out of v1 scope rather than silently
			// mistranslated to "=".
			return "", &Reject{Reason: "wrap_error requires a plain = or := assignment", Detail: at}
		}
		tok := "="
		if st.Tok == token.DEFINE {
			tok = ":="
		}
		lhsParts := make([]string, len(st.Lhs))
		for i, l := range st.Lhs[:len(st.Lhs)-1] {
			lhsParts[i] = exprText(l)
		}
		lhsParts[len(lhsParts)-1] = "err"
		newLineText = strings.Join(lhsParts, ", ") + " " + tok + " " + exprText(st.Rhs[0])

	case *ast.ExprStmt:
		call := unwrapCall(st.X)
		if call == nil {
			return "", &Reject{Reason: "wrap_error requires a call expression", Detail: at}
		}
		n, rej := ctx.calleeResultCount(call)
		if rej != nil {
			return "", rej
		}
		lhs := make([]string, n)
		for i := range lhs[:n-1] {
			lhs[i] = "_"
		}
		lhs[n-1] = "err"
		newLineText = strings.Join(lhs, ", ") + " := " + exprText(st.X)

	default:
		return "", &Reject{Reason: "wrap_error targets an assignment or expression-statement call",
			Detail: nodeKindName(node)}
	}

	zeros := make([]string, len(results)-1)
	for i, r := range results[:len(results)-1] {
		zeros[i] = zeroValueFor(r)
	}
	retExprs := append(zeros, fmt.Sprintf("fmt.Errorf(%q, err)", message+": %w"))
	guard := "if err != nil {\n\treturn " + strings.Join(retExprs, ", ") + "\n}"

	start := lineStart(src, fset.Position(node.Pos()).Offset)
	end := lineEndNL(src, fset.Position(node.End()).Offset)
	out := make([]byte, 0, len(src)+len(newLineText)+len(guard)+8)
	out = append(out, src[:start]...)
	out = append(out, newLineText...)
	out = append(out, '\n')
	out = append(out, guard...)
	out = append(out, '\n')
	out = append(out, src[end:]...)

	fset2 := token.NewFileSet()
	decl2, err := declInFile(fset2, ctx.file, out, ctx.sym)
	if err != nil {
		return "", &Reject{Reason: "wrap_error result does not parse", Detail: err.Error()}
	}
	nt2 := buildNodeTable(decl2)
	guardLine := bytes.Count(out[:start], []byte("\n")) + 2
	newHandle := ""
	for _, h := range nt2.order {
		if fset2.Position(nt2.nodes[h].Pos()).Line == guardLine {
			newHandle = h
			break
		}
	}
	if newHandle == "" {
		return "", &Reject{Reason: "wrap_error guard not found after reparse"}
	}

	ctx.src[ctx.file] = out
	ctx.fileLastOp[ctx.file] = ctx.opIndex
	return newHandle, nil
}

// wrapErrorOp automates the check-and-return-wrapped-error idiom.
type wrapErrorOp struct{}

func (wrapErrorOp) name() string { return "wrap_error" }

func (wrapErrorOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		At      string `json:"at"`
		Message string `json:"message"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	h, rej := ctx.wrapError(a.At, a.Message)
	if rej != nil {
		return rej
	}
	ctx.bindResult(h)
	return nil
}

func init() {
	opRegistry["add_assign"] = func() patchOp { return addAssignOp{} }
	opRegistry["add_call"] = func() patchOp { return addCallOp{} }
	opRegistry["add_return"] = func() patchOp { return addReturnOp{} }
	opRegistry["add_defer"] = func() patchOp { return addDeferOp{} }
	opRegistry["add_go"] = func() patchOp { return addGoOp{} }
	opRegistry["delete_node"] = func() patchOp { return deleteNodeOp{} }
	opRegistry["add_if"] = func() patchOp { return addIfOp{} }
	opRegistry["add_for"] = func() patchOp { return addForOp{} }
	opRegistry["add_switch"] = func() patchOp { return addSwitchOp{} }
	opRegistry["add_case"] = func() patchOp { return addCaseOp{} }
	opRegistry["set_cond"] = func() patchOp { return setCondOp{} }
	opRegistry["replace_expr"] = func() patchOp { return replaceExprOp{} }
	opRegistry["wrap_stmts"] = func() patchOp { return wrapStmtsOp{} }
	opRegistry["wrap_error"] = func() patchOp { return wrapErrorOp{} }
}
