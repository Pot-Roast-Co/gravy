package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

var (
	projGravy = core.Project{ID: "p1", Name: "gravy", Slug: "gravy", TargetBranch: "main"}
	projMojo  = core.Project{ID: "p2", Name: "mojo", Slug: "mojo"}
	projHerdr = core.Project{ID: "p3", Name: "herdr", Slug: "herdr"}
)

// populated returns a fleet with all three sections non-empty, across three repositories.
func populated() *fakeService {
	f := newFake()
	f.status.Projects = append(f.status.Projects,
		api.ProjectStatus{Project: projHerdr})
	f.status.Attention = []api.AttentionItem{
		{Attention: core.Attention{ID: "a1", Reason: core.ReasonReviewPending, TicketID: "8ecd21bc"},
			Project: projGravy, Ticket: core.Ticket{ID: "8ecd21bc", Title: "Add Multiply"}, Age: 4 * time.Minute},
	}
	f.status.Running = []api.RunningTicket{
		{Ticket: core.Ticket{ID: "c9d4", Branch: "gravy/c9d4-dashboard"}, Project: projMojo,
			Run:     core.Run{ProviderID: "claude-code", Model: "sonnet", HostID: "local"},
			Elapsed: 3 * time.Minute, Activity: "implementing"},
	}
	f.status.Ready = []api.QueuedTicket{
		{Ticket: core.Ticket{ID: "f196d04b", Title: "Add Divide"}, Project: projHerdr,
			Held: "serialized; c9d4 in flight (running)"},
	}
	return f
}

// TestSectionOrderIsFixed is AC1. The order answers "what needs me" before "what is happening",
// and it is not configurable.
func TestSectionOrderIsFixed(t *testing.T) {
	m := boot(t, populated(), 100, 30)
	view := m.View()

	needs := strings.Index(view, "NEEDS YOU")
	running := strings.Index(view, "RUNNING")
	ready := strings.Index(view, "READY")

	if needs < 0 || running < 0 || ready < 0 {
		t.Fatalf("a section is missing entirely:\n%s", view)
	}
	if needs >= running || running >= ready {
		t.Errorf("sections are out of order (needs=%d running=%d ready=%d)", needs, running, ready)
	}
}

// TestSectionHeadersCarryCounts is the rest of AC1: a header without a count makes you read the
// rows to learn how much there is.
func TestSectionHeadersCarryCounts(t *testing.T) {
	m := boot(t, populated(), 100, 30)
	view := m.View()
	for _, want := range []string{"NEEDS YOU (1)", "RUNNING (1)", "READY (1)"} {
		if !strings.Contains(view, want) {
			t.Errorf("view omits %q:\n%s", want, view)
		}
	}
}

// TestEveryProjectAppearsOnOneScreen is AC2, and the point of the whole screen: three active
// repositories are visible without switching context.
func TestEveryProjectAppearsOnOneScreen(t *testing.T) {
	m := boot(t, populated(), 100, 30)
	view := m.View()
	for _, name := range []string{"gravy", "mojo", "herdr"} {
		if !strings.Contains(view, name) {
			t.Errorf("project %q is not on the dashboard:\n%s", name, view)
		}
	}
	// The running row must name its branch, which is how you tell which worktree is which.
	if !strings.Contains(view, "c9d4-dashboard") {
		t.Errorf("the running row does not name its branch:\n%s", view)
	}
}

// TestRunningRowNamesProviderModelAndHost is AC3.
func TestRunningRowNamesProviderModelAndHost(t *testing.T) {
	m := boot(t, populated(), 120, 30)
	view := m.View()
	if !strings.Contains(view, "claude-code/sonnet@local") {
		t.Errorf("the running row does not say which agent on which host:\n%s", view)
	}
}

// TestEnterOpensTheRightScreen is AC4.
//
// Asserted on the active section rather than on the rendered title: every section's name is in
// the header on every frame, so a view that mentions "Needs You" is not evidence of arriving
// there. A pending review opens its card directly — the queue is where it is listed, not where
// it is decided.
func TestEnterOpensTheRightScreen(t *testing.T) {
	tests := []struct {
		name  string
		downs int
		want  Section
		focus string
	}{
		{"pending review row", 0, SectionReview, "8ecd21bc"},
		{"running row", 1, SectionRunning, "c9d4"},
		{"ready row", 2, SectionReady, "f196d04b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := boot(t, populated(), 100, 30)
			for i := 0; i < tc.downs; i++ {
				m = send(t, m, key("j"))
			}
			m, cmd := sendCmd(t, m, key("enter"))
			if cmd == nil {
				t.Fatal("enter produced no command")
			}
			m = drive(t, m, cmd)
			if m.active != tc.want {
				t.Errorf("enter opened %s, want %s", m.active.Title(), tc.want.Title())
			}
			if m.focus != tc.focus {
				t.Errorf("enter focused %q, want %q", m.focus, tc.focus)
			}
		})
	}
}

// TestHeldTicketSaysWhy is AC8. A ticket the cap is holding back must not read as an
// unexplained absence from Running.
func TestHeldTicketSaysWhy(t *testing.T) {
	m := boot(t, populated(), 100, 30)
	view := m.View()
	if !strings.Contains(view, "held") || !strings.Contains(view, "serialized") {
		t.Errorf("a held ticket does not explain itself:\n%s", view)
	}
}

