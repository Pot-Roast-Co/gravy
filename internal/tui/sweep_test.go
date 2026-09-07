package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bobbybrady/gravy/internal/api"
	"github.com/bobbybrady/gravy/internal/core"
)

// sweepFixture is five pending reviews across two projects, one of which is holding its queue.
func sweepFixture() *fakeService {
	f := newFake()
	f.status.Projects = []api.ProjectStatus{
		{Project: projGravy}, // not blocked
		{Project: projMojo, Blocked: "serialized; m1 awaiting your review (review)"},
	}
	add := func(id, project string, p core.Project, age time.Duration) {
		f.status.Attention = append(f.status.Attention, api.AttentionItem{
			Attention: core.Attention{ID: "att-" + id, Reason: core.ReasonReviewPending,
				TicketID: id, ProjectID: project},
			Project: p, Ticket: core.Ticket{ID: id, Title: "work " + id}, Age: age,
		})
	}
	add("g1", projGravy.ID, projGravy, 5*time.Hour)
	add("m1", projMojo.ID, projMojo, 4*time.Hour)
	add("g2", projGravy.ID, projGravy, 3*time.Hour)
	add("m2", projMojo.ID, projMojo, 2*time.Hour)
	add("g3", projGravy.ID, projGravy, time.Hour)
	return f
}

// startSweep enters the sweep from the dashboard and loads the first card.
func startSweep(t *testing.T, f *fakeService) Model {
	t.Helper()
	m := boot(t, f, 90, 26)
	m, cmd := sendCmd(t, m, key("S"))
	if cmd == nil {
		t.Fatal("S did not start a sweep")
	}
	m = send(t, m, cmd())
	// The frame switches to Review and the screen loads the first ticket.
	m = send(t, m, reviewLoadedMsg{bundle: bundleFor(f, "m1")})
	return m
}

func bundleFor(f *fakeService, id string) api.ReviewBundle {
	b := f.review
	b.Ticket = core.Ticket{ID: id, Title: "work " + id, State: core.StateReview}
	return b
}

// TestSweepOrdersBlockingRepositoriesFirst is AC3.
func TestSweepOrdersBlockingRepositoriesFirst(t *testing.T) {
	f := sweepFixture()
	m := boot(t, f, 90, 26)
	got := sweepOrder(m.viewContext())

	if len(got) != 5 {
		t.Fatalf("sweep covers %d tickets, want 5: %v", len(got), got)
	}
	// mojo is holding its queue, so its reviews come first.
	if got[0] != "m1" || got[1] != "m2" {
		t.Errorf("order = %v, want the blocked repository's reviews first", got)
	}
	// Within each group, oldest first.
	if got[2] != "g1" || got[3] != "g2" || got[4] != "g3" {
		t.Errorf("order = %v, want oldest first within each group", got)
	}
}

// TestSweepVisitsEveryReviewOnceAndExits is AC1.
func TestSweepVisitsEveryReviewOnceAndExits(t *testing.T) {
	f := sweepFixture()
	m := startSweep(t, f)

	if !strings.Contains(m.View(), "sweep 1 of 5") {
		t.Fatalf("no progress indicator:\n%s", m.View())
	}

	order := sweepOrder(m.viewContext())
	for i := range order {
		if want := fmt.Sprintf("sweep %d of 5", i+1); !strings.Contains(m.View(), want) {
			t.Fatalf("at step %d the indicator is wrong:\n%s", i+1, m.View())
		}
		m, cmd := sendCmd(t, m, key("a"))
		if cmd == nil {
			t.Fatalf("approve produced no command at step %d", i+1)
		}
		m = send(t, m, cmd())
		if i+1 < len(order) {
			m = send(t, m, reviewLoadedMsg{bundle: bundleFor(f, order[i+1])})
		}
		_ = m
	}

	// Every ticket approved exactly once, in sweep order.
	if len(f.approved) != 5 {
		t.Fatalf("approved %d tickets, want 5: %v", len(f.approved), f.approved)
	}
	for i, id := range order {
		if f.approved[i] != id {
			t.Errorf("approved[%d] = %q, want %q", i, f.approved[i], id)
		}
	}
	if !strings.Contains(m.View(), "sweep finished") {
		t.Errorf("the sweep did not exit cleanly:\n%s", m.View())
	}
	if strings.Contains(m.View(), "sweep 5 of 5") {
		t.Errorf("the sweep is still running after the last ticket:\n%s", m.View())
	}
}

