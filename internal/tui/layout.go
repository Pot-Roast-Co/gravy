package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/core"
)

// col is one cell in a row.
type col struct {
	text string
	// width is the cell's fixed size. Ignored when flex is set.
	width int
	// flex takes whatever width the fixed columns leave. At most one column should set it.
	flex  bool
	right bool
	style lipgloss.Style
}

// columns lays cells out to exactly total width.
//
// Every cell is truncated and padded as plain text before any style is applied, so escape
// sequences can never affect the arithmetic — the bug that turns a tidy table into a ragged one
// the first time a value is coloured.
func columns(total int, cols ...col) string {
	if total <= 0 || len(cols) == 0 {
		return ""
	}

	gaps := len(cols) - 1
	fixed := 0
	for _, c := range cols {
		if !c.flex {
			fixed += c.width
		}
	}
	flex := total - fixed - gaps
	if flex < 0 {
		flex = 0
	}

	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		w := c.width
		if c.flex {
			w = flex
		}
		parts = append(parts, c.style.Render(fit(c.text, w, c.right)))
	}

	line := strings.Join(parts, " ")
	// Belt and braces: a rounding error must never push the status bar off the screen.
	return lipgloss.NewStyle().MaxWidth(total).Render(line)
}

// fit truncates s to width with an ellipsis and pads it out, aligning right when asked.
func fit(s string, width int, right bool) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	switch {
	case len(r) > width && width == 1:
		return "…"
	case len(r) > width:
		s = string(r[:width-1]) + "…"
	}
	pad := strings.Repeat(" ", width-len([]rune(s)))
	if right {
		return pad + s
	}
	return s + pad
}

// age renders a duration compactly enough for a table column.
//
// A queue without ages hides the item ignored for two days behind the one raised a minute ago,
// so this favours being readable at a glance over being precise.
func age(d time.Duration) string {
	switch {
	case d < 0:
		return "—"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// projectName is what a row calls a repository.
func projectName(p core.Project) string {
	switch {
	case p.Name != "":
		return p.Name
	case p.Slug != "":
		return p.Slug
	default:
		return "—"
	}
}

// branchName drops the gravy/ prefix every worktree branch carries; repeating it on every row
// spends columns to say nothing.
func branchName(b string) string {
	if b == "" {
		return "—"
	}
	return strings.TrimPrefix(b, "gravy/")
}

// shortID is the prefix of an id, which is all a human needs to tell rows apart.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// pinFooter scrolls lines to fit, keeping footer on the bottom line.
//
// A footer inside the scrolling region is a footer you never see once the content is longer than
// the terminal — which is exactly when its hints, its validation messages and its unsaved-changes
// warning matter most.
func pinFooter(lines []string, selected, height int, th Theme, footer string) string {
	if height <= 1 {
		return window(lines, selected, height, th)
	}
	body := window(lines, selected, height-2, th)
	return strings.Join([]string{body, "", footer}, "\n")
}
