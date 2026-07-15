package snapshot

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"strings"

	"golang.org/x/tools/go/packages"
)

type nodeTable struct {
	decl  *ast.FuncDecl
	file  string
	nodes map[string]ast.Node
	order []string
}

// handleWalk visits every statement in the body in preorder. It is the
// single definition of handle order; view rendering and patch addressing
// both derive from it.
func handleWalk(body *ast.BlockStmt, visit func(ast.Stmt)) {
	var walk func(list []ast.Stmt)
	walk = func(list []ast.Stmt) {
		for _, st := range list {
			visit(st)
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
						visit(cc)
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

func (s *Snapshot) nodeTableFor(pkgPath, sym string) (*nodeTable, *Reject) {
	p, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, &Reject{Reason: "handles exist only inside functions", Detail: objKind(obj)}
	}
	decl, file := findFuncDecl(p, fn)
	if decl == nil || decl.Body == nil {
		return nil, &Reject{Reason: "function declaration not found", Detail: sym}
	}
	nt := &nodeTable{decl: decl, file: file, nodes: map[string]ast.Node{}}
	i := 0
	handleWalk(decl.Body, func(st ast.Stmt) {
		i++
		h := fmt.Sprintf("n%d", i)
		nt.nodes[h] = st
		nt.order = append(nt.order, h)
	})
	return nt, nil
}

// declText returns the source text of obj's top-level declaration, including
// its doc comment, for symbols that are not functions (const, var, type).
func (s *Snapshot) declText(p *packages.Package, obj types.Object) (string, error) {
	file, start, end := s.findDeclRange(p, obj.Name(), obj.Name())
	if file == "" {
		return "", &Reject{Reason: "declaration not found", Detail: obj.Name()}
	}
	src, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return string(src[start:end]), nil
}

// View renders the declaration with a handle prefix on each statement line.
func (s *Snapshot) View(pkgPath, sym string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.ensureFresh(); err != nil {
		return nil, err
	}
	p, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	gen := s.generation(pkgPath, sym)
	// Non-functions: plain source slice, no handles.
	if _, isFn := obj.(*types.Func); !isFn {
		text, err := s.declText(p, obj)
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok", "generation": gen, "text": text, "nodes": 0}, nil
	}
	nt, rej := s.nodeTableFor(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	src, err := os.ReadFile(nt.file)
	if err != nil {
		return nil, err
	}
	start := s.fset.Position(nt.decl.Pos()).Offset
	end := s.fset.Position(nt.decl.End()).Offset
	// Annotate: map each handle's statement start line to a prefix.
	prefix := map[int]string{}
	for _, h := range nt.order {
		line := s.fset.Position(nt.nodes[h].Pos()).Line
		if _, taken := prefix[line]; !taken {
			prefix[line] = h + ": "
		}
	}
	firstLine := s.fset.Position(nt.decl.Pos()).Line
	var b strings.Builder
	for i, line := range strings.Split(string(src[start:end]), "\n") {
		if p, ok := prefix[firstLine+i]; ok {
			b.WriteString(p)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return map[string]any{"status": "ok", "generation": gen,
		"text": b.String(), "nodes": len(nt.order)}, nil
}