// TestSweepApprovesThroughTheSameCodePath is AC2: the sweep is a mode over the review card, not
// a fork of it, so approval semantics cannot drift between the two.
func TestSweepApprovesThroughTheSameCodePath(t *testing.T) {
	f := sweepFixture()

	// Approving from the plain review screen.
	direct := boot(t, f, 90, 26)
	direct = send(t, direct, key("5"))
	direct = send(t, direct, enteredMsg{focus: "m1"})
	direct = send(t, direct, reviewLoadedMsg{bundle: bundleFor(f, "m1")})
	_, dcmd := sendCmd(t, direct, key("a"))
	if dcmd == nil {
		t.Fatal("approve from the review screen produced no command")
	}
	send(t, direct, dcmd())
	fromScreen := append([]string{}, f.approved...)

	// And from inside a sweep.
	f.approved = nil
	m := startSweep(t, f)
	_, scmd := sendCmd(t, m, key("a"))
	if scmd == nil {
		t.Fatal("approve inside the sweep produced no command")
	}
	send(t, m, scmd())

	if len(fromScreen) != 1 || len(f.approved) != 1 || fromScreen[0] != f.approved[0] {
		t.Errorf("sweep approved %v, screen approved %v — they must be the same call",
			f.approved, fromScreen)
	}
}

// TestSweepSkipLeavesTheTicketAlone covers `s`.
func TestSweepSkipLeavesTheTicketAlone(t *testing.T) {
	f := sweepFixture()
	m := startSweep(t, f)

	m = send(t, m, key("s"))
	if len(f.approved) != 0 || len(f.rejected) != 0 || len(f.changes) != 0 {
		t.Error("skipping decided the ticket")
	}
	if !strings.Contains(m.View(), "sweep 2 of 5") {
		t.Errorf("skip did not advance:\n%s", m.View())
	}
}

// TestExitingMidSweepLeavesTheRestUntouched is AC5.
func TestExitingMidSweepLeavesTheRestUntouched(t *testing.T) {
	f := sweepFixture()
	m := startSweep(t, f)

	m, cmd := sendCmd(t, m, key("a"))
	m = send(t, m, cmd())
	m = send(t, m, reviewLoadedMsg{bundle: bundleFor(f, "m2")})

	m = send(t, m, key("q")) // exit the sweep, not the program

	if len(f.approved) != 1 {
		t.Errorf("approved = %v, want only the one decided before exiting", f.approved)
	}
	if !strings.Contains(m.View(), "4 left untouched") {
		t.Errorf("exiting did not report what was left:\n%s", m.View())
	}
	if strings.Contains(m.View(), "sweep 2 of 5") {
		t.Errorf("the sweep is still running after q:\n%s", m.View())
	}
}

// TestSweepExitDoesNotQuitTheProgram is the reason screens capture keys: `q` is the global quit
// key, and the sweep binds it to exit.
func TestSweepExitDoesNotQuitTheProgram(t *testing.T) {
	f := sweepFixture()
	m := startSweep(t, f)

	_, cmd := sendCmd(t, m, key("q"))
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatal("q inside a sweep quit the program instead of the sweep")
		}
	}
}

// TestNoBulkApproveAffordance is AC6, asserted rather than assumed: one keystroke approves one
// ticket, and nothing on the screen offers otherwise.
func TestNoBulkApproveAffordance(t *testing.T) {
	f := sweepFixture()
	m := startSweep(t, f)

	m, cmd := sendCmd(t, m, key("a"))
	m = send(t, m, cmd())
	if len(f.approved) != 1 {
		t.Fatalf("one keystroke approved %d tickets", len(f.approved))
	}

	view := m.View()
	for _, forbidden := range []string{"approve all", "bulk", "approve remaining"} {
		if strings.Contains(strings.ToLower(view), forbidden) {
			t.Errorf("the sweep offers a bulk affordance (%q):\n%s", forbidden, view)
		}
	}
}

// TestTypingIntoAPromptDoesNotQuit is the bug the sweep exposed: the frame handled `q` before
// the screen saw it, so typing a "q" into a feedback prompt quit the program.
func TestTypingIntoAPromptDoesNotQuit(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 80, 24)
	m = send(t, m, key("r")) // feedback prompt

	m, cmd := sendCmd(t, m, key("q"))
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatal("typing q into the feedback prompt quit the program")
		}
	}
	if !strings.Contains(m.View(), "what needs to change: q") {
		t.Errorf("the prompt did not receive the q:\n%s", m.View())
	}

	// ctrl+c must still work, whatever has the keyboard.
	_, cmd = sendCmd(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c produced no command while a prompt was open")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Error("ctrl+c did not quit while a prompt was open")
	}
}

// TestTypingATicketTitleDoesNotJumpSections is the same bug on the queue form.
func TestTypingATicketTitleDoesNotJumpSections(t *testing.T) {
	f := backlogFixture()
	m := openBacklog(t, f)
	m = send(t, m, key("n"))

	for _, c := range "q1 fix" { // a q and a section digit
		m = send(t, m, key(string(c)))
	}
	view := m.View()
	if !strings.Contains(view, "New ticket") {
		t.Fatalf("typing into the form left it:\n%s", view)
	}
	if !strings.Contains(view, "q1 fix") {
		t.Errorf("the form did not receive the keystrokes:\n%s", view)
	}
}
