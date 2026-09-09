package agentrun

import (
	"context"
	"fmt"

	"github.com/pot-roast-co/gravy/internal/git"
)

// checkoutRepo is the extra git surface a review checkout needs beyond Repo.
//
// Separate, like landRepo, so the orchestrator's Repo interface stays the surface a run needs
// and a reviewer's convenience does not become a method every fake has to grow.
type checkoutRepo interface {
	ReviewCheckout(ctx context.Context, ticketID, commit, base string) (git.Worktree, error)
	DiscardReviewCheckout(ctx context.Context, ticketID string) error
}

// Checkouts makes a ticket's committed work readable in an editor.
type Checkouts struct{ orch *Orchestrator }

// Checkouts returns the checkout maker.
func (o *Orchestrator) Checkouts() *Checkouts { return &Checkouts{orch: o} }

// ReviewCheckout prepares a ticket's work as uncommitted changes and returns where it is.
func (c *Checkouts) ReviewCheckout(ctx context.Context, ticketID string) (string, error) {
	o := c.orch

	ticket, err := o.store.GetTicket(ctx, ticketID)
	if err != nil {
		return "", fmt.Errorf("review checkout: %w", err)
	}
	project, err := o.store.GetProject(ctx, ticket.ProjectID)
	if err != nil {
		return "", fmt.Errorf("review checkout: %w", err)
	}
	repo, err := o.repos.For(project)
	if err != nil {
		return "", fmt.Errorf("review checkout: %w", err)
	}
	co, ok := repo.(checkoutRepo)
	if !ok {
		return "", fmt.Errorf("review checkout: this repository does not support checkouts")
	}

	// The ticket's own branch, not HEAD: the checkout is created from the primary repository,
	// where HEAD is the target branch and has none of this work on it.
	if ticket.Branch == "" {
		return "", fmt.Errorf("review checkout: %s has no branch", ticketID)
	}

	// The commit under review and the state it was built on. The agent's work is exactly one
	// commit, which is the diff the review card shows too.
	wt, err := co.ReviewCheckout(ctx, ticketID, ticket.Branch, ticket.Branch+"~1")
	if err != nil {
		return "", err
	}
	return wt.Path, nil
}

// DiscardReviewCheckout throws a review checkout away.
func (c *Checkouts) DiscardReviewCheckout(ctx context.Context, ticketID string) error {
	o := c.orch

	ticket, err := o.store.GetTicket(ctx, ticketID)
	if err != nil {
		return nil // nothing to discard for a ticket that is gone
	}
	project, err := o.store.GetProject(ctx, ticket.ProjectID)
	if err != nil {
		return nil
	}
	repo, err := o.repos.For(project)
	if err != nil {
		return nil
	}
	co, ok := repo.(checkoutRepo)
	if !ok {
		return nil
	}
	return co.DiscardReviewCheckout(ctx, ticketID)
}
