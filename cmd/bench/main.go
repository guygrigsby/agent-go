// Command bench prepares and runs the raw-vs-semantic agent benchmark.
//
//	bench prep-rename -scratch <dir with repo clones> [-tasks f] [-out f]
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: bench <prep-rename> [flags]")
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	scratch := fs.String("scratch", "", "directory containing the bench repo clones")
	tasks := fs.String("tasks", "bench/tasks.json", "validated task list")
	out := fs.String("out", "bench/tasks-rename.json", "output manifest")
	fs.Parse(args)

	switch cmd {
	case "prep-rename":
		if *scratch == "" {
			fail("prep-rename requires -scratch")
		}
		if err := prepRename(*scratch, *tasks, *out); err != nil {
			fail("prep-rename: %v", err)
		}
	default:
		fail("unknown command %q", cmd)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
