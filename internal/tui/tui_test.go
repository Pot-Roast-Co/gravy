package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bobbybrady/gravy/internal/api"
	"github.com/bobbybrady/gravy/internal/core"
)

// fakeService is an api.Service that answers from memory. The frame is supposed to hold no
// domain logic, so a fake with no store behind it should be enough to drive every path.
type fakeService struct {
	status  api.SystemStatus
	statusE error
	events  chan api.Event
	stopped bool

	// review is what GetReview returns; the rest record what the screen asked for, which is
	// what the approve/request-changes/reject assertions check.
	review    api.ReviewBundle
	reviewErr error
	actionErr error
	approved  []string
	rejected  []string
	changes   map[string]string

	// logs feeds StreamLogs; killed records what the detail screen asked to stop.
	logs      chan api.LogLine
	killed    []string
	continued []string
	resolved  []string
	runs      []core.Run
	explain   api.Explanation
}

func newFake() *fakeService {
	return &fakeService{
		events: make(chan api.Event, 8),
		status: api.SystemStatus{
			Projects: []api.ProjectStatus{
				{Project: core.Project{ID: "p1", Name: "gravy", Slug: "gravy"}},
				{Project: core.Project{ID: "p2", Name: "mojo", Slug: "mojo"}},
			},
			Hosts: []api.HostStatus{{ID: "local", UsedSlots: 1, TotalSlots: 4}},
		},
	}
}

func (f *fakeService) Status(context.Context) (api.SystemStatus, error) {
	if f.statusE != nil {
		return api.SystemStatus{}, f.statusE
	}
	return f.status, nil
}

func (f *fakeService) Events(context.Context) (<-chan api.Event, func(), error) {
	if f.statusE != nil {
		return nil, nil, f.statusE
	}
	return f.events, func() { f.stopped = true }, nil
}

func (f *fakeService) GetReview(_ context.Context, id string) (api.ReviewBundle, error) {
	if f.reviewErr != nil {
		return api.ReviewBundle{}, f.reviewErr
	}
	return f.review, nil
}

func (f *fakeService) Approve(_ context.Context, id string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.approved = append(f.approved, id)
	return nil
}

func (f *fakeService) Reject(_ context.Context, id string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.rejected = append(f.rejected, id)
	return nil
}

func (f *fakeService) RequestChanges(_ context.Context, id, feedback string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	if f.changes == nil {
		f.changes = map[string]string{}
	}
	f.changes[id] = feedback
	return nil
}

func (f *fakeService) StreamLogs(ctx context.Context, runID string) (<-chan api.LogLine, func(), error) {
	if f.logs != nil {
		return f.logs, func() {}, nil
	}
	ch := make(chan api.LogLine)
	close(ch)
	return ch, func() {}, nil
}

func (f *fakeService) Continue(_ context.Context, ticketID string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.continued = append(f.continued, ticketID)
	return nil
}

func (f *fakeService) KillRun(_ context.Context, runID string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.killed = append(f.killed, runID)
	return nil
}

func (f *fakeService) ListProjects(context.Context) ([]core.Project, error) { return nil, nil }
func (f *fakeService) AddProject(context.Context, api.AddProjectReq) (core.Project, error) {
	return core.Project{}, nil
}
func (f *fakeService) ListTickets(context.Context, api.TicketFilter) ([]core.Ticket, error) {
	return nil, nil
}
func (f *fakeService) CreateTicket(context.Context, api.CreateTicketReq) (core.Ticket, error) {
	return core.Ticket{}, nil
}
func (f *fakeService) MoveTicket(context.Context, string, core.Event) (core.State, error) {
	return "", nil
}
func (f *fakeService) ListRuns(context.Context, string) ([]core.Run, error)    { return nil, nil }
func (f *fakeService) ListAttention(context.Context) ([]core.Attention, error) { return nil, nil }
func (f *fakeService) ResolveAttention(_ context.Context, id string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.resolved = append(f.resolved, id)
	return nil
}
func (f *fakeService) ExplainTicket(context.Context, string) (api.Explanation, error) {
	return f.explain, nil
}

// ---- helpers -------------------------------------------------------------

// boot returns a model that has been sized and connected, which is the state every screen
// assertion cares about.
func boot(t *testing.T, svc api.Service, w, h int) Model {
	t.Helper()
	m := New(svc)
	m = send(t, m, tea.WindowSizeMsg{Width: w, Height: h})

	// Init's command is what connects; run it and feed the result back, exactly as Bubble Tea
	// would, so the test exercises the real transition rather than a hand-built state.
	msg := m.Init()()
	return send(t, m, msg)
}

