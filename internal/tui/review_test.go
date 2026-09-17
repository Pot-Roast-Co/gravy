package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
	rev "github.com/pot-roast-co/gravy/internal/review"
	"github.com/pot-roast-co/gravy/internal/store"
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
	m = send(t, m, key(SectionReview.Key()))
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
	// Past maxPatchLines, which is a guard against a generated file rather than a reading
	// limit — an ordinary change must not hit it.
	var big strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&big, "+line %d\n", i)
	}
	f.review.Diff.Files = []git.FileDiff{
		{Path: "huge.go", Status: "modified", Additions: 2000, Patch: big.String()},
	}

	m := openReview(t, f, 80, 50)
	m = send(t, m, key("enter"))
	m = send(t, m, key("G")) // the marker is at the end of a patch that now scrolls
	view := m.View()

	if !strings.Contains(view, "truncated") {
		t.Errorf("a 2000-line patch was not truncated:\n%s", view)
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
// TestExternalEscapesRefuseAMissingWorktree: the worktree lives on the machine running the
// daemon, which is not necessarily this one. Launching an editor on a directory that is not
// there is worse than saying so.
func TestExternalEscapesRefuseAMissingWorktree(t *testing.T) {
	f := reviewFixture()
	// e goes through the service to build a review checkout, so it is covered separately.
	for _, k := range []string{"d", "!"} {
		m := openReview(t, f, 80, 24)
		m, cmd := sendCmd(t, m, key(k))
		if cmd != nil {
			t.Errorf("key %q launched a tool on a worktree that is not on this machine", k)
		}
		if view := m.View(); !strings.Contains(view, "not on this machine") {
			t.Errorf("key %q gave no reason:\n%s", k, view)
		}
	}
}

// TestExternalEscapesHandOffTheTerminal covers the real path, on a worktree that exists.
func TestExternalEscapesHandOffTheTerminal(t *testing.T) {
	wt := t.TempDir()
	f := reviewFixture()
	f.review.Ticket.WorktreePath = wt

	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true") // a real program, so building the command succeeds
	t.Setenv("SHELL", "/bin/sh")

	for _, k := range []string{"d", "!"} {
		m := openReview(t, f, 80, 24)
		m, cmd := sendCmd(t, m, key(k))
		if cmd == nil {
			t.Errorf("key %q did not hand off:\n%s", k, m.View())
		}
	}
}

// TestEditorHandOffNeedsAnEditor: a key that silently does nothing because the environment is
// empty is indistinguishable from a broken one.
func TestEditorHandOffNeedsAnEditor(t *testing.T) {
	f := reviewFixture()
	f.checkoutPath = t.TempDir()
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")

	m := openReview(t, f, 80, 24)
	m, cmd := sendCmd(t, m, key("e"))
	if cmd == nil {
		t.Fatal("e did not ask for a checkout")
	}
	m = send(t, m, cmd()) // the checkout arrives; the editor is chosen here

	if view := m.View(); !strings.Contains(view, "VISUAL or EDITOR") {
		t.Errorf("e did not say what to set:\n%s", view)
	}
}

// TestReviewOffersTheEscapes: they are only useful if the screen says they exist.
func TestReviewOffersTheEscapes(t *testing.T) {
	// Wide enough for the whole footer; it trims from the end on narrow terminals.
	m := openReview(t, reviewFixture(), 140, 30)
	view := m.View()
	for _, want := range []string{"e editor", "d difftool", "! shell"} {
		if !strings.Contains(view, want) {
			t.Errorf("the footer omits %q:\n%s", want, view)
		}
	}
}

// TestReviewWithNothingPendingGuides is the empty state.
func TestReviewWithNothingPendingGuides(t *testing.T) {
	m := boot(t, newFake(), 80, 24)
	m = send(t, m, key(SectionReview.Key()))
	m = send(t, m, enteredMsg{})
	if !strings.Contains(m.View(), "Nothing awaiting review") {
		t.Errorf("empty review screen is not explained:\n%s", m.View())
	}
}

// TestReviewLoadFailureIsVisible: a card that silently stays blank looks like an empty queue.
func TestReviewLoadFailureIsVisible(t *testing.T) {
	f := reviewFixture()
	m := boot(t, f, 80, 24)
	m = send(t, m, key(SectionReview.Key()))
	m = send(t, m, reviewErrMsg{err: fmt.Errorf("worktree is gone")})
	view := m.View()
	if !strings.Contains(view, "Could not load") || !strings.Contains(view, "worktree is gone") {
		t.Errorf("a failed load is not reported:\n%s", view)
	}
}

// TestEditorOpensAReviewCheckout is the point of the checkout.
//
// Gravy commits what the agent produced, so the ticket's own worktree is clean and an editor's
// git integration has nothing to show there. The checkout puts the same content on disk with the
// index at its base, which is the state every editor is built to display.
func TestEditorOpensAReviewCheckout(t *testing.T) {
	f := reviewFixture()
	f.checkoutPath = t.TempDir()
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true")

	m := openReview(t, f, 80, 24)
	m, cmd := sendCmd(t, m, key("e"))
	if cmd == nil {
		t.Fatal("e did not ask for a checkout")
	}
	m, cmd = sendCmd(t, m, cmd())
	if cmd == nil {
		t.Fatalf("the checkout did not open an editor:\n%s", m.View())
	}
	if len(f.checkedOut) != 1 {
		t.Fatalf("ReviewCheckout called %d times, want 1", len(f.checkedOut))
	}

	// The ticket's own worktree must not be what gets opened: it is clean, so an editor shows
	// nothing there.
	if f.checkoutPath == f.review.Ticket.WorktreePath {
		t.Fatal("the fixture cannot distinguish the checkout from the worktree")
	}
}

// TestEditorReportsAFailedCheckout: a key that silently does nothing is indistinguishable from a
// broken one.
func TestEditorReportsAFailedCheckout(t *testing.T) {
	f := reviewFixture()
	f.checkoutErr = fmt.Errorf("GR-100 has no branch")

	m := openReview(t, f, 80, 24)
	m, cmd := sendCmd(t, m, key("e"))
	if cmd == nil {
		t.Fatal("e did not ask for a checkout")
	}
	m = send(t, m, cmd())

	if view := m.View(); !strings.Contains(view, "has no branch") {
		t.Errorf("the failure is not shown:\n%s", view)
	}
}

// TestEditorRefusesACheckoutElsewhere: the checkout is made on the machine running the daemon,
// which is not necessarily this one.
func TestEditorRefusesACheckoutElsewhere(t *testing.T) {
	f := reviewFixture()
	f.checkoutPath = "/definitely/not/here"
	t.Setenv("EDITOR", "true")

	m := openReview(t, f, 80, 24)
	m, cmd := sendCmd(t, m, key("e"))
	m, cmd = sendCmd(t, m, cmd())
	if cmd != nil {
		t.Fatal("launched an editor on a path that is not on this machine")
	}
	if view := m.View(); !strings.Contains(view, "not on this machine") {
		t.Errorf("no reason given:\n%s", view)
	}
}

// TestApproveDiscardsTheCheckout: one review checkout per reviewed ticket, left behind forever,
// is a directory per ticket that nothing ever cleans up.
func TestApproveDiscardsTheCheckout(t *testing.T) {
	f := reviewFixture()
	f.checkoutPath = t.TempDir()
	t.Setenv("EDITOR", "true")

	m := openReview(t, f, 80, 24)
	m, cmd := sendCmd(t, m, key("e"))
	m, _ = sendCmd(t, m, cmd())

	m, cmd = sendCmd(t, m, key("a"))
	if cmd == nil {
		t.Fatal("a did not approve")
	}
	send(t, m, cmd())

	if len(f.approved) != 1 {
		t.Fatalf("approve called %d times", len(f.approved))
	}
}

// TestReviewShowsTheTicket: reviewing a diff without the ticket in front of you is checking
// whether it looks reasonable, not whether it did what was asked.
func TestReviewShowsTheTicket(t *testing.T) {
	f := reviewFixture()
	f.review.Ticket.Body = "Done looks like: an allowlist applied before the changeset."

	m := openReview(t, f, 120, 40)
	if strings.Contains(m.View(), "Done looks like") {
		t.Fatal("the ticket body is shown before it is asked for")
	}
	if !strings.Contains(m.View(), "t ticket") {
		t.Errorf("the footer does not offer the ticket:\n%s", m.View())
	}

	m = send(t, m, key("t"))
	view := m.View()
	if !strings.Contains(view, "Done looks like") {
		t.Errorf("t did not show the ticket:\n%s", view)
	}
	if !strings.Contains(view, "t hide ticket") {
		t.Errorf("the footer does not offer to hide it again:\n%s", view)
	}

	m = send(t, m, key("t"))
	if strings.Contains(m.View(), "Done looks like") {
		t.Error("t did not hide the ticket again")
	}
}

// TestReviewScrollsTheDiff is the complaint the 24-line cap created: an ordinary change was cut
// in half and you were told to leave for an editor to read the rest.
func TestReviewScrollsTheDiff(t *testing.T) {
	f := reviewFixture()
	var big strings.Builder
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&big, "+line %02d\n", i)
	}
	f.review.Diff.Files = []git.FileDiff{
		{Path: "big.go", Status: "modified", Additions: 80, Patch: big.String()},
	}

	m := openReview(t, f, 100, 24)
	m = send(t, m, key("enter")) // expand

	if strings.Contains(m.View(), "truncated") {
		t.Error("an 80-line diff was truncated; the cap is meant for generated files")
	}

	// The end of the diff is reachable, which it was not before.
	m = send(t, m, key("G"))
	if !strings.Contains(m.View(), "line 79") {
		t.Errorf("the end of the diff is not reachable:\n%s", m.View())
	}
	m = send(t, m, key("g"))
	if !strings.Contains(m.View(), "line 00") {
		t.Errorf("g did not return to the top:\n%s", m.View())
	}
}

