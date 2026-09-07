package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/bobbybrady/gravy/internal/api"
	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/git"
	"github.com/bobbybrady/gravy/internal/store"
)

// reviewFixture is a typical ticket awaiting judgement: green validation, a short narrative, one
// assumption, and a two-file diff.
func reviewFixture() *fakeService {
	f := newFake()
	cost := 0.0983
	f.status.Attention = []api.AttentionItem{{
		Attention: core.Attention{ID: "a1", Reason: core.ReasonReviewPending, TicketID: "8ecd21bc"},
		Project:   projGravy, Ticket: core.Ticket{ID: "8ecd21bc"},
	}}
	f.review = api.ReviewBundle{
		Ticket: core.Ticket{ID: "8ecd21bc09d6c96d", Title: "Add a Multiply function to the calc package",
			Branch: "gravy/8ecd21bc-add-multiply", State: core.StateReview,
			WorktreePath: "/home/bobby/.gravy/projects/testrepo/worktrees/gravy-8ecd21bc"},
		Project: projGravy,
		Run: core.Run{ID: "d3b64258aa", ProviderID: "claude-code", Model: "sonnet",
			HostID: "local", Turns: 7, CostUSD: &cost},
		Validations: []store.Validation{
			{Step: "test", ExitCode: 0}, {Step: "build", ExitCode: 0},
		},
		Summary: core.Summary{
			Narrative:   "Adds Multiply and a table-driven test covering positive, negative and zero.",
			Assumptions: []string{"integer overflow is out of scope for this ticket"},
		},
		Diff: git.Diff{Files: []git.FileDiff{
			{Path: "calc.go", Status: "modified", Additions: 3, Deletions: 0,
				Patch: "@@ -4,3 +4,6 @@\n func Add(a, b int) int { return a + b }\n+\n+// Multiply returns the product of a and b.\n+func Multiply(a, b int) int { return a * b }"},
			{Path: "calc_test.go", Status: "modified", Additions: 21, Deletions: 0},
		}},
	}
	return f
}

// openReview boots the frame onto a loaded review card.
func openReview(t *testing.T, f *fakeService, w, h int) Model {
	t.Helper()
	m := boot(t, f, w, h)
	m = send(t, m, key("5"))
	m = send(t, m, enteredMsg{focus: f.review.Ticket.ID})
	m = send(t, m, reviewLoadedMsg{bundle: f.review})
	return m
}

// TestReviewCardFitsStandardTerminal is AC1. Routine work should be approvable without
// scrolling; a card that needs paging is a card nobody reads before pressing a.
func TestReviewCardFitsStandardTerminal(t *testing.T) {
	m := openReview(t, reviewFixture(), 80, 24)
	view := m.View()

	lines := strings.Split(view, "\n")
	if len(lines) != 24 {
		t.Fatalf("rendered %d lines, want 24", len(lines))
	}
	for i, ln := range lines {
		if wd := lipgloss.Width(ln); wd > 80 {
			t.Errorf("line %d is %d cells wide: %q", i, wd, ln)
		}
	}
	if strings.Contains(view, "more (↑↓") {
		t.Errorf("a typical ticket's card had to scroll:\n%s", view)
	}
	// The evidence a reviewer decides on must all be present.
	for _, want := range []string{
		"Add a Multiply function", "gravy", "8ecd21bc-add-multiply",
		"claude-code/sonnet@local", "7 turns", "$0.0983",
		"test", "build", "passed", "2 file(s)  +24 -0", "calc.go",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("card omits %q", want)
		}
	}
}

// TestAssumptionsAreImpossibleToMiss is AC2. An assumption is the reason to actually read a diff
// you would otherwise wave through.
func TestAssumptionsAreImpossibleToMiss(t *testing.T) {
	m := openReview(t, reviewFixture(), 80, 24)
	view := m.View()
	if !strings.Contains(view, "assumption") || !strings.Contains(view, "integer overflow") {
		t.Errorf("the summary's assumption is not surfaced:\n%s", view)
	}
	if !strings.Contains(view, "⚠") {
		t.Errorf("the assumption carries no visual flag:\n%s", view)
	}
}

// TestEnterExpandsAndCollapses is AC3.
func TestEnterExpandsAndCollapses(t *testing.T) {
	m := openReview(t, reviewFixture(), 80, 24)
	if strings.Contains(m.View(), "func Multiply") {
		t.Fatal("the card started expanded")
	}

	m = send(t, m, key("enter"))
	if !strings.Contains(m.View(), "func Multiply") {
		t.Errorf("enter did not expand the diff:\n%s", m.View())
	}
	m = send(t, m, key("enter"))
	if strings.Contains(m.View(), "func Multiply") {
		t.Errorf("enter did not collapse the diff again:\n%s", m.View())
	}
}

// TestLargePatchTruncatesWithAnOffer is AC3's other half: review is fast triage with an escape
// to real tools, not a diff viewer.
func TestLargePatchTruncatesWithAnOffer(t *testing.T) {
	f := reviewFixture()
	var big strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&big, "+line %d\n", i)
	}
	f.review.Diff.Files = []git.FileDiff{
		{Path: "huge.go", Status: "modified", Additions: 500, Patch: big.String()},
	}

	m := openReview(t, f, 80, 50)
	m = send(t, m, key("enter"))
	view := m.View()

	if !strings.Contains(view, "truncated") {
		t.Errorf("a 500-line patch was not truncated:\n%s", view)
	}
	if n := len(strings.Split(view, "\n")); n != 50 {
		t.Errorf("expanded view is %d lines, want 50", n)
	}
}