func send(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	out, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return out
}

func sendCmd(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	out, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return out, cmd
}

func key(s string) tea.KeyMsg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	panic("unhandled key " + s)
}

// ---- tests ---------------------------------------------------------------

// TestFrameFitsStandardTerminal is AC3 at the size that must always work.
func TestFrameFitsStandardTerminal(t *testing.T) {
	m := boot(t, newFake(), 80, 24)
	view := m.View()

	lines := strings.Split(view, "\n")
	if len(lines) != 24 {
		t.Errorf("rendered %d lines, want exactly 24: the frame must fill its terminal and no more", len(lines))
	}
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w > 80 {
			t.Errorf("line %d is %d cells wide, want <= 80: %q", i, w, ln)
		}
	}
	// The status bar is the bottom line, not floating under short content.
	if last := lines[len(lines)-1]; !strings.Contains(last, "workers 1/4") {
		t.Errorf("bottom line = %q, want the status bar", last)
	}
}

// TestResizeNeverPanics is AC3's adversarial half. A terminal can be dragged to one column, and
// a frame that panics there takes the user's session with it.
func TestResizeNeverPanics(t *testing.T) {
	sizes := []struct{ w, h int }{
		{80, 24}, {200, 60}, {40, 10}, {20, 5}, {1, 1}, {0, 0}, {80, 2}, {80, 1}, {3, 24},
	}
	for _, sz := range sizes {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			m := boot(t, newFake(), sz.w, sz.h)
			view := m.View()
			if sz.w <= 0 || sz.h <= 0 {
				if view != "" {
					t.Errorf("view = %q, want empty before a real size arrives", view)
				}
				return
			}
			if n := len(strings.Split(view, "\n")); n > sz.h {
				t.Errorf("rendered %d lines into a %d-line terminal", n, sz.h)
			}
			for _, ln := range strings.Split(view, "\n") {
				if w := lipgloss.Width(ln); w > sz.w {
					t.Errorf("line is %d cells wide, want <= %d: %q", w, sz.w, ln)
				}
			}
		})
	}
}

// TestHelpIsGeneratedFromTheKeymap is AC2's real risk: help that drifts from what the keys
// actually do is worse than no help, because it is believed.
func TestHelpIsGeneratedFromTheKeymap(t *testing.T) {
	m := boot(t, newFake(), 100, 30)
	m = send(t, m, key("?"))
	view := m.View()

	for _, b := range DefaultKeyMap().Bindings() {
		if !strings.Contains(view, b.Help) {
			t.Errorf("help overlay omits %q (key %q)", b.Help, b.Label())
		}
	}
	for _, s := range AllSections {
		if !strings.Contains(view, s.Title()) {
			t.Errorf("help overlay omits section %q", s.Title())
		}
	}
}

// TestGlobalKeysWorkFromEverySection is AC2.
func TestGlobalKeysWorkFromEverySection(t *testing.T) {
	for _, start := range AllSections {
		t.Run(start.Title(), func(t *testing.T) {
			m := boot(t, newFake(), 100, 30)
			m = send(t, m, key(start.Key()))

			// `?` opens help from here.
			m = send(t, m, key("?"))
			if !strings.Contains(m.View(), "Keys") {
				t.Errorf("? did not open help from %s", start.Title())
			}
			m = send(t, m, key("esc"))
			if strings.Contains(m.View(), "toggle this help") {
				t.Errorf("esc did not close help from %s", start.Title())
			}

			// Every section key jumps from here.
			for _, dest := range AllSections {
				m = send(t, m, key(dest.Key()))
				if !strings.Contains(m.View(), dest.Title()) {
					t.Errorf("key %q from %s did not reach %s", dest.Key(), start.Title(), dest.Title())
				}
			}

			// `q` quits from here.
			_, cmd := sendCmd(t, m, key("q"))
			if cmd == nil {
				t.Fatalf("q from %s produced no command, want tea.Quit", start.Title())
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Errorf("q from %s did not quit", start.Title())
			}
		})
	}
}