// TestReviewTabMovesBetweenFiles keeps j/k free for the diff.
func TestReviewTabMovesBetweenFiles(t *testing.T) {
	f := reviewFixture()
	f.review.Diff.Files = []git.FileDiff{
		{Path: "one.go", Status: "modified"},
		{Path: "two.go", Status: "modified"},
	}
	m := openReview(t, f, 100, 30)

	scr := func(m Model) *review { return m.screens[SectionReview].(*review) }
	if scr(m).cursor != 0 {
		t.Fatalf("cursor starts at %d", scr(m).cursor)
	}
	m = send(t, m, key("tab"))
	if scr(m).cursor != 1 {
		t.Errorf("tab did not move to the next file: cursor %d", scr(m).cursor)
	}
	m = send(t, m, key("tab")) // wraps
	if scr(m).cursor != 0 {
		t.Errorf("tab did not wrap: cursor %d", scr(m).cursor)
	}
}

// TestReviewFooterTrimsRatherThanOverflowing: losing "tab file" is survivable, losing
// "a approve" is not.
func TestReviewFooterTrimsRatherThanOverflowing(t *testing.T) {
	for _, w := range []int{40, 60, 80, 140} {
		m := openReview(t, reviewFixture(), w, 24)
		lines := strings.Split(m.View(), "\n")
		for i, ln := range lines {
			if got := lipgloss.Width(ln); got > w {
				t.Errorf("width %d: line %d is %d cells: %q", w, i, got, ln)
			}
		}
		if !strings.Contains(m.View(), "a approve") {
			t.Errorf("width %d: the footer dropped the approve key:\n%s", w, m.View())
		}
	}
}

