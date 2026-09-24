package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider"
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

// TestAgentOutputEndingIsReported: a tail that simply stops looks identical to one that is idle,
// but the agent finishing is not the ticket finishing, so it asks again rather than declaring
// the run over.
func TestAgentOutputEndingIsReported(t *testing.T) {
	m := openRunning(t, runningFixture())
	m = send(t, m, logLineMsg{runID: "run-2", line: api.LogLine{Text: "last words", Stream: "agent"}})
	m, cmd := sendCmd(t, m, logClosedMsg{runID: "run-2"})
	view := m.View()
	if !strings.Contains(view, "agent output ended") {
		t.Errorf("the end of the stream is not reported:\n%s", view)
	}
	if strings.Contains(view, "the run ended") {
		t.Errorf("the end of the agent's output is reported as the end of the run:\n%s", view)
	}
	if cmd == nil {
		t.Error("the stream closing did not re-read the ticket to learn what comes next")
	}
}

var (
	t0      = time.Date(2026, 9, 23, 14, 0, 0, 0, time.UTC)
	fetched = core.Progress{TicketID: "c9d4aa01", At: t0, Phase: core.PhaseFetch,
		Detail: "fetching origin so the work starts on top of the latest main"}
	cut = core.Progress{TicketID: "c9d4aa01", At: t0.Add(2 * time.Second), Phase: core.PhaseWorktree,
		Detail: "worktree cut at /tmp/wt on gravy/c9d4-divide, based on origin/main"}
	started = core.Progress{TicketID: "c9d4aa01", RunID: "run-2", At: t0.Add(5 * time.Second),
		Phase: core.PhaseAgentStart, Detail: "claude-code/sonnet started (pid 42), attempt 2 of 3"}
	exited = core.Progress{TicketID: "c9d4aa01", RunID: "run-2", At: t0.Add(time.Minute),
		Phase: core.PhaseAgentExit, Detail: "claude-code/sonnet exited success after 5 turns in 55s"}
	buildOK = core.Progress{TicketID: "c9d4aa01", RunID: "run-2", At: t0.Add(61 * time.Second),
		Phase: core.PhaseValidationStep, Detail: "build passed in 1.2s (exit 0)"}
	vetOK = core.Progress{TicketID: "c9d4aa01", RunID: "run-2", At: t0.Add(63 * time.Second),
		Phase: core.PhaseValidationStep, Detail: "vet passed in 800ms (exit 0)"}
	testing3 = core.Progress{TicketID: "c9d4aa01", RunID: "run-2", At: t0.Add(64 * time.Second),
		Phase: core.PhaseValidationStep, Detail: "running test (go test ./...), step 3 of 3"}
)

// openTall is openRunning with room for the timeline and the log together, and a journal.
func openTall(t *testing.T, f *fakeService, journal ...core.Progress) Model {
	t.Helper()
	f.progress = journal
	m := boot(t, f, 110, 44)
	m = send(t, m, key(SectionRunning.Key()))
	m = send(t, m, enteredMsg{focus: "c9d4aa01"})
	return send(t, m, runDetailMsg{
		ticketID: "c9d4aa01", runs: f.runs, explain: f.explain, journal: journal, journalOK: true,
	})
}

