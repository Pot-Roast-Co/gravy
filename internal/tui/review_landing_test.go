package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

func landingReview(t *testing.T) (*review, *fakeService, ViewContext) {
	t.Helper()
	f := reviewFixture()
	r := newReview()
	r.ticketID = f.review.Ticket.ID
	r.bundle, r.loaded = f.review, true
	return r, f, ViewContext{Theme: DefaultTheme(), Svc: f, Width: 140, Height: 30}
}

// TestApproveTwiceSendsOneApproval is the regression.
//
// Landing takes minutes — rebase, re-validation, push — and the screen used to spend all of it
// unchanged. A human who saw nothing happen pressed "a" again; the second approval reached the
// daemon, found the ticket already landing, and was correctly refused. That refusal was then
// rendered as "approved and landed failed", over a merge that had in fact succeeded.
func TestApproveTwiceSendsOneApproval(t *testing.T) {
	r, f, ctx := landingReview(t)

	// "a" opens the chooser; "enter" is the default outcome, squash and push.
	screen, chooser := r.handleKey(key("a"), ctx)
	rv := screen.(*review)
	if chooser != nil {
		t.Fatal("opening the chooser started work")
	}
	if rv.mode != reviewApproving {
		t.Fatalf("mode = %v after a, want reviewApproving", rv.mode)
	}
	screen, first := rv.handleKey(key("enter"), ctx)
	rv = screen.(*review)
	if first == nil {
		t.Fatal("choosing an outcome produced no approval")
	}
	if rv.mode != reviewLanding {
		t.Fatalf("mode = %v after approving, want reviewLanding", rv.mode)
	}

	for i := 0; i < 3; i++ {
		for _, k := range []string{"a", "enter"} {
			screen, again := rv.handleKey(key(k), ctx)
			rv = screen.(*review)
			if again != nil {
				t.Fatalf("press %d (%q) produced a second approval", i+2, k)
			}
		}
	}

	// Only the first press ever reaches the service.
	first()
	if len(f.approved) != 1 {
		t.Fatalf("service saw %d approvals, want 1", len(f.approved))
	}
}

// The other decisions are refused too: they were made about a ticket that has already left.
func TestLandingRefusesTheOtherDecisions(t *testing.T) {
	r, f, ctx := landingReview(t)
	screen, _ := r.handleKey(key("a"), ctx)
	screen, _ = screen.(*review).handleKey(key("enter"), ctx)
	rv := screen.(*review)

	for _, k := range []string{"r", "x", "v", "T", "s"} {
		screen, cmd := rv.handleKey(key(k), ctx)
		rv = screen.(*review)
		if cmd != nil {
			t.Errorf("%q acted while a landing was in flight", k)
		}
		if rv.mode != reviewLanding {
			t.Errorf("%q left landing mode", k)
		}
	}
	if len(f.rejected) != 0 {
		t.Errorf("a rejection got through: %v", f.rejected)
	}
}

// A duplicate that does get through — a second window, or the CLI — reads as a duplicate, never
// as a failure over a merge that worked.
func TestAlreadyLandedIsNotReportedAsFailure(t *testing.T) {
	r, _, ctx := landingReview(t)
	r.mode = reviewLanding

	err := fmt.Errorf("land: %s is done: %w", r.ticketID, core.ErrAlreadyLanded)
	screen, _ := r.Update(reviewActedMsg{verb: "approved and landed", err: err}, ctx)
	rv := screen.(*review)

	if strings.Contains(rv.notice, "failed") {
		t.Errorf("duplicate approval reported as a failure: %q", rv.notice)
	}
	if !strings.Contains(rv.notice, "already landing") {
		t.Errorf("notice = %q", rv.notice)
	}
	if rv.mode != reviewBrowsing {
		t.Errorf("mode = %v, want browsing once the action settled", rv.mode)
	}
}

// A real failure still says so.
func TestGenuineLandFailureStillReportsFailure(t *testing.T) {
	r, _, ctx := landingReview(t)
	r.mode = reviewLanding

	screen, _ := r.Update(reviewActedMsg{
		verb: "approved and landed",
		err:  errors.New("land: re-validation: check FAILED (exit 2)"),
	}, ctx)
	rv := screen.(*review)

	if !strings.Contains(rv.notice, "failed") {
		t.Errorf("a real failure did not say so: %q", rv.notice)
	}
}

// Landing must not trap the human on this screen: the daemon finishes the merge either way.
func TestLandingDoesNotCaptureKeys(t *testing.T) {
	r := newReview()
	r.mode = reviewLanding
	if r.CapturesKeys() {
		t.Error("landing captured the keyboard; the human cannot leave the screen")
	}
}