// TestRereviewAsksAgain is the case the key exists for: the advisory pass ran once, inside the
// run, and failed for a reason since fixed — a misrouted model. Without this the verdict on the
// card stays broken and the only way to ask again is re-running the whole ticket.
func TestRereviewAsksAgain(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 140, 30)

	if !strings.Contains(m.View(), "v re-review") {
		t.Errorf("the footer does not offer a re-review:\n%s", m.View())
	}

	m, cmd := sendCmd(t, m, key("v"))
	if cmd == nil {
		t.Fatal("v asked for nothing")
	}
	send(t, m, cmd())

	if len(f.rereviewed) != 1 {
		t.Fatalf("Rereview called %d times, want 1", len(f.rereviewed))
	}
	if f.rereviewed[0] != f.review.Ticket.ID {
		t.Errorf("re-reviewed %q, want the ticket on screen", f.rereviewed[0])
	}
}

// TestRereviewReportsItsFailure: the automatic pass swallows errors because it is advisory, but
// this one was asked for, and silence would read as a key that does nothing.
func TestRereviewReportsItsFailure(t *testing.T) {
	f := reviewFixture()
	f.rereviewErr = fmt.Errorf("no review model is configured")

	m := openReview(t, f, 140, 30)
	m, cmd := sendCmd(t, m, key("v"))
	m = send(t, m, cmd())

	if view := m.View(); !strings.Contains(view, "no review model is configured") {
		t.Errorf("the failure is not shown:\n%s", view)
	}
}

