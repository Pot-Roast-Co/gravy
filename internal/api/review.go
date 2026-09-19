package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
	"github.com/pot-roast-co/gravy/internal/review"
	"github.com/pot-roast-co/gravy/internal/store"
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
	// Verdict is the automated reviewer's advisory opinion. It annotates the diff for the
	// human and never decides the ticket's fate; nothing in this package reads it to make a
	// decision, and nothing should.
	Verdict review.Verdict
}

// Lander lands approved work.
//
// An interface so api says what it needs rather than depending on how landing works, and so a
// client can exist without one — a read-only status client has no business being able to merge.
type Lander interface {
	// Approve returns the state the ticket reached. A landing that parks — validation failed,
	// a merge conflict, a dirty checkout — is not an error, so an error alone cannot tell a
	// caller whether the work merged.
	Approve(ctx context.Context, ticketID string, how core.Approval) (core.State, error)
	// Continue retries a landing after a human has resolved a conflict in the preserved
	// worktree. It goes back through the gate, because the resolution changed the code that
	// was approved.
	// Continue retries a landing and, like Approve, returns the state it reached: a retry can
	// park again, and reporting that as success is the same lie one step later.
	Continue(ctx context.Context, ticketID string, how core.Approval) (core.State, error)
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
		// A verdict that cannot be read is reported as unavailable rather than as a pass:
		// looking like a clean review is the one way an advisory tool does real harm.
		if raw := strings.TrimSpace(rb.Run.Verdict); raw != "" {
			if err := json.Unmarshal([]byte(raw), &rb.Verdict); err != nil {
				rb.Verdict = review.Verdict{Unavailable: "the stored verdict could not be read"}
			}
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
	// A worktree that is no longer there is not an error worth failing the whole review over.
	//
	// Landing removes it, and a human resolving a conflict can remove it too. The record of it
	// is cleared first now, so this should not happen — but "should not" is not a guarantee
	// across two processes, and the cost of being wrong is a screen that says a finished
	// ticket could not be loaded. No worktree means no diff, which is the truth about work
	// that has already landed.
	if !h.FS().Exists(t.WorktreePath) {
		return rb, nil
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
func (l *Local) Approve(ctx context.Context, ticketID string, how core.Approval) (core.State, error) {
	if l.lander == nil {
		return "", fmt.Errorf("this client cannot land work")
	}
	state, err := l.lander.Approve(ctx, ticketID, how)
	if err != nil {
		return state, err
	}
	// The review checkout is a view of work that is now decided, so it goes. Left behind, one
	// accumulates per reviewed ticket and never gets cleaned up by anything.
	_ = l.DiscardReviewCheckout(ctx, ticketID)

	l.events.publish(Event{Kind: EventTicketChanged, TicketID: ticketID})
	l.events.publish(Event{Kind: EventAttentionChanged, TicketID: ticketID})
	return state, nil
}

// RequestChanges sends work back for another attempt, carrying the reviewer's note.
//
// The worktree is deliberately preserved: the agent picks up where it left off, and the note is
// the entire value of the retry.
//
// It serves both places a human sends work back from. Out of Review that is request_changes; out
// of Needs You — a parked run, a failed validation — it is requeue. The distinction is the state
// machine's, not the caller's: from the human's side both are "try again, and here is why".
//
// It is deliberately still the direct route. The discussion step (OpenDiscussion, SendChanges)
// is what the TUI offers, and it ends here — but a script that has always called this keeps
// working unchanged, and the preservation constraints agreed in earlier rounds still reach the
// prompt, because those live on the ticket rather than in whichever call sent the work back.
func (l *Local) RequestChanges(ctx context.Context, ticketID, feedback string) error {
	if strings.TrimSpace(feedback) == "" {
		return fmt.Errorf("request changes: say what needs to change")
	}
	return l.requestChanges(ctx, ticketID, feedback)
}

// requestChanges is the shared lifecycle: record the note, move the ticket, clear the queue.
func (l *Local) requestChanges(ctx context.Context, ticketID, feedback string) error {
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
func (l *Local) Continue(ctx context.Context, ticketID string, how core.Approval) (core.State, error) {
	if l.lander == nil {
		return "", fmt.Errorf("this client cannot land work")
	}
	state, err := l.lander.Continue(ctx, ticketID, how)
	if err != nil {
		return state, err
	}
	l.events.publish(Event{Kind: EventTicketChanged, TicketID: ticketID})
	l.events.publish(Event{Kind: EventAttentionChanged, TicketID: ticketID})
	return state, nil
}

// MarkMerged records that a human merged a handed-off ticket themselves.
//
// Gravy did not do the merge and cannot verify it, so this records what the human says rather
// than checking git. That is the bargain hand-off makes: the work left gravy's hands, and the
// person it went to is the one who knows where it ended up.
func (l *Local) MarkMerged(ctx context.Context, ticketID string) (core.State, error) {
	t, err := l.db.GetTicket(ctx, ticketID)
	if err != nil {
		return "", err
	}
	if t.State != core.StateHandedOff {
		return "", fmt.Errorf("ticket %s is %s, not handed off", ticketID, t.State)
	}
	state, err := l.db.SetTicketState(ctx, ticketID, core.EventLanded)
	if err != nil {
		return "", err
	}
	l.events.publish(Event{Kind: EventTicketChanged, TicketID: ticketID})
	return state, nil
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

	// Its review checkout goes with it, for the same reason.
	_ = l.DiscardReviewCheckout(ctx, ticketID)

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

// Rereviewer runs the advisory pass again on work already awaiting judgement.
type Rereviewer interface {
	Rereview(ctx context.Context, ticketID string) error
}

// WithRereviewer lets the service re-run the advisory review.
func (l *Local) WithRereviewer(r Rereviewer) *Local {
	l.rereviewer = r
	return l
}

// Rereview asks for a fresh advisory verdict.
//
// Unlike the pass that runs inside a run, this reports its failures: it was asked for, so
// silence would read as a key that does nothing.
func (l *Local) Rereview(ctx context.Context, ticketID string) error {
	if l.rereviewer == nil {
		return fmt.Errorf("this client cannot run reviews")
	}
	if err := l.rereviewer.Rereview(ctx, ticketID); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventRunChanged, TicketID: ticketID})
	return nil
}

// Checkouts makes a ticket's work readable in an editor.
//
// A capability rather than a method on the service, for the same reason landing is one: a client
// that may only read the queue has no business creating checkouts on the machine running the
// daemon.
type Checkouts interface {
	// ReviewCheckout returns a path showing the ticket's work as uncommitted changes.
	ReviewCheckout(ctx context.Context, ticketID string) (string, error)
	// DiscardReviewCheckout throws one away. Discarding one that is not there is not an error.
	DiscardReviewCheckout(ctx context.Context, ticketID string) error
}

// WithCheckouts gives the service the ability to open work in an editor.
func (l *Local) WithCheckouts(c Checkouts) *Local {
	l.checkouts = c
	return l
}

// ReviewCheckout prepares a ticket's work for reading in an editor, and returns where it is.
//
// The path is on the machine running the daemon. That is the same machine for a local host, and
// the caller is expected to check before launching an editor at it.
func (l *Local) ReviewCheckout(ctx context.Context, ticketID string) (string, error) {
	if l.checkouts == nil {
		return "", fmt.Errorf("this client cannot open work in an editor")
	}
	return l.checkouts.ReviewCheckout(ctx, ticketID)
}

// DiscardReviewCheckout throws away a ticket's review checkout.
func (l *Local) DiscardReviewCheckout(ctx context.Context, ticketID string) error {
	if l.checkouts == nil {
		return nil
	}
	return l.checkouts.DiscardReviewCheckout(ctx, ticketID)
}
