package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
)

// connState is how the frame is currently getting its data.
type connState int

const (
	// connConnecting covers starting the daemon and the first Status call.
	connConnecting connState = iota
	connReady
	connUnreachable
)

// Messages the frame handles. They exist so every transition is a value a test can send,
// rather than a side effect only a real terminal can produce.
type (
	connectedMsg struct {
		status api.SystemStatus
		events <-chan api.Event
		stop   func()
	}
	connectErrMsg   struct{ err error }
	statusMsg       struct{ status api.SystemStatus }
	eventMsg        struct{ event api.Event }
	eventsClosedMsg struct{}

	// gotoMsg is how a screen navigates: it names a destination and the row that should be
	// selected there, and the frame does the switching.
	gotoMsg struct {
		section Section
		focus   string
	}

	// enteredMsg tells a screen it has just become active, and which row it should open. It is
	// how a screen knows to load: no message reaches a screen that is not being shown.
	enteredMsg struct{ focus string }

	// refreshedMsg tells the active screen the fleet snapshot moved, so a screen holding data
	// of its own can re-read it. Screens that render straight from Status ignore it.
	refreshedMsg struct{}

	// sweepMsg starts a review sweep. It is raised by whichever screen the human pressed S on
	// and handled by the Review screen, which owns the card the sweep renders.
	sweepMsg struct{ ids []string }

	// navNoticeMsg carries the outcome of a navigation back to the screen the human pressed
	// enter on, for the cases where there is nowhere better to go. A row that opens nothing and
	// says nothing is indistinguishable from a wedged client.
	navNoticeMsg struct{ text string }
)

// navNotice reports a navigation that did not happen, to whichever screen is showing.
func navNotice(text string) tea.Cmd {
	return func() tea.Msg { return navNoticeMsg{text: text} }
}

// newAttention reports whether the Needs You queue grew, which is the moment worth hearing.
//
// Growth, not change: resolving an item also changes the queue, and a bell for work leaving it
// is a bell for something the human just did.
func (m Model) newAttention(next api.SystemStatus) bool {
	return m.bell && m.conn == connReady && len(next.Attention) > m.attention
}

// refreshScreen tells the active screen the snapshot moved.
func (m Model) refreshScreen() tea.Cmd {
	return func() tea.Msg { return refreshedMsg{} }
}

// ringBell writes the terminal bell.
//
// To stderr, because Bubble Tea renders on stdout and a stray byte there would land in the
// middle of a frame. The daemon writes its own bell into a log file, where nobody can hear it —
// this is the copy that reaches a person.
var ringBell tea.Cmd = func() tea.Msg {
	fmt.Fprint(os.Stderr, "\a")
	return nil
}

// bellSettingMsg carries whether notifications are switched on at all.
type bellSettingMsg struct{ enabled bool }

// loadBellSetting asks once, at connect. A client that cannot read settings simply stays quiet.
func loadBellSetting(svc api.Service) tea.Cmd {
	return func() tea.Msg {
		st, err := svc.GetSettings(context.Background())
		if err != nil {
			return bellSettingMsg{enabled: false}
		}
		return bellSettingMsg{enabled: st.Config.Notifications.Mode != config.NotifyOff}
	}
}

func entered(focus string) tea.Cmd {
	return func() tea.Msg { return enteredMsg{focus: focus} }
}

// Sweep returns a command that starts a review sweep over the given tickets.
func Sweep(ids []string) tea.Cmd {
	return func() tea.Msg { return sweepMsg{ids: ids} }
}

// Goto returns a command asking the frame to switch section.
func Goto(s Section, focus string) tea.Cmd {
	return func() tea.Msg { return gotoMsg{section: s, focus: focus} }
}

