package tui

import "github.com/charmbracelet/lipgloss"

// Theme holds the styles every screen draws with.
//
// One good default, expressed as tokens rather than literal colours at each call site, so a
// second theme later is a different Theme value and not an edit to every screen. Colours are
// adaptive: the terminal's own palette decides light or dark, because a scheme that fights the
// user's terminal is worse than no scheme.
type Theme struct {
	// Text is ordinary content.
	Text lipgloss.Style
	// Muted is secondary detail — ages, counts, hints.
	Muted lipgloss.Style
	// Accent marks the active section and selected rows.
	Accent lipgloss.Style
	// Danger is failure: unreachable daemons, red validation.
	Danger lipgloss.Style
	// Warning is an amber flag — assumptions, held queues.
	Warning lipgloss.Style
	// Success is green validation and landed work.
	Success lipgloss.Style

	// Header is the top line carrying the section tabs.
	Header lipgloss.Style
	// Tab and ActiveTab render one section name in the header.
	Tab       lipgloss.Style
	ActiveTab lipgloss.Style
	// StatusBar is the persistent bottom line.
	StatusBar lipgloss.Style
	// Key renders a keybinding inside help and hints.
	Key lipgloss.Style
	// Overlay frames the help panel.
	Overlay lipgloss.Style
}

// DefaultTheme is the one good default.
func DefaultTheme() Theme {
	var (
		fg     = lipgloss.AdaptiveColor{Light: "236", Dark: "252"}
		muted  = lipgloss.AdaptiveColor{Light: "245", Dark: "244"}
		accent = lipgloss.AdaptiveColor{Light: "27", Dark: "39"}
		danger = lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
		warn   = lipgloss.AdaptiveColor{Light: "130", Dark: "214"}
		good   = lipgloss.AdaptiveColor{Light: "28", Dark: "78"}
		rule   = lipgloss.AdaptiveColor{Light: "252", Dark: "238"}
	)

	base := lipgloss.NewStyle().Foreground(fg)
	return Theme{
		Text:    base,
		Muted:   lipgloss.NewStyle().Foreground(muted),
		Accent:  lipgloss.NewStyle().Foreground(accent),
		Danger:  lipgloss.NewStyle().Foreground(danger),
		Warning: lipgloss.NewStyle().Foreground(warn),
		Success: lipgloss.NewStyle().Foreground(good),

		Header:    lipgloss.NewStyle().Foreground(fg).Bold(true),
		Tab:       lipgloss.NewStyle().Foreground(muted).Padding(0, 1),
		ActiveTab: lipgloss.NewStyle().Foreground(accent).Bold(true).Padding(0, 1).Underline(true),
		StatusBar: lipgloss.NewStyle().Foreground(muted),
		Key:       lipgloss.NewStyle().Foreground(accent).Bold(true),
		Overlay: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).BorderForeground(rule).Padding(0, 2),
	}
}
