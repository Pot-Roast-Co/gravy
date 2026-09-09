package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// openPlan boots the frame on the Plan screen with a project selected.
//
// The fake registers two, and planning refuses to guess between them — a conversation about
// "the project" has to know which one.
func openPlan(t *testing.T, f *fakeService) Model {
	t.Helper()
	m := boot(t, f, 100, 30)
	m = send(t, m, key("p"))
	return send(t, m, key(SectionPlan.Key()))
}

// deliver runs a command and feeds every message it produces back to the model, unwrapping the
// batch a turn returns when it starts the planner and its log tail at the same time.
func deliver(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	var walk func(m Model, c tea.Cmd, depth int) Model
	walk = func(m Model, c tea.Cmd, depth int) Model {
		if c == nil || depth > 8 {
			return m
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				m = walk(m, sub, depth+1)
			}
			return m
		}
		next, follow := sendCmd(t, m, msg)
		return walk(next, follow, depth+1)
	}
	return walk(m, cmd, 0)
}

func proposal(titles ...string) []core.PlannedTicket {
	out := make([]core.PlannedTicket, 0, len(titles))
	for _, ti := range titles {
		out = append(out, core.PlannedTicket{Title: ti, Body: "what done looks like", Route: core.RouteImplementation})
	}
	return out
}

// TestPlanAsksWhatIsNext is the arrive-with-nothing-in-mind path.
func TestPlanAsksWhatIsNext(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Reply: "I suggest wiring up the Done screen.", Tickets: proposal("Build the Done screen")}
	m := openPlan(t, f)

	m, cmd := sendCmd(t, m, key("n"))
	if cmd == nil {
		t.Fatal("n did not ask the planner anything")
	}
	m = deliver(t, m, cmd)

	if len(f.planned) != 1 {
		t.Fatalf("Plan called %d times, want 1", len(f.planned))
	}
	// An empty message is what means "what should be next?" — the planner supplies the
	// question, so the screen does not have to hard-code one the prompt already asks better.
	if f.planned[0].Message != "" {
		t.Errorf("message = %q, want empty for the what-next question", f.planned[0].Message)
	}
	view := m.View()
	for _, want := range []string{"Done screen", "Proposed"} {
		if !strings.Contains(view, want) {
			t.Errorf("view omits %q:\n%s", want, view)
		}
	}
}

// TestPlanGrillsOverOneSession is the whole point of the screen: the conversation continues
// rather than restarting, so the provider keeps the context instead of being re-sent it.
func TestPlanGrillsOverOneSession(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Session: "sess-42", Reply: "How should it handle an empty list?"}
	m := openPlan(t, f)

	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	// Grill it.
	m = send(t, m, key("i"))
	m = typeKeys(t, m, "show a placeholder")
	m, cmd = sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter sent nothing")
	}
	m = deliver(t, m, cmd)

	if len(f.planned) != 2 {
		t.Fatalf("Plan called %d times, want 2", len(f.planned))
	}
	if f.planned[1].Session != "sess-42" {
		t.Errorf("second turn session = %q, want the first turn's session", f.planned[1].Session)
	}
	if f.planned[1].Message != "show a placeholder" {
		t.Errorf("second turn message = %q", f.planned[1].Message)
	}
	if !strings.Contains(m.View(), "show a placeholder") {
		t.Error("the human's own words are not in the transcript")
	}
}

// TestPlanTypingDoesNotTriggerGlobals: the input is prose, and prose contains q, p and digits.
func TestPlanTypingDoesNotTriggerGlobals(t *testing.T) {
	f := newFake()
	m := openPlan(t, f)
	m = send(t, m, key("i"))

	before := m.projectIdx
	const text = "queue 7 plans, properly"
	m = typeKeys(t, m, text)

	if m.quitting {
		t.Fatal("a q in the question quit the program")
	}
	if m.active != SectionPlan {
		t.Errorf("a digit in the question jumped to %v", m.active)
	}
	if m.projectIdx != before {
		t.Error("a p in the question cycled the project filter")
	}
	scr := m.screens[SectionPlan].(*plan)
	if scr.input != text {
		t.Errorf("input = %q, want %q", scr.input, text)
	}
}