// Model is the frame: header, body, status bar, global keys and the event subscription.
type Model struct {
	wizard        *wizard
	wizardSeq     int
	initialTicket string
	daemonSound   bool
	svc           api.Service
	theme         Theme
	keys          KeyMap

	screens map[Section]Screen
	active  Section

	width, height int

	conn    connState
	connErr error
	status  api.SystemStatus

	// projectIdx indexes status.Projects, or -1 for every project at once. The dashboard spans
	// all projects by default because remembering which agent is on which repository is the
	// pain being removed.
	projectIdx int
	// projectID is which project that index meant, so a snapshot that reordered or dropped the
	// list — a project archived from another client, say — re-points the filter at the same
	// repository rather than silently at its neighbour.
	projectID string

	// attention is how many items were in the Needs You queue at the last refresh, so a new
	// one can be heard. The daemon cannot ring a bell — it has no terminal, and writes one
	// into its log file — so the client that does own a terminal rings it.
	attention int
	// bell is off when the human has turned notifications off entirely.
	bell bool
	// agents is what the daemon found when asked, for the first-run screen.
	agents []api.AgentStatus

	showHelp bool
	// adding is the global P prompt. It sits on the frame, not on a screen, so that it is
	// reachable from the empty first-run dashboard.
	adding        addProject
	projectSearch projectSearch
	filtering     bool
	filter        string
	// focus is the row a screen asked the destination to select when navigating.
	focus string

	events     <-chan api.Event
	stopEvents func()
	quitting   bool
}

// New returns the frame, wired to a service.
func New(svc api.Service) Model {
	return Model{
		svc:        svc,
		theme:      DefaultTheme(),
		keys:       DefaultKeyMap(),
		screens:    newPlaceholders(),
		active:     SectionDashboard,
		projectIdx: -1,
	}
}

type openTicketMsg struct{ id string }
type ticketDestinationMsg struct {
	id     string
	status api.SystemStatus
	err    error
	// reviewing is what the daemon said about a ticket the snapshot does not carry. An
	// attention row resolved from another client leaves a ticket sitting in Review with nothing
	// in the queue naming it, and the Review card is still where it belongs.
	reviewing bool
	// projectID is the repository the ticket belongs to when only the ticket lookup found it.
	projectID string
}

// OpenTicket asks a running TUI to navigate to the ticket using fresh state.
func OpenTicket(id string) tea.Msg { return openTicketMsg{id: id} }

// openTicket is the same request raised from inside the TUI.
func openTicket(id string) tea.Cmd {
	return func() tea.Msg { return openTicketMsg{id: id} }
}

// ticketRoute is where a ticket opens, resolved from a fresh snapshot rather than from the row
// that happened to be on screen when the key was pressed.
type ticketRoute struct {
	section Section
	// focus is always the ticket id: every destination screen matches a row on it, and it is
	// the one identifier that survives an attention row being resolved and reopened.
	focus string
	// projectID is the repository the destination belongs to, so the frame can drop a scope
	// that would hide the row it is navigating to.
	projectID string
}

// resolveTicket says where a ticket belongs now, from the snapshot alone.
//
// A pending review goes to the Review card directly. The Needs You queue is where an item is
// listed, not where it is acted on — its own enter key on a review_pending row has always handed
// straight over to Review, so routing through it adds a screen and a chance to land on nothing.
func resolveTicket(st api.SystemStatus, id string) (ticketRoute, bool) {
	if id == "" {
		return ticketRoute{}, false
	}
	for _, a := range st.Attention {
		if a.Attention.TicketID != id {
			continue
		}
		project := a.Project.ID
		if project == "" {
			project = a.Attention.ProjectID
		}
		// The state is authoritative: it is what the ticket is actually in, while the reason is
		// only what the row was raised for and outlives the decision that answered it. The reason
		// is consulted solely when the snapshot did not carry the ticket and the state is unknown.
		if a.Ticket.State == core.StateReview {
			return ticketRoute{section: SectionReview, focus: id, projectID: project}, true
		}
		if a.Attention.Reason != core.ReasonReviewPending {
			return ticketRoute{section: SectionNeedsYou, focus: id, projectID: project}, true
		}
		if a.Ticket.State == "" {
			return ticketRoute{section: SectionReview, focus: id, projectID: project}, true
		}
		// review_pending over a ticket that has moved on — approved from another client, say, and
		// now landing. A card for work already decided is exactly the empty, wrong destination this
		// routing exists to avoid, so let the queues below answer: Running carries a landing ticket.
		// If neither does, the caller's ListTickets fallback and "left the queue" notice take over.
		break
	}
	for _, r := range st.Running {
		if r.Ticket.ID == id {
			return ticketRoute{section: SectionRunning, focus: id, projectID: r.Project.ID}, true
		}
	}
	for _, r := range st.Ready {
		if r.Ticket.ID == id {
			return ticketRoute{section: SectionReady, focus: id, projectID: r.Project.ID}, true
		}
	}
	return ticketRoute{}, false
}

