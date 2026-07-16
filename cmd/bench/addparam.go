package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path"
	"slices"
	"strings"
)

// prepAddParam derives add-param manifests from each add-param-kind task's
// ground-truth commit, mirroring prepRename's shape.
func prepAddParam(scratch, tasksFile, outFile string) error {
	var tasks []Task
	b, err := os.ReadFile(tasksFile)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &tasks); err != nil {
		return err
	}
	var out []Manifest
	clean := 0
	for _, t := range tasks {
		if !slices.Contains(t.Kinds, "add-param") {
			continue
		}
		repo := path.Join(scratch, t.Repo)
		m := Manifest{Repo: t.Repo, SHA: t.SHA, Kind: "add-param",
			Prompt: strings.TrimSpace(issueRef.ReplaceAllString(t.Subject, "")), GoFiles: t.GoFiles}
		m.AddParams, m.NeedsReview = extractAddParams(repo, t.SHA)
		if len(m.AddParams) > 0 {
			clean++
			m.Prompt = ensureParamsNamed(m.Prompt, m.AddParams)
		}
		out = append(out, m)
	}
	b, _ = json.MarshalIndent(out, "", " ")
	if err := os.WriteFile(outFile, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d/%d extracted cleanly -> %s\n", clean, len(out), outFile)
	return nil
}

// ensureParamsNamed appends the explicit targets so the prompt names what
// the predicate scores, the same contract rename prompts carry.
func ensureParamsNamed(prompt string, specs []AddParamSpec) string {
	var lines []string
	for _, s := range specs {
		if !strings.Contains(prompt, s.Name) || !strings.Contains(prompt, s.Sym) {
			lines = append(lines, fmt.Sprintf("add parameter %s %s to %s", s.Name, s.Type, s.Sym))
		}
	}
	if len(lines) == 0 {
		return prompt
	}
	return prompt + "\n\nSpecifically: " + strings.Join(lines, "; ")
}

// extractAddParams recovers (pkg, sym, name, type) add-param specs from a
// ground-truth commit: a function present on both sides whose parameter
// list gained named parameters, with results unchanged (a result change is
// a signature task, not add-param). Returns a review note when the diff
// holds no such function.
func extractAddParams(repo, sha string) ([]AddParamSpec, string) {
	files := changedGoFiles(repo, sha)
	if len(files) == 0 {
		return nil, "no Go files in diff"
	}
	var specs []AddParamSpec
	for _, f := range files {
		before := parseFuncs(repo, sha+"^:"+f)
		after := parseFuncs(repo, sha+":"+f)
		for sym, bf := range before {
			af, ok := after[sym]
			if !ok || renderFieldList(af.fset, af.decl.Type.Results) != renderFieldList(bf.fset, bf.decl.Type.Results) {
				continue
			}
			bParams := paramSet(bf.decl)
			for _, field := range af.decl.Type.Params.List {
				typ := renderExpr(af.fset, field.Type)
				for _, id := range field.Names {
					if !bParams[id.Name] {
						specs = append(specs, AddParamSpec{
							Pkg: pkgPath(repo, sha, f), Sym: sym, Name: id.Name, Type: typ})
					}
				}
			}
		}
	}
	if len(specs) == 0 {
		return nil, "no function gained a named parameter in the diff"
	}
	return specs, ""
}

type funcAt struct {
	decl *ast.FuncDecl
	fset *token.FileSet
}

// parseFuncs indexes a revision's FuncDecls by sym (Recv.Name or Name).
func parseFuncs(repo, spec string) map[string]funcAt {
	out := map[string]funcAt{}
	src := gitShow(repo, spec)
	if src == "" {
		return out
	}
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		return out
	}
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		sym := fd.Name.Name
		if recv := recvType(fd); recv != "" {
			sym = recv + "." + sym
		}
		out[sym] = funcAt{fd, fset}
	}
	return out
}

func paramSet(fd *ast.FuncDecl) map[string]bool {
	set := map[string]bool{}
	for _, field := range fd.Type.Params.List {
		for _, id := range field.Names {
			set[id.Name] = true
		}
	}
	return set
}

func renderExpr(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	printer.Fprint(&b, fset, e)
	return b.String()
}

func renderFieldList(fset *token.FileSet, fl *ast.FieldList) string {
	if fl == nil {
		return ""
	}
	var b bytes.Buffer
	printer.Fprint(&b, fset, fl)
	return b.String()
}
