package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
)

// fakeService is an api.Service that answers from memory. The frame is supposed to hold no
// domain logic, so a fake with no store behind it should be enough to drive every path.
type fakeService struct {
	setupPreview api.SetupPreview
	setupErr     error
	setups       []api.SetupRequest
	status       api.SystemStatus
	statusE      error
	events       chan api.Event
	stopped      bool

	// review is what GetReview returns; the rest record what the screen asked for, which is
	// what the approve/request-changes/reject assertions check.
	review        api.ReviewBundle
	reviewErr     error
	actionErr     error
	approveState  core.State
	continueState core.State
	attached      []string
	approvedHow   []core.Approval
	merged        []string
	requeued      []string
	approved      []string
	rejected      []string
	changes       map[string]string

	// The change discussion. talk is what the service hands back; said, drafts, sent and
	// canceled record what the screen asked for, which is how "a message starts no work" is
	// asserted. reply and propose are what the agent answers with.
	talk       api.DiscussionView
	talkErr    error
	said       []api.DiscussReq
	drafts     []core.ChangeInstruction
	sent       []api.SendChangesReq
	canceled   []string
	reply      string
	replyDraft core.ChangeInstruction

	// reconnected records which machines the screen asked the daemon to reach again;
	// reconnectTo is what it finds when it looks.
	reconnected  []string
	reconnectTo  api.HostStatus
	reconnectErr error

	// logs feeds StreamLogs; killed records what the detail screen asked to stop.
	logs      chan api.LogLine
	killed    []string
	continued []string
	resolved  []string

	// queue is what ListQueue returns; the rest record what the queue screen asked for.
	settings        api.Settings
	saved           []config.Config
	projects        []core.Project
	savedProjects   []core.Project
	deletedProjects []string
	// archived records the ArchiveProject calls the Projects screen made, as id -> flag.
	archived    []archiveCall
	agentStatus []api.AgentStatus
	allTickets  []core.Ticket
	// listTicketsErr fails ListTickets, the fallback lookup navigation uses when the status
	// snapshot does not carry the selected ticket.
	listTicketsErr error
	queue          []api.TicketDetail
	added          []api.AddProjectReq
	planReply      api.PlanReply
	planErr        error
	planned        []api.PlanReq
	// checkoutPath is what ReviewCheckout returns; the rest record what was asked for.
	checkoutPath string
	checkoutErr  error
	checkedOut   []string
	rereviewed   []string
	rereviewErr  error
	discarded    []string
	planOK       []api.ApprovePlanReq
	created      []api.CreateTicketReq
	updated      []core.Ticket
	deleted      []string
	reordered    [][3]string
	moved        [][2]string
	moveErr      error
	runs         []core.Run
	explain      api.Explanation
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

func (f *fakeService) Status(_ context.Context, filter api.ProjectFilter) (api.SystemStatus, error) {
	if f.statusE != nil {
		return api.SystemStatus{}, f.statusE
	}
	if filter.IncludeArchived {
		return f.status, nil
	}
	out := f.status
	out.Projects = nil
	for _, ps := range f.status.Projects {
		// The same rule Local applies: archived, but still on the dashboard while it has work
		// in flight.
		if ps.Project.Archived && ps.Active == nil {
			continue
		}
		out.Projects = append(out.Projects, ps)
	}
	return out, nil
}

func (f *fakeService) ReconnectHost(_ context.Context, id string) (api.HostStatus, error) {
	f.reconnected = append(f.reconnected, id)
	if f.reconnectErr != nil {
		return api.HostStatus{}, f.reconnectErr
	}
	hs := f.reconnectTo
	if hs.ID == "" {
		hs.ID = id
	}
	return hs, nil
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

func (f *fakeService) Approve(_ context.Context, id string, how core.Approval) (core.State, error) {
	if f.actionErr != nil {
		return "", f.actionErr
	}
	f.approved = append(f.approved, id)
	f.approvedHow = append(f.approvedHow, how)
	if f.approveState != "" {
		return f.approveState, nil
	}
	return core.StateDone, nil
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

func (f *fakeService) OpenDiscussion(_ context.Context, id string) (api.DiscussionView, error) {
	if f.talkErr != nil {
		return api.DiscussionView{}, f.talkErr
	}
	f.talk.Discussion.TicketID = id
	return f.talk, nil
}

func (f *fakeService) Discuss(_ context.Context, req api.DiscussReq) (api.DiscussionView, error) {
	f.said = append(f.said, req)
	if f.talkErr != nil {
		return api.DiscussionView{}, f.talkErr
	}
	if req.Proposal != nil {
		f.talk.Discussion.Proposal = *req.Proposal
	}
	f.talk.Discussion.Say(core.RoleHuman, req.Message, time.Time{})
	f.talk.Discussion.Say(core.RoleAgent, f.reply, time.Time{})
	if !f.replyDraft.Empty() {
		f.talk.Discussion.Proposal = f.replyDraft
	}
	return f.talk, nil
}

func (f *fakeService) SaveProposal(_ context.Context, req api.ProposalReq) (api.DiscussionView, error) {
	f.drafts = append(f.drafts, req.Proposal)
	if f.talkErr != nil {
		return api.DiscussionView{}, f.talkErr
	}
	f.talk.Discussion.Proposal = req.Proposal
	return f.talk, nil
}

func (f *fakeService) SendChanges(_ context.Context, req api.SendChangesReq) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.sent = append(f.sent, req)
	return nil
}

func (f *fakeService) CancelDiscussion(_ context.Context, id string) error {
	f.canceled = append(f.canceled, id)
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

func (f *fakeService) AttachRepository(_ context.Context, projectID, path string) (core.Project, error) {
	if f.actionErr != nil {
		return core.Project{}, f.actionErr
	}
	f.attached = append(f.attached, projectID+"="+path)
	return core.Project{ID: projectID, RepoPath: path}, nil
}

func (f *fakeService) Requeue(_ context.Context, ticketID string) (core.State, error) {
	if f.actionErr != nil {
		return "", f.actionErr
	}
	f.requeued = append(f.requeued, ticketID)
	return core.StateReady, nil
}

func (f *fakeService) MarkMerged(_ context.Context, ticketID string) (core.State, error) {
	if f.actionErr != nil {
		return "", f.actionErr
	}
	f.merged = append(f.merged, ticketID)
	return core.StateDone, nil
}

func (f *fakeService) Continue(_ context.Context, ticketID string, _ core.Approval) (core.State, error) {
	if f.actionErr != nil {
		return "", f.actionErr
	}
	f.continued = append(f.continued, ticketID)
	if f.continueState != "" {
		return f.continueState, nil
	}
	return core.StateDone, nil
}

func (f *fakeService) KillRun(_ context.Context, runID string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.killed = append(f.killed, runID)
	return nil
}

// archiveCall is one ArchiveProject call, recorded so a test can assert what was asked for.
type archiveCall struct {
	id       string
	archived bool
}

func (f *fakeService) ListProjects(_ context.Context, filter api.ProjectFilter) ([]core.Project, error) {
	if filter.IncludeArchived {
		return f.projects, nil
	}
	var out []core.Project
	for _, p := range f.projects {
		if !p.Archived {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeService) ArchiveProject(_ context.Context, id string, archived bool) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.archived = append(f.archived, archiveCall{id: id, archived: archived})
	for i, p := range f.projects {
		if p.ID == id {
			f.projects[i].Archived = archived
		}
	}
	return nil
}
func (f *fakeService) AddProject(_ context.Context, req api.AddProjectReq) (core.Project, error) {
	if f.actionErr != nil {
		return core.Project{}, f.actionErr
	}
	f.added = append(f.added, req)
	return core.Project{ID: "proj-new", Name: filepath.Base(req.Path)}, nil
}
func (f *fakeService) DetectAgents(context.Context) []api.AgentStatus { return f.agentStatus }

func (f *fakeService) Rereview(_ context.Context, ticketID string) error {
	f.rereviewed = append(f.rereviewed, ticketID)
	return f.rereviewErr
}

func (f *fakeService) ReviewCheckout(_ context.Context, ticketID string) (string, error) {
	if f.checkoutErr != nil {
		return "", f.checkoutErr
	}
	f.checkedOut = append(f.checkedOut, ticketID)
	return f.checkoutPath, nil
}

func (f *fakeService) DiscardReviewCheckout(_ context.Context, ticketID string) error {
	f.discarded = append(f.discarded, ticketID)
	return nil
}

func (f *fakeService) Plan(_ context.Context, req api.PlanReq) (api.PlanReply, error) {
	f.planned = append(f.planned, req)
	if f.planErr != nil {
		return api.PlanReply{}, f.planErr
	}
	reply := f.planReply
	if reply.Session == "" {
		reply.Session = "sess-1"
	}
	return reply, nil
}

func (f *fakeService) ApprovePlan(_ context.Context, req api.ApprovePlanReq) ([]core.Ticket, error) {
	if f.actionErr != nil {
		return nil, f.actionErr
	}
	f.planOK = append(f.planOK, req)
	out := make([]core.Ticket, 0, len(req.Tickets))
	for i, t := range req.Tickets {
		out = append(out, core.Ticket{ID: fmt.Sprintf("plan-%d", i), Title: t.Title})
	}
	return out, nil
}

func (f *fakeService) DeleteProject(_ context.Context, id string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.deletedProjects = append(f.deletedProjects, id)
	return nil
}

func (f *fakeService) ListTickets(context.Context, api.TicketFilter) ([]core.Ticket, error) {
	if f.listTicketsErr != nil {
		return nil, f.listTicketsErr
	}
	return f.allTickets, nil
}
func (f *fakeService) CreateTicket(_ context.Context, req api.CreateTicketReq) (core.Ticket, error) {
	if f.actionErr != nil {
		return core.Ticket{}, f.actionErr
	}
	f.created = append(f.created, req)
	return core.Ticket{ID: "new-1", Title: req.Title, State: core.StateBacklog}, nil
}
func (f *fakeService) MoveTicket(_ context.Context, id string, ev core.Event) (core.State, error) {
	if f.moveErr != nil {
		return "", f.moveErr
	}
	f.moved = append(f.moved, [2]string{id, string(ev)})
	return core.StateReady, nil
}

func (f *fakeService) GetSettings(context.Context) (api.Settings, error) {
	if f.actionErr != nil {
		return api.Settings{}, f.actionErr
	}
	return f.settings, nil
}

func (f *fakeService) UpdateSettings(_ context.Context, c config.Config) (api.Settings, error) {
	if f.actionErr != nil {
		return api.Settings{}, f.actionErr
	}
	f.saved = append(f.saved, c)
	f.settings.Config = c
	return f.settings, nil
}

func (f *fakeService) UpdateProject(_ context.Context, p core.Project) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.projects = append(f.projects, p)
	f.savedProjects = append(f.savedProjects, p)
	return nil
}

func (f *fakeService) ListQueue(context.Context, api.TicketFilter) ([]api.TicketDetail, error) {
	return f.queue, nil
}

func (f *fakeService) UpdateTicket(_ context.Context, t core.Ticket) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.updated = append(f.updated, t)
	return nil
}

func (f *fakeService) ReorderTicket(_ context.Context, id, before, after string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.reordered = append(f.reordered, [3]string{id, before, after})
	return nil
}

func (f *fakeService) DeleteTicket(_ context.Context, id string) error {
	if f.actionErr != nil {
		return f.actionErr
	}
	f.deleted = append(f.deleted, id)
	return nil
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
	// Counted in runes, not bytes: "é" is one key press and two bytes, and measuring it as
	// two would fall through to the panic below.
	if utf8.RuneCountInString(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
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

			// Every section key jumps from here. Asserted on the active section rather than
			// on the rendered title: Settings is deliberately absent from the header row, so
			// a view that mentions it is not what "arrived" means.
			for _, dest := range AllSections {
				m = send(t, m, key(dest.Key()))
				if m.active != dest {
					t.Errorf("key %q from %s reached %v, want %s",
						dest.Key(), start.Title(), m.active, dest.Title())
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
	if !strings.Contains(m.View(), "search: q") {
		t.Errorf("the filter did not receive the keystroke:\n%s", m.View())
	}

	m = send(t, m, key("esc"))
	if strings.Contains(m.View(), "search: q") {
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

func (f *fakeService) SetupInfo(context.Context) (api.SetupInfo, error) {
	return api.SetupInfo{Settings: f.settings, Agents: f.agentStatus, Projects: f.projects}, f.setupErr
}
func (f *fakeService) PreviewSetup(context.Context, api.AddProjectReq) (api.SetupPreview, error) {
	return f.setupPreview, f.setupErr
}
func (f *fakeService) ApplySetup(_ context.Context, req api.SetupRequest) (api.Settings, error) {
	f.setups = append(f.setups, req)
	return api.Settings{Config: req.Config}, f.setupErr
}