// resolveTicketCmd reads the ticket's current state through the API and reports where it opens.
//
// Read-only by construction: it calls Status and, at most, ListTickets. Navigating to a ticket
// never approves it, never resolves its attention item and never moves it.
func resolveTicketCmd(svc api.Service, id string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		st, err := svc.Status(ctx, api.ProjectFilter{})
		if err != nil {
			return ticketDestinationMsg{id: id, err: err}
		}
		msg := ticketDestinationMsg{id: id, status: st}
		if _, ok := resolveTicket(st, id); ok {
			return msg
		}
		// The snapshot does not carry it anywhere. Before calling it gone, ask the one question
		// that has a different answer: is it still waiting on a human?
		ts, err := svc.ListTickets(ctx, api.TicketFilter{State: core.StateReview})
		if err != nil {
			// The question that would have distinguished "gone" from "still in Review" went
			// unanswered, so nothing here is known. Reporting the snapshot would refresh the row
			// away and call the work finished on the strength of a failed read; report the
			// failure instead and leave the human a row to press again.
			return ticketDestinationMsg{id: id, err: err}
		}
		for _, t := range ts {
			if t.ID == id {
				msg.reviewing, msg.projectID = true, t.ProjectID
				break
			}
		}
		return msg
	}
}

// WithDaemonSound suppresses duplicate terminal bells when desktop audio owns alerts.
func (m Model) WithDaemonSound() Model { m.daemonSound = true; return m }

// WithTicket opens the destination using its current state once connected.
func (m Model) WithTicket(id string) Model { m.initialTicket = id; return m }

// notificationDestination is resolveTicket's section, with Review as the fallback for a ticket
// the snapshot no longer carries — the card still loads from the ticket id alone.
func notificationDestination(st api.SystemStatus, id string) Section {
	if r, ok := resolveTicket(st, id); ok {
		return r.section
	}
	return SectionReview
}

// Init connects and subscribes. Nothing polls: the first render comes from this, and every
// subsequent one from an event.
func (m Model) Init() tea.Cmd { return connect(m.svc) }

func connect(svc api.Service) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		st, err := svc.Status(ctx, api.ProjectFilter{})
		if err != nil {
			return connectErrMsg{err}
		}
		ch, stop, err := svc.Events(ctx)
		if err != nil {
			return connectErrMsg{err}
		}
		return connectedMsg{status: st, events: ch, stop: stop}
	}
}

// waitForEvent blocks on the stream and re-arms itself, which is what makes the UI push-driven.
// There is no ticker anywhere in this package.
func waitForEvent(ch <-chan api.Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg{e}
	}
}

func refreshStatus(svc api.Service) tea.Cmd {
	return func() tea.Msg {
		st, err := svc.Status(context.Background(), api.ProjectFilter{})
		if err != nil {
			return connectErrMsg{err}
		}
		return statusMsg{st}
	}
}

