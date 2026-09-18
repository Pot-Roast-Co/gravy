package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// TestParkedLandingDoesNotReportSuccess is the regression.
//
// Landing re-validates before it merges, and a failed re-validation parks the ticket in Needs You
// rather than merging. Parking is not an error, so Approve returned nil and the screen said
// "approved and landed" over work that never left the worktree. It happened for real: `make check`
// failed against a target that had moved, the ticket parked with validation_failed, and the only
// thing that said so was a row on another screen.
func TestParkedLandingDoesNotReportSuccess(t *testing.T) {
	r, _, ctx := landingReview(t)
	r.mode = reviewLanding

	screen, _ := r.Update(reviewActedMsg{
		verb:  "approved and landed",
		state: core.StateNeedsYou,
	}, ctx)
	rv := screen.(*review)

	if strings.Contains(rv.notice, "approved and landed") {
		t.Errorf("said the work landed when it parked: %q", rv.notice)
	}
	if !strings.Contains(rv.notice, "did not land") {
		t.Errorf("notice = %q, want it to say the work did not land", rv.notice)
	}
	if !strings.Contains(rv.notice, "Needs You") {
		t.Errorf("notice = %q, want it to point at where the work went", rv.notice)
	}
}

// A landing that actually merged still reports success.
func TestLandedReportsSuccess(t *testing.T) {
	r, _, ctx := landingReview(t)
	r.mode = reviewLanding

	screen, _ := r.Update(reviewActedMsg{verb: "approved and landed", state: core.StateDone}, ctx)
	rv := screen.(*review)

	if !strings.Contains(rv.notice, "approved and landed") {
		t.Errorf("a successful landing stopped saying so: %q", rv.notice)
	}
	if strings.Contains(rv.notice, "did not land") {
		t.Errorf("a successful landing reported as parked: %q", rv.notice)
	}
}

// Parking still clears the card: the ticket has left Review either way, and leaving it on screen
// invites approving it again.
func TestParkedLandingLeavesTheReviewScreen(t *testing.T) {
	r, _, ctx := landingReview(t)
	r.mode = reviewLanding

	screen, _ := r.Update(reviewActedMsg{verb: "approved and landed", state: core.StateNeedsYou}, ctx)
	rv := screen.(*review)

	if rv.ticketID != "" {
		t.Errorf("parked ticket %q is still on the review card", rv.ticketID)
	}
	if rv.mode != reviewBrowsing {
		t.Errorf("mode = %v, want browsing once the landing settled", rv.mode)
	}
}
