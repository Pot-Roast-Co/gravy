package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// Each key on the chooser sends the outcome it names, and nothing sends an outcome the human did
// not pick. Getting this mapping wrong pushes work someone deliberately kept local.
func TestApproveChooserSendsTheChosenOutcome(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want core.Approval
	}{
		{"enter", core.ApprovePush},
		{"n", core.ApproveLocal},
		{"h", core.ApproveHandOff},
	} {
		t.Run(tc.key, func(t *testing.T) {
			r, f, ctx := landingReview(t)
			screen, _ := r.handleKey(key("a"), ctx)
			screen, cmd := screen.(*review).handleKey(key(tc.key), ctx)
			if cmd == nil {
				t.Fatalf("%q produced no approval", tc.key)
			}
			cmd()
			if len(f.approvedHow) != 1 || f.approvedHow[0] != tc.want {
				t.Fatalf("%q sent %v, want %v", tc.key, f.approvedHow, tc.want)
			}
			_ = screen
		})
	}
}

// The chooser names all three outcomes and the branch they act on, so the decision is made
// against what will actually happen rather than remembered.
func TestApproveChooserNamesTheOutcomes(t *testing.T) {
	r, _, ctx := landingReview(t)
	r.bundle.Project.TargetBranch = "main"
	screen, _ := r.handleKey(key("a"), ctx)
	view := screen.(*review).View(ctx)

	for _, want := range []string{"squash onto main and push", "do not push", "hand off", "esc"} {
		if !strings.Contains(view, want) {
			t.Errorf("the chooser omits %q:\n%s", want, view)
		}
	}
}

// Opening the chooser is not approving. esc must leave the ticket exactly as it was.
func TestApproveChooserCancels(t *testing.T) {
	r, f, ctx := landingReview(t)

	screen, cmd := r.handleKey(key("a"), ctx)
	if cmd != nil {
		t.Fatal("opening the chooser started work")
	}
	screen, _ = screen.(*review).handleKey(key("esc"), ctx)
	rv := screen.(*review)

	if rv.mode != reviewBrowsing {
		t.Errorf("mode = %v after esc, want browsing", rv.mode)
	}
	if len(f.approved) != 0 {
		t.Errorf("esc approved anyway: %v", f.approved)
	}
	if rv.ticketID == "" {
		t.Error("esc dropped the ticket off the card")
	}
}

// A handed-off ticket did not merge, and the screen says so rather than reporting a landing.
func TestHandedOffReadsAsHandedOff(t *testing.T) {
	r, _, ctx := landingReview(t)
	r.mode = reviewLanding

	screen, _ := r.Update(reviewActedMsg{
		verb: "approved and handed to you", state: core.StateHandedOff,
	}, ctx)
	rv := screen.(*review)

	if strings.Contains(rv.notice, "landed") {
		t.Errorf("a hand-off claimed a landing: %q", rv.notice)
	}
	if !strings.Contains(rv.notice, "nothing merged") {
		t.Errorf("notice = %q, want it to say nothing merged", rv.notice)
	}
	if !strings.Contains(rv.notice, "yours") {
		t.Errorf("notice = %q, want it to say the branch is theirs", rv.notice)
	}
	if rv.mode != reviewBrowsing {
		t.Errorf("mode = %v, want browsing once it settled", rv.mode)
	}
}
