package tui

import (
	"github.com/charmbracelet/x/ansi"
	"strings"
)

// inputLines wraps at terminal cell boundaries and keeps the insertion point visible.
// Clipping affects only rendering; the full value is preserved for submission.
func inputLines(label, value string, width, height int, th Theme) []string {
	width = max(1, width)
	rows := strings.Split(ansi.Hardwrap(value+"▏", width, true), "\n")
	limit := max(1, height-1)
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
		label = "↑ earlier text · " + label
	}
	out := []string{th.Muted.Render(trunc(label, width))}
	for _, row := range rows {
		out = append(out, th.Accent.Render(row))
	}
	return out
}

func feedbackInput(value, hint string, width, height int, th Theme) string {
	rows := inputLines("what needs to change:", value, width, max(2, min(8, height-2)), th)
	rows = append(rows, th.Muted.Render(trunc(hint, max(1, width))))
	return strings.Join(rows, "\n")
}

func cursorRow(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "▏") {
			return i
		}
	}
	return -1
}

// actionFooter keeps status messages above the available actions, wrapping instead of hiding keys.
func actionFooter(notice, actions string, width int, th Theme) string {
	width = max(1, width)
	actions = ansi.Wrap(actions, width, "")
	if notice == "" {
		return actions
	}
	return th.Warning.Render(trunc(notice, width)) + "\n" + actions
}
