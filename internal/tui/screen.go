package tui

import (
	"strconv"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
)

// Section is one destination in the frame, reachable by its number key.
//
// M0 ships seven, which is why the global keymap is 1-7. Done and history (GR-030) is M1 and
// deliberately absent rather than present and empty.
type Section int

// The sections, in the order they appear in the header and on the number keys.
const (
	SectionDashboard Section = iota
	SectionBacklog
	SectionReady
	SectionRunning
	SectionReview
	SectionNeedsYou
	SectionSettings
)

// AllSections lists every section in header order.
var AllSections = []Section{
	SectionDashboard, SectionBacklog, SectionReady, SectionRunning,
	SectionReview, SectionNeedsYou, SectionSettings,
}

// Title is the section's name in the header and help.
func (s Section) Title() string {
	switch s {
	case SectionDashboard:
		return "Dashboard"
	case SectionBacklog:
		return "Backlog"
	case SectionReady:
		return "Ready"
	case SectionRunning:
		return "Running"
	case SectionReview:
		return "Review"
	case SectionNeedsYou:
		return "Needs You"
	case SectionSettings:
		return "Settings"
	default:
		return "?"
	}
}

// Key is the number key that jumps to the section.
func (s Section) Key() string { return strconv.Itoa(int(s) + 1) }

// ViewContext is everything a screen needs to draw itself.
//
// The frame fetches; screens render. A screen that wants data not in here is asking for an
// api.Service method, not for permission to open the store.
type ViewContext struct {
	// Svc is the service. Screens issue their own reads and calls through it — the TUI holds
	// no domain logic, but "render Service results and send Service calls" is exactly its job.
	Svc api.Service
	// Width and Height are the space the screen owns, excluding the header and status bar.
	Width, Height int
	Theme         Theme
	// Status is the most recent system snapshot, refreshed on push.
	Status api.SystemStatus
	// Filter is the active `/` filter, empty when none.
	Filter string
	// Focus is the row id a screen was asked to select when navigated to, empty otherwise.
	Focus string
	// Project is the name of the project the frame is filtered to, empty for all of them.
	Project string
}

// keyCapturer is a screen that needs the keyboard before the global keymap sees it.
//
// A prompt, a form, or a mode with its own exit key. Without this the frame's `q` quits the
// program while someone is typing a word containing one, and `1`-`7` jump section mid-sentence.
// ctrl+c is never captured: there is always one key that gets you out.
type keyCapturer interface {
	CapturesKeys() bool
}

// capturing reports whether s wants the keyboard to itself right now.
func capturing(s Screen) bool {
	c, ok := s.(keyCapturer)
	return ok && c.CapturesKeys()
}

// Screen is one section's content.
//
// The frame owns layout, global keys, fetching and the event subscription. A screen handles only
// the keys the frame did not claim, and renders into the space it is given.
type Screen interface {
	// Update handles a message the frame did not consume. It receives the same context View
	// does, because a key press usually acts on the data currently shown — pressing enter on a
	// row has to know which row that is.
	Update(msg tea.Msg, ctx ViewContext) (Screen, tea.Cmd)
	// View renders the screen's body. It must fit within ctx.Width and ctx.Height.
	View(ctx ViewContext) string
}
