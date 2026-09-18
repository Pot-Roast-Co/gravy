package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// drive runs a command and feeds every message it produces back in, the way Bubble Tea's event
// loop does.
//
// Navigation from the dashboard is three hops — ask the daemon, resolve a destination, enter the
// screen — so a test that runs one command and asserts on the view is asserting on the screen it
// started from. That is how the bug this file covers survived a passing test suite.
func drive(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i > 32 {
			t.Fatal("commands did not settle: a navigation is looping")
		}
		msg := cmd()
		if msg == nil {
			return m
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				m = drive(t, m, c)
			}
			return m
		}
		m, cmd = sendCmd(t, m, msg)
	}
	return m
}

// press sends a key and drives whatever it starts to completion.
func press(t *testing.T, m Model, k string) Model {
	t.Helper()
	m, cmd := sendCmd(t, m, key(k))
	return drive(t, m, cmd)
}

// reviewFleet is the reproduction fixture: one repository with a review waiting on a human, and a
// second repository with an agent working, so the frame can be scoped away from the review.
func reviewFleet() *fakeService {
	f := newFake()
	f.status.Attention = []api.AttentionItem{
		{Attention: core.Attention{ID: "att-1", ProjectID: projGravy.ID,
			Reason: core.ReasonReviewPending, TicketID: "8ecd21bc"},
			Project: projGravy,
			Ticket:  core.Ticket{ID: "8ecd21bc", ProjectID: projGravy.ID, Title: "Add Multiply", State: core.StateReview},
			Age:     4 * time.Minute},
	}
	f.status.Running = []api.RunningTicket{
		{Ticket: core.Ticket{ID: "c9d4", ProjectID: projMojo.ID, Branch: "gravy/c9d4-dashboard"},
			Project: projMojo,
			Run:     core.Run{ProviderID: "claude-code", Model: "sonnet", HostID: "local"},
			Elapsed: 3 * time.Minute, Activity: "implementing"},
	}
	f.review = api.ReviewBundle{
		Ticket:  core.Ticket{ID: "8ecd21bc", Title: "Add Multiply", State: core.StateReview},
		Project: projGravy,
	}
	f.allTickets = []core.Ticket{
		{ID: "8ecd21bc", ProjectID: projGravy.ID, Title: "Add Multiply", State: core.StateReview},
	}
	return f
}

// mutated reports the service calls that change a ticket. Navigation is a read.
func mutated(f *fakeService) []string {
	var out []string
	for _, c := range [][2]any{
		{"approve", len(f.approved)}, {"reject", len(f.rejected)}, {"resolve", len(f.resolved)},
		{"requestChanges", len(f.changes)}, {"move", len(f.moved)}, {"continue", len(f.continued)},
		{"update", len(f.updated)}, {"kill", len(f.killed)},
	} {
		if c[1].(int) > 0 {
			out = append(out, fmt.Sprintf("%s x%d", c[0], c[1]))
		}
	}
	return out
}

// TestPendingReviewOpensTheReviewCard is the reproduction.
//
// Selecting a pending review on the Dashboard sent the human to Needs You, which — unlike the
// Dashboard — is scoped by the frame's project filter. With the filter anywhere else the
// destination was an empty "Needs You (0)", and the only thing that cleared it was restarting
// Gravy, because a restart resets the filter. The row must open the card for the work it names.
func TestPendingReviewOpensTheReviewCard(t *testing.T) {
	f := reviewFleet()
	m := boot(t, f, 100, 30)

	// Scope the frame at the repository the review is *not* in — the state a restart discards.
	m = send(t, m, key("p"))
	for m.projectName() != "mojo" {
		m = send(t, m, key("p"))
		if m.projectName() == "" {
			t.Fatal("cycled past every project without reaching mojo")
		}
	}

	m = press(t, m, "enter")

	if m.active != SectionReview {
		t.Fatalf("enter on a pending review reached %s, want Review", m.active.Title())
	}
	r, ok := m.screens[SectionReview].(*review)
	if !ok {
		t.Fatalf("Review screen is %T", m.screens[SectionReview])
	}
	if r.ticketID != "8ecd21bc" {
		t.Errorf("the card opened %q, want the selected ticket", r.ticketID)
	}
	view := m.View()
	if !strings.Contains(view, "Add Multiply") {
		t.Errorf("the review card did not render the selected ticket:\n%s", view)
	}
	if strings.Contains(view, "Needs You (0)") {
		t.Errorf("landed on an empty Needs You:\n%s", view)
	}
	if got := mutated(f); got != nil {
		t.Errorf("navigation mutated ticket state: %v", got)
	}
}