// Update handles global concerns and delegates the rest to the active screen.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case startWizardMsg:
		cmd := m.startWizard()
		return m, cmd
	case wizardInfoMsg:
		if m.wizard == nil || m.wizard.token != msg.token {
			return m, nil
		}
		w := m.wizard
		w.busy = false
		if msg.err != nil {
			w.notice = msg.err.Error()
			return m, nil
		}
		w.info = cloneSetup(msg.info)
		w.edit.cfg = cloneSetup(msg.info).Settings.Config
		w.edit.agents = msg.info.Settings.Agents
		w.edit.loadedOK = true
		w.nextStep(0)
		return m, nil
	case wizardPreviewMsg:
		if m.wizard == nil || m.wizard.token != msg.token {
			return m, nil
		}
		w := m.wizard
		w.busy = false
		if msg.err != nil {
			w.notice = msg.err.Error()
			return m, nil
		}
		w.project = &msg.preview.Project
		w.evidence = msg.preview.Evidence
		w.nextStep(3)
		return m, nil
	case wizardSavedMsg:
		if m.wizard == nil || m.wizard.token != msg.token {
			return m, nil
		}
		if msg.err != nil {
			m.wizard.busy = false
			m.wizard.notice = msg.err.Error()
			return m, nil
		}
		m.wizard = nil
		m.active = SectionSettings
		return m, tea.Batch(refreshStatus(m.svc), entered(""))

	case openTicketMsg:
		if msg.id == "" {
			return m, Goto(SectionDashboard, "")
		}
		return m, resolveTicketCmd(m.svc, msg.id)
	case ticketDestinationMsg:
		if msg.err != nil {
			// The snapshot the frame already holds is still good and the row is still there to
			// press again, so this is a message rather than a dead client: restarting Gravy was
			// never the fix for a read that failed once. The row and the way out lead, because
			// the footer truncates to the terminal's width and a long reason would push
			// "enter retries" off the end — a dead end again.
			return m, navNotice(fmt.Sprintf("could not open %s — enter retries: %v",
				shortID(msg.id), msg.err))
		}
		m.status = msg.status
		m = m.resyncProjectFilter()

		route, ok := resolveTicket(msg.status, msg.id)
		if !ok && msg.reviewing {
			// Its queue entry went, the work did not: resolved from another client, or
			// recovered at startup. The card is still the right place to land.
			route, ok = ticketRoute{section: SectionReview, focus: msg.id, projectID: msg.projectID}, true
		}
		if !ok {
			// Nothing to open. The frame has just refreshed, so the screen underneath this
			// message is current — say what happened and leave the human on it.
			return m, tea.Batch(
				m.refreshScreen(),
				navNotice(shortID(msg.id)+" has left the queue — it is no longer waiting on you; this screen is up to date"),
			)
		}
		m = m.unscopeFor(route.projectID)
		return m, Goto(route.section, route.focus)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case connectedMsg:
		m.conn, m.connErr = connReady, nil
		m.status = msg.status
		// The count at connect is the baseline: arriving to a queue that already has three
		// items in it is not three things happening now.
		m.attention = len(msg.status.Attention)
		m.events, m.stopEvents = msg.events, msg.stop
		cmds := []tea.Cmd{waitForEvent(msg.events), loadBellSetting(m.svc)}
		if m.initialTicket != "" {
			cmds = append(cmds, Goto(notificationDestination(msg.status, m.initialTicket), m.initialTicket))
			m.initialTicket = ""
		}
		// Only on a fresh install: probing spawns processes, and paying for that on every
		// launch to answer a question that stops mattering after the first project would be
		// a tax on everybody else.
		if len(msg.status.Projects) == 0 {
			cmds = append(cmds, func() tea.Msg { return startWizardMsg{} })
		}
		return m, tea.Batch(cmds...)

	case bellSettingMsg:
		m.bell = msg.enabled && !m.daemonSound
		return m, nil

	case agentsDetectedMsg:
		m.agents = msg.agents
		return m, nil

	case connectErrMsg:
		m.conn, m.connErr = connUnreachable, msg.err
		return m, nil

	case statusMsg:
		rang := m.newAttention(msg.status)
		m.status = msg.status
		m.attention = len(msg.status.Attention)
		m.conn, m.connErr = connReady, nil
		m = m.resyncProjectFilter()
		if rang {
			return m, tea.Batch(ringBell, m.refreshScreen())
		}
		screen, cmd := m.screens[m.active].Update(refreshedMsg{}, m.viewContext())
		m.screens[m.active] = screen
		return m, cmd

	case eventMsg:
		// Every kind means "re-read what you render", so one path serves all of them.
		return m, tea.Batch(refreshStatus(m.svc), waitForEvent(m.events))

	case eventsClosedMsg:
		// The stream ending means the daemon went away; say so rather than freezing on stale
		// data that will never update again.
		m.conn = connUnreachable
		if m.connErr == nil {
			m.connErr = fmt.Errorf("the event stream closed")
		}
		return m, nil

	case addProjectDoneMsg:
		m.adding.done(msg)
		if msg.err != nil {
			return m, nil
		}
		// Registering changes what every screen renders, so re-read rather than waiting for
		// an event the frame may not be subscribed to yet on a first run.
		return m, refreshStatus(m.svc)

	case sweepMsg:
		m = m.show(SectionReview)
		screen, cmd := m.screens[SectionReview].Update(msg, m.viewContext())
		m.screens[SectionReview] = screen
		return m, cmd

	case projectBacklogMsg:
		for i, project := range m.status.Projects {
			if project.Project.ID == msg.projectID {
				// The frame's scope, so every other screen follows the repository just
				// chosen, and a search for its name, so the Backlog you land on is narrowed
				// to it and says so in a way you can clear with esc.
				//
				// The Backlog used to keep a project filter of its own for this. Two notions
				// of "which project am I looking at" is one more than anybody can hold, and
				// the second one was invisible from every other screen.
				m.projectIdx, m.projectID = i, project.Project.ID
				m.active, m.showHelp = SectionBacklog, false
				m.focus, m.filtering = "", false
				m.filter = projectName(project.Project)
				return m, entered("")
			}
		}
		return m, refreshStatus(m.svc)

	case gotoMsg:
		m = m.show(msg.section)
		m.focus = msg.focus
		return m, entered(msg.focus)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	screen, cmd := m.screens[m.active].Update(msg, m.viewContext())
	m.screens[m.active] = screen
	return m, cmd
}

