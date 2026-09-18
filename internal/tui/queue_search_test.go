package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// "/" narrows the backlog to one repository, which is what the project chooser is for.
func TestSearchMatchesTheProjectName(t *testing.T) {
	f := fleetBacklog()
	m := openBacklog(t, f)

	m = send(t, m, key("/"))
	for _, r := range []string{"m", "o", "j", "o"} {
		m = send(t, m, key(r))
	}
	m = send(t, m, key("enter"))

	view := m.View()
	if !strings.Contains(view, "Mojo seed loop") {
		t.Errorf("searching a project name hid that project's own work:\n%s", view)
	}
	if strings.Contains(view, "Add Divide") {
		t.Errorf("another project's tickets survived the search:\n%s", view)
	}
}

// Title and body still match; the project name is an addition, not a replacement.
func TestSearchStillMatchesTitleAndBody(t *testing.T) {
	f := fleetBacklog()

	for _, tc := range []struct{ typed, want, gone string }{
		{"Divide", "Add Divide", "Mojo seed loop"},
		{"zero", "Add Divide", "Mojo seed loop"}, // "handle the zero case" is body
	} {
		m := openBacklog(t, f)
		m = send(t, m, key("/"))
		for _, r := range strings.Split(tc.typed, "") {
			m = send(t, m, key(r))
		}
		m = send(t, m, key("enter"))
		m = send(t, m, queueLoadedMsg{state: core.StateBacklog, items: f.queue})

		view := m.View()
		if !strings.Contains(view, tc.want) {
			t.Errorf("%q lost %q:\n%s", tc.typed, tc.want, view)
		}
		if strings.Contains(view, tc.gone) {
			t.Errorf("%q kept %q:\n%s", tc.typed, tc.gone, view)
		}
	}
}