// TestNavigationWidensAScopeThatWouldHideTheRow is the other half of the same failure: the
// Dashboard spans every repository, so following one of its rows must not land somewhere scoped
// elsewhere and therefore empty.
func TestNavigationWidensAScopeThatWouldHideTheRow(t *testing.T) {
	f := reviewFleet()
	// A reason with no Review card of its own, so the destination really is Needs You.
	f.status.Attention[0].Attention.Reason = core.ReasonMergeConflict
	f.status.Attention[0].Ticket.State = core.StateNeedsYou

	m := boot(t, f, 100, 30)
	m = send(t, m, key("p"))
	for m.projectName() != "mojo" {
		m = send(t, m, key("p"))
		if m.projectName() == "" {
			t.Fatal("cycled past every project without reaching mojo")
		}
	}
	m = press(t, m, "enter")

	if m.active != SectionNeedsYou {
		t.Fatalf("reached %s, want Needs You", m.active.Title())
	}
	view := m.View()
	if strings.Contains(view, "Needs You (0)") {
		t.Errorf("a project scope hid the row that was just selected:\n%s", view)
	}
	if !strings.Contains(view, "merge_conflict") {
		t.Errorf("the selected row is not on the destination screen:\n%s", view)
	}
}

// TestNavigationUsesIdentityNotPosition covers the row moving under the cursor.
//
// The snapshot is replaced on every push. A cursor that is only a number selects whatever has
// arrived at that position, so an agent finishing while someone reaches for enter would open
// another repository's ticket.
func TestNavigationUsesIdentityNotPosition(t *testing.T) {
	f := reviewFleet()
	m := boot(t, f, 100, 30)

	// Select the running row, which is second.
	m = send(t, m, key("j"))

	// A new attention item arrives above it, pushing every row down one.
	f.status.Attention = append([]api.AttentionItem{{
		Attention: core.Attention{ID: "att-0", ProjectID: projMojo.ID,
			Reason: core.ReasonMergeConflict, TicketID: "zz99"},
		Project: projMojo,
		Ticket:  core.Ticket{ID: "zz99", ProjectID: projMojo.ID, Title: "Rebase fell over"},
	}}, f.status.Attention...)
	m = send(t, m, statusMsg{status: f.status})

	m = press(t, m, "enter")

	if m.active != SectionRunning {
		t.Fatalf("reached %s, want Running: the cursor followed the position, not the row", m.active.Title())
	}
	if m.focus != "c9d4" {
		t.Errorf("opened %q, want the row that was selected before the push", m.focus)
	}
}

// TestSelectedEntryDisappearingBeforeNavigation is the race the ticket names: the work is decided
// between the key press and the answer coming back.
func TestSelectedEntryDisappearingBeforeNavigation(t *testing.T) {
	f := reviewFleet()
	m := boot(t, f, 100, 30)

	// enter asks the frame to open the ticket; the frame then asks the daemon where it is.
	m, cmd := sendCmd(t, m, key("enter"))
	m, cmd = sendCmd(t, m, cmd())

	// Approved from another client in the meantime: no queue entry, and no longer in Review.
	f.status.Attention = nil
	f.allTickets = nil

	m = drive(t, m, cmd)

	if m.active != SectionDashboard {
		t.Fatalf("left the Dashboard for %s with nothing to show there", m.active.Title())
	}
	view := m.View()
	if !strings.Contains(view, "8ecd21bc") || !strings.Contains(view, "left the queue") {
		t.Errorf("the dashboard did not say what happened to the row:\n%s", view)
	}
	// Refreshed, not stale: the row it could not open is gone from the screen too.
	if !strings.Contains(view, "NEEDS YOU (0)") {
		t.Errorf("the dashboard was not refreshed:\n%s", view)
	}
	if got := mutated(f); got != nil {
		t.Errorf("a failed navigation mutated ticket state: %v", got)
	}
}

// TestSelectedEntryChangingBeforeNavigation is the same race with a different outcome: the ticket
// moved rather than finished, so there is somewhere real to send the human.
func TestSelectedEntryChangingBeforeNavigation(t *testing.T) {
	f := reviewFleet()
	m := boot(t, f, 100, 30)

	m, cmd := sendCmd(t, m, key("enter"))
	m, cmd = sendCmd(t, m, cmd())

	// Sent back to the agent from another client: the review is gone and the work is running.
	f.status.Attention = nil
	f.allTickets = nil
	f.status.Running = append(f.status.Running, api.RunningTicket{
		Ticket:  core.Ticket{ID: "8ecd21bc", ProjectID: projGravy.ID, Branch: "gravy/8ecd21bc-multiply"},
		Project: projGravy, Run: core.Run{ProviderID: "claude-code", Model: "sonnet", HostID: "local"},
		Activity: "implementing",
	})

	m = drive(t, m, cmd)

	if m.active != SectionRunning {
		t.Fatalf("reached %s, want Running: the ticket moved before navigation completed", m.active.Title())
	}
	if m.focus != "8ecd21bc" {
		t.Errorf("focused %q, want the selected ticket", m.focus)
	}
}

