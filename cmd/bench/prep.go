package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/guygrigsby/agent-go/bench"
)

// Task mirrors bench/tasks.json entries.
type Task struct {
	Repo    string   `json:"repo"`
	SHA     string   `json:"sha"`
	Subject string   `json:"subject"`
	Kinds   []string `json:"kinds"`
	GoFiles int      `json:"go_files"`
}

// Manifest and RenameSpec are shared with the runner; the prep tool
// writes what the runner scores.
type (
	Manifest     = bench.Manifest
	RenameSpec   = bench.RenameSpec
	AddParamSpec = bench.AddParamSpec
)

var (
	subjectRename = regexp.MustCompile(`(?i)renam\w*\s+` + "`?" + `(\w+)` + "`?" + `\s*(?:to|->|→)\s*` + "`?" + `(\w+)` + "`?")
	issueRef      = regexp.MustCompile(`\s*\(#\d+\)\s*$`)
)

// prepRename derives (pkg, sym, to) rename specs from each rename-kind
// task's ground-truth commit: the subject names the pair when it can, the
// identifier count-diff recovers it otherwise, and declarations are located
// by parsing the parent-revision files rather than pattern-matching them.
func prepRename(scratch, tasksFile, outFile string) error {
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
		if !slices.Contains(t.Kinds, "rename") {
			continue
		}
		repo := path.Join(scratch, t.Repo)
		m := Manifest{Repo: t.Repo, SHA: t.SHA, Kind: "rename",
			Prompt: strings.TrimSpace(issueRef.ReplaceAllString(t.Subject, "")), GoFiles: t.GoFiles}
		m.Renames, m.NeedsReview = extract(repo, t.SHA, m.Prompt)
		if len(m.Renames) > 0 {
			clean++
			m.Prompt = ensureTargetsNamed(m.Prompt, m.Renames)
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

func extract(repo, sha, subject string) ([]RenameSpec, string) {
	files := changedGoFiles(repo, sha)
	if len(files) == 0 {
		return nil, "no Go files in diff"
	}
	decls := parseDecls(repo, sha+"^", files)
	after := parseDecls(repo, sha, files)
	lost, gained := identDiff(repo, sha, files)
	// confirmed: the ground truth must actually contain the renamed
	// declaration under the same owner (Type.Field keeps its Type). This is
	// what separates a rename from a delete-plus-unrelated-add.
	confirmed := func(spec RenameSpec) bool {
		d, ok := after[spec.To]
		if !ok {
			return false
		}
		owner := ""
		if i := strings.IndexByte(spec.Sym, '.'); i >= 0 {
			owner = spec.Sym[:i+1]
		}
		return d.sym == owner+spec.To
	}

	if m := subjectRename.FindStringSubmatch(subject); m != nil && plausible(m[1], m[2]) {
		old, new_ := m[1], m[2]
		if d, ok := decls[old]; ok {
			spec := RenameSpec{Pkg: pkgPath(repo, sha, d.file), Sym: d.sym, To: new_}
			if confirmed(spec) {
				return []RenameSpec{spec}, ""
			}
		}
		// Family rename: the subject pair is a substring of the real
		// identifiers (Rename MaxEntries to MaxQuotas renames
		// ApiRateLimiterMaxEntries...).
		var specs []RenameSpec
		for member := range lost {
			renamed := strings.ReplaceAll(member, old, new_)
			if member == renamed || gained[renamed] == 0 {
				continue
			}
			if d, ok := decls[member]; ok {
				if spec := (RenameSpec{Pkg: pkgPath(repo, sha, d.file), Sym: d.sym, To: renamed}); confirmed(spec) {
					specs = append(specs, spec)
				}
			}
		}
		if len(specs) > 0 {
			return specs, ""
		}
		return nil, fmt.Sprintf("subject names %s->%s but no declaration found in diff", old, new_)
	}
	// No subject pair: greedy count-diff matching, best-affinity partner
	// first, each identifier used once. Handles commits that rename several
	// symbols at once without crossing their partners.
	type cand struct {
		lo, gn string
		n, lcp int
	}
	var cands []cand
	for lo, ln := range lost {
		for gn, gc := range gained {
			if ln == gc && ln >= 3 && plausible(lo, gn) && sharesToken(lo, gn) {
				cands = append(cands, cand{lo, gn, ln, lcp(lo, gn)})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].lcp != cands[j].lcp {
			return cands[i].lcp > cands[j].lcp
		}
		if cands[i].n != cands[j].n {
			return cands[i].n > cands[j].n
		}
		if cands[i].lo != cands[j].lo {
			return cands[i].lo < cands[j].lo
		}
		return cands[i].gn < cands[j].gn
	})
	usedLo, usedGn := map[string]bool{}, map[string]bool{}
	var specs []RenameSpec
	for _, c := range cands {
		if usedLo[c.lo] || usedGn[c.gn] {
			continue
		}
		d, ok := decls[c.lo]
		if !ok {
			continue
		}
		spec := RenameSpec{Pkg: pkgPath(repo, sha, d.file), Sym: d.sym, To: c.gn}
		if !confirmed(spec) {
			continue
		}
		usedLo[c.lo], usedGn[c.gn] = true, true
		specs = append(specs, spec)
	}
	if len(specs) > 0 {
		return specs, ""
	}
	return nil, "no rename pair extracted"
}

// ensureTargetsNamed appends the rename mapping to the prompt when the
// commit subject does not already name every pair. A real user states what
// the new name should be; recovering it from the ground-truth diff is not
// part of the task. Only bare names are given — finding the declarations
// and every reference stays the agent's work.
func ensureTargetsNamed(prompt string, specs []RenameSpec) string {
	complete := true
	for _, r := range specs {
		base := r.Sym[strings.LastIndexByte(r.Sym, '.')+1:]
		if !strings.Contains(prompt, base) || !strings.Contains(prompt, r.To) {
			complete = false
			break
		}
	}
	if complete {
		return prompt
	}
	var pairs []string
	for _, r := range specs {
		base := r.Sym[strings.LastIndexByte(r.Sym, '.')+1:]
		pairs = append(pairs, base+" -> "+r.To)
	}
	return prompt + "\n\nSpecifically, rename: " + strings.Join(pairs, "; ")
}

func lcp(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

type decl struct {
	file string
	sym  string // Name, Recv.Name, or Owner.Field
}

// parseDecls indexes every top-level declaration (and struct field and
// method) in the given files at rev.
func parseDecls(repo, rev string, files []string) map[string]decl {
	decls := map[string]decl{}
	put := func(file, name, sym string) {
		if _, exists := decls[name]; !exists {
			decls[name] = decl{file, sym}
		}
	}
	for _, f := range files {
		src := gitShow(repo, rev+":"+f)
		if src == "" {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range af.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				name := d.Name.Name
				if recv := recvType(d); recv != "" {
					put(f, name, recv+"."+name)
				} else {
					put(f, name, name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						put(f, spec.Name.Name, spec.Name.Name)
						if st, ok := spec.Type.(*ast.StructType); ok {
							for _, field := range st.Fields.List {
								for _, id := range field.Names {
									put(f, id.Name, spec.Name.Name+"."+id.Name)
								}
							}
						}
					case *ast.ValueSpec:
						for _, id := range spec.Names {
							put(f, id.Name, id.Name)
						}
					}
				}
			}
		}
	}
	return decls
}

func recvType(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	t := d.Recv.List[0].Type
	for {
		switch tt := t.(type) {
		case *ast.StarExpr:
			t = tt.X
		case *ast.IndexExpr: // generic receiver
			t = tt.X
		case *ast.IndexListExpr:
			t = tt.X
		case *ast.Ident:
			return tt.Name
		default:
			return ""
		}
	}
}

// identDiff counts identifier occurrences lost and gained across the commit.
// Parsing (rather than tokenizing text) keeps comments and strings out.
func identDiff(repo, sha string, files []string) (lost, gained map[string]int) {
	lost, gained = map[string]int{}, map[string]int{}
	for _, f := range files {
		old := identCounts(gitShow(repo, sha+"^:"+f), f)
		new_ := identCounts(gitShow(repo, sha+":"+f), f)
		for tok, n := range old {
			if d := n - new_[tok]; d > 0 {
				lost[tok] += d
			}
		}
		for tok, n := range new_ {
			if d := n - old[tok]; d > 0 {
				gained[tok] += d
			}
		}
	}
	return lost, gained
}

func identCounts(src, filename string) map[string]int {
	counts := map[string]int{}
	if src == "" {
		return counts
	}
	af, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.SkipObjectResolution)
	if err != nil {
		return counts
	}
	ast.Inspect(af, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			counts[id.Name]++
		}
		return true
	})
	return counts
}

