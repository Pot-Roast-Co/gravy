package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// archiveFixture is the projects fixture plus one repository whose work is over.
func archiveFixture() *fakeService {
	f := projectFixture()
	f.projects = append(f.projects, core.Project{
		ID: "p4", Slug: "acorn", Name: "acorn", RepoPath: "/home/bobby/Projects/acorn",
		TargetBranch: "main", MergeMode: core.LandMerge, MaxConcurrency: 1, Archived: true,
	})
	return f
}

// expandArchived puts the cursor on the archived group's header and opens it.
func expandArchived(t *testing.T, m Model) Model {
	t.Helper()
	scr := m.screens[SectionProjects].(*projects)
	for i, row := range scr.rows() {
		if !row.group {
			continue
		}
		for scr.cursor < i {
			m = send(t, m, key("j"))
		}
		for scr.cursor > i {
			m = send(t, m, key("k"))
		}
		return send(t, m, key("enter"))
	}
	t.Fatal("no archived group on screen")
	return m
}

// TestArchivedProjectsCollapseAtTheBottom is what makes archiving worth doing: the list a human
// scans is this week's work, and the finished repositories are one keystroke away rather than
// interleaved with it.
func TestArchivedProjectsCollapseAtTheBottom(t *testing.T) {
	f := archiveFixture()
	m := openProjects(t, f)
	view := m.View()

	// The heading counts the working set. Counting archived projects in it would be the
	// problem this screen had — a number that only ever grows.
	if !strings.Contains(view, "Projects (3)") {
		t.Errorf("heading does not count only the working set:\n%s", view)
	}
	if !strings.Contains(view, "archived (1)") {
		t.Errorf("no archived group:\n%s", view)
	}
	// "acorn" sorts first by name, so a screen that drew it in place would show it above
	// gravy. Collapsed means collapsed.
	if strings.Contains(view, "acorn") {
		t.Errorf("an archived project is drawn in the list while the group is collapsed:\n%s", view)
	}

	m = expandArchived(t, m)
	view = m.View()
	if !strings.Contains(view, "acorn") {
		t.Errorf("enter did not expand the archived group:\n%s", view)
	}
	if !strings.Contains(view, "archived ·") {
		t.Errorf("the archived row does not say why it is down there:\n%s", view)
	}

	// The group header is not a project: the keys that act on one must do nothing there rather
	// than act on whichever project the cursor happens to be next to.
	scr := m.screens[SectionProjects].(*projects)
	for i, row := range scr.rows() {
		if !row.group {
			continue
		}
		for scr.cursor < i {
			m = send(t, m, key("j"))
		}
		m, cmd := sendCmd(t, m, key("z"))
		if cmd != nil {
			t.Error("z on the archived group's header asked the service for something")
		}
		if got := m.screens[SectionProjects].(*projects).mode; got != projectBrowsing {
			t.Errorf("mode = %v after z on the group header, want browsing", got)
		}
		break
	}
	if len(f.archived) != 0 {
		t.Errorf("service saw %+v without a project being selected", f.archived)
	}
}

// TestArchiveConfirmsAndGoesThroughTheService.
//
// The confirm is one key rather than a typed name because archiving keeps everything and the
// same key undoes it — but it is still a confirm, because z is one keystroke away from the
// movement keys and a project silently leaving the list is a confusing way to find that out.
func TestArchiveConfirmsAndGoesThroughTheService(t *testing.T) {
	f := archiveFixture()
	m := openProjects(t, f)
	m = selectProject(t, m, "gravy")

	m = send(t, m, key("z"))
	scr := m.screens[SectionProjects].(*projects)
	if scr.mode != projectConfirmArchive {
		t.Fatal("z did not ask for confirmation")
	}
	if !strings.Contains(scr.notice, "archive gravy?") {
		t.Errorf("notice = %q, want it to name the project and the act", scr.notice)
	}

	// Anything but y cancels.
	m, cmd := sendCmd(t, m, key("n"))
	if cmd != nil {
		t.Fatal("a cancelled confirm still called the service")
	}
	if len(f.archived) != 0 {
		t.Fatalf("archived %+v after cancelling", f.archived)
	}
	if got := m.screens[SectionProjects].(*projects).mode; got != projectBrowsing {
		t.Errorf("mode = %v after cancelling, want browsing", got)
	}

	m = send(t, m, key("z"))
	m, cmd = sendCmd(t, m, key("y"))
	if cmd == nil {
		t.Fatal("confirming did not call the service")
	}
	m = send(t, m, cmd())

	if len(f.archived) != 1 || f.archived[0] != (archiveCall{id: "p1", archived: true}) {
		t.Fatalf("service saw %+v, want p1 archived", f.archived)
	}

	// The reload the service reply triggers, fed back the way Bubble Tea would.
	m = send(t, m, projectsLoadedMsg{projects: f.projects, tickets: f.allTickets})
	view := m.View()
	if !strings.Contains(view, "Projects (2)") {
		t.Errorf("the working set did not shrink:\n%s", view)
	}
	// Expanded on archiving, so the project is visibly somewhere rather than apparently gone.
	if !strings.Contains(view, "archived (2)") || !strings.Contains(view, "gravy") {
		t.Errorf("the archived project is nowhere to be seen:\n%s", view)
	}
}

