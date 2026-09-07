package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bobbybrady/gravy/internal/api"
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
)

func entered(focus string) tea.Cmd {
	return func() tea.Msg { return enteredMsg{focus: focus} }
}

// Goto returns a command asking the frame to switch section.
func Goto(s Section, focus string) tea.Cmd {
	return func() tea.Msg { return gotoMsg{section: s, focus: focus} }
}

// Model is the frame: header, body, status bar, global keys and the event subscription.
type Model struct {
	svc   api.Service
	theme Theme
	keys  KeyMap

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

	showHelp  bool
	filtering bool
	filter    string
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

// Init connects and subscribes. Nothing polls: the first render comes from this, and every
// subsequent one from an event.
func (m Model) Init() tea.Cmd { return connect(m.svc) }

func connect(svc api.Service) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		st, err := svc.Status(ctx)
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
		st, err := svc.Status(context.Background())
		if err != nil {
			return connectErrMsg{err}
		}
		return statusMsg{st}
	}
}

// Update handles global concerns and delegates the rest to the active screen.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case connectedMsg:
		m.conn, m.connErr = connReady, nil
		m.status = msg.status
		m.events, m.stopEvents = msg.events, msg.stop
		return m, waitForEvent(msg.events)

	case connectErrMsg:
		m.conn, m.connErr = connUnreachable, msg.err
		return m, nil

	case statusMsg:
		m.status = msg.status
		m.conn, m.connErr = connReady, nil
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

	case gotoMsg:
		m.active, m.showHelp = msg.section, false
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
		m.filtering, m.filter = true, ""
		return m, nil

	case m.keys.Project.Matches(key):
		m.projectIdx = m.nextProject()
		return m, nil
	}

	if s, ok := m.keys.SectionFor(key); ok {
		m.active, m.showHelp = s, false
		// Tell the destination it is now on screen, so a screen that loads its own data
		// knows to. Nothing else reaches a screen that was not being shown.
		return m, entered(m.focus)
	}

	screen, cmd := m.screens[m.active].Update(msg, m.viewContext())
	m.screens[m.active] = screen
	return m, cmd
}

// nextProject cycles all projects -> each project in turn -> all projects.
func (m Model) nextProject() int {
	if len(m.status.Projects) == 0 {
		return -1
	}
	if m.projectIdx+1 >= len(m.status.Projects) {
		return -1
	}
	return m.projectIdx + 1
}

// projectName is the active filter's name, empty when every project is shown.
func (m Model) projectName() string {
	if m.projectIdx < 0 || m.projectIdx >= len(m.status.Projects) {
		return ""
	}
	return m.status.Projects[m.projectIdx].Project.Name
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
	parts := make([]string, 0, len(AllSections))
	for _, s := range AllSections {
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
		filter:    m.filter, filtering: m.filtering,
	}.view(m.theme, m.width)
}
