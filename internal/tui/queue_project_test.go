package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// fleetBacklog is a backlog with work in two repositories and a third registered but empty, which
// is the only shape in which a project filter can be wrong.
func fleetBacklog() *fakeService {
	f := newFake()
	f.status.Projects = append(f.status.Projects, api.ProjectStatus{Project: projHerdr})
	f.queue = []api.TicketDetail{
		{Ticket: core.Ticket{ID: "g1", Title: "Add Divide", Body: "handle the zero case", Position: 1024},
			Project: projGravy},
		{Ticket: core.Ticket{ID: "g2", Title: "Wire notifications", Body: "bell and OS", Position: 2048},
			Project: projGravy},
		{Ticket: core.Ticket{ID: "m1", Title: "Mojo seed loop", Body: "plant and water", Position: 1024},
			Project: projMojo},
	}
	return f
}

// searchBacklogFor types a search into the Backlog, which is how a human narrows it to one
// repository now that the project chooser is gone.
func searchBacklogFor(t *testing.T, m Model, needle string) Model {
	t.Helper()
	m = send(t, m, key("/"))
	for _, r := range strings.Split(needle, "") {
		m = send(t, m, key(r))
	}
	return send(t, m, key("enter"))
}

// TestBacklogSearchSelectsSwitchesAndClears is the acceptance path: narrow to a project, switch
// to another, and get back to all of them without leaving the screen.
func TestBacklogSearchSelectsSwitchesAndClears(t *testing.T) {
	f := fleetBacklog()
	m := searchBacklogFor(t, openBacklog(t, f), "gravy")
	if view := m.View(); !strings.Contains(view, "Backlog (2)") || strings.Contains(view, "Mojo seed loop") {
		t.Fatalf("searching for one project did not narrow to it:\n%s", view)
	}

	m = send(t, m, key("esc"))
	m = searchBacklogFor(t, m, "mojo")
	if view := m.View(); !strings.Contains(view, "Backlog (1)") || strings.Contains(view, "Add Divide") {
		t.Fatalf("switching projects kept the old one:\n%s", view)
	}

	// esc is the way back to everything, and the screen says so.
	m = send(t, m, key("esc"))
	if view := m.View(); !strings.Contains(view, "Backlog (3)") {
		t.Fatalf("esc did not clear the search:\n%s", view)
	}
}

// TestEmptyFilteredBacklogSaysHowToGetOut: a narrowed-to-nothing backlog must not look like an
// empty one, and must carry its own way out.
func TestEmptyFilteredBacklogSaysHowToGetOut(t *testing.T) {
	m := searchBacklogFor(t, openBacklog(t, fleetBacklog()), "herdr")

	view := m.View()
	for _, want := range []string{"Backlog (0)", "nothing here", "matching herdr", "esc"} {
		if !strings.Contains(view, want) {
			t.Errorf("an empty filtered backlog omits %q:\n%s", want, view)
		}
	}

	m = send(t, m, key("esc"))
	if view := m.View(); !strings.Contains(view, "Backlog (3)") {
		t.Errorf("the search could not be cleared from an empty backlog:\n%s", view)
	}
}

// TestSelectionsHiddenByASearchAreNotCounted is the adversarial case, kept from the chooser it
// was written for: a selection the filter hides is a bulk action over rows nobody can see.
func TestSelectionsHiddenByASearchAreNotCounted(t *testing.T) {
	f := fleetBacklog()
	m := openBacklog(t, f)

	m = send(t, m, key("x")) // g1
	m = send(t, m, key("j"))
	m = send(t, m, key("x")) // g2
	if !strings.Contains(m.View(), "2 selected") {
		t.Fatalf("the selection was not made:\n%s", m.View())
	}

	m = searchBacklogFor(t, m, "mojo")
	if view := m.View(); strings.Contains(view, "selected)") {
		t.Errorf("the footer counts a selection that is off screen:\n%s", view)
	}

	// And the bulk action reaches only what is visible.
	q := m.screens[SectionBacklog].(*queue)
	targets := q.targets(q.visible(m.viewContext()))
	for _, tk := range targets {
		if tk.ID == "g1" || tk.ID == "g2" {
			t.Errorf("a hidden ticket %q was a target", tk.ID)
		}
	}
}

// TestSearchedBacklogStillWritesToARealProject: "n" on a narrowed backlog must still know which
// repository a new ticket belongs to.
func TestSearchedBacklogStillWritesToARealProject(t *testing.T) {
	f := fleetBacklog()
	m := searchBacklogFor(t, openBacklog(t, f), "mojo")
	q := m.screens[SectionBacklog].(*queue)

	if id := q.projectID(m.viewContext()); id == "" {
		t.Error("a new ticket on a narrowed backlog has no project to belong to")
	}
}
