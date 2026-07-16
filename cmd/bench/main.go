// Command bench prepares, runs, and reports the raw-vs-semantic agent
// benchmark.
//
//	bench prep-rename -scratch <dir with repo clones> [-tasks f] [-out f]
//	bench report [-curve out.csv] <run dir>...
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: bench <prep-rename|report> [flags]")
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	scratch := fs.String("scratch", "", "directory containing the bench repo clones")
	tasks := fs.String("tasks", "bench/tasks.json", "validated task list")
	out := fs.String("out", "bench/tasks-rename.json", "output manifest")
	curve := fs.String("curve", "", "write the completion-time curve CSV here")
	mlflow := fs.String("mlflow", "", "MLflow tracking server URI for export")
	fs.Parse(args)

	switch cmd {
	case "prep-rename":
		if *scratch == "" {
			fail("prep-rename requires -scratch")
		}
		if err := prepRename(*scratch, *tasks, *out); err != nil {
			fail("prep-rename: %v", err)
		}
	case "prep-addparam":
		if *scratch == "" {
			fail("prep-addparam requires -scratch")
		}
		if *out == "bench/tasks-rename.json" {
			*out = "bench/tasks-addparam.json"
		}
		if err := prepAddParam(*scratch, *tasks, *out); err != nil {
			fail("prep-addparam: %v", err)
		}
	case "prep-move":
		if *scratch == "" {
			fail("prep-move requires -scratch")
		}
		if *out == "bench/tasks-rename.json" {
			*out = "bench/tasks-move.json"
		}
		if err := prepMove(*scratch, *tasks, *out); err != nil {
			fail("prep-move: %v", err)
		}
	case "mine":
		if *scratch == "" || fs.NArg() == 0 {
			fail("mine requires -scratch and a repo name: bench mine -scratch <dir> <repo>")
		}
		repo := fs.Arg(0)
		if *out == "bench/tasks-rename.json" {
			*out = "bench/candidates-" + repo + ".json"
		}
		if err := mine(*scratch+"/"+repo, repo, *out, 2, 40); err != nil {
			fail("mine: %v", err)
		}
	case "export":
		if *mlflow == "" || fs.NArg() == 0 {
			fail("export requires -mlflow <uri> and at least one run dir")
		}
		if err := exportMLflow(*mlflow, fs.Args()); err != nil {
			fail("export: %v", err)
		}
	case "report":
		if fs.NArg() == 0 {
			fail("report requires at least one run dir")
		}
		if err := report(fs.Args(), *curve); err != nil {
			fail("report: %v", err)
		}
	default:
		fail("unknown command %q", cmd)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