// TestDaemonUnreachableIsActionable is AC4. An empty dashboard and a dead daemon must not look
// the same, or the user reads "nothing to do" from "nothing is working".
func TestDaemonUnreachableIsActionable(t *testing.T) {
	f := newFake()
	f.statusE = fmt.Errorf("dial unix /run/gravy.sock: connect: no such file or directory")

	m := boot(t, f, 80, 24)
	view := m.View()

	for _, want := range []string{"Cannot reach", "gravy serve", "no such file or directory"} {
		if !strings.Contains(view, want) {
			t.Errorf("unreachable view omits %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "daemon ok") {
		t.Error("status bar claims the daemon is fine while it is unreachable")
	}
}

// TestEventStreamClosingIsReported covers the other way the daemon goes away: the stream ends
// while the frame holds a snapshot that will now never update.
func TestEventStreamClosingIsReported(t *testing.T) {
	f := newFake()
	m := boot(t, f, 80, 24)
	if strings.Contains(m.View(), "Cannot reach") {
		t.Fatal("frame started unreachable")
	}

	close(f.events)
	// The waiting command observes the closed channel and reports it.
	_, cmd := sendCmd(t, m, key("1"))
	_ = cmd
	m = send(t, m, eventsClosedMsg{})
	if !strings.Contains(m.View(), "Cannot reach") {
		t.Error("a closed event stream left the frame claiming a healthy daemon")
	}
}

// TestPushUpdatesWithoutPolling is AC5.
//
// The assertion is structural as well as behavioural: an event must produce a refresh, and the
// package must contain no ticker at all. A polling loop would make the first half pass while
// quietly defeating the point.
func TestPushUpdatesWithoutPolling(t *testing.T) {
	f := newFake()
	m := boot(t, f, 80, 24)

	if got := strings.Contains(m.View(), "needs you 0"); !got {
		t.Fatalf("expected an empty attention count to start:\n%s", m.View())
	}

	// The daemon reports a change; the frame re-reads and re-renders.
	f.status.Attention = []api.AttentionItem{{Attention: core.Attention{ID: "a1", Reason: core.ReasonReviewPending}}}
	f.events <- api.Event{Kind: api.EventAttentionChanged}

	m, cmd := sendCmd(t, m, eventMsg{event: api.Event{Kind: api.EventAttentionChanged}})
	if cmd == nil {
		t.Fatal("an event produced no follow-up command, so the UI would never refresh")
	}
	m = send(t, m, statusMsg{status: f.status})
	if !strings.Contains(m.View(), "needs you 1") {
		t.Errorf("the frame did not re-render after a push:\n%s", m.View())
	}

	// No ticker anywhere in the package.
	for _, name := range goFiles(t) {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"tea.Tick", "time.Tick", "time.NewTicker"} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s uses %s: the UI must update on push, not on a timer", name, banned)
			}
		}
	}
}

// TestFilterCapturesTheKeyboard is the papercut a global keymap invites: typing a "q" into a
// filter must not quit.
func TestFilterCapturesTheKeyboard(t *testing.T) {
	m := boot(t, newFake(), 80, 24)
	m = send(t, m, key("/"))

	m, cmd := sendCmd(t, m, key("q"))
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatal("typing q into a filter quit the program")
		}
	}
	if !strings.Contains(m.View(), "/q") {
		t.Errorf("the filter did not receive the keystroke:\n%s", m.View())
	}

	m = send(t, m, key("esc"))
	if strings.Contains(m.View(), "/q") {
		t.Error("esc did not cancel the filter")
	}
}

// TestProjectCycleIsVisible covers `p`: the status bar must name the project being filtered to,
// since a filtered screen that looks unfiltered is how you misread a queue.
func TestProjectCycleIsVisible(t *testing.T) {
	m := boot(t, newFake(), 80, 24)
	if !strings.Contains(m.View(), "all projects") {
		t.Fatalf("frame does not start showing every project:\n%s", m.View())
	}
	m = send(t, m, key("p"))
	if !strings.Contains(m.View(), "gravy") {
		t.Errorf("p did not select the first project:\n%s", m.View())
	}
	m = send(t, m, key("p"))
	if !strings.Contains(m.View(), "mojo") {
		t.Errorf("p did not advance to the second project:\n%s", m.View())
	}
	m = send(t, m, key("p"))
	if !strings.Contains(m.View(), "all projects") {
		t.Errorf("p did not cycle back to every project:\n%s", m.View())
	}
}

func goFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			out = append(out, filepath.Join(".", e.Name()))
		}
	}
	return out
}