// TestUnarchiveIsTheSameKey: the way back has to be as findable as the way out, or archiving is
// a one-way door with a friendly name.
func TestUnarchiveIsTheSameKey(t *testing.T) {
	f := archiveFixture()
	m := openProjects(t, f)
	m = expandArchived(t, m)
	m = selectProject(t, m, "acorn")

	m = send(t, m, key("z"))
	if notice := m.screens[SectionProjects].(*projects).notice; !strings.Contains(notice, "unarchive acorn?") {
		t.Errorf("notice = %q, want the confirm to offer the way back", notice)
	}
	m, cmd := sendCmd(t, m, key("y"))
	if cmd == nil {
		t.Fatal("confirming did not call the service")
	}
	send(t, m, cmd())

	if len(f.archived) != 1 || f.archived[0] != (archiveCall{id: "p4", archived: false}) {
		t.Errorf("service saw %+v, want p4 unarchived", f.archived)
	}
}

// TestProjectFilterFallsBackWhenItsProjectIsArchived.
//
// The frame filters every screen to one project. If that project is archived from anywhere else
// — the CLI, another client — the next snapshot no longer has it, and a frame that kept its
// index would either point at whichever project inherited the slot or show screens that are
// empty for a reason none of them explains. All projects is the honest fallback.
func TestProjectFilterFallsBackWhenItsProjectIsArchived(t *testing.T) {
	f := newFake()
	m := boot(t, f, 110, 30)

	m = send(t, m, key("p")) // filter to gravy, the first project
	if m.projectIdx != 0 || m.projectName() != "gravy" {
		t.Fatalf("p filtered to %d (%q), want gravy", m.projectIdx, m.projectName())
	}

	// gravy is archived elsewhere and leaves the snapshot; mojo takes its index.
	f.status.Projects = []api.ProjectStatus{
		{Project: core.Project{ID: "p2", Name: "mojo", Slug: "mojo"}},
	}
	st, err := f.Status(context.Background(), api.ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	m = send(t, m, statusMsg{status: st})

	if m.projectIdx != -1 {
		t.Errorf("projectIdx = %d (%q), want every project — the filtered one is gone",
			m.projectIdx, m.projectName())
	}
	if m.projectName() != "" {
		t.Errorf("frame still claims to be filtered to %q", m.projectName())
	}
}

// TestProjectCycleSkipsArchivedProjects: an archived project is in the snapshot only while its
// last work is in flight, and cycling onto a repository that is finished is not a view anybody
// asked for.
func TestProjectCycleSkipsArchivedProjects(t *testing.T) {
	f := newFake()
	f.status.Projects = []api.ProjectStatus{
		{Project: core.Project{ID: "p1", Name: "gravy", Slug: "gravy"}},
		// Still listed because it has a ticket in flight; still not somewhere to park the frame.
		{Project: core.Project{ID: "p2", Name: "mojo", Slug: "mojo", Archived: true},
			Active: &core.Ticket{ID: "t9", ProjectID: "p2", State: core.StateReview}},
		{Project: core.Project{ID: "p3", Name: "acorn", Slug: "acorn"}},
	}
	m := boot(t, f, 110, 30)

	for _, want := range []string{"gravy", "acorn", ""} {
		m = send(t, m, key("p"))
		if got := m.projectName(); got != want {
			t.Fatalf("p cycled to %q, want %q", got, want)
		}
	}
}