// TestVerdictFindingsAreReadableInFull: the screen scrolls, so cutting a reviewer's reasoning at
// the right-hand edge buys nothing and costs the half of the sentence that says what to do.
func TestVerdictFindingsAreReadableInFull(t *testing.T) {
	f := reviewFixture()
	f.review.Verdict = rev.Verdict{
		Overall: rev.Concerns,
		Summary: "The authorization holes are addressed and covered by attacker tests. " +
			"The required ROADMAP.md status update is missing from the diff.",
		Findings: []rev.Finding{{
			Severity: rev.High,
			File:     "ROADMAP.md",
			Rationale: "The ticket explicitly requires moving the High finding to fixed status " +
				"with the commit hash, but this change does not update ROADMAP.md at all.",
		}},
	}

	m := openReview(t, f, 90, 40)
	view := m.View()

	// The end of the summary, which a one-line render would have cut.
	if !strings.Contains(view, "missing from the diff") {
		t.Errorf("the summary is truncated:\n%s", view)
	}
	// The end of the rationale, likewise.
	if !strings.Contains(view, "does not update ROADMAP.md") {
		t.Errorf("the finding's rationale is truncated:\n%s", view)
	}
	// Severity as a word, not only a colour.
	if !strings.Contains(view, "[high]") {
		t.Errorf("severity is only conveyed by colour:\n%s", view)
	}
	if !strings.Contains(view, "ROADMAP.md") {
		t.Errorf("the finding does not say where:\n%s", view)
	}
}

// TestVerdictStillFitsItsTerminal: wrapping must not let a long verdict overflow.
func TestVerdictStillFitsItsTerminal(t *testing.T) {
	f := reviewFixture()
	f.review.Verdict = rev.Verdict{
		Overall:  rev.Fail,
		Summary:  strings.Repeat("a long considered summary sentence. ", 12),
		Findings: []rev.Finding{{Severity: rev.Medium, File: "a.go", Rationale: strings.Repeat("why ", 60)}},
	}
	for _, w := range []int{50, 80, 120} {
		m := openReview(t, f, w, 24)
		for i, ln := range strings.Split(m.View(), "\n") {
			if got := lipgloss.Width(ln); got > w {
				t.Errorf("width %d: line %d is %d cells: %q", w, i, got, ln)
			}
		}
	}
}

