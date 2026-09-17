package agentrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/validate"
)

// Lander carries approved work onto the target branch.
//
// Approval starts landing; it does not guarantee it. The target may have moved since the work
// was reviewed, and green-against-yesterday is not evidence about today.
type Lander struct {
	orch *Orchestrator
}

// Land returns the lander.
func (o *Orchestrator) Land() *Lander { return &Lander{orch: o} }

// LandResult reports what happened.
type LandResult struct {
	TicketID string
	// State is the ticket's state afterwards: Done, or NeedsYou when a human is required.
	State core.State
	// Target is the branch the work merged into, carried so callers can say where it went.
	// A landing that reports only "merged" is indistinguishable from one that merged
	// somewhere nobody meant, which is a failure that reports success.
	Target string
	// MergeCommit is the squash commit on the target branch.
	MergeCommit string
	Pushed      bool
	// Revalidated reports whether configured validation was run before merging.
	Revalidated bool
	// ConflictFiles is populated when the rebase could not be replayed.
	ConflictFiles []string
	// Validation holds re-validation results, when it ran.
	Validation validate.Results
}

// Approve records human approval and lands the work.
//
// Nothing reaches a target branch except through here, and this is only reachable from Review by
// an explicit human action. There is no configuration flag that skips it.
func (l *Lander) Approve(ctx context.Context, ticketID string) (LandResult, error) {
	o := l.orch
	res := LandResult{TicketID: ticketID}

	ticket, err := o.store.GetTicket(ctx, ticketID)
	if err != nil {
		return res, fmt.Errorf("land: %w", err)
	}
	if ticket.State != core.StateReview {
		return res, fmt.Errorf("land: ticket %s is %s, not awaiting review", ticketID, ticket.State)
	}
	project, err := o.store.GetProject(ctx, ticket.ProjectID)
	if err != nil {
		return res, fmt.Errorf("land: %w", err)
	}
	repo, err := o.repos.For(project)
	if err != nil {
		return res, fmt.Errorf("land: %w", err)
	}
	// Validation before landing runs in the worktree, so it runs on the machine that has the
	// worktree. Picking any registered host here once sent "mix deps.get" over ssh to a laptop
	// that was closed, and the approval came back as a validation failure.
	h := repo.Host()

	// Review -> Landing. The transition table permits this edge only from Review, which is the
	// approval gate expressed structurally rather than as a check that could be forgotten.
	if _, err := o.store.SetTicketState(ctx, ticketID, core.EventApprove); err != nil {
		return res, fmt.Errorf("land: %w", err)
	}

	// The human has just given the judgement the queue was waiting for. Any row still open for
	// this ticket — review_pending, or the merge_conflict that Continue came back from — is
	// answered by that approval; a fresh one is opened if landing parks the ticket again.
	if _, err := o.store.ResolveAttentionForTicket(ctx, ticketID); err != nil {
		return res, fmt.Errorf("land: %w", err)
	}

	wt := git.Worktree{Path: ticket.WorktreePath, Branch: ticket.Branch, Base: project.TargetBranch}
	res.Target = project.TargetBranch

	state, err := l.land(ctx, &res, ticket, project, repo, wt, h)
	res.State = state
	return res, err
}

// Continue retries landing after a human has resolved a conflict in the worktree.
func (l *Lander) Continue(ctx context.Context, ticketID string) (LandResult, error) {
	o := l.orch
	res := LandResult{TicketID: ticketID}

	ticket, err := o.store.GetTicket(ctx, ticketID)
	if err != nil {
		return res, fmt.Errorf("land: %w", err)
	}
	if ticket.State != core.StateNeedsYou {
		return res, fmt.Errorf("land: ticket %s is %s, not parked for your attention", ticketID, ticket.State)
	}
	// Back to Review, then approve again: a resolved conflict still goes through the gate,
	// because the human resolving it has changed the code that was approved.
	if _, err := o.store.SetTicketState(ctx, ticketID, core.EventReturnToReview); err != nil {
		return res, fmt.Errorf("land: %w", err)
	}
	return l.Approve(ctx, ticketID)
}

