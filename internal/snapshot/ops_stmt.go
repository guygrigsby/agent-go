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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	expr, err := parser.ParseExpr(a.Expr)
	if err != nil {
		return &Reject{Reason: "expression does not parse", Detail: err.Error()}
	}
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.X
	}
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
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
	node, ok := nt.nodes[at]
	if !ok {
		return &Reject{Reason: "unknown handle",
			Detail:     fmt.Sprintf("op %d: %s", ctx.opIndex, at),
			DidYouMean: nearestHandles(at, nt.order)}
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
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
}