// TestPlanApproveIsTheGate: nothing reaches the backlog until a human says so.
func TestPlanApproveIsTheGate(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Reply: "here", Tickets: proposal("one", "two")}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	if len(f.planOK) != 0 {
		t.Fatal("proposing created tickets without approval")
	}

	m, cmd = sendCmd(t, m, key("a"))
	if cmd == nil {
		t.Fatal("a did not approve")
	}
	deliver(t, m, cmd)

	if len(f.planOK) != 1 {
		t.Fatalf("ApprovePlan called %d times, want 1", len(f.planOK))
	}
	if got := len(f.planOK[0].Tickets); got != 2 {
		t.Errorf("approved %d tickets, want 2", got)
	}
	if f.planOK[0].Ready {
		t.Error("a approved straight to Ready; that is A")
	}
}

func TestPlanApproveToReady(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Reply: "here", Tickets: proposal("one")}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	m, cmd = sendCmd(t, m, key("A"))
	deliver(t, m, cmd)

	if len(f.planOK) != 1 || !f.planOK[0].Ready {
		t.Fatalf("A did not approve into Ready: %+v", f.planOK)
	}
}

// TestPlanRouteIsARouteNotAModel: the screen chooses a bucket, and that choice is what the
// approved tickets ask for.
func TestPlanRouteIsARouteNotAModel(t *testing.T) {
	f := newFake()
	f.status.Buckets = []core.Route{core.RouteCheap, core.RouteStandard, core.RouteStrong}
	f.planReply = api.PlanReply{Reply: "here", Tickets: proposal("one")}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	// The proposal seeds the route; r cycles it through the configured buckets.
	m = send(t, m, key("r"))
	scr := m.screens[SectionPlan].(*plan)
	chosen := scr.route
	if chosen == "" {
		t.Fatal("r left the route empty")
	}

	m, cmd = sendCmd(t, m, key("a"))
	m = deliver(t, m, cmd)

	if len(f.planOK) != 1 {
		t.Fatal("nothing approved")
	}
	for i, tk := range f.planOK[0].Tickets {
		if tk.Route != chosen {
			t.Errorf("ticket %d route = %q, want the chosen bucket %q", i, tk.Route, chosen)
		}
	}
}

func TestPlanApproveWithNothingProposed(t *testing.T) {
	f := newFake()
	m := openPlan(t, f)

	m, cmd := sendCmd(t, m, key("a"))
	if cmd != nil {
		t.Fatal("approving an empty plan called the service")
	}
	scr := m.screens[SectionPlan].(*plan)
	if scr.notice == "" {
		t.Error("approving nothing said nothing")
	}
}

func TestPlanFailureIsShown(t *testing.T) {
	f := newFake()
	f.planErr = fmt.Errorf("bucket \"planning\" has no agents configured")
	m := openPlan(t, f)

	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	if !strings.Contains(m.View(), "no agents configured") {
		t.Errorf("the planner's failure is not on screen:\n%s", m.View())
	}
	scr := m.screens[SectionPlan].(*plan)
	if scr.busy {
		t.Error("still busy after a failure; the screen would be stuck")
	}
}

// TestPlanShowsDependencies: a linked set is the point of planning, so the links have to be
// visible before they are approved.
func TestPlanShowsDependencies(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Reply: "two steps", Tickets: []core.PlannedTicket{
		{Title: "schema", Route: core.RouteImplementation},
		{Title: "api on top", Route: core.RouteImplementation, DependsOn: []int{0}},
	}}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	view := m.View()
	if !strings.Contains(view, "after #1") {
		t.Errorf("the dependency is not shown:\n%s", view)
	}
}

func TestPlanStartOverClearsTheSession(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Session: "sess-9", Reply: "hello", Tickets: proposal("one")}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	m = send(t, m, key("x"))
	scr := m.screens[SectionPlan].(*plan)
	if scr.session != "" || len(scr.entries) != 0 || len(scr.tickets) != 0 {
		t.Errorf("x left state behind: %+v", scr)
	}

	// The next question starts a fresh conversation rather than resuming the abandoned one.
	m, cmd = sendCmd(t, m, key("n"))
	deliver(t, m, cmd)
	if f.planned[len(f.planned)-1].Session != "" {
		t.Error("the new conversation resumed the abandoned session")
	}
}

