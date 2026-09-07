package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/git"
	"github.com/bobbybrady/gravy/internal/store"
)

// ReviewBundle is everything the Review screen shows about one ticket, assembled in one read.
//
// It is one call rather than five because a reviewer is deciding on the strength of what is in
// front of them: a diff from one moment paired with validation results from another is not
// evidence about the same thing.
type ReviewBundle struct {
	Ticket      core.Ticket
	Project     core.Project
	Run         core.Run
	Validations []store.Validation
	Diff        git.Diff
	Summary     core.Summary
	// Verdict is the automated review's advisory note, empty until GR-020 lands. It annotates
	// the diff for the human and never decides the ticket's fate.
	Verdict string
}

// Lander lands approved work.
//
// An interface so api says what it needs rather than depending on how landing works, and so a
// client can exist without one — a read-only status client has no business being able to merge.
type Lander interface {
	Approve(ctx context.Context, ticketID string) error
	// Continue retries a landing after a human has resolved a conflict in the preserved
	// worktree. It goes back through the gate, because the resolution changed the code that
	// was approved.
	Continue(ctx context.Context, ticketID string) error
}

// GetReview assembles the evidence for one ticket awaiting judgement.
func (l *Local) GetReview(ctx context.Context, ticketID string) (ReviewBundle, error) {
	var rb ReviewBundle

	t, err := l.db.GetTicket(ctx, ticketID)
	if err != nil {
		return rb, err
	}
	p, err := l.db.GetProject(ctx, t.ProjectID)
	if err != nil {
		return rb, err
	}
	rb.Ticket, rb.Project = t, p

	runs, err := l.db.ListRunsForTicket(ctx, ticketID)
	if err != nil {
		return rb, err
	}
	if len(runs) > 0 {
		rb.Run = runs[0]
		if rb.Validations, err = l.db.ListValidations(ctx, rb.Run.ID); err != nil {
			return rb, err
		}
	}

	// A ticket with no summary yet is ordinary — GR-019 generates them — so its absence is not
	// an error the reviewer should be shown.
	if s, err := l.db.GetSummary(ctx, ticketID); err == nil {
		rb.Summary = s
	} else if !errors.Is(err, store.ErrNotFound) {
		return rb, err
	}

	// The diff is the evidence. Everything else on this screen describes it.
	if t.WorktreePath == "" {
		return rb, nil
	}
	h := l.aHost()
	if h == nil {
		return rb, fmt.Errorf("no host available to read %s", t.WorktreePath)
	}
	repo := git.NewLocalRepo(h, p.RepoPath, "")
	wt := git.Worktree{Path: t.WorktreePath, Branch: t.Branch, Base: p.TargetBranch}
	if rb.Diff, err = repo.Diff(ctx, wt, p.TargetBranch); err != nil {
		return rb, fmt.Errorf("diff %s: %w", ticketID, err)
	}
	return rb, nil
}

// Approve records human approval and lands the work.
//
// It delegates to the lander, which owns the gate. There is no path to a target branch that does
// not pass through here, and no configuration that skips it.
func (l *Local) Approve(ctx context.Context, ticketID string) error {
	if l.lander == nil {
		return fmt.Errorf("this client cannot land work")
	}
	if err := l.lander.Approve(ctx, ticketID); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventTicketChanged, TicketID: ticketID})
	l.events.publish(Event{Kind: EventAttentionChanged, TicketID: ticketID})
	return nil
}

// RequestChanges sends work back for another attempt, carrying the reviewer's note.
//
// The worktree is deliberately preserved: the agent picks up where it left off, and the note is
// the entire value of the retry.
//
// It serves both places a human sends work back from. Out of Review that is request_changes; out
// of Needs You — a parked run, a failed validation — it is requeue. The distinction is the state
// machine's, not the caller's: from the human's side both are "try again, and here is why".
func (l *Local) RequestChanges(ctx context.Context, ticketID, feedback string) error {
	if strings.TrimSpace(feedback) == "" {
		return fmt.Errorf("request changes: say what needs to change")
	}
	t, err := l.db.GetTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	t.Feedback = feedback
	if err := l.db.UpdateTicket(ctx, t); err != nil {
		return err
	}

	ev := core.EventRequestChanges
	if t.State == core.StateNeedsYou {
		ev = core.EventRequeue
	}
	if _, err := l.db.SetTicketState(ctx, ticketID, ev); err != nil {
		return err
	}
	if _, err := l.db.ResolveAttentionForTicket(ctx, ticketID); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventTicketChanged, TicketID: ticketID, State: core.StateReady})
	l.events.publish(Event{Kind: EventAttentionChanged, TicketID: ticketID})
	return nil
}

// Continue retries a landing after a human has resolved a conflict.
func (l *Local) Continue(ctx context.Context, ticketID string) error {
	if l.lander == nil {
		return fmt.Errorf("this client cannot land work")
	}
	if err := l.lander.Continue(ctx, ticketID); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventTicketChanged, TicketID: ticketID})
	l.events.publish(Event{Kind: EventAttentionChanged, TicketID: ticketID})
	return nil
}

// Reject abandons a ticket and removes its worktree.
func (l *Local) Reject(ctx context.Context, ticketID string) error {
	t, err := l.db.GetTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	state, err := l.db.SetTicketState(ctx, ticketID, core.EventReject)
	if err != nil {
		return err
	}
	if _, err := l.db.ResolveAttentionForTicket(ctx, ticketID); err != nil {
		return err
	}

	// A rejected ticket's worktree is dead weight on disk. Failing to remove it must not fail
	// the rejection, which has already been recorded — report it and move on.
	if t.WorktreePath != "" {
		if p, perr := l.db.GetProject(ctx, t.ProjectID); perr == nil {
			if h := l.aHost(); h != nil {
				repo := git.NewLocalRepo(h, p.RepoPath, "")
				wt := git.Worktree{Path: t.WorktreePath, Branch: t.Branch, Base: p.TargetBranch}
				err = repo.RemoveWorktree(ctx, wt)
			}
		}
	}

	l.events.publish(Event{Kind: EventTicketChanged, TicketID: ticketID, State: state})
	l.events.publish(Event{Kind: EventAttentionChanged, TicketID: ticketID})
	if err != nil {
		return fmt.Errorf("ticket %s was rejected, but its worktree could not be removed: %w", ticketID, err)
	}
	return nil
}
