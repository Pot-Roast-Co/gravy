package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/review"
)

// WithReviewer enables the advisory review pass.
func (o *Orchestrator) WithReviewer(r *review.Reviewer) *Orchestrator {
	o.reviewer = r
	return o
}

// WithReview enables the review pass, resolving the review bucket for each review.
//
// Per review rather than once at startup: a project can pin its own review bucket, and a model
// chosen when the daemon booted would ignore that — silently, since the verdict it records looks
// the same whichever model produced it.
func (o *Orchestrator) WithReview(resolve RouteResolver, budget int) *Orchestrator {
	return o.WithReviewer(review.New(providerModel{orch: o, resolve: resolve}, budget))
}

// runReview records an advisory verdict on the run.
//
// Every failure here is swallowed on purpose. The review is advisory, so a review that cannot
// run must never stop work reaching a human — and the verdict it records in that case says so
// rather than looking like a clean pass.
func (o *Orchestrator) runReview(ctx context.Context, ticket core.Ticket, project core.Project,
	repo Repo, wt git.Worktree, loop loopResult) {

	if o.reviewer == nil || loop.runID == "" {
		return
	}

	diff, err := repo.Diff(ctx, wt, project.TargetBranch)
	if err != nil {
		o.recordVerdict(ctx, loop.runID, review.Verdict{
			Unavailable: "the diff could not be read: " + err.Error()})
		return
	}

	v := o.reviewer.Review(ctx, review.Request{
		Ticket: ticket, Project: project, Diff: diff, Validation: loop.validation,
	})
	o.recordVerdict(ctx, loop.runID, v)
}

func (o *Orchestrator) recordVerdict(ctx context.Context, runID string, v review.Verdict) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	run, err := o.store.GetRun(ctx, runID)
	if err != nil {
		return
	}
	run.Verdict = string(b)
	if err := o.store.UpdateRun(ctx, run); err != nil {
		o.log.Warn("could not record the review verdict", "run", runID, "error", err)
	}
}

// providerModel runs the review prompt through a registered agent CLI.
//
// It runs in a scratch directory, not the ticket's worktree. The reviewer is given the diff in
// its prompt and has no business touching the tree the work is in — and a read-only reviewer
// that cannot reach the repository cannot accidentally become a second author.
type providerModel struct {
	orch    *Orchestrator
	resolve RouteResolver
}

func (m providerModel) Complete(ctx context.Context, project core.Project, prompt string) (string, error) {
	if m.resolve == nil {
		return "", fmt.Errorf("no review model is configured")
	}
	// The project's own bucket table wins here exactly as it does for the run being reviewed.
	choice, err := m.resolve(ctx, core.RouteReview, core.Constraints{Routes: project.Routes})
	if err != nil {
		return "", fmt.Errorf("resolve the review bucket: %w", err)
	}
	p, ok := m.orch.providers[choice.ProviderID]
	if !ok {
		return "", fmt.Errorf("provider %q is not registered", choice.ProviderID)
	}
	h := m.orch.anyHost()
	if h == nil {
		return "", fmt.Errorf("no host available to run the review")
	}

	dir, err := os.MkdirTemp("", "gravy-review")
	if err != nil {
		return "", fmt.Errorf("review scratch directory: %w", err)
	}
	defer os.RemoveAll(dir)

	runID := m.orch.newID()
	handle, err := p.Run(ctx, h, provider.AgentTask{
		RunID:        runID,
		WorktreePath: dir,
		Prompt:       prompt,
		Model:        choice.Model,
		Timeout:      reviewTimeout,
		// One turn: the reviewer answers from the prompt. A reviewer that goes looking is a
		// reviewer that costs more than the review saves.
		MaxTurns:  1,
		LogPath:   filepath.Join(m.orch.cfg.RunsDir, runID, "review.log"),
		Allowlist: core.Allowlist{},
	})
	if err != nil {
		return "", fmt.Errorf("start the reviewer: %w", err)
	}

	// The answer arrives on the event stream; Outcome carries no text.
	//
	// Messages only. Collecting every event's text swept provider errors into the answer, so a
	// refused request came back as prose the parser then rejected — the human was told "the
	// review model's answer could not be read" when what actually happened was a 400 saying
	// the configured model does not exist.
	var b, problems strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range handle.Events() {
			switch {
			case ev.Kind == provider.EventMessage && ev.Text != "":
				b.WriteString(ev.Text)
				b.WriteString("\n")
			case ev.Kind == provider.EventError && ev.Text != "":
				problems.WriteString(strings.TrimSpace(ev.Text))
				problems.WriteString("; ")
			}
		}
	}()

	outcome, waitErr := handle.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	if waitErr != nil {
		return "", waitErr
	}
	if b.Len() == 0 {
		// Report what the provider said rather than the classification, which for a rejected
		// request is a generic task failure that explains nothing.
		if detail := strings.TrimSuffix(strings.TrimSpace(problems.String()), ";"); detail != "" {
			return "", fmt.Errorf("the reviewer failed: %s", detail)
		}
		return "", fmt.Errorf("the reviewer failed: %s", outcome.Note)
	}
	return b.String(), nil
}

// reviewTimeout bounds the advisory pass. It is short on purpose: a review that takes as long as
// the work delays the human it was meant to help.
const reviewTimeout = 3 * time.Minute

// Rereview runs the advisory pass again for a ticket already awaiting judgement.
//
// The pass normally runs once, inside the run that produced the work. That is right for the
// ordinary case and useless for the one where it failed for a reason since fixed — a misrouted
// model, a provider that was cooling down — because the verdict on the card stays broken with no
// way to ask again short of re-running the whole ticket.
//
// Unlike the automatic pass this reports its errors. It was asked for, so silence would read as
// a key that does nothing.
func (o *Orchestrator) Rereview(ctx context.Context, ticketID string) error {
	if o.reviewer == nil {
		return fmt.Errorf("no review model is configured")
	}

	ticket, err := o.store.GetTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	project, err := o.store.GetProject(ctx, ticket.ProjectID)
	if err != nil {
		return err
	}
	repo, err := o.repos.For(project)
	if err != nil {
		return err
	}

	runs, err := o.store.ListRunsForTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return fmt.Errorf("%s has no run to review", ticketID)
	}

	wt, ok, err := repo.OpenWorktree(ctx, ticket.Branch)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s has no worktree to read", ticketID)
	}

	diff, err := repo.Diff(ctx, wt, project.TargetBranch)
	if err != nil {
		return fmt.Errorf("the diff could not be read: %w", err)
	}

	v := o.reviewer.Review(ctx, review.Request{Ticket: ticket, Project: project, Diff: diff})
	o.recordVerdict(ctx, runs[0].ID, v)
	if v.Unavailable != "" {
		return fmt.Errorf("%s", v.Unavailable)
	}
	return nil
}