func TestPlanFitsItsTerminal(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{
		Reply:   strings.Repeat("a long considered answer about the architecture ", 20),
		Tickets: proposal("one", "two", "three"),
	}
	for _, w := range []int{40, 80, 120} {
		m := boot(t, f, w, 24)
		m = send(t, m, key("p"))
		m = send(t, m, key(SectionPlan.Key()))
		m, cmd := sendCmd(t, m, key("n"))
		if cmd == nil {
			t.Fatalf("width %d: n asked nothing", w)
		}
		m = deliver(t, m, cmd)

		lines := strings.Split(m.View(), "\n")
		if len(lines) != 24 {
			t.Errorf("width %d: rendered %d lines, want 24", w, len(lines))
		}
		for i, ln := range lines {
			if got := lipgloss.Width(ln); got > w {
				t.Errorf("width %d: line %d is %d cells: %q", w, i, got, ln)
			}
		}
	}
}

// TestPlanNamesItsProject: a planner that thinks for a minute without naming its subject leaves
// you watching a spinner you cannot check.
func TestPlanNamesItsProject(t *testing.T) {
	f := newFake()
	m := openPlan(t, f) // selects "gravy", the first of the fake's two projects

	if !strings.Contains(m.View(), "gravy") {
		t.Errorf("the idle screen does not name its project:\n%s", m.View())
	}

	// And while it is working, which is when it matters.
	m, cmd := sendCmd(t, m, key("n"))
	if cmd == nil {
		t.Fatal("n asked nothing")
	}
	view := m.View()
	if !strings.Contains(view, "thinking about gravy") {
		t.Errorf("the busy state does not name its project:\n%s", view)
	}
}

// TestPlanSaysWhyItCannotPlan distinguishes the two reasons up front, rather than waiting for a
// keypress to explain that nothing will happen.
func TestPlanSaysWhyItCannotPlan(t *testing.T) {
	for _, tc := range []struct {
		name     string
		projects []api.ProjectStatus
		want     string
	}{
		{
			name: "nothing registered",
			want: "add one with P",
		},
		{
			name: "several registered, none picked",
			projects: []api.ProjectStatus{
				{Project: core.Project{ID: "p1", Name: "gravy"}},
				{Project: core.Project{ID: "p2", Name: "mojo"}},
			},
			want: "pick one with p",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.status.Projects = tc.projects
			m := boot(t, f, 100, 30)
			m = send(t, m, key(SectionPlan.Key()))

			if !strings.Contains(m.View(), tc.want) {
				t.Errorf("view does not say %q:\n%s", tc.want, m.View())
			}
			// And pressing n does not silently do nothing.
			m, cmd := sendCmd(t, m, key("n"))
			if cmd != nil {
				t.Error("planned without knowing which project")
			}
			if scr := m.screens[SectionPlan].(*plan); scr.notice == "" {
				t.Error("n was refused silently")
			}
		})
	}
}

// TestPlanUsesTheOnlyProjectWithoutBeingAsked: with one repository registered there is nothing
// to disambiguate, and making someone press p first would be ceremony.
func TestPlanUsesTheOnlyProjectWithoutBeingAsked(t *testing.T) {
	f := newFake()
	f.status.Projects = []api.ProjectStatus{{Project: core.Project{ID: "solo", Name: "onlyrepo"}}}
	m := boot(t, f, 100, 30)
	m = send(t, m, key(SectionPlan.Key()))

	if !strings.Contains(m.View(), "onlyrepo") {
		t.Errorf("the only project is not named:\n%s", m.View())
	}
	m, cmd := sendCmd(t, m, key("n"))
	if cmd == nil {
		t.Fatal("n did not plan against the only registered project")
	}
	deliver(t, m, cmd)
	if len(f.planned) != 1 || f.planned[0].ProjectID != "solo" {
		t.Errorf("planned against %+v, want the only project", f.planned)
	}
}