// TestResolvedAttentionStillOpensTheCard covers the entry being resolved while the work is not.
//
// Acknowledging or recovering an attention row empties the queue without deciding anything, and
// the ticket sits in Review with nothing naming it. The snapshot alone would call that gone.
func TestResolvedAttentionStillOpensTheCard(t *testing.T) {
	f := reviewFleet()
	m := boot(t, f, 100, 30)

	m, cmd := sendCmd(t, m, key("enter"))
	m, cmd = sendCmd(t, m, cmd())

	f.status.Attention = nil // the queue entry went; the ticket did not

	m = drive(t, m, cmd)

	if m.active != SectionReview {
		t.Fatalf("reached %s, want Review", m.active.Title())
	}
	if !strings.Contains(m.View(), "Add Multiply") {
		t.Errorf("the card did not open for a ticket still awaiting review:\n%s", m.View())
	}
}

// TestStaleReviewReasonFollowsTheTicketState covers the attention row outliving the decision it
// was raised for: approved from a second client, the ticket is landing while the row still reads
// review_pending. The state is what the ticket is in; the reason is only what the row was for.
func TestStaleReviewReasonFollowsTheTicketState(t *testing.T) {
	f := reviewFleet()
	f.status.Attention[0].Ticket.State = core.StateLanding
	f.status.Running = append(f.status.Running, api.RunningTicket{
		Ticket:  core.Ticket{ID: "8ecd21bc", ProjectID: projGravy.ID, Branch: "gravy/8ecd21bc-multiply", State: core.StateLanding},
		Project: projGravy, Run: core.Run{ProviderID: "claude-code", Model: "sonnet", HostID: "local"},
		Activity: "landing",
	})

	m := boot(t, f, 100, 30)
	m = press(t, m, "enter")

	if m.active != SectionRunning {
		t.Fatalf("a stale review_pending row reached %s, want Running: the ticket is already landing",
			m.active.Title())
	}
	if m.focus != "8ecd21bc" {
		t.Errorf("focused %q, want the selected ticket", m.focus)
	}
	if got := mutated(f); got != nil {
		t.Errorf("navigation mutated ticket state: %v", got)
	}
}

// TestStaleReviewReasonOnAFinishedTicketStaysPut is the same staleness with nowhere to go: the
// work is done, so there is no card and no queue row. The human stays on a refreshed Dashboard
// rather than on a Review card for a decision already made.
func TestStaleReviewReasonOnAFinishedTicketStaysPut(t *testing.T) {
	f := reviewFleet()
	f.status.Attention[0].Ticket.State = core.StateDone
	f.allTickets = nil // nothing is awaiting review any more

	m := boot(t, f, 100, 30)
	m = press(t, m, "enter")

	if m.active != SectionDashboard {
		t.Fatalf("a stale review_pending row over finished work reached %s, want the Dashboard",
			m.active.Title())
	}
	view := m.View()
	if !strings.Contains(view, "8ecd21bc") || !strings.Contains(view, "left the queue") {
		t.Errorf("the dashboard did not say what happened to the row:\n%s", view)
	}
	if got := mutated(f); got != nil {
		t.Errorf("navigation mutated ticket state: %v", got)
	}
}

// TestReviewReasonWithoutATicketStillOpensTheCard is the other side of the same check: when the
// snapshot does not carry the ticket there is no state to be authoritative, and the reason is all
// the frame has. It must still open the card.
func TestReviewReasonWithoutATicketStillOpensTheCard(t *testing.T) {
	f := reviewFleet()
	f.status.Attention[0].Ticket = core.Ticket{} // the snapshot carried the row, not the ticket

	m := boot(t, f, 100, 30)
	m = press(t, m, "enter")

	if m.active != SectionReview {
		t.Fatalf("reached %s, want Review", m.active.Title())
	}
	r, ok := m.screens[SectionReview].(*review)
	if !ok {
		t.Fatalf("Review screen is %T", m.screens[SectionReview])
	}
	if r.ticketID != "8ecd21bc" {
		t.Errorf("the card opened %q, want the selected ticket", r.ticketID)
	}
	if got := mutated(f); got != nil {
		t.Errorf("navigation mutated ticket state: %v", got)
	}
}

