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
	case "review":
		return runReview(ctx, args[1:])
	case "approve":
		return runApprove(ctx, args[1:])
	case "continue":
		return runContinue(ctx, args[1:])
	case "changes":
		return runChanges(ctx, args[1:])
	case "reject":
		return runReject(ctx, args[1:])
	case "try":
		// A smoke test for the pieces built so far: worktree, agent, diff. Not the product.
		return runTry(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	case "":
		return runTUI(ctx)
	case "serve":
		return runServe(ctx, args[1:])
	case "stop":
		return runStop(args[1:])
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
  gravy review [<id>]          what is awaiting your judgement, and its diff
  gravy approve <id>           approve, rebase, squash-merge, push
  gravy reject <id>            abandon a ticket
  gravy continue <id>          retry a landing after you resolved a conflict
  gravy try "<task>"           one-off smoke test in a throwaway repo

Nothing merges without "gravy approve". No flag changes that.

Run gravy with no arguments for the dashboard.

  gravy serve                  run the daemon in the foreground
  gravy stop                   stop the running daemon

The daemon owns the queue. Running gravy starts one if none is running, and it
outlives the terminal that started it.

Not built yet: the Backlog, Ready, Running and Needs You screens.
See docs/MILESTONES.md.`)
}