// TestPlanIsPinnedToItsProject is the bug the project switcher creates.
//
// The frame's p cycles a global filter. A conversation resumes a provider session that belongs
// to whichever repository it started against, so re-reading the filter every turn means pressing
// p mid-conversation continues one project's discussion while telling the service it is about
// another — and approving files that plan's tickets into the wrong backlog entirely.
func TestPlanIsPinnedToItsProject(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Session: "sess-mojo", Reply: "here", Tickets: proposal("mojo work")}

	m := boot(t, f, 100, 30)
	m = send(t, m, key("p")) // gravy
	m = send(t, m, key("p")) // mojo
	m = send(t, m, key(SectionPlan.Key()))

	started, _ := planProject(m.viewContext())
	if started != "p2" {
		t.Fatalf("expected to start against mojo, got %q", started)
	}

	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	// Switch the frame filter to the other project, mid-conversation.
	m = send(t, m, key("p")) // all projects
	m = send(t, m, key("p")) // gravy

	// Continuing must not resume mojo's session under gravy's id.
	m = send(t, m, key("i"))
	m = typeKeys(t, m, "and then?")
	m, cmd = sendCmd(t, m, key("enter"))
	if cmd != nil {
		deliver(t, m, cmd)
	}
	for _, req := range f.planned {
		if req.Session == "sess-mojo" && req.ProjectID != "p2" {
			t.Errorf("resumed mojo's session under project %q", req.ProjectID)
		}
	}

	// Approving must not file mojo's plan into gravy's backlog.
	m, cmd = sendCmd(t, m, key("a"))
	if cmd != nil {
		deliver(t, m, cmd)
	}
	for _, req := range f.planOK {
		if req.ProjectID != "p2" {
			t.Errorf("approved mojo's plan into project %q", req.ProjectID)
		}
	}
}

// TestPlanSaysWhenTheFilterMovedOff: showing one project in the header and another in the
// status bar with no explanation is how the misattribution bug looked from the outside.
func TestPlanSaysWhenTheFilterMovedOff(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Session: "s", Reply: "here", Tickets: proposal("work")}
	m := openPlan(t, f) // pinned to gravy
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	m = send(t, m, key("p")) // move the filter to mojo
	view := m.View()
	if !strings.Contains(view, "about gravy") {
		t.Errorf("the view does not say which project the conversation is about:\n%s", view)
	}
	if !strings.Contains(view, "x starts over on mojo") {
		t.Errorf("the view does not offer the way out:\n%s", view)
	}
}

// TestPlanOffersTheSwitcher: p is the selector, and it is only worth naming when there is more
// than one project and no conversation to disturb.
func TestPlanOffersTheSwitcher(t *testing.T) {
	f := newFake()
	m := openPlan(t, f)
	if !strings.Contains(m.View(), "p project") {
		t.Errorf("the switcher is not offered:\n%s", m.View())
	}

	// Once a conversation is pinned, p no longer changes it, so it stops being advertised.
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)
	if strings.Contains(m.View(), "p project") {
		t.Error("the switcher is still offered after the conversation is pinned")
	}
}

// TestPlanStartOverRepins lets you move to the other project deliberately.
func TestPlanStartOverRepins(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Session: "s", Reply: "here", Tickets: proposal("work")}
	m := openPlan(t, f) // gravy
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	m = send(t, m, key("p")) // filter to mojo
	m = send(t, m, key("x")) // start over
	m, cmd = sendCmd(t, m, key("n"))
	if cmd == nil {
		t.Fatal("n did not plan after starting over")
	}
	deliver(t, m, cmd)

	last := f.planned[len(f.planned)-1]
	if last.ProjectID != "p2" {
		t.Errorf("after x, planned against %q, want mojo", last.ProjectID)
	}
	if last.Session != "" {
		t.Error("after x, the new conversation resumed the old session")
	}
}

// TestPlanShowsWhatItIsDoing: a minute of silence is indistinguishable from a hang.
func TestPlanShowsWhatItIsDoing(t *testing.T) {
	f := newFake()
	m := openPlan(t, f)

	m, cmd := sendCmd(t, m, key("n"))
	if cmd == nil {
		t.Fatal("n asked nothing")
	}
	// A line arrives from the tail while the turn is still in flight.
	m = send(t, m, planLogLineMsg{line: api.LogLine{Text: "reading CLAUDE.md"}})

	view := m.View()
	if !strings.Contains(view, "thinking about gravy") {
		t.Errorf("the busy state is missing:\n%s", view)
	}
	if !strings.Contains(view, "reading CLAUDE.md") {
		t.Errorf("the planner's current step is not shown:\n%s", view)
	}

	// Once the turn lands, the activity line goes away rather than lingering as a stale claim.
	m = deliver(t, m, cmd)
	if scr := m.screens[SectionPlan].(*plan); scr.activity != "" {
		t.Errorf("activity %q survived the turn", scr.activity)
	}
}