// viewContext is the snapshot both Update and View see, so a key acts on exactly the data the
// user is looking at.
func (m Model) viewContext() ViewContext {
	height := m.height - 2 // header and status bar
	if height < 0 {
		height = 0
	}
	return ViewContext{
		Svc:     m.svc,
		Project: m.projectName(),
		Width:   m.width, Height: height,
		Theme: m.theme, Status: m.status, Filter: m.filter,
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// ctrl+c always quits, whatever has the keyboard: there is always one key that gets you
	// out of a wedged screen.
	if key == "ctrl+c" {
		m.quitting = true
		if m.stopEvents != nil {
			m.stopEvents()
		}
		return m, tea.Quit
	}

	if m.wizard != nil {
		close, cmd := m.wizard.key(msg, m.svc)
		if close {
			m.wizard = nil
		}
		return m, cmd
	}
	if (m.active == SectionSettings && key == "W" && !capturing(m.screens[m.active])) || (m.active == SectionDashboard && len(m.status.Projects) == 0 && key == "P") {
		if s, ok := m.screens[SectionSettings].(*settings); ok && s.dirty {
			s.notice = "save or reload your settings edits before opening setup"
			return m, nil
		}
		cmd := m.startWizard()
		return m, cmd
	}

	// The prompt is modal: while it is open it takes every key before anything else, or a
	// path containing a "q" would quit the program mid-word.
	if m.adding.open {
		cmd := m.adding.handleKey(msg, m.svc)
		return m, cmd
	}

	if m.projectSearch.open {
		return m.searchProjectKey(msg)
	}

	// A screen with a prompt or a mode of its own gets the keyboard before the global keymap,
	// so typing a "q" does not quit the program.
	if !m.showHelp && capturing(m.screens[m.active]) {
		screen, cmd := m.screens[m.active].Update(msg, m.viewContext())
		m.screens[m.active] = screen
		return m, cmd
	}

	// A filter in progress owns the keyboard, or `q` would quit instead of typing a "q".
	if m.filtering {
		switch {
		case m.keys.Cancel.Matches(key):
			m.filtering, m.filter = false, ""
		case key == "enter":
			m.filtering = false
		case key == "backspace":
			if m.filter != "" {
				m.filter = m.filter[:len(m.filter)-1]
			}
		case len(msg.Runes) == 1:
			m.filter += string(msg.Runes)
		}
		return m, nil
	}

	switch {
	case m.keys.Quit.Matches(key):
		m.quitting = true
		if m.stopEvents != nil {
			m.stopEvents()
		}
		return m, tea.Quit

	case m.keys.Help.Matches(key):
		m.showHelp = !m.showHelp
		return m, nil

	case m.keys.Cancel.Matches(key):
		m.showHelp = false
		m.filter = ""
		return m, nil

	case m.keys.Filter.Matches(key):
		if m.active == SectionProjects || m.active == SectionPlan {
			m.projectSearch = projectSearch{open: true}
			return m, nil
		}
		m.filtering, m.filter = true, ""
		return m, nil

	case m.keys.Project.Matches(key):
		m.projectIdx = m.nextProject()
		m.projectID = ""
		if m.projectIdx >= 0 {
			m.projectID = m.status.Projects[m.projectIdx].Project.ID
		}
		return m, nil

	case m.keys.AddProject.Matches(key):
		m.adding.show()
		m.showHelp = false
		return m, nil
	}

	if s, ok := m.keys.SectionFor(key); ok {
		m = m.show(s)
		// Tell the destination it is now on screen, so a screen that loads its own data
		// knows to. Nothing else reaches a screen that was not being shown.
		return m, entered(m.focus)
	}

	screen, cmd := m.screens[m.active].Update(msg, m.viewContext())
	m.screens[m.active] = screen
	return m, cmd
}

// nextProject cycles all projects -> each project in turn -> all projects.
//
// Archived projects are skipped. The default snapshot leaves them out already, so this is the
// belt to that braces: the frame never filters onto a repository that is finished, whatever a
// snapshot taken with archived projects included happens to contain.
func (m Model) nextProject() int {
	for i := m.projectIdx + 1; i < len(m.status.Projects); i++ {
		if !m.status.Projects[i].Project.Archived {
			return i
		}
	}
	return -1
}

// projectName is the active filter's name, empty when every project is shown.
func (m Model) projectName() string {
	if m.projectIdx < 0 || m.projectIdx >= len(m.status.Projects) {
		return ""
	}
	return m.status.Projects[m.projectIdx].Project.Name
}

// show switches to a section and drops the state that should not follow the human there.
//
// The text filter is a search, and a search belongs to the screen it was typed on. It used to be
// one string on the frame, cleared only by esc, so it followed you everywhere: filter the
// backlog, press 1, and the dashboard quietly hides everything that does not match a word you
// typed about tickets. Worse on Needs You, where the status bar counts the whole fleet and the
// screen counts what survived the filter — a queue saying "nothing needs you" beside a bar
// reading "needs you 1".
//
// What does not reset: the project scope, and the Backlog's own project chooser. Those are
// scopes rather than searches — deliberate answers to "which repository am I working on" that a
// human sets once and expects to still be there. The rule is that searching is transient and
// scoping is not.
func (m Model) show(s Section) Model {
	if s != m.active {
		m.filter, m.filtering = "", false
	}
	m.active, m.showHelp = s, false
	return m
}

// resyncProjectFilter re-points the filter at the project it was set on, and drops it when that
// project has been archived or is no longer in the snapshot.
//
// Falling back to all projects is the right failure: a frame left filtered to a repository that
// has left the working set shows screens that are empty for a reason none of them explains.
func (m Model) resyncProjectFilter() Model {
	if m.projectIdx < 0 {
		return m
	}
	for i, ps := range m.status.Projects {
		if ps.Project.ID == m.projectID && !ps.Project.Archived {
			m.projectIdx = i
			return m
		}
	}
	m.projectIdx, m.projectID = -1, ""
	return m
}

// unscopeFor drops a filter that would hide the row being navigated to.
//
// The Dashboard spans every repository on purpose, so following one of its rows must not land on
// a screen scoped somewhere else and therefore empty. That empty screen reads as "nothing needs
// you", and the only way out of it used to be restarting Gravy.
func (m Model) unscopeFor(projectID string) Model {
	m.filter, m.filtering = "", false
	if projectID == "" || m.projectIdx < 0 || m.projectID == projectID {
		return m
	}
	// Back to every project rather than across to the destination's: widening shows the row
	// without silently re-pointing a scope the human set deliberately at something else.
	m.projectIdx, m.projectID = -1, ""
	return m
}

// View renders header, body and status bar into exactly the terminal's size.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	// Bubble Tea sends the first WindowSizeMsg after Init; render nothing until it arrives
	// rather than guessing at a size and flashing a wrong layout.
	if m.width <= 0 || m.height <= 0 {
		return ""
	}

	header := m.headerView()
	bar := m.statusBarView()

	bodyHeight := m.height - lipgloss.Height(header) - lipgloss.Height(bar)
	if bodyHeight < 0 {
		bodyHeight = 0
	}

	body := m.bodyView(bodyHeight)

	out := strings.Join([]string{header, body, bar}, "\n")
	// The frame never overflows its terminal, at any size: a wide row scrolls inside a screen,
	// it does not push the status bar off the bottom.
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(out)
}

