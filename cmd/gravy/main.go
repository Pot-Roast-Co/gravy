// Command gravy is the single Gravy binary: TUI by default, plus `serve` and CLI subcommands.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gravy:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("gravy", version)
		return nil
	case "try":
		// A smoke test for the pieces built so far: worktree, agent, diff. Not the product.
		return runTry(args[1:])
	case "", "serve":
		// The TUI (GR-024) and the daemon (GR-018) do not exist yet. Until they do, the
		// binary exists to prove the build, not to do anything.
		return fmt.Errorf("not implemented yet: gravy is pre-alpha (see docs/MILESTONES.md)")
	default:
		return fmt.Errorf("unknown command %q (try: version, try)", cmd)
	}
}
