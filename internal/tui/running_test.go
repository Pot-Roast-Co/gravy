package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// runningFixture is a ticket on its second attempt, with the first one's failure recorded.
func runningFixture() *fakeService {
	f := newFake()
	f.status.Running = []api.RunningTicket{{
		Ticket:  core.Ticket{ID: "c9d4aa01", Title: "Add a Divide function", Branch: "gravy/c9d4-divide"},
		Project: projGravy,
		Run:     core.Run{ID: "run-2", ProviderID: "claude-code", Model: "sonnet", HostID: "local"},
		Elapsed: 3 * time.Minute, Activity: "implementing",
	}}
	ended := time.Now().Add(-4 * time.Minute)
	f.runs = []core.Run{
		{ID: "run-2", ProviderID: "claude-code", Model: "sonnet", HostID: "local",
			Turns: 5, TokensIn: 1200, TokensOut: 340},
		{ID: "run-1", ProviderID: "claude-code", Model: "sonnet", HostID: "local",
			EndedAt:      &ended,
			FailureClass: core.TaskFailure, FailureNote: "go test ./... failed: divide_test.go:12"},
	}
	f.explain = api.Explanation{
		TicketID: "c9d4aa01", Eligible: true,
		Why: []string{
			`route "implementation" resolved to claude-code/sonnet`,
			"host local chosen: idle",
		},
	}
	f.logs = make(chan api.LogLine, 64)
	return f
}

// openRunning boots the frame onto a loaded run detail.
func openRunning(t *testing.T, f *fakeService) Model {
	t.Helper()
	m := boot(t, f, 90, 26)
	m = send(t, m, key(SectionRunning.Key()))
	m = send(t, m, enteredMsg{focus: "c9d4aa01"})
	m = send(t, m, runDetailMsg{runs: f.runs, explain: f.explain})
	return m
}

// TestRouteTraceIsVisible is AC3: why this provider, this model, this host.
func TestRouteTraceIsVisible(t *testing.T) {
	m := openRunning(t, runningFixture())
	view := m.View()

	for _, want := range []string{"claude-code/sonnet@local", "resolved to claude-code/sonnet", "host local chosen"} {
		if !strings.Contains(view, want) {
			t.Errorf("view omits %q:\n%s", want, view)
		}
	}
}

// TestRetriesShowTheirReasons is AC4. A retry count with no story tells you nothing about
// whether trying again was reasonable.
func TestRetriesShowTheirReasons(t *testing.T) {
	m := openRunning(t, runningFixture())
	view := m.View()

	if !strings.Contains(view, "attempt 2") {
		t.Errorf("the attempt count is not shown:\n%s", view)
	}
	if !strings.Contains(view, "task_failure") {
		t.Errorf("the earlier attempt's class is not shown:\n%s", view)
	}
	// The attempt still going must not report itself as having succeeded, which is what the
	// zero value of FailureClass would otherwise say.
	if !strings.Contains(view, "in flight") {
		t.Errorf("the current attempt is not marked as still running:\n%s", view)
	}
	if !strings.Contains(view, "divide_test.go") {
		t.Errorf("the earlier attempt's reason is not shown:\n%s", view)
	}
}

// TestLogFollowsAndPauses is AC1.
func TestLogFollowsAndPauses(t *testing.T) {
	f := runningFixture()
	m := openRunning(t, f)

	// The stream opens and lines arrive.
	for i := 0; i < 40; i++ {
		m = send(t, m, logLineMsg{line: api.LogLine{Text: fmt.Sprintf("line %d", i), Stream: "agent"}})
	}
	view := m.View()
	if !strings.Contains(view, "following") {
		t.Errorf("the tail is not following by default:\n%s", view)
	}
	if !strings.Contains(view, "line 39") {
		t.Errorf("the newest line is not visible while following:\n%s", view)
	}

	// Scrolling back pauses, and shows older output.
	for i := 0; i < 10; i++ {
		m = send(t, m, key("k"))
	}
	view = m.View()
	if !strings.Contains(view, "paused") {
		t.Errorf("scrolling back did not pause the tail:\n%s", view)
	}
	if strings.Contains(view, "line 39") {
		t.Errorf("the view is still pinned to the tail after scrolling back:\n%s", view)
	}

	// New output must not yank the view while paused.
	m = send(t, m, logLineMsg{line: api.LogLine{Text: "line 40", Stream: "agent"}})
	if strings.Contains(m.View(), "line 40") {
		t.Errorf("a new line dragged a paused view back to the tail:\n%s", m.View())
	}

	// G returns to the live tail.
	m = send(t, m, key("G"))
	view = m.View()
	if !strings.Contains(view, "following") || !strings.Contains(view, "line 40") {
		t.Errorf("G did not return to the tail:\n%s", view)
	}
}

// TestKillConfirmsFirst is AC2. Killing frees a worker and destroys in-progress work, so it is
// not one keystroke away from the scroll keys.
func TestKillConfirmsFirst(t *testing.T) {
	f := runningFixture()
	m := openRunning(t, f)

	m, cmd := sendCmd(t, m, key("K"))
	if cmd != nil {
		t.Fatal("K killed the run with no confirmation")
	}
	if !strings.Contains(m.View(), "kill this run") {
		t.Fatalf("K did not ask for confirmation:\n%s", m.View())
	}
	if len(f.killed) != 0 {
		t.Fatal("the run was killed before confirming")
	}

	// Declining backs out.
	m, cmd = sendCmd(t, m, key("n"))
	if cmd != nil || len(f.killed) != 0 {
		t.Fatal("declining still killed the run")
	}

	m = send(t, m, key("K"))
	m, cmd = sendCmd(t, m, key("y"))
	if cmd == nil {
		t.Fatal("confirming did not kill")
	}
	m = send(t, m, cmd())

	if len(f.killed) != 1 || f.killed[0] != "run-2" {
		t.Errorf("killed = %v, want the current run", f.killed)
	}
	if !strings.Contains(m.View(), "worker slot is free") {
		t.Errorf("the kill was not reported:\n%s", m.View())
	}
}

// TestNothingRunningGuides is the empty state.
func TestNothingRunningGuides(t *testing.T) {
	m := boot(t, newFake(), 90, 26)
	m = send(t, m, key(SectionRunning.Key()))
	m = send(t, m, enteredMsg{})
	if !strings.Contains(m.View(), "Nothing running") {
		t.Errorf("empty running screen is not explained:\n%s", m.View())
	}
}

// TestRunEndingIsReported: a tail that simply stops looks identical to one that is idle.
func TestRunEndingIsReported(t *testing.T) {
	m := openRunning(t, runningFixture())
	m = send(t, m, logClosedMsg{})
	if !strings.Contains(m.View(), "the run ended") {
		t.Errorf("the end of the stream is not reported:\n%s", m.View())
	}
}

// TestScrollbackIsBounded keeps a chatty run from growing the TUI without limit.
func TestScrollbackIsBounded(t *testing.T) {
	f := runningFixture()
	m := openRunning(t, f)
	for i := 0; i < maxLogLines+500; i++ {
		m = send(t, m, logLineMsg{line: api.LogLine{Text: fmt.Sprintf("l%d", i)}})
	}
	scr, ok := m.screens[SectionRunning].(*running)
	if !ok {
		t.Fatal("running screen is not registered")
	}
	if len(scr.lines) > maxLogLines {
		t.Errorf("kept %d lines, want at most %d", len(scr.lines), maxLogLines)
	}
	if !strings.Contains(m.View(), fmt.Sprintf("l%d", maxLogLines+499)) {
		t.Error("the newest line was dropped instead of the oldest")
	}
}
