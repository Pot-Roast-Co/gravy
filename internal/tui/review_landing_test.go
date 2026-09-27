package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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

// approveOpen presses "a" then "enter" on the card and returns the screen and the landing call.
func approveOpen(t *testing.T, r *review, ctx ViewContext) (*review, tea.Cmd) {
	t.Helper()
	screen, chooser := r.handleKey(key("a"), ctx)
	if chooser != nil {
		t.Fatal("opening the chooser started work")
	}
	if rv := screen.(*review); rv.mode != reviewApproving {
		t.Fatalf("mode = %v after a, want reviewApproving", rv.mode)
	}
	screen, land := screen.(*review).handleKey(key("enter"), ctx)
	if land == nil {
		t.Fatal("choosing an outcome produced no approval")
	}
	return screen.(*review), land
}

// reopen puts a ticket back on the card, as following it from the dashboard would.
func reopen(r *review, id string) {
	r.ticketID, r.loaded = id, true
	r.bundle.Ticket.ID = id
}

// TestApprovingDoesNotHoldTheScreen is the regression.
//
// Approving used to put the whole screen into a landing mode that refused every decision until
// the daemon answered. Review covers every project, so one slow or stuck landing — a
// re-validation, a push that hangs — held up review of all of them. The approval now runs in
// the background and the card moves on at once.
func TestApprovingDoesNotHoldTheScreen(t *testing.T) {
	r, _, ctx := landingReview(t)
	id := r.ticketID

	rv, _ := approveOpen(t, r, ctx)
	if rv.ticketID == id {
		t.Fatal("the approved ticket is still on the card")
	}
	if rv.mode != reviewBrowsing {
		t.Errorf("mode = %v, want browsing: nothing should wait on the landing", rv.mode)
	}
	if !strings.Contains(rv.notice, "background") {
		t.Errorf("notice = %q, want it to say the landing is running in the background", rv.notice)
	}

	// Another ticket — here, from any project — can be decided while the first still lands.
	reopen(rv, "another-project-ticket")
	screen, _ := rv.handleKey(key("a"), ctx)
	if screen.(*review).mode != reviewApproving {
		t.Error("a second ticket could not be approved while the first was landing")
	}
}

// A landing's answer arrives minutes later, when the card shows something else. It reports on
// its own ticket and leaves the card alone.
func TestALateLandingResultLeavesTheCurrentCard(t *testing.T) {
	r, _, ctx := landingReview(t)
	first := r.ticketID
	rv, _ := approveOpen(t, r, ctx)
	reopen(rv, "being-reviewed-now")

	screen, _ := rv.Update(reviewActedMsg{ticketID: first, verb: "approved and landed", state: core.StateDone}, ctx)
	rv = screen.(*review)

	if rv.ticketID != "being-reviewed-now" {
		t.Errorf("the late result cleared the card: ticket = %q", rv.ticketID)
	}
	if !strings.Contains(rv.notice, shortID(first)) {
		t.Errorf("notice = %q, want it to name the ticket that landed", rv.notice)
	}
	if rv.landing[first] {
		t.Error("the landed ticket is still marked as landing")
	}
}

// TestApproveTwiceSendsOneApproval: a human who opens the same ticket again while it lands
// cannot approve it twice.
func TestApproveTwiceSendsOneApproval(t *testing.T) {
	r, f, ctx := landingReview(t)
	id := r.ticketID
	rv, first := approveOpen(t, r, ctx)
	reopen(rv, id)

	for i := 0; i < 3; i++ {
		for _, k := range []string{"a", "enter"} {
			screen, again := rv.handleKey(key(k), ctx)
			rv = screen.(*review)
			if again != nil {
				t.Fatalf("press %d (%q) produced a second approval", i+2, k)
			}
		}
	}
	if f := first(); f == nil {
		t.Fatal("the approval command returned nothing")
	}
	if len(f.approved) != 1 {
		t.Fatalf("service saw %d approvals, want 1", len(f.approved))
	}
}

// The other decisions are refused too for the landing ticket: they were made about a ticket
// that has already been decided.
func TestLandingRefusesTheOtherDecisions(t *testing.T) {
	r, f, ctx := landingReview(t)
	id := r.ticketID
	rv, _ := approveOpen(t, r, ctx)
	reopen(rv, id)

	for _, k := range []string{"r", "x", "v", "T", "s"} {
		screen, cmd := rv.handleKey(key(k), ctx)
		rv = screen.(*review)
		if cmd != nil {
			t.Errorf("%q acted while the ticket was landing", k)
		}
		if !strings.Contains(rv.notice, "landing in the background") {
			t.Errorf("%q: notice = %q", k, rv.notice)
		}
	}
	if len(f.rejected) != 0 {
		t.Errorf("a rejection got through: %v", f.rejected)
	}
}

func landed(r *review, msg reviewActedMsg) *review {
	r.landing[msg.ticketID] = true
	screen, _ := r.Update(msg, ViewContext{Theme: DefaultTheme(), Width: 140, Height: 30})
	return screen.(*review)
}

// A duplicate that does get through — a second window, or the CLI — reads as a duplicate, never
// as a failure over a merge that worked.
func TestAlreadyLandedIsNotReportedAsFailure(t *testing.T) {
	r, _, _ := landingReview(t)
	err := fmt.Errorf("land: %s is done: %w", r.ticketID, core.ErrAlreadyLanded)
	rv := landed(r, reviewActedMsg{ticketID: r.ticketID, verb: "approved and landed", err: err})

	if strings.Contains(rv.notice, "failed") {
		t.Errorf("duplicate approval reported as a failure: %q", rv.notice)
	}
	if !strings.Contains(rv.notice, "already landing") {
		t.Errorf("notice = %q", rv.notice)
	}
}

// A real failure still says so, and names the ticket.
func TestGenuineLandFailureStillReportsFailure(t *testing.T) {
	r, _, _ := landingReview(t)
	rv := landed(r, reviewActedMsg{
		ticketID: r.ticketID, verb: "approved and landed",
		err: errors.New("land: re-validation: check FAILED (exit 2)"),
	})
	if !strings.Contains(rv.notice, "failed") || !strings.Contains(rv.notice, shortID(r.ticketID)) {
		t.Errorf("a real failure did not say so for its ticket: %q", rv.notice)
	}
}

// Landing must not trap the human on this screen: the daemon finishes the merge either way.
func TestLandingDoesNotCaptureKeys(t *testing.T) {
	r := newReview()
	r.landing["x"] = true
	if r.CapturesKeys() {
		t.Error("landing captured the keyboard; the human cannot leave the screen")
	}
}
