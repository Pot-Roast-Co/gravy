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

// filterBacklogTo drives the chooser to a labelled row, rather than counting keystrokes against
// an order the screen is free to change.
func filterBacklogTo(t *testing.T, m Model, label string) Model {
	t.Helper()
	m = send(t, m, key(projectFilterKey))
	q := m.screens[SectionBacklog].(*queue)
	if q.mode != queueProjectPick {
		t.Fatalf("%q did not open the project chooser", projectFilterKey)
	}
	for i, opt := range q.projectOptions(m.viewContext()) {
		if opt.label != label {
			continue
		}
		for q.pickCursor < i {
			m = send(t, m, key("j"))
		}
		for q.pickCursor > i {
			m = send(t, m, key("k"))
		}
		return send(t, m, key("enter"))
	}
	t.Fatalf("no row called %q in the chooser", label)
	return m
}

// TestBacklogProjectFilterSelectsSwitchesAndClears is the acceptance path: choose a project,
// switch to another, and get back to every project without leaving the screen.
func TestBacklogProjectFilterSelectsSwitchesAndClears(t *testing.T) {
	m := openBacklog(t, fleetBacklog())

	view := m.View()
	for _, want := range []string{"Backlog (3)", "project " + allProjectsLabel, "f to change"} {
		if !strings.Contains(view, want) {
			t.Fatalf("an unfiltered backlog omits %q:\n%s", want, view)
		}
	}

	m = filterBacklogTo(t, m, "mojo")
	view = m.View()
	if !strings.Contains(view, "Mojo seed loop") || strings.Contains(view, "Add Divide") {
		t.Errorf("choosing mojo did not narrow the list:\n%s", view)
	}
	for _, want := range []string{"Backlog (1)", "project mojo", "f to change"} {
		if !strings.Contains(view, want) {
			t.Errorf("the filtered backlog omits %q:\n%s", want, view)
		}
	}

	m = filterBacklogTo(t, m, "gravy")
	view = m.View()
	if !strings.Contains(view, "Add Divide") || !strings.Contains(view, "Wire notifications") {
		t.Errorf("switching to gravy did not show its tickets:\n%s", view)
	}
	if strings.Contains(view, "Mojo seed loop") || !strings.Contains(view, "project gravy") {
		t.Errorf("switching projects left the previous one's work on screen:\n%s", view)
	}

	m = filterBacklogTo(t, m, allProjectsLabel)
	view = m.View()
	if !strings.Contains(view, "Backlog (3)") || !strings.Contains(view, "Mojo seed loop") {
		t.Errorf("the filter could not be cleared from the backlog:\n%s", view)
	}
}

// TestBacklogProjectChooserCanBeCancelled leaves the list exactly as it was.
func TestBacklogProjectChooserCanBeCancelled(t *testing.T) {
	m := filterBacklogTo(t, openBacklog(t, fleetBacklog()), "gravy")

	m = send(t, m, key(projectFilterKey))
	view := m.View()
	for _, want := range []string{"Show which project?", allProjectsLabel, "gravy", "mojo", "herdr", "esc cancel"} {
		if !strings.Contains(view, want) {
			t.Errorf("the chooser omits %q:\n%s", want, view)
		}
	}

	m = send(t, m, key("j"))
	m = send(t, m, key("esc"))

	if q := m.screens[SectionBacklog].(*queue); q.projectFilter != projGravy.ID {
		t.Fatalf("filter = %q after cancelling, want it unchanged", q.projectFilter)
	}
	view = m.View()
	if !strings.Contains(view, "project gravy") || strings.Contains(view, "Mojo seed loop") {
		t.Errorf("cancelling the chooser changed the list:\n%s", view)
	}
}

// TestBacklogFilterDoesNotLeakToOtherScreens is the isolation half of the ticket, in both
// directions: the backlog's own choice must not move the frame's, and the frame's `p` must not
// silently re-filter the backlog underneath it.
func TestBacklogFilterDoesNotLeakToOtherScreens(t *testing.T) {
	f := fleetBacklog()
	m := filterBacklogTo(t, openBacklog(t, f), "mojo")

	if m.projectName() != "" {
		t.Errorf("the frame filter followed the backlog to %q", m.projectName())
	}
	if !strings.Contains(m.View(), "all projects") {
		t.Errorf("the status bar claims a fleet-wide filter that was never set:\n%s", m.View())
	}

	// Ready renders the whole fleet, because nothing narrowed it.
	m = send(t, m, key(SectionReady.Key()))
	m = send(t, m, enteredMsg{})
	m = send(t, m, queueLoadedMsg{state: core.StateReady, items: f.queue})
	view := m.View()
	if !strings.Contains(view, "Add Divide") || !strings.Contains(view, "Mojo seed loop") {
		t.Errorf("the backlog's filter narrowed Ready too:\n%s", view)
	}

	// Back on the backlog, cycling the frame's filter leaves this screen on its own choice.
	m = send(t, m, key(SectionBacklog.Key()))
	m = send(t, m, enteredMsg{})
	m = send(t, m, queueLoadedMsg{state: core.StateBacklog, items: f.queue})
	m = send(t, m, key("p"))
	if m.projectName() != "gravy" {
		t.Fatalf("p did not cycle the frame filter; it is on %q", m.projectName())
	}
	view = m.View()
	if !strings.Contains(view, "project mojo") || strings.Contains(view, "Add Divide") {
		t.Errorf("the frame's filter overrode the backlog's own:\n%s", view)
	}
}

