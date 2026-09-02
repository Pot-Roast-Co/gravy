// Command gravy is the single Gravy binary: TUI by default, plus `serve` and CLI subcommands.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
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

	// Ctrl-C stops the queue loop cleanly, letting in-flight runs finish rather than orphaning
	// agent processes and their children.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("gravy", version)
		return nil
	case "project":
		return runProject(ctx, args[1:])
	case "ticket":
		return runTicket(ctx, args[1:])
	case "run":
		return runQueue(ctx, args[1:])
	case "status":
		return runStatus(ctx, args[1:])
	case "try":
		// A smoke test for the pieces built so far: worktree, agent, diff. Not the product.
		return runTry(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	case "":
		// The TUI (GR-024) is the eventual default. Until it exists, say what does work
		// rather than failing with nothing to go on.
		usage()
		return nil
	case "serve":
		return fmt.Errorf("not implemented yet: the daemon is GR-007. use `gravy run` to work the queue in the foreground")
	default:
		return fmt.Errorf("unknown command %q — run `gravy help`", cmd)
	}
}

func usage() {
	fmt.Println(`gravy — an engineering manager for coding agents (pre-alpha)

  gravy project add <path>     register a repository
  gravy project list           what is registered
  gravy ticket add "<title>"   write a ticket and queue it
  gravy ticket list            what is in the queue
  gravy run                    work the queue: agents, validation, stop at review
  gravy run --once             work it until idle, then exit
  gravy status                 what needs you, and what is happening
  gravy try "<task>"           one-off smoke test in a throwaway repo

Nothing merges. Work stops at review and waits for you.

Not built yet: the daemon (work stops when you close the terminal), the review
and approve commands, and the TUI. See docs/MILESTONES.md.`)
}