// TestRunningScreenByPhase is the screen in each phase a running ticket passes through. The
// phases before the agent starts and after it exits have no output at all, and a screen that
// only knew how to show output showed them as a hang.
func TestRunningScreenByPhase(t *testing.T) {
	cases := []struct {
		name    string
		state   core.State
		run     core.Run
		runs    []core.Run
		journal []core.Progress
		lines   []api.LogLine
		// activity is the snapshot's; the API appends the silence to it past the threshold.
		activity string
		want     []string
		notWant  []string
	}{
		{
			name:    "assigned",
			state:   core.StateAssigned,
			journal: []core.Progress{fetched},
			want: []string{
				"fetching origin", "fetching ·", "▾ timeline", "no agent yet", "attempt 1",
			},
			notWant: []string{"waiting for output", "Nothing running"},
		},
		{
			name:  "running with live counters",
			state: core.StateRunning,
			// The snapshot's row is newer than the run list the screen read on entry.
			run: core.Run{ID: "run-2", ProviderID: "claude-code", Model: "sonnet", HostID: "local",
				Turns: 9, TokensIn: 4800, TokensOut: 910},
			journal:  []core.Progress{fetched, cut, started},
			activity: "claude-code/sonnet started (pid 42), attempt 2 of 3 — no output for 6m",
			lines: []api.LogLine{
				{RunID: "run-2", Stream: "event", Kind: provider.EventToolUse, Tool: "Edit", Text: "Edit divide.go"},
			},
			want: []string{
				"9 turns", "4800 in / 910 out", "attempt 2 of 3", "agent working",
				"no output for 6m", "▸ timeline · 3 entries", "Edit divide.go",
				// The explanation and attempt history are still there.
				"host local chosen", "task_failure",
			},
			notWant: []string{"5 turns", "worktree cut at"},
		},
		{
			name:    "validating",
			state:   core.StateValidating,
			journal: []core.Progress{fetched, cut, started, exited, buildOK, vetOK, testing3},
			want: []string{
				"validating ·", "▾ timeline",
				"build passed in 1.2s (exit 0)", "vet passed in 800ms (exit 0)",
				"running test (go test ./...), step 3 of 3",
				"the agent has finished",
			},
			notWant: []string{"waiting for output"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := runningFixture()
			f.status.Running[0].Ticket.State = tc.state
			f.status.Running[0].Run = tc.run
			if tc.activity != "" {
				f.status.Running[0].Activity = tc.activity
			}
			if tc.runs != nil {
				f.runs = tc.runs
			}
			if tc.run.ID == "" && tc.state == core.StateAssigned {
				f.runs = nil
			}
			m := openTall(t, f, tc.journal...)
			for _, l := range tc.lines {
				m = send(t, m, logLineMsg{runID: l.RunID, line: l})
			}
			view := m.View()
			for _, w := range tc.want {
				if !strings.Contains(view, w) {
					t.Errorf("view omits %q:\n%s", w, view)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(view, w) {
					t.Errorf("view shows %q:\n%s", w, view)
				}
			}
		})
	}
}

// TestRefreshNoticesPhaseChanges is the bug this screen was rebuilt for: it loaded once and never
// noticed the ticket moving on.
func TestRefreshNoticesPhaseChanges(t *testing.T) {
	f := runningFixture()
	f.status.Running[0].Ticket.State = core.StateRunning
	m := openTall(t, f, fetched, cut, started)

	f.status.Running[0].Ticket.State = core.StateValidating
	f.progress = []core.Progress{fetched, cut, started, exited, buildOK}
	m, cmd := sendCmd(t, m, statusMsg{status: f.status})
	if cmd == nil {
		t.Fatal("a refresh did not re-read the run and its journal")
	}
	m = send(t, m, cmd())

	view := m.View()
	for _, want := range []string{"build passed in 1.2s (exit 0)", "▾ timeline", "validating ·"} {
		if !strings.Contains(view, want) {
			t.Errorf("after the refresh the view omits %q:\n%s", want, view)
		}
	}
}

// TestTimelineToggles: `t` folds a long journal away so it does not starve the log, and back.
func TestTimelineToggles(t *testing.T) {
	f := runningFixture()
	f.status.Running[0].Ticket.State = core.StateRunning
	m := openTall(t, f, fetched, cut, started)

	if strings.Contains(m.View(), "worktree cut at") {
		t.Fatalf("the timeline is expanded while the agent is running:\n%s", m.View())
	}
	m = send(t, m, key("t"))
	if !strings.Contains(m.View(), "worktree cut at") {
		t.Fatalf("t did not expand the timeline:\n%s", m.View())
	}
	m = send(t, m, key("t"))
	if strings.Contains(m.View(), "worktree cut at") {
		t.Fatalf("t did not collapse the timeline:\n%s", m.View())
	}
}

// TestTimelineCannotStarveTheLog: a long journal is capped, and the log still gets rows.
func TestTimelineCannotStarveTheLog(t *testing.T) {
	f := runningFixture()
	f.status.Running[0].Ticket.State = core.StateValidating
	var journal []core.Progress
	for i := 0; i < 60; i++ {
		journal = append(journal, core.Progress{TicketID: "c9d4aa01", At: t0.Add(time.Duration(i) * time.Second),
			Phase: core.PhaseValidationStep, Detail: fmt.Sprintf("step%02d passed in 1s (exit 0)", i)})
	}
	m := openTall(t, f, journal...)
	m = send(t, m, logLineMsg{runID: "run-2", line: api.LogLine{Text: "the newest output", Stream: "agent"}})

	view := m.View()
	if !strings.Contains(view, "step59") {
		t.Errorf("the newest journal entry is not shown:\n%s", view)
	}
	if !strings.Contains(view, "earlier") {
		t.Errorf("the capped journal does not say what it hid:\n%s", view)
	}
	if !strings.Contains(view, "the newest output") {
		t.Errorf("the journal pushed the log off the screen:\n%s", view)
	}
}

// TestRetrySwitchesLogStreams: a self-correction attempt is a new run with its own output. The
// old tail must stop, the new one start, and the timeline say so — and a line still in flight
// from the old stream must not land in the new one.
func TestRetrySwitchesLogStreams(t *testing.T) {
	f := runningFixture()
	f.status.Running[0].Ticket.State = core.StateRunning
	first := core.Run{ID: "run-1", ProviderID: "claude-code", Model: "sonnet", HostID: "local"}
	f.runs = []core.Run{first}
	m := openTall(t, f, fetched, cut)
	m = send(t, m, logLineMsg{runID: "run-1", line: api.LogLine{Text: "first attempt output", Stream: "agent"}})

	ended := t0.Add(time.Minute)
	first.EndedAt = &ended
	first.FailureClass, first.FailureNote = core.TaskFailure, "go test ./... failed: divide_test.go:12"
	retry := core.Progress{TicketID: "c9d4aa01", RunID: "run-1", At: t0.Add(70 * time.Second),
		Phase: core.PhaseRetry, Detail: "attempt 1 of 2 did not pass; retrying with the failure in the prompt"}
	second := core.Run{ID: "run-2", ProviderID: "claude-code", Model: "sonnet", HostID: "local"}

	m, cmd := sendCmd(t, m, runDetailMsg{
		ticketID: "c9d4aa01", runs: []core.Run{second, first},
		journal: []core.Progress{fetched, cut, retry}, journalOK: true,
	})
	if cmd == nil {
		t.Fatal("a newer run did not open its log stream")
	}
	scr := m.screens[SectionRunning].(*running)
	if scr.runID != "run-2" {
		t.Fatalf("the screen follows %q, want run-2", scr.runID)
	}

	m = send(t, m, key("t")) // the running default is collapsed; open it to read the switch
	view := m.View()
	for _, want := range []string{
		"log now follows attempt 2", "retrying with the failure", "— task_failure: go",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view omits %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "first attempt output") {
		t.Errorf("the failed attempt's output is still shown as the live tail:\n%s", view)
	}

	// The old stream's stragglers are dropped, and its closing does not end the new one.
	m = send(t, m, logLineMsg{runID: "run-1", line: api.LogLine{Text: "straggler", Stream: "agent"}})
	m = send(t, m, logClosedMsg{runID: "run-1"})
	m = send(t, m, logLineMsg{runID: "run-2", line: api.LogLine{Text: "second attempt output", Stream: "agent"}})
	view = m.View()
	if strings.Contains(view, "straggler") {
		t.Errorf("a line from the old stream landed in the new one:\n%s", view)
	}
	if strings.Contains(view, "agent output ended") {
		t.Errorf("the old stream closing ended the new one:\n%s", view)
	}
	if !strings.Contains(view, "second attempt output") {
		t.Errorf("the new stream's output is not shown:\n%s", view)
	}
}

// TestHandoffDestinations: when the ticket leaves the running set the screen keeps its final
// timeline and says where it went and which key follows it there.
func TestHandoffDestinations(t *testing.T) {
	cases := []struct {
		name    string
		state   core.State
		reason  core.AttentionReason
		journal []core.Progress
		want    string
	}{
		{name: "review", state: core.StateReview, reason: core.ReasonReviewPending,
			want: "moved to Review · press 7"},
		{name: "needs you", state: core.StateNeedsYou, reason: core.ReasonValidationFailed,
			want: "parked in Needs You: validation_failed · press 8"},
		{name: "blocked", state: core.StateBlocked, reason: core.ReasonAgentQuestion,
			want: "blocked on a question · press 8"},
		{name: "needs you before its row arrives", state: core.StateNeedsYou,
			journal: []core.Progress{{TicketID: "c9d4aa01", At: t0.Add(2 * time.Minute),
				Phase: core.PhaseHandoff, Detail: "parked in Needs You: provider_auth"}},
			want: "parked in Needs You: provider_auth · press 8"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := runningFixture()
			f.status.Running[0].Ticket.State = core.StateValidating
			m := openTall(t, f, fetched, cut, started, exited, buildOK)

			f.status.Running = nil
			if tc.reason != "" {
				f.status.Attention = []api.AttentionItem{{
					Attention: core.Attention{ID: "a1", TicketID: "c9d4aa01", Reason: tc.reason},
					Project:   projGravy,
					Ticket:    core.Ticket{ID: "c9d4aa01", Title: "Add a Divide function"},
				}}
			}
			f.explain.State = tc.state
			f.progress = append([]core.Progress{fetched, cut, started, exited, buildOK}, tc.journal...)
			m, cmd := sendCmd(t, m, statusMsg{status: f.status})
			if cmd == nil {
				t.Fatal("the ticket leaving did not re-read it")
			}
			m = send(t, m, cmd())

			view := m.View()
			for _, want := range []string{tc.want, "Add a Divide function", "build passed in 1.2s (exit 0)"} {
				if !strings.Contains(view, want) {
					t.Errorf("view omits %q:\n%s", want, view)
				}
			}
			for _, bad := range []string{"the run ended", "Nothing running"} {
				if strings.Contains(view, bad) {
					t.Errorf("view shows %q:\n%s", bad, view)
				}
			}
		})
	}
}