// TestFailedResolveRetriesWithoutRestarting is the first half of "recover inside the TUI": the
// lookup that tells the frame where to go fails.
func TestFailedResolveRetriesWithoutRestarting(t *testing.T) {
	f := reviewFleet()
	m := boot(t, f, 100, 30)

	f.statusE = fmt.Errorf("dial unix /run/gravy.sock: connect: connection refused")
	m = press(t, m, "enter")

	if m.active != SectionDashboard {
		t.Fatalf("a failed lookup navigated to %s anyway", m.active.Title())
	}
	view := m.View()
	if !strings.Contains(view, "could not open") || !strings.Contains(view, "connection refused") {
		t.Errorf("the dashboard did not report the failure:\n%s", view)
	}
	if !strings.Contains(view, "enter retries") {
		t.Errorf("the dashboard did not say how to recover:\n%s", view)
	}

	// The same keystroke, in the same process, once the daemon answers again.
	f.statusE = nil
	m = press(t, m, "enter")

	if m.active != SectionReview {
		t.Fatalf("the retry reached %s, want Review", m.active.Title())
	}
	if !strings.Contains(m.View(), "Add Multiply") {
		t.Errorf("the retry did not open the card:\n%s", m.View())
	}
}

// TestFailedFallbackLookupRetriesWithoutRestarting covers the half-failure between the two:
// the snapshot arrives and does not carry the ticket, and the one call that could tell the
// difference between "resolved elsewhere, still in Review" and "gone" fails.
//
// Nothing is known at that point, so the frame must not spend the snapshot: adopting it refreshed
// the row away and told the human the work had left the queue, on the strength of a read that
// never completed — leaving a dashboard with nothing to press and a restart as the only way back.
func TestFailedFallbackLookupRetriesWithoutRestarting(t *testing.T) {
	f := reviewFleet()
	m := boot(t, f, 100, 30)

	// The queue entry is gone from the snapshot — acknowledged elsewhere — and the question that
	// would say whether the ticket is still in Review cannot be asked.
	f.status.Attention = nil
	f.listTicketsErr = fmt.Errorf("rpc: connection reset by peer")

	m = press(t, m, "enter")

	if m.active != SectionDashboard {
		t.Fatalf("an unanswered lookup navigated to %s anyway", m.active.Title())
	}
	view := m.View()
	if !strings.Contains(view, "could not open") || !strings.Contains(view, "connection reset by peer") {
		t.Errorf("the dashboard did not report the failure:\n%s", view)
	}
	if !strings.Contains(view, "enter retries") {
		t.Errorf("the dashboard did not say how to recover:\n%s", view)
	}
	if strings.Contains(view, "left the queue") {
		t.Errorf("a failed read was reported as the work finishing:\n%s", view)
	}
	// The row the human just pressed is still there to press again.
	if !strings.Contains(view, "Add Multiply") {
		t.Errorf("the dashboard dropped the row on a failed read:\n%s", view)
	}

	// The same keystroke, in the same process, once the daemon answers again.
	f.listTicketsErr = nil
	m = press(t, m, "enter")

	if m.active != SectionReview {
		t.Fatalf("the retry reached %s, want Review", m.active.Title())
	}
	if !strings.Contains(m.View(), "Add Multiply") {
		t.Errorf("the retry did not open the card:\n%s", m.View())
	}
	if got := mutated(f); got != nil {
		t.Errorf("navigation mutated ticket state: %v", got)
	}
}

// TestFailedReviewLoadRetriesWithoutRestarting is the second half: the frame gets where it is
// going and the card itself will not load. That screen used to take no keys at all.
func TestFailedReviewLoadRetriesWithoutRestarting(t *testing.T) {
	f := reviewFleet()
	f.reviewErr = fmt.Errorf("read diff: unexpected EOF")
	m := boot(t, f, 100, 30)

	m = press(t, m, "enter")
	if m.active != SectionReview {
		t.Fatalf("reached %s, want Review", m.active.Title())
	}
	view := m.View()
	for _, want := range []string{"Could not load the review", "unexpected EOF", "enter retries"} {
		if !strings.Contains(view, want) {
			t.Errorf("the failed card omits %q:\n%s", want, view)
		}
	}

	f.reviewErr = nil
	m = press(t, m, "enter")

	view = m.View()
	if strings.Contains(view, "Could not load the review") {
		t.Errorf("the retry did not clear the error:\n%s", view)
	}
	if !strings.Contains(view, "Add Multiply") {
		t.Errorf("the retry did not load the card:\n%s", view)
	}
	if got := mutated(f); got != nil {
		t.Errorf("recovering from a failed load mutated ticket state: %v", got)
	}
}
