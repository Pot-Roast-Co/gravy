package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/core"
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
		SectionPlan:      "",
		SectionProjects:  "",
		SectionBacklog:   "GR-026",
		SectionReady:     "GR-026",
		SectionRunning:   "GR-027",
		SectionReview:    "GR-028",
		SectionNeedsYou:  "GR-029",
		SectionSettings:  "",
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
	screens[SectionBacklog] = newQueue(core.StateBacklog)
	screens[SectionReady] = newQueue(core.StateReady)
	screens[SectionSettings] = newSettings()
	screens[SectionPlan] = newPlan()
	screens[SectionProjects] = newProjects()
	return screens
}

func (p placeholder) Update(tea.Msg, ViewContext) (Screen, tea.Cmd) { return p, nil }

func (p placeholder) View(ctx ViewContext) string {
	if ctx.Height <= 0 || ctx.Width <= 0 {
		return ""
	}
	note := "not built yet"
	if p.ticket != "" {
		note += " — " + p.ticket
	} else {
		// Settings has no ticket: it is named in ARCHITECTURE.md 9 but is not in M0's scope,
		// and claiming a ticket number it does not have would be a lie in the UI.
		note += " — not scheduled; configure with ~/.gravy/config.yaml and `gravy project add`"
	}
	lines := []string{
		ctx.Theme.Header.Render(p.section.Title()),
		"",
		ctx.Theme.Muted.Render(note),
	}
	if len(lines) > ctx.Height {
		lines = lines[:ctx.Height]
	}
	return strings.Join(lines, "\n")
}