// TestBacklogFilterSurvivesNavigation covers the session-lifetime half: leaving and coming back
// must not quietly widen the list you were working.
func TestBacklogFilterSurvivesNavigation(t *testing.T) {
	f := fleetBacklog()
	m := filterBacklogTo(t, openBacklog(t, f), "gravy")

	for _, section := range []Section{SectionDashboard, SectionReady, SectionRunning, SectionBacklog} {
		m = send(t, m, key(section.Key()))
		m = send(t, m, enteredMsg{})
	}
	m = send(t, m, queueLoadedMsg{state: core.StateBacklog, items: f.queue})

	view := m.View()
	if !strings.Contains(view, "project gravy") || !strings.Contains(view, "Backlog (2)") {
		t.Errorf("the backlog filter did not survive the trip:\n%s", view)
	}
	if strings.Contains(view, "Mojo seed loop") {
		t.Errorf("another project's work came back with it:\n%s", view)
	}
}

// TestBacklogFilterDropsHiddenSelections is the adversarial case: a selection left behind by a
// filter change is a bulk action over rows the human can no longer see.
func TestBacklogFilterDropsHiddenSelections(t *testing.T) {
	f := fleetBacklog()
	m := openBacklog(t, f)

	m = send(t, m, key("x")) // g1
	m = send(t, m, key("j"))
	m = send(t, m, key("x")) // g2
	if !strings.Contains(m.View(), "2 selected") {
		t.Fatalf("the selection was not made:\n%s", m.View())
	}

	m = filterBacklogTo(t, m, "mojo")
	if q := m.screens[SectionBacklog].(*queue); len(q.selected) != 0 {
		t.Errorf("selected = %v, want the hidden rows dropped", q.selected)
	}
	if view := m.View(); strings.Contains(view, "selected)") {
		t.Errorf("the footer still counts a selection that is off screen:\n%s", view)
	}

	// The bulk move therefore acts on the visible ticket only.
	m, cmd := sendCmd(t, m, key(" "))
	if cmd == nil {
		t.Fatal("space produced no command")
	}
	m = send(t, m, cmd())
	if len(f.moved) != 1 || f.moved[0][0] != "m1" {
		t.Fatalf("moved %v, want only the visible ticket", f.moved)
	}

	// And switching back does not resurrect them.
	m = filterBacklogTo(t, m, "gravy")
	if view := m.View(); strings.Contains(view, "selected)") || strings.Contains(view, "✓") {
		t.Errorf("a dropped selection came back with the filter:\n%s", view)
	}
}

// TestEmptyFilteredBacklogKeepsItsControls: a backlog emptied by its own filter must still say
// what is filtering it and how to change it, or it reads as a repository with no work in it.
func TestEmptyFilteredBacklogKeepsItsControls(t *testing.T) {
	m := filterBacklogTo(t, openBacklog(t, fleetBacklog()), "herdr")

	view := m.View()
	for _, want := range []string{"Backlog (0)", "nothing here", "project herdr", "f to change"} {
		if !strings.Contains(view, want) {
			t.Errorf("an empty filtered backlog omits %q:\n%s", want, view)
		}
	}

	// The chooser still opens from here, which is the way back out.
	m = filterBacklogTo(t, m, allProjectsLabel)
	if view := m.View(); !strings.Contains(view, "Backlog (3)") {
		t.Errorf("the filter could not be cleared from an empty backlog:\n%s", view)
	}
}

// TestFilteredBacklogWritesToItsOwnProject: `n` on a filtered backlog belongs to the repository
// on screen, not to the fleet's ambiguity.
func TestFilteredBacklogWritesToItsOwnProject(t *testing.T) {
	f := fleetBacklog()
	m := filterBacklogTo(t, openBacklog(t, f), "mojo")

	m = send(t, m, key("n"))
	for _, c := range "Add Modulo" {
		m = send(t, m, key(string(c)))
	}
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatalf("enter did not submit the form:\n%s", m.View())
	}
	send(t, m, cmd())

	if len(f.created) != 1 || f.created[0].ProjectID != projMojo.ID {
		t.Fatalf("created = %+v, want one ticket in the filtered project", f.created)
	}
}

// TestBacklogFilterDropsAnArchivedProject: a filter pointing at a repository that has left the
// working set would leave the screen empty with no explanation on it.
func TestBacklogFilterDropsAnArchivedProject(t *testing.T) {
	f := fleetBacklog()
	m := filterBacklogTo(t, openBacklog(t, f), "mojo")

	f.status.Projects = []api.ProjectStatus{{Project: projGravy}, {Project: projHerdr}}
	m = send(t, m, statusMsg{status: f.status})
	m = send(t, m, queueLoadedMsg{state: core.StateBacklog, items: f.queue})

	if q := m.screens[SectionBacklog].(*queue); q.projectFilter != "" {
		t.Fatalf("filter = %q, want it dropped with the project", q.projectFilter)
	}
	if view := m.View(); !strings.Contains(view, "project "+allProjectsLabel) {
		t.Errorf("the screen does not say it is back to every project:\n%s", view)
	}
}
