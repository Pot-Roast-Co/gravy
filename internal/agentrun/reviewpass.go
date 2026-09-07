package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/git"
	"github.com/bobbybrady/gravy/internal/provider"
	"github.com/bobbybrady/gravy/internal/review"
)

// WithReviewer enables the advisory review pass.
func (o *Orchestrator) WithReviewer(r *review.Reviewer) *Orchestrator {
	o.reviewer = r
	return o
}

// WithReview enables the review pass using a registered provider and model.
func (o *Orchestrator) WithReview(providerID, model string, budget int) *Orchestrator {
	return o.WithReviewer(review.New(providerModel{orch: o, providerID: providerID, model: model}, budget))
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
	orch       *Orchestrator
	providerID string
	model      string
}

func (m providerModel) Complete(ctx context.Context, prompt string) (string, error) {
	p, ok := m.orch.providers[m.providerID]
	if !ok {
		return "", fmt.Errorf("provider %q is not registered", m.providerID)
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
		Model:        m.model,
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
	var b strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range handle.Events() {
			if ev.Text != "" {
				b.WriteString(ev.Text)
				b.WriteString("\n")
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
	if outcome.Class != provider.Success && b.Len() == 0 {
		return "", fmt.Errorf("the reviewer failed: %s", outcome.Note)
	}
	return b.String(), nil
}

// reviewTimeout bounds the advisory pass. It is short on purpose: a review that takes as long as
// the work delays the human it was meant to help.
const reviewTimeout = 3 * time.Minute
