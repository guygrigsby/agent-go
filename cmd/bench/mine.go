package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Subject classifiers, deliberately conservative: the prep extractors and
// the oracle replay validate every candidate downstream, so a false
// positive costs one review note, but a loose regex floods the list.
var (
	kindRename   = regexp.MustCompile(`(?i)\brenam`)
	kindAddParam = regexp.MustCompile(`(?i)\badd\w*\b.{0,40}\b(?:param(?:eter)?|argument)\b|(?i)\bpass\b.{0,40}\bcontext\b`)
	kindMove     = regexp.MustCompile(`(?i)\bmov(?:e[ds]?|ing)\b.{0,60}\b(?:to|into)\b`)
)

func classifyKinds(subject string) []string {
	var kinds []string
	if kindRename.MatchString(subject) {
		kinds = append(kinds, "rename")
	}
	if kindAddParam.MatchString(subject) {
		kinds = append(kinds, "add-param")
	}
	if kindMove.MatchString(subject) {
		kinds = append(kinds, "move")
	}
	return kinds
}

// mineTasks scans a clone's history for refactor-shaped commits and emits
// candidate Task entries. minGo/maxGo bound the changed-Go-file count:
// below is trivial, above is a monster no cap survives.
func mineTasks(repoDir, repoName string, minGo, maxGo int) ([]Task, error) {
	out, err := exec.Command("git", "-C", repoDir, "log", "--no-merges",
		"--format=%H%x00%s").Output()
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		sha, subject, ok := strings.Cut(line, "\x00")
		if !ok {
			continue
		}
		kinds := classifyKinds(subject)
		if len(kinds) == 0 {
			continue
		}
		n := changedGoFileCount(repoDir, sha)
		if n < minGo || n > maxGo {
			continue
		}
		tasks = append(tasks, Task{Repo: repoName, SHA: sha, Subject: subject,
			Kinds: kinds, GoFiles: n})
	}
	return tasks, nil
}

func changedGoFileCount(repoDir, sha string) int {
	out, err := exec.Command("git", "-C", repoDir, "diff-tree",
		"--no-commit-id", "--name-only", "-r", sha).Output()
	if err != nil {
		return 0
	}
	n := 0
	for f := range strings.SplitSeq(string(out), "\n") {
		if strings.HasSuffix(f, ".go") {
			n++
		}
	}
	return n
}

func mine(repoDir, repoName, outFile string, minGo, maxGo int) error {
	tasks, err := mineTasks(repoDir, repoName, minGo, maxGo)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(tasks, "", " ")
	if err := os.WriteFile(outFile, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d candidates -> %s\n", len(tasks), outFile)
	return nil
}
