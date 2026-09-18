package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
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
	ids, err := parseFlags(fs, args)
	if err != nil {
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
	if len(ids) == 0 {
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

	return showReview(ctx, a, ids[0], *full)
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
	fmt.Printf("  merges into %s\n", project.TargetBranch)
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

	fmt.Printf("\n  approve: gravy approve %s  (merges into %s)\n", t.ID, project.TargetBranch)
	fmt.Printf("  reject:  gravy reject %s\n", t.ID)
	fmt.Printf("  worktree: %s\n", t.WorktreePath)
	return nil
}

// runApprove records approval and lands the work.
func runApprove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy approve", flag.ContinueOnError)
	noPush := fs.Bool("no-push", false, "squash onto the target branch but do not push it")
	handOff := fs.Bool("hand-off", false, "merge nothing; keep the branch and worktree for you")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gravy approve [flags] <ticket-id>")
		fmt.Fprintln(os.Stderr, "\nApproves reviewed work. With no flags it squashes onto the")
		fmt.Fprintln(os.Stderr, "target branch and pushes, which is what approving has always meant.")
		fmt.Fprintln(os.Stderr, "\nflags:")
		fs.PrintDefaults()
	}
	args, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(args) != 1 {
		fs.Usage()
		return fmt.Errorf("expected exactly one ticket")
	}
	if *noPush && *handOff {
		return fmt.Errorf("-no-push and -hand-off are different outcomes; pick one")
	}
	how := core.ApprovePush
	switch {
	case *handOff:
		how = core.ApproveHandOff
	case *noPush:
		how = core.ApproveLocal
	}
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	// Named before the merge, not only after it. Approval is the last point at which a wrong
	// target branch is cheap to notice, and "approving <id>" alone never showed one.
	if t, err := a.db.GetTicket(ctx, args[0]); err == nil {
		if p, err := a.db.GetProject(ctx, t.ProjectID); err == nil {
			switch how {
			case core.ApproveHandOff:
				fmt.Printf("approving %s — %s untouched, the branch stays yours\n", args[0], p.TargetBranch)
			case core.ApproveLocal:
				fmt.Printf("approving %s — merges into %s, no push\n", args[0], p.TargetBranch)
			default:
				fmt.Printf("approving %s — merges into %s\n", args[0], p.TargetBranch)
			}
		} else {
			fmt.Printf("approving %s\n", args[0])
		}
	} else {
		fmt.Printf("approving %s\n", args[0])
	}
	res, err := a.orch.Land().Approve(ctx, args[0], how)
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
	res, err := a.orch.Land().Continue(ctx, args[0], core.ApprovePush)
	return reportLanding(res, err)
}

func reportLanding(res agentrun.LandResult, err error) error {
	if err != nil {
		// Same rule as the review screen: a duplicate approval is not a failed one, and
		// exiting non-zero over a merge that succeeded is how a correct refusal becomes a
		// bug report — or a retry that does real damage in a script.
		if errors.Is(err, core.ErrAlreadyLanded) {
			fmt.Printf("  already landed; nothing to do\n")
			return nil
		}
		return err
	}
	switch res.State {
	case core.StateHandedOff:
		fmt.Printf("  %s is yours: nothing merged\n", res.TicketID)
		fmt.Println("  the branch is rebased onto the target and green against it,")
		fmt.Println("  and its worktree is kept so you can merge it however you like")
	case core.StateDone:
		fmt.Printf("  merged as %s", shortHash(res.MergeCommit))
		if res.Target != "" {
			fmt.Printf(" into %s", res.Target)
		}
		if res.Pushed {
			fmt.Print(", pushed")
		} else if res.Approval == core.ApproveLocal {
			fmt.Printf(", not pushed — `git -C . push origin %s` when you are ready", res.Target)
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

// runMarkMerged records that a human merged a handed-off ticket themselves.
func runMarkMerged(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: gravy done <ticket-id>")
	}
	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	state, err := a.svc.MarkMerged(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s is %s — anything waiting on it can start now\n", args[0], state)
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

// runChanges sends work back to the agent with feedback.
//
// The review loop had approve and reject but no way to ask for changes outside the TUI, so
// anything scripted could only accept or discard — and feedback longer than a line was awkward
// to type into a single-line prompt. Reading it from a file or stdin is how a considered review
// gets written.
func runChanges(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy changes", flag.ContinueOnError)
	file := fs.String("f", "", "read the feedback from a file, or - for stdin")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: gravy changes [-f file] <ticket-id> [feedback...]`)
		fmt.Fprintln(os.Stderr, "\nSends work back to the agent. Your words become its next prompt.")
		fmt.Fprintln(os.Stderr, "\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return fmt.Errorf("no ticket given")
	}

	id := rest[0]
	feedback := strings.TrimSpace(strings.Join(rest[1:], " "))
	if *file != "" {
		var (
			b   []byte
			err error
		)
		if *file == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(*file)
		}
		if err != nil {
			return fmt.Errorf("read feedback: %w", err)
		}
		feedback = strings.TrimSpace(string(b))
	}
	// Refused here as well as in the service: an agent asked to try again with no new
	// information will usually produce the same output, at the price of a whole run.
	if feedback == "" {
		return fmt.Errorf("say what needs to change")
	}

	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.svc.RequestChanges(ctx, id, feedback); err != nil {
		return err
	}
	fmt.Printf("%s is back in the queue with your feedback\n", id)
	return nil
}
