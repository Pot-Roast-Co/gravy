package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bobbybrady/gravy/internal/agentrun"
	"github.com/bobbybrady/gravy/internal/api"
	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/git"
)

// runReview shows what is waiting for judgement.
func runReview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy review", flag.ContinueOnError)
	full := fs.Bool("diff", false, "print the full patch rather than a summary")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gravy review [flags] [<ticket-id>]")
		fmt.Fprintln(os.Stderr, "\nShows work awaiting your approval. With no id, lists everything pending.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	pending, err := a.svc.ListTickets(ctx, api.TicketFilter{State: core.StateReview})
	if err != nil {
		return err
	}
	if fs.NArg() == 0 {
		if len(pending) == 0 {
			fmt.Println("nothing awaiting review")
			return nil
		}
		fmt.Printf("%d ticket(s) awaiting your review:\n\n", len(pending))
		for _, t := range pending {
			fmt.Printf("  %s  %s\n", t.ID, t.Title)
		}
		fmt.Println("\ninspect one with: gravy review <ticket-id>")
		return nil
	}

	return showReview(ctx, a, fs.Arg(0), *full)
}

func showReview(ctx context.Context, a *app, ticketID string, full bool) error {
	t, err := a.db.GetTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	project, err := a.db.GetProject(ctx, t.ProjectID)
	if err != nil {
		return err
	}

	fmt.Printf("%s  %s\n", t.ID, t.Title)
	fmt.Printf("  project %s, branch %s, state %s\n", project.Slug, t.Branch, t.State)
	if strings.TrimSpace(t.Body) != "" {
		fmt.Printf("\n  %s\n", strings.ReplaceAll(strings.TrimSpace(t.Body), "\n", "\n  "))
	}

	runs, err := a.svc.ListRuns(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(runs) > 0 {
		r := runs[0]
		fmt.Printf("\n  run %s via %s/%s: %s, %d turns", shortHash(r.ID), r.ProviderID, r.Model, r.FailureClass, r.Turns)
		if r.CostUSD != nil {
			fmt.Printf(", $%.4f", *r.CostUSD)
		}
		fmt.Println()
		vals, err := a.db.ListValidations(ctx, r.ID)
		if err != nil {
			return err
		}
		for _, v := range vals {
			status := "passed"
			if v.ExitCode != 0 {
				status = fmt.Sprintf("FAILED (exit %d)", v.ExitCode)
			}
			fmt.Printf("  validation %-10s %s\n", v.Step, status)
		}
	}

	// The diff is the evidence. The summary a human approves is generated from it, never from
	// the agent's own account of what it did.
	if t.WorktreePath == "" {
		fmt.Println("\n  (no worktree; nothing to diff)")
		return nil
	}
	repo := git.NewLocalRepo(a.host, project.RepoPath, "")
	wt := git.Worktree{Path: t.WorktreePath, Branch: t.Branch, Base: project.TargetBranch}
	diff, err := repo.Diff(ctx, wt, project.TargetBranch)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}

	adds, dels := diff.Totals()
	fmt.Printf("\n  %d file(s), +%d -%d\n", len(diff.Files), adds, dels)
	for _, f := range diff.Files {
		fmt.Printf("    %-9s %-44s +%d -%d\n", f.Status, f.Path, f.Additions, f.Deletions)
	}
	if full {
		fmt.Println()
		for _, f := range diff.Files {
			fmt.Println(f.Patch)
		}
	} else if len(diff.Files) > 0 {
		fmt.Println("\n  full patch: gravy review -diff " + t.ID)
	}

	fmt.Printf("\n  approve: gravy approve %s\n", t.ID)
	fmt.Printf("  reject:  gravy reject %s\n", t.ID)
	fmt.Printf("  worktree: %s\n", t.WorktreePath)
	return nil
}

// runApprove records approval and lands the work.
func runApprove(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: gravy approve <ticket-id>")
	}
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	fmt.Printf("approving %s\n", args[0])
	res, err := a.orch.Land().Approve(ctx, args[0])
	return reportLanding(res, err)
}

// runContinue retries a landing after a human has resolved a conflict.
func runContinue(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: gravy continue <ticket-id>")
	}
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	fmt.Printf("retrying the landing of %s\n", args[0])
	res, err := a.orch.Land().Continue(ctx, args[0])
	return reportLanding(res, err)
}

func reportLanding(res agentrun.LandResult, err error) error {
	if err != nil {
		return err
	}
	switch res.State {
	case core.StateDone:
		fmt.Printf("  merged as %s", shortHash(res.MergeCommit))
		if res.Pushed {
			fmt.Print(", pushed")
		}
		fmt.Println()
		if res.Revalidated {
			fmt.Println("  re-validated: the target had moved, so green-before was not evidence")
		} else {
			fmt.Println("  no re-validation needed: the branch was already on top of target")
		}
		fmt.Printf("  %s is done\n", res.TicketID)
	case core.StateNeedsYou:
		fmt.Printf("  stopped: %s needs you\n", res.TicketID)
		if len(res.ConflictFiles) > 0 {
			fmt.Println("  conflicting files:")
			for _, f := range res.ConflictFiles {
				fmt.Printf("    %s\n", f)
			}
			fmt.Println("\n  the worktree is preserved and usable. resolve it there, then:")
			fmt.Printf("    gravy continue %s\n", res.TicketID)
		}
		if !res.Validation.Green() {
			fmt.Println("  re-validation failed after the target moved:")
			fmt.Printf("    %s\n", strings.ReplaceAll(res.Validation.Summary(), "\n", "\n    "))
		}
	default:
		fmt.Printf("  ended in %s\n", res.State)
	}
	return nil
}

// runReject abandons a ticket.
func runReject(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: gravy reject <ticket-id>")
	}
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	state, err := a.svc.MoveTicket(ctx, args[0], core.EventReject)
	if err != nil {
		return err
	}
	fmt.Printf("%s is %s\n", args[0], state)
	return nil
}