// TestPlanFailedTurnStaysInTheTranscript: a question left sitting with no answer under it reads
// as though it were ignored.
func TestPlanFailedTurnStaysInTheTranscript(t *testing.T) {
	f := newFake()
	f.planErr = fmt.Errorf("the planner answered with nothing")
	m := openPlan(t, f)

	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	scr := m.screens[SectionPlan].(*plan)
	if len(scr.entries) == 0 {
		t.Fatal("the failure left no trace in the conversation")
	}
	last := scr.entries[len(scr.entries)-1]
	if !last.failed {
		t.Errorf("the last entry is not marked failed: %+v", last)
	}
	if !strings.Contains(m.View(), "answered with nothing") {
		t.Errorf("the failure is not in the transcript:\n%s", m.View())
	}
	if scr.busy {
		t.Error("still busy after a failure")
	}
}

// TestPlanNamesTheAgent: the screen already shows a route for the proposed tickets, which is a
// different thing from the model doing the planning. Showing only one makes them easy to confuse.
func TestPlanNamesTheAgent(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{
		Session: "claude-code:s1", Agent: "claude-code/opus",
		Reply: "here", Tickets: proposal("one"),
	}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	view := m.View()
	if !strings.Contains(view, "claude-code/opus") {
		t.Errorf("the planning agent is not named:\n%s", view)
	}

	// A resume does not re-resolve a route, so it reports no agent — the screen must keep
	// showing the one it already knows rather than blanking it mid-conversation.
	f.planReply = api.PlanReply{Session: "claude-code:s1", Reply: "more"}
	m = send(t, m, key("i"))
	m = typeKeys(t, m, "why?")
	m, cmd = sendCmd(t, m, key("enter"))
	m = deliver(t, m, cmd)

	if !strings.Contains(m.View(), "claude-code/opus") {
		t.Errorf("the agent was forgotten on resume:\n%s", m.View())
	}
}

// longReply is a plan answer taller than any terminal this is drawn in.
func longReply() string {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "line %02d of a long considered answer about the architecture\n", i)
	}
	return b.String()
}

// TestPlanFollowsTheNewestOutput is the bug: a conversation grows downwards, so pinning the view
// to the top hides the answer behind a "more" indicator the arrows did not even move.
func TestPlanFollowsTheNewestOutput(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Reply: longReply(), Tickets: proposal("the proposal")}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	view := m.View()
	if strings.Contains(view, "line 00") {
		t.Error("the view is pinned to the top of a long answer")
	}
	// The proposal is the last thing rendered, and the whole point of the turn.
	if !strings.Contains(view, "the proposal") {
		t.Errorf("the newest content is not visible:\n%s", view)
	}
}

func TestPlanScrolls(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Reply: longReply()}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	// g goes to the top, and the top is where the answer begins.
	m = send(t, m, key("g"))
	if !strings.Contains(m.View(), "line 00") {
		t.Errorf("g did not reach the top:\n%s", m.View())
	}

	// k/j move, and stop at the top rather than running past it.
	m = send(t, m, key("k"))
	if scr := m.screens[SectionPlan].(*plan); scr.scroll != 0 {
		t.Errorf("scroll = %d at the top, want 0", scr.scroll)
	}
	m = send(t, m, key("j"))
	if scr := m.screens[SectionPlan].(*plan); scr.scroll != 1 {
		t.Errorf("scroll = %d after one j, want 1", scr.scroll)
	}

	// G returns to following the end.
	m = send(t, m, key("G"))
	scr := m.screens[SectionPlan].(*plan)
	if !scr.follow {
		t.Error("G did not resume following")
	}
	if strings.Contains(m.View(), "line 00") {
		t.Error("G did not move back to the newest output")
	}
}

// TestPlanNewTurnReturnsToTheBottom: having scrolled back to re-read something, the answer to
// the next question must not arrive off-screen.
func TestPlanNewTurnReturnsToTheBottom(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Reply: longReply()}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	m = send(t, m, key("g")) // scroll back to re-read
	m, cmd = sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	if scr := m.screens[SectionPlan].(*plan); !scr.follow {
		t.Error("a new turn did not return to the bottom")
	}
}