// TestApproveCallsTheGate is AC4.
func TestApproveCallsTheGate(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 80, 24)

	m, cmd := sendCmd(t, m, key("a"))
	if cmd == nil {
		t.Fatal("a produced no command")
	}
	m = send(t, m, cmd())

	if len(f.approved) != 1 || f.approved[0] != f.review.Ticket.ID {
		t.Fatalf("approved = %v, want the open ticket", f.approved)
	}
	// The ticket has left the queue; the screen must not still be offering to approve it.
	if strings.Contains(m.View(), "a approve") {
		t.Errorf("the card survived its own approval:\n%s", m.View())
	}
}

// TestRequestChangesPromptsAndSendsFeedback is AC5.
func TestRequestChangesPromptsAndSendsFeedback(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 80, 24)

	m = send(t, m, key("r"))
	if !strings.Contains(m.View(), "what needs to change") {
		t.Fatalf("r did not prompt for feedback:\n%s", m.View())
	}
	for _, c := range "add docs" {
		m = send(t, m, key(string(c)))
	}
	if !strings.Contains(m.View(), "add docs") {
		t.Errorf("the prompt did not echo what was typed:\n%s", m.View())
	}

	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter did not submit the feedback")
	}
	m = send(t, m, cmd())

	if got := f.changes[f.review.Ticket.ID]; got != "add docs" {
		t.Errorf("feedback = %q, want %q", got, "add docs")
	}
	if len(f.approved) != 0 {
		t.Error("requesting changes approved the ticket")
	}
	// The ticket has gone back to the agent, so the card must not still be offering to
	// approve the work that was just rejected.
	if strings.Contains(m.View(), "a approve") {
		t.Errorf("the card survived sending the work back:\n%s", m.View())
	}
}

// TestEmptyFeedbackIsRefused: sending work back with no reason wastes the retry the note exists
// to make worthwhile.
func TestEmptyFeedbackIsRefused(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 80, 24)
	m = send(t, m, key("r"))

	m, cmd := sendCmd(t, m, key("enter"))
	if cmd != nil {
		t.Fatal("empty feedback was submitted")
	}
	if !strings.Contains(m.View(), "say what needs to change") {
		t.Errorf("no explanation for the refusal:\n%s", m.View())
	}
	if len(f.changes) != 0 {
		t.Errorf("changes were requested anyway: %v", f.changes)
	}
}

// TestRejectRequiresConfirmation is AC6. Rejection deletes a worktree, so it must not be one
// keystroke away from the cursor keys.
func TestRejectRequiresConfirmation(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 80, 24)

	m, cmd := sendCmd(t, m, key("x"))
	if cmd != nil {
		t.Fatal("x rejected immediately, with no confirmation")
	}
	if !strings.Contains(m.View(), "reject this ticket") {
		t.Fatalf("x did not ask for confirmation:\n%s", m.View())
	}
	if len(f.rejected) != 0 {
		t.Fatal("the ticket was rejected before confirming")
	}

	// Anything other than y backs out.
	_, cmd = sendCmd(t, m, key("n"))
	if cmd != nil || len(f.rejected) != 0 {
		t.Fatal("declining the confirmation still rejected the ticket")
	}

	m = send(t, m, key("x"))
	m, cmd = sendCmd(t, m, key("y"))
	if cmd == nil {
		t.Fatal("confirming did not reject")
	}
	send(t, m, cmd())
	if len(f.rejected) != 1 {
		t.Errorf("rejected = %v, want one ticket", f.rejected)
	}
}

// TestExternalEscapesSayTheyAreNotWired covers the part of the ticket that is deliberately not
// built: a key that silently does nothing is worse than one that explains itself.
func TestExternalEscapesSayTheyAreNotWired(t *testing.T) {
	f := reviewFixture()
	for _, k := range []string{"e", "d", "!"} {
		m := openReview(t, f, 80, 24)
		m = send(t, m, key(k))
		view := m.View()
		if !strings.Contains(view, "not wired up yet") {
			t.Errorf("key %q gave no feedback:\n%s", k, view)
		}
		if !strings.Contains(view, "worktree") {
			t.Errorf("key %q did not say where the worktree is:\n%s", k, view)
		}
	}
}

// TestReviewWithNothingPendingGuides is the empty state.
func TestReviewWithNothingPendingGuides(t *testing.T) {
	m := boot(t, newFake(), 80, 24)
	m = send(t, m, key("5"))
	m = send(t, m, enteredMsg{})
	if !strings.Contains(m.View(), "Nothing awaiting review") {
		t.Errorf("empty review screen is not explained:\n%s", m.View())
	}
}

// TestReviewLoadFailureIsVisible: a card that silently stays blank looks like an empty queue.
func TestReviewLoadFailureIsVisible(t *testing.T) {
	f := reviewFixture()
	m := boot(t, f, 80, 24)
	m = send(t, m, key("5"))
	m = send(t, m, reviewErrMsg{err: fmt.Errorf("worktree is gone")})
	view := m.View()
	if !strings.Contains(view, "Could not load") || !strings.Contains(view, "worktree is gone") {
		t.Errorf("a failed load is not reported:\n%s", view)
	}
}
