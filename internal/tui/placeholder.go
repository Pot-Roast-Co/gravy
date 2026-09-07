package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// placeholder stands in for a section whose screen is a later ticket.
//
// It names the ticket rather than showing an empty pane, so the frame is navigable now and the
// gap reads as "not built yet" instead of "broken".
type placeholder struct {
	section Section
	ticket  string
}

// newPlaceholders returns a screen for every section, which later tickets replace one at a time.
func newPlaceholders() map[Section]Screen {
	tickets := map[Section]string{
		SectionDashboard: "GR-025",
		SectionBacklog:   "GR-026",
		SectionReady:     "GR-026",
		SectionRunning:   "GR-027",
		SectionReview:    "GR-028",
		SectionNeedsYou:  "GR-029",
		SectionSettings:  "GR-037",
	}
	screens := make(map[Section]Screen, len(AllSections))
	for _, s := range AllSections {
		screens[s] = placeholder{section: s, ticket: tickets[s]}
	}
	// Built screens replace their placeholder here as each ticket lands.
	screens[SectionDashboard] = dashboard{}
	screens[SectionReview] = newReview()
	screens[SectionRunning] = newRunning()
	screens[SectionNeedsYou] = newNeedsYou()
	return screens
}

func (p placeholder) Update(tea.Msg, ViewContext) (Screen, tea.Cmd) { return p, nil }

func (p placeholder) View(ctx ViewContext) string {
	if ctx.Height <= 0 || ctx.Width <= 0 {
		return ""
	}
	lines := []string{
		ctx.Theme.Header.Render(p.section.Title()),
		"",
		ctx.Theme.Muted.Render(fmt.Sprintf("not built yet — %s", p.ticket)),
	}
	if len(lines) > ctx.Height {
		lines = lines[:ctx.Height]
	}
	return strings.Join(lines, "\n")
}
