package tui

import (
	"fmt"
	"strings"
)

// helpView renders the `?` overlay from the keymap.
//
// Generated, never hand-written: the list cannot drift from what Update actually dispatches
// because both read the same struct.
func helpView(k KeyMap, th Theme, width int) string {
	bindings := k.Bindings()

	labels := make([]string, len(bindings))
	widest := 0
	for i, b := range bindings {
		labels[i] = b.Label()
		if n := len(labels[i]); n > widest {
			widest = n
		}
	}

	var b strings.Builder
	b.WriteString(th.Header.Render("Keys"))
	b.WriteString("\n\n")
	for i, bind := range bindings {
		pad := strings.Repeat(" ", widest-len(labels[i]))
		fmt.Fprintf(&b, "%s%s  %s\n", th.Key.Render(labels[i]), pad, th.Text.Render(bind.Help))
	}

	b.WriteString("\n")
	b.WriteString(th.Header.Render("Sections"))
	b.WriteString("\n\n")
	for _, s := range AllSections {
		fmt.Fprintf(&b, "%s%s  %s\n",
			th.Key.Render(s.Key()), strings.Repeat(" ", widest-len(s.Key())), th.Text.Render(s.Title()))
	}

	// One line, after the sections: the overlay has to fit a short terminal, and the global
	// keys are what must never be cut off.
	b.WriteString("\n")
	running := make([]string, 0, len(runningBindings))
	for _, bind := range runningBindings {
		running = append(running, th.Key.Render(bind.Label())+" "+th.Text.Render(bind.Short))
	}
	b.WriteString(th.Header.Render("On Running  ") + strings.Join(running, th.Muted.Render(" · ")))
	b.WriteString("\n")

	body := strings.TrimRight(b.String(), "\n")
	// A narrow terminal gets the plain list: a border that cannot fit would wrap into noise.
	if width < 30 {
		return body
	}
	return th.Overlay.Render(body)
}