// TestPlanTabCyclesProposals keeps the arrows free for the transcript.
func TestPlanTabCyclesProposals(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Reply: "here", Tickets: proposal("first", "second")}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	scr := func(m Model) *plan { return m.screens[SectionPlan].(*plan) }
	if scr(m).expand {
		t.Fatal("a proposal is expanded before tab")
	}
	m = send(t, m, key("tab"))
	if s := scr(m); !s.expand || s.cursor != 0 {
		t.Errorf("first tab = expand %v cursor %d, want true 0", s.expand, s.cursor)
	}
	m = send(t, m, key("tab"))
	if s := scr(m); !s.expand || s.cursor != 1 {
		t.Errorf("second tab = expand %v cursor %d, want true 1", s.expand, s.cursor)
	}
	m = send(t, m, key("tab")) // past the end collapses
	if s := scr(m); s.expand || s.cursor != 0 {
		t.Errorf("tab past the end = expand %v cursor %d, want false 0", s.expand, s.cursor)
	}
}

// TestPlanScrollingNeverOverflows: the indicator and footer must stay on screen at any size.
func TestPlanScrollingNeverOverflows(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Reply: longReply(), Tickets: proposal("one", "two")}
	for _, size := range [][2]int{{40, 10}, {80, 24}, {120, 40}, {60, 6}} {
		w, h := size[0], size[1]
		m := boot(t, f, w, h)
		m = send(t, m, key("p"))
		m = send(t, m, key(SectionPlan.Key()))
		m, cmd := sendCmd(t, m, key("n"))
		m = deliver(t, m, cmd)

		for _, keyName := range []string{"g", "j", "j", "G"} {
			m = send(t, m, key(keyName))
			lines := strings.Split(m.View(), "\n")
			if len(lines) != h {
				t.Fatalf("%dx%d after %q: %d lines, want %d", w, h, keyName, len(lines), h)
			}
			for i, ln := range lines {
				if got := lipgloss.Width(ln); got > w {
					t.Fatalf("%dx%d after %q: line %d is %d cells", w, h, keyName, i, got)
				}
			}
		}
	}
}

// TestPlanLeadsWithTheNextStep is the flow complaint: leading with "what next" after an answer
// invites pressing it again, which re-asks the same question and stacks a duplicate on the
// transcript instead of continuing the thread.
func TestPlanLeadsWithTheNextStep(t *testing.T) {
	f := newFake()
	m := openPlan(t, f)

	// Nothing yet: asking is the step.
	if got := m.View(); !strings.Contains(got, "what I should work on next") {
		t.Errorf("the empty screen does not offer the opening question:\n%s", got)
	}

	// With a proposal on the table: replying is the step, and re-asking is not advertised.
	f.planReply = api.PlanReply{Session: "s", Reply: "I propose this", Tickets: proposal("the work")}
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	view := m.View()
	if !strings.Contains(view, "enter reply") {
		t.Errorf("replying is not offered once there is a proposal:\n%s", view)
	}
	if strings.Contains(view, "what I should work on next") {
		t.Error("the opening question is still advertised after it was answered")
	}
	if !strings.Contains(view, "> enter to reply") {
		t.Errorf("there is no visible place to answer:\n%s", view)
	}
}

// TestPlanEnterStartsAReply: the standing prompt has to actually work, or it is a lie.
func TestPlanEnterStartsAReply(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Session: "s", Reply: "I propose this", Tickets: proposal("the work")}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	m = send(t, m, key("enter"))
	if scr := m.screens[SectionPlan].(*plan); !scr.editing {
		t.Fatal("enter did not open the reply")
	}
	m = typeKeys(t, m, "too big, split it")
	m, cmd = sendCmd(t, m, key("enter"))
	m = deliver(t, m, cmd)

	if len(f.planned) != 2 {
		t.Fatalf("Plan called %d times, want 2", len(f.planned))
	}
	if f.planned[1].Message != "too big, split it" {
		t.Errorf("reply = %q", f.planned[1].Message)
	}
	if f.planned[1].Session != "s" {
		t.Errorf("the reply started a new conversation instead of continuing one")
	}
}