// TestEmptyStatesGuide is AC7: an empty section says what to do next, not "0 results".
func TestEmptyStatesGuide(t *testing.T) {
	m := boot(t, newFake(), 100, 30)
	view := m.View()
	for _, want := range []string{
		"Gravy does not need you",
		"no agents working",
		"gravy ticket add",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("empty state omits guidance %q:\n%s", want, view)
		}
	}
}

// TestNoProjectsTellsYouHowToStart is the emptiest state of all.
func TestNoProjectsTellsYouHowToStart(t *testing.T) {
	f := newFake()
	f.status.Projects = nil
	m := boot(t, f, 100, 30)
	m = send(t, m, agentsDetectedMsg{agents: []api.AgentStatus{
		{ProviderID: "claude-code", Installed: true, Authenticated: true},
	}})
	view := m.View()

	// A fresh install gets told what Gravy is and what to press, rather than an empty
	// dashboard with nothing on it.
	for _, want := range []string{"engineering manager", "P", "Review", "without your approval"} {
		if !strings.Contains(view, want) {
			t.Errorf("a fresh install is not told how to start — missing %q:\n%s", want, view)
		}
	}
}

// TestLongQueueScrollsWithoutOverflowing is AC6, and the adversarial case: a queue longer than
// the terminal must scroll, report what it hid, and never push the status bar off the screen.
func TestLongQueueScrollsWithoutOverflowing(t *testing.T) {
	f := populated()
	for i := 0; i < 60; i++ {
		f.status.Ready = append(f.status.Ready, api.QueuedTicket{
			Ticket:  core.Ticket{ID: fmt.Sprintf("t%02d", i), Title: fmt.Sprintf("queued work %d", i)},
			Project: projGravy,
		})
	}

	m := boot(t, f, 80, 24)
	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) != 24 {
		t.Fatalf("rendered %d lines into a 24-line terminal", len(lines))
	}
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w > 80 {
			t.Errorf("line %d overflows: %d cells", i, w)
		}
	}
	if !strings.Contains(view, "more") {
		t.Errorf("a truncated queue does not say how much it hid:\n%s", view)
	}
	// The status bar survives at the bottom.
	if !strings.Contains(lines[len(lines)-1], "workers") {
		t.Errorf("the status bar was pushed off the screen: %q", lines[len(lines)-1])
	}

	// Driving the cursor to the end must scroll it into view, not run off the bottom.
	m = send(t, m, key("G"))
	view = m.View()
	if !strings.Contains(view, "▸") {
		t.Errorf("the cursor is not visible after jumping to the end:\n%s", view)
	}
	if n := len(strings.Split(view, "\n")); n != 24 {
		t.Errorf("scrolled view is %d lines, want 24", n)
	}
}

// TestDashboardRedrawsOnPush is AC5 at the screen level: the frame refreshes, and the dashboard
// reflects it without being asked.
func TestDashboardRedrawsOnPush(t *testing.T) {
	f := populated()
	m := boot(t, f, 100, 30)
	if strings.Contains(m.View(), "merge_conflict") {
		t.Fatal("the fixture already had a conflict")
	}

	f.status.Attention = append(f.status.Attention, api.AttentionItem{
		Attention: core.Attention{ID: "a2", Reason: core.ReasonMergeConflict, TicketID: "zz99"},
		Project:   projMojo, Ticket: core.Ticket{ID: "zz99", Title: "Rebase fell over"},
	})
	m, cmd := sendCmd(t, m, eventMsg{event: api.Event{Kind: api.EventAttentionChanged}})
	if cmd == nil {
		t.Fatal("a push produced no refresh")
	}
	m = send(t, m, statusMsg{status: f.status})

	view := m.View()
	if !strings.Contains(view, "merge_conflict") {
		t.Errorf("the dashboard did not pick up the pushed change:\n%s", view)
	}
	if !strings.Contains(view, "NEEDS YOU (2)") {
		t.Errorf("the section count did not follow the push:\n%s", view)
	}
}

// TestCursorSurvivesAShrinkingQueue covers the crash nobody writes a test for: rows disappearing
// under a cursor that was pointing at the last one.
func TestCursorSurvivesAShrinkingQueue(t *testing.T) {
	f := populated()
	m := boot(t, f, 100, 30)
	m = send(t, m, key("G")) // last row

	f.status.Attention, f.status.Running, f.status.Ready = nil, nil, nil
	m = send(t, m, statusMsg{status: f.status})

	view := m.View() // must not panic, and must render the empty states
	if !strings.Contains(view, "Gravy does not need you") {
		t.Errorf("emptying the fleet under the cursor lost the empty state:\n%s", view)
	}

	if _, cmd := sendCmd(t, m, key("enter")); cmd != nil {
		if _, ok := cmd().(gotoMsg); ok {
			t.Error("enter navigated using a row that no longer exists")
		}
	}
}

// TestRunningRowShowsActivityAndSilence: the row carries the journal's sentence rather than a
// state name, wide enough to read, and an agent gone quiet is flagged on its own.
func TestRunningRowShowsActivityAndSilence(t *testing.T) {
	f := populated()
	f.status.Running[0].Activity = "running test (go test ./...), step 3 of 3 — no output for 6m"
	m := boot(t, f, 140, 30)
	view := m.View()

	if !strings.Contains(view, "running test (go test ./...)") {
		t.Errorf("the activity is cut down to a state-name width:\n%s", view)
	}
	if !strings.Contains(view, "no output 6m") {
		t.Errorf("the silence is not flagged:\n%s", view)
	}
	if strings.Contains(view, "no output for 6m") {
		t.Errorf("the silence is left inside the activity rather than drawn as a warning:\n%s", view)
	}
}
