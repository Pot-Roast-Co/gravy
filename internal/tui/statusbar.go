package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// statusBar is the persistent bottom line: where you are, what the daemon is doing, and how much
// is waiting on you.
//
// It is a value rather than a method on Model so its content can be asserted directly, without
// standing up a terminal.
type statusBar struct {
	conn    connState
	connErr error
	// project is the active project filter's name, empty when every project is shown.
	project               string
	usedSlots, totalSlots int
	attention             int
	filter                string
	filtering             bool
}

func (s statusBar) view(th Theme, width int) string {
	if width <= 0 {
		return ""
	}

	var health string
	switch s.conn {
	case connConnecting:
		health = th.Muted.Render("starting gravy daemon…")
	case connReady:
		health = th.Success.Render("daemon ok")
	case connUnreachable:
		health = th.Danger.Render("daemon unreachable")
	}

	project := s.project
	if project == "" {
		project = "all projects"
	}

	attention := th.Muted.Render(fmt.Sprintf("needs you %d", s.attention))
	if s.attention > 0 {
		attention = th.Warning.Render(fmt.Sprintf("needs you %d", s.attention))
	}

	left := strings.Join([]string{
		health,
		th.Muted.Render(project),
		th.Muted.Render(fmt.Sprintf("workers %d/%d", s.usedSlots, s.totalSlots)),
		attention,
	}, th.Muted.Render("  ·  "))

	// A filter in progress replaces the hint, because that is what the next keystroke does.
	// Settings is not on the number row — that row is the ticket lifecycle — so its key lives
	// here, beside the other one that is always available. Off the row but never hidden.
	right := th.Muted.Render(SettingsKey + " settings  ·  ? help")
	if s.filtering {
		right = th.Accent.Render("/" + s.filter)
	} else if s.filter != "" {
		right = th.Muted.Render("filter: " + s.filter)
	}

	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		// Too narrow for both: the health of the queue outranks a hint the user can discover.
		return th.StatusBar.MaxWidth(width).Render(left)
	}
	return th.StatusBar.MaxWidth(width).Render(left + strings.Repeat(" ", gap) + right)
}