// TestPlanMidConversationOffersAnotherOption: n still works, but it is honestly labelled — it
// asks for a different suggestion rather than repeating the opening question.
func TestPlanMidConversationOffersAnotherOption(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Session: "s", Reply: "still thinking about it"}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	if got := m.View(); !strings.Contains(got, "another option") {
		t.Errorf("n is not honestly labelled mid-conversation:\n%s", got)
	}
}

// TestPlanGrillMeTurnsTheQuestionsAround is the other direction of grilling.
//
// The opening question must be answered rather than deflected, which is why the prompt forbids
// leading with questions. This is the asked-for exception: the human presses c and the planner
// interrogates them, at the point where the answers actually change the plan.
func TestPlanGrillMeTurnsTheQuestionsAround(t *testing.T) {
	f := newFake()
	f.planReply = api.PlanReply{Session: "s", Reply: "I propose this", Tickets: proposal("the work")}
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("n"))
	m = deliver(t, m, cmd)

	if !strings.Contains(m.View(), "c grill me") {
		t.Errorf("grilling is not offered once there is a proposal:\n%s", m.View())
	}

	m, cmd = sendCmd(t, m, key("c"))
	if cmd == nil {
		t.Fatal("c asked nothing")
	}
	m = deliver(t, m, cmd)

	if len(f.planned) != 2 {
		t.Fatalf("Plan called %d times, want 2", len(f.planned))
	}
	sent := f.planned[1].Message
	if !strings.Contains(sent, "interrogate me") {
		t.Errorf("c did not ask to be questioned, sent: %q", sent)
	}
	if f.planned[1].Session != "s" {
		t.Error("grilling started a new conversation instead of continuing one")
	}
	// The transcript shows the action, not the paragraph the human never wrote.
	view := m.View()
	if !strings.Contains(view, "grill me") {
		t.Errorf("the grill turn is not in the transcript:\n%s", view)
	}
	if strings.Contains(view, "interrogate me about this plan") {
		t.Error("the canned instruction was dumped into the transcript verbatim")
	}
}

// TestPlanGrillNeedsSomethingToGrill
func TestPlanGrillNeedsSomethingToGrill(t *testing.T) {
	f := newFake()
	m := openPlan(t, f)
	m, cmd := sendCmd(t, m, key("c"))
	if cmd != nil {
		t.Fatal("c grilled an empty conversation")
	}
	if scr := m.screens[SectionPlan].(*plan); scr.notice == "" {
		t.Error("c was refused silently")
	}
}

// TestPlanEmptyScreenDoesNotRepeatItself: the body already spells out n and i.
func TestPlanEmptyScreenDoesNotRepeatItself(t *testing.T) {
	f := newFake()
	m := openPlan(t, f)
	view := m.View()
	if n := strings.Count(view, "describe"); n != 1 {
		t.Errorf("the opening hints appear %d times, want 1:\n%s", n, view)
	}
}

// TestPlanNoticeDoesNotSquatOnTheFooter is the same bug on the Plan screen.
func TestPlanNoticeDoesNotSquatOnTheFooter(t *testing.T) {
	f := newFake()
	m := openPlan(t, f)

	m, _ = sendCmd(t, m, key("c")) // refused: nothing to grill yet
	if !strings.Contains(m.View(), "nothing to grill") {
		t.Fatal("the refusal was never shown")
	}

	m = send(t, m, key("j"))
	view := m.View()
	if strings.Contains(view, "nothing to grill") {
		t.Errorf("the notice outlived the key that followed it:\n%s", view)
	}
	if !strings.Contains(view, "what I should work on next") {
		t.Errorf("the footer and body hints did not come back:\n%s", view)
	}
}

// TestTypingIsVisibleOnAFreshPlanScreen is a bug hit in real use: pressing i on an empty
// conversation started an edit whose text was never drawn, because the intro branch of the view
// returned before reaching the prompt. The footer said "enter to send" while the screen showed
// nothing of what had been typed.
func TestTypingIsVisibleOnAFreshPlanScreen(t *testing.T) {
	m := openPlan(t, newFake())
	if strings.Contains(m.View(), "describe something") == false {
		t.Fatal("the fresh plan screen is not showing its intro")
	}

	m = send(t, m, key("i"))
	for _, r := range "nuns with guns" {
		m = send(t, m, key(string(r)))
	}

	if !strings.Contains(m.View(), "nuns with guns") {
		t.Errorf("typed text is not on screen:\n%s", m.View())
	}
}