var camel = regexp.MustCompile(`[A-Z]?[a-z0-9]+|[A-Z]+`)

func sharesToken(a, b string) bool {
	seen := map[string]bool{}
	for _, t := range camel.FindAllString(a, -1) {
		if len(t) >= 3 {
			seen[strings.ToLower(t)] = true
		}
	}
	for _, t := range camel.FindAllString(b, -1) {
		if seen[strings.ToLower(t)] {
			return true
		}
	}
	return false
}

func plausible(old, new_ string) bool {
	return old != "" && new_ != "" && !strings.EqualFold(old, new_) &&
		!token.IsKeyword(old) && !token.IsKeyword(new_) &&
		token.IsIdentifier(old) && token.IsIdentifier(new_) &&
		types.Universe.Lookup(old) == nil && types.Universe.Lookup(new_) == nil
}

func pkgPath(repo, sha, declFile string) string {
	mod := ""
	for line := range strings.SplitSeq(gitShow(repo, sha+"^:go.mod"), "\n") {
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			mod = strings.TrimSpace(rest)
			break
		}
	}
	if dir := path.Dir(declFile); dir != "." {
		return mod + "/" + dir
	}
	return mod
}

func changedGoFiles(repo, sha string) []string {
	out, _ := exec.Command("git", "-C", repo, "diff", "--name-only", sha+"^", sha).Output()
	var files []string
	for f := range strings.SplitSeq(string(out), "\n") {
		if strings.HasSuffix(f, ".go") {
			files = append(files, f)
		}
	}
	return files
}

func gitShow(repo, spec string) string {
	out, err := exec.Command("git", "-C", repo, "show", spec).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