// TestRequestChangesStartsFromTheVerdict: the reviewer has just read the diff and said what is
// wrong with it. Making the human retype that to send it back is asking them to be a courier
// between two machines.
func TestRequestChangesStartsFromTheVerdict(t *testing.T) {
	f := reviewFixture()
	f.review.Verdict = rev.Verdict{
		Overall: rev.Concerns,
		Summary: "The required ROADMAP.md status update is missing from the diff.",
		Findings: []rev.Finding{{
			Severity:  rev.Low,
			File:      "ROADMAP.md",
			Rationale: "The ticket requires moving the High finding to fixed with the commit hash.",
		}},
	}

	m := openReview(t, f, 110, 30)
	m = send(t, m, key("r"))

	scr := m.screens[SectionReview].(*review)
	for _, want := range []string{"ROADMAP.md", "missing from the diff", "[low]"} {
		if !strings.Contains(scr.feedback, want) {
			t.Errorf("the seeded feedback omits %q: %q", want, scr.feedback)
		}
	}

	// It is a draft: editable, and sendable as-is.
	m = typeKeys(t, m, " Also bump the version.")
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter sent nothing")
	}
	send(t, m, cmd())

	sent := f.changes[f.review.Ticket.ID]
	if !strings.Contains(sent, "ROADMAP.md") || !strings.Contains(sent, "Also bump the version.") {
		t.Errorf("what was sent = %q", sent)
	}
}

// TestRequestChangesCanBeClearedInOneKey: backspacing a paragraph a machine wrote for you is not
// a reasonable thing to ask.
func TestRequestChangesCanBeClearedInOneKey(t *testing.T) {
	f := reviewFixture()
	f.review.Verdict = rev.Verdict{
		Overall:  rev.Concerns,
		Summary:  "Something long enough to be annoying to delete by hand.",
		Findings: []rev.Finding{{Severity: rev.Low, File: "a.go", Rationale: "and a rationale too"}},
	}
	m := openReview(t, f, 110, 30)
	m = send(t, m, key("r"))
	if scr := m.screens[SectionReview].(*review); scr.feedback == "" {
		t.Fatal("nothing was seeded")
	}

	m = send(t, m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if scr := m.screens[SectionReview].(*review); scr.feedback != "" {
		t.Errorf("ctrl+u left %q", scr.feedback)
	}
}

// TestRequestChangesIsEmptyWithoutFindings: a verdict that found nothing must not send the agent
// a cheerful summary of its own success.
func TestRequestChangesIsEmptyWithoutFindings(t *testing.T) {
	f := reviewFixture()
	f.review.Verdict = rev.Verdict{Overall: rev.Pass, Summary: "Looks good to me."}

	m := openReview(t, f, 110, 30)
	m = send(t, m, key("r"))
	if scr := m.screens[SectionReview].(*review); scr.feedback != "" {
		t.Errorf("seeded %q from a passing verdict", scr.feedback)
	}
}

// TestReviewCardNamesTheMergeTarget is the regression for work that landed on the wrong branch.
//
// The gravy project carried a feature branch as its target for six days. Every approval rebased,
// validated, squash-merged and pushed onto it, reported success, and left main untouched. Nothing
// on this screen — the screen where "a" performs that merge — ever said where the work was going.
func TestReviewCardNamesTheMergeTarget(t *testing.T) {
	f := reviewFixture()
	f.review.Project.TargetBranch = "feat/planning-projects-and-remote-hosts"
	view := openReview(t, f, 140, 30).View()

	if !strings.Contains(view, "feat/planning-projects-and-remote-hosts") {
		t.Errorf("the card does not say where approval sends the work:\n%s", view)
	}
	if !strings.Contains(view, "→") {
		t.Errorf("no branch → target on the card:\n%s", view)
	}
}

// A project with no target branch renders the source branch alone, not an arrow pointing nowhere.
func TestReviewCardWithoutATargetOmitsTheArrow(t *testing.T) {
	f := reviewFixture()
	f.review.Project.TargetBranch = ""
	view := openReview(t, f, 140, 30).View()

	if strings.Contains(view, "→") {
		t.Errorf("rendered an arrow with no target:\n%s", view)
	}
	if !strings.Contains(view, "8ecd21bc-add-multiply") {
		t.Errorf("lost the branch entirely:\n%s", view)
	}
}
