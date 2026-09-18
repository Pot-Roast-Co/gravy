package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// TestFilterDoesNotFollowYouToAnotherScreen is the reported behaviour.
//
// Filter the backlog, go to the dashboard, come back, and the whole list should be there. The
// filter was one string on the frame cleared only by esc, so it followed you instead — and
// because the frame only shows "filter: x" in a corner, the usual experience was a screen that
// was quietly, inexplicably short.
func TestFilterDoesNotFollowYouToAnotherScreen(t *testing.T) {
	f := fleetBacklog()
	m := openBacklog(t, f)

	// Search for one ticket, hiding the rest.
	m = send(t, m, key("/"))
	for _, r := range []string{"D", "i", "v", "i", "d", "e"} {
		m = send(t, m, key(r))
	}
	m = send(t, m, key("enter"))
	if view := m.View(); strings.Contains(view, "Mojo seed loop") {
		t.Fatalf("the filter was never applied:\n%s", view)
	}

	m = send(t, m, key("1")) // Dashboard
	if strings.Contains(m.View(), "filter: Divide") {
		t.Errorf("the backlog's search followed us to the dashboard:\n%s", m.View())
	}

	m = send(t, m, key(SectionBacklog.Key())) // back to Backlog
	m = send(t, m, queueLoadedMsg{state: core.StateBacklog, items: f.queue})
	view := m.View()
	if strings.Contains(view, "filter: Divide") {
		t.Errorf("the filter came back with us:\n%s", view)
	}
	// The whole list is there again.
	if !strings.Contains(view, "Mojo seed loop") {
		t.Errorf("returning to the backlog did not restore the full list:\n%s", view)
	}
}

// Staying put keeps it: re-entering the same section is not navigation, and a filter that
// vanished on a redraw would be worse than one that persisted.
func TestFilterSurvivesStayingOnTheSameScreen(t *testing.T) {
	m := openBacklog(t, fleetBacklog())
	m = send(t, m, key("/"))
	m = send(t, m, key("z"))
	m = send(t, m, key("enter"))

	m = send(t, m, key("4")) // the section it is already on
	if !strings.Contains(m.View(), "filter: z") {
		t.Errorf("pressing the current section's key cleared its own filter:\n%s", m.View())
	}
}

// A filter says how to clear itself. One you cannot see the way out of is one you clear by
// quitting the program.
func TestTheStatusBarSaysHowToClearAFilter(t *testing.T) {
	m := openBacklog(t, fleetBacklog())
	m = send(t, m, key("/"))
	for _, r := range []string{"g", "r", "a", "v", "y"} {
		m = send(t, m, key(r))
	}
	m = send(t, m, key("enter"))

	view := m.View()
	if !strings.Contains(view, "filter: gravy") {
		t.Fatalf("the filter is not shown at all:\n%s", view)
	}
	if !strings.Contains(view, "esc clears") {
		t.Errorf("nothing says how to clear it:\n%s", view)
	}

	m = send(t, m, key("esc"))
	if view := m.View(); strings.Contains(view, "filter: gravy") {
		t.Errorf("esc did not clear it:\n%s", view)
	}
}