func (m Model) headerView() string {
	// The numbered sections only. Settings is configuration rather than a stage of the
	// lifecycle, and its key is advertised in the status bar instead.
	parts := make([]string, 0, len(NumberedSections))
	for _, s := range NumberedSections {
		label := fmt.Sprintf("%s %s", s.Key(), s.Title())
		if s == m.active {
			parts = append(parts, m.theme.ActiveTab.Render(label))
			continue
		}
		parts = append(parts, m.theme.Tab.Render(label))
	}
	tabs := lipgloss.JoinHorizontal(lipgloss.Top, parts...)

	// Too narrow for the full strip: name where you are rather than a truncated row of numbers.
	if lipgloss.Width(tabs) > m.width {
		return m.theme.ActiveTab.MaxWidth(m.width).Render(
			fmt.Sprintf("%s %s", m.active.Key(), m.active.Title()))
	}
	return tabs
}

func (m Model) bodyView(height int) string {
	if height <= 0 {
		return ""
	}

	var body string
	switch {
	// A fresh install gets told what Gravy is and what to do, rather than an empty dashboard.
	// The Dashboard only: pressing 2 should show Plan, even on a first run — a setup screen
	// that swallows every section is a wall, not a welcome.
	case m.wizard != nil:
		ctx := m.viewContext()
		ctx.Height = height
		body = m.wizard.view(ctx)
	case m.active == SectionDashboard && m.conn == connReady &&
		len(m.status.Projects) == 0 && !m.adding.open && !m.showHelp:
		body = setupView(m.agents, m.theme, m.width)
	case m.adding.open:
		body = m.adding.view(m.theme, m.width)
	case m.projectSearch.open:
		body = m.projectSearchView(height)
	case m.showHelp:
		body = helpView(m.keys, m.theme, m.width)
	case m.conn == connUnreachable:
		body = m.unreachableView()
	case m.conn == connConnecting:
		body = m.theme.Muted.Render("starting gravy daemon…")
	default:
		ctx := m.viewContext()
		ctx.Height = height
		ctx.Focus = m.focus
		body = m.screens[m.active].View(ctx)
	}

	// Pad to the full body height so the status bar sits on the bottom line rather than
	// floating under short content.
	lines := strings.Split(body, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// unreachableView says what went wrong and what to do about it. A frame that renders an empty
// dashboard when the daemon is down is indistinguishable from an idle queue.
func (m Model) unreachableView() string {
	reason := "the daemon is not answering"
	if m.connErr != nil {
		reason = m.connErr.Error()
	}
	return strings.Join([]string{
		m.theme.Danger.Render("Cannot reach the gravy daemon."),
		"",
		m.theme.Muted.Render(reason),
		"",
		m.theme.Text.Render("Start it with " + m.theme.Key.Render("gravy serve") + ","),
		m.theme.Text.Render("or work the queue in the foreground with " + m.theme.Key.Render("gravy run") + "."),
	}, "\n")
}

func (m Model) statusBarView() string {
	var used, total int
	for _, h := range m.status.Hosts {
		used += h.UsedSlots
		total += h.TotalSlots
	}
	return statusBar{
		conn: m.conn, connErr: m.connErr,
		project:   m.projectName(),
		usedSlots: used, totalSlots: total,
		attention: len(m.status.Attention),
		updateLatest: func() string {
			if m.status.Update.Available {
				return m.status.Update.Latest
			}
			return ""
		}(),
		filter: m.filter, filtering: m.filtering,
	}.view(m.theme, m.width)
}