// TestLogRendersByKind: every decodable event reads as a sentence, never as the JSON it was
// stored as, and each kind is drawn in its own style.
func TestLogRendersByKind(t *testing.T) {
	th := DefaultTheme()
	cases := []struct {
		name  string
		line  api.LogLine
		text  string
		style lipgloss.Style
	}{
		{"tool use", api.LogLine{Stream: "event", Kind: provider.EventToolUse, Tool: "Edit", Text: "Edit main.go"},
			"Edit main.go", th.Accent},
		{"tool use missing its name", api.LogLine{Stream: "event", Kind: provider.EventToolUse, Tool: "Bash", Text: "go test ./..."},
			"Bash go test ./...", th.Accent},
		{"message", api.LogLine{Stream: "event", Kind: provider.EventMessage, Text: "Adding the function now."},
			"Adding the function now.", th.Text},
		{"error", api.LogLine{Stream: "event", Kind: provider.EventError, Text: "error: overloaded"},
			"error: overloaded", th.Danger},
		{"thinking", api.LogLine{Stream: "event", Kind: provider.EventThinking, Text: "… thinking"},
			"… thinking", th.Muted},
		{"usage", api.LogLine{Stream: "event", Kind: provider.EventUsage, Text: "usage: 10 tokens in, 2 out"},
			"usage: 10 tokens in, 2 out", th.Muted},
		{"started", api.LogLine{Stream: "event", Kind: provider.EventStarted, Text: "started session s1"},
			"started session s1", th.Muted},
		{"finished", api.LogLine{Stream: "event", Kind: provider.EventFinished, Text: "finished after 3 turns"},
			"finished after 3 turns", th.Muted},
		{"undecoded event from an older daemon",
			api.LogLine{Stream: "event", Text: `{"Kind":"tool_use","Tool":"Read","Text":"Read go.mod","Fields":{"file_path":"go.mod"}}`},
			"Read go.mod", th.Accent},
		{"undecodable event", api.LogLine{Stream: "event", Text: "not json at all"},
			"not json at all", th.Muted},
		{"raw agent output", api.LogLine{Stream: "agent", Text: "plain stdout"},
			"plain stdout", th.Text},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, style := logStyle(tc.line, th)
			if text != tc.text {
				t.Errorf("text = %q, want %q", text, tc.text)
			}
			if style.GetForeground() != tc.style.GetForeground() {
				t.Errorf("style = %v, want %v", style.GetForeground(), tc.style.GetForeground())
			}
		})
	}

	// And on screen: the stored JSON never reaches the view.
	m := openRunning(t, runningFixture())
	m = send(t, m, logLineMsg{runID: "run-2", line: api.LogLine{Stream: "event",
		Text: `{"Kind":"message","Text":"hello from the agent"}`}})
	view := m.View()
	if strings.Contains(view, `{"Kind"`) || !strings.Contains(view, "hello from the agent") {
		t.Errorf("a decodable event was shown as JSON:\n%s", view)
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

// TestHelpListsRunningKeys: the overlay and the footer read the same list, so `t` cannot work
// without being documented.
func TestHelpListsRunningKeys(t *testing.T) {
	m := boot(t, newFake(), 100, 30)
	m = send(t, m, key("?"))
	view := m.View()
	for _, b := range runningBindings {
		if !strings.Contains(view, b.Short) {
			t.Errorf("help overlay omits the Running key %q (%s)", b.Label(), b.Short)
		}
	}
}