// land performs fetch, rebase, validation, squash-merge and cleanup.
func (l *Lander) land(ctx context.Context, res *LandResult, ticket core.Ticket, project core.Project, repo Repo, wt git.Worktree, h host.Host) (core.State, error) {
	o := l.orch

	lander, ok := repo.(landRepo)
	if !ok {
		return "", fmt.Errorf("land: repository does not support landing")
	}

	if err := repo.Fetch(ctx); err != nil {
		return l.park(ctx, ticket, core.ReasonMergeConflict, map[string]any{
			"error": fmt.Sprintf("fetch failed: %v", err),
		})
	}

	// Nothing lands out of a dirty worktree. Git will not rebase one, so without this the
	// approval fails several steps later wearing the wrong name — and the human is sent
	// looking for a merge conflict on a branch that has none.
	//
	// It is a real case rather than a theoretical one: the review screen opens a shell in this
	// worktree on purpose, so running the test suite there is expected, and a build that
	// writes a tracked file leaves exactly this state behind.
	if dirt, derr := lander.DirtyFiles(ctx, wt); derr == nil && len(dirt) > 0 {
		return l.park(ctx, ticket, core.ReasonValidationFailed, map[string]any{
			"reason":   "the worktree has uncommitted changes, so nothing can be rebased",
			"files":    dirt,
			"worktree": wt.Path,
		})
	}

	target, err := repo.TargetRef(ctx, project.TargetBranch)
	if err != nil {
		return "", fmt.Errorf("land: %w", err)
	}

	rebase, err := lander.Rebase(ctx, wt, target)
	if err != nil {
		return "", fmt.Errorf("land: %w", err)
	}
	if !rebase.Clean {
		// Gravy attempts nothing further. No resolution, no retry loop, no three-way
		// cleverness: the worktree is preserved exactly as the human needs to find it, and
		// the conflicting paths are recorded so they do not have to go looking.
		res.ConflictFiles = rebase.ConflictFiles

		payload := map[string]any{
			"files":    rebase.ConflictFiles,
			"worktree": wt.Path,
			"target":   target,
		}
		if rebase.Detail != "" {
			payload["detail"] = rebase.Detail
		}
		// A refusal is not a conflict, and calling it one sends the human looking for
		// overlapping edits that do not exist. What it usually means is something left in the
		// worktree — commonly by whoever last opened a shell there to run the tests.
		if rebase.Refused {
			payload["reason"] = "git would not start the rebase"
			return l.park(ctx, ticket, core.ReasonValidationFailed, payload)
		}
		return l.park(ctx, ticket, core.ReasonMergeConflict, payload)
	}

	// Validate every landing attempt. A no-op rebase can follow a failed validation
	// or a human resolution, so it is not evidence that this tree is green.
	if len(project.Validation) > 0 {
		res.Revalidated = true
		results, err := o.newValidator(o.newID()).Run(ctx, h, wt.Path, project.Validation)
		res.Validation = results
		if err != nil {
			return "", fmt.Errorf("land: re-validation: %w", err)
		}
		if !results.Green() {
			return l.park(ctx, ticket, core.ReasonValidationFailed, map[string]any{
				"stage":   "validation before landing",
				"summary": results.Summary(),
			})
		}
	}

	message := fmt.Sprintf("%s\n\nLanded by gravy from %s.", ticket.Title, wt.Branch)
	merged, err := lander.SquashMerge(ctx, wt, project.TargetBranch, message)
	if err != nil {
		// Same refusal-is-not-a-conflict rule as the rebase above, one step later. The squash
		// happens in the main checkout, so that is where the uncommitted changes are — and a
		// human sent to the preserved worktree to resolve a conflict would find a clean tree,
		// no conflicting files, and nothing to do.
		var dirty *git.DirtyCheckoutError
		if errors.As(err, &dirty) {
			return l.park(ctx, ticket, core.ReasonCheckoutDirty, map[string]any{
				"reason":   "the main checkout has uncommitted changes, which a squash would carry into the merge",
				"files":    dirty.Files,
				"checkout": dirty.Checkout,
				"branch":   wt.Branch,
			})
		}
		// A push rejection leaves the local target ahead of the remote; the ticket parks so
		// a human can look rather than Gravy retrying into a worse state.
		return l.park(ctx, ticket, core.ReasonMergeConflict, map[string]any{
			"error":  err.Error(),
			"stage":  "merge and push",
			"branch": wt.Branch,
		})
	}
	res.MergeCommit, res.Pushed = merged.MergeCommit, merged.Pushed

	// Done, then clean up. The order matters: a failure to remove a worktree must not undo a
	// merge that already happened.
	state, err := o.store.SetTicketState(ctx, ticket.ID, core.EventLanded)
	if err != nil {
		return "", fmt.Errorf("land: %w", err)
	}

	// Cleanup failures are logged, never fatal: the work has merged, and refusing to call the
	// ticket Done because a directory could not be removed would be the wrong trade.
	if err := repo.RemoveWorktree(ctx, wt); err != nil {
		o.log.Warn("could not remove worktree after landing", "ticket", ticket.ID, "error", err)
	} else if err := lander.DeleteBranch(ctx, wt.Branch); err != nil {
		o.log.Warn("could not delete branch after landing", "ticket", ticket.ID, "error", err)
	}

	if err := o.updateTicketFields(ctx, ticket.ID, func(t *core.Ticket) {
		t.WorktreePath = ""
	}); err != nil {
		return state, fmt.Errorf("land: %w", err)
	}
	return state, nil
}

// park moves a landing ticket to Needs You with a reason.
func (l *Lander) park(ctx context.Context, ticket core.Ticket, reason core.AttentionReason, payload map[string]any) (core.State, error) {
	o := l.orch
	state, err := o.store.SetTicketState(ctx, ticket.ID, core.EventLandFailed)
	if err != nil {
		return "", fmt.Errorf("land: park %s: %w", ticket.ID, err)
	}
	if err := o.raiseAttention(ctx, core.Attention{
		ID:        o.newID(),
		ProjectID: ticket.ProjectID,
		TicketID:  ticket.ID,
		Reason:    reason,
		Payload:   payload,
		CreatedAt: time.Now(),
	}, ticket.Title); err != nil {
		return state, fmt.Errorf("land: open attention: %w", err)
	}
	return state, nil
}

// landRepo is the extra git surface landing needs beyond Repo.
type landRepo interface {
	Rebase(ctx context.Context, w git.Worktree, onto string) (git.RebaseResult, error)
	// DirtyFiles lists uncommitted changes, checked before a rebase that would refuse them.
	DirtyFiles(ctx context.Context, w git.Worktree) ([]string, error)
	SquashMerge(ctx context.Context, w git.Worktree, target, message string) (git.LandResult, error)
	DeleteBranch(ctx context.Context, branch string) error
}
