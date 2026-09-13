package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// openProjects boots the frame on the Projects screen with its data loaded.
func openProjects(t *testing.T, f *fakeService) Model {
	t.Helper()
	m := boot(t, f, 110, 30)
	m = send(t, m, key(SectionProjects.Key()))
	return send(t, m, projectsLoadedMsg{projects: f.projects, tickets: f.allTickets})
}

// selectProject moves the cursor onto a project by name, rather than by counting keystrokes
// against an order the screen is free to change.
func selectProject(t *testing.T, m Model, name string) Model {
	t.Helper()
	scr := m.screens[SectionProjects].(*projects)
	for i, p := range scr.projects {
		if p.Name == name {
			for scr.cursor < i {
				m = send(t, m, key("j"))
			}
			for scr.cursor > i {
				m = send(t, m, key("k"))
			}
			return m
		}
	}
	t.Fatalf("no project called %q on screen", name)
	return m
}

func projectFixture() *fakeService {
	f := newFake()
	f.projects = []core.Project{
		{
			ID: "p1", Slug: "gravy", Name: "gravy", RepoPath: "/home/bobby/Projects/gravy",
			TargetBranch: "main", MergeMode: core.LandMerge, MaxConcurrency: 1,
		},
		{
			ID: "p2", Slug: "pocket-blooms", Name: "pocket-blooms",
			RepoPath: "/Users/bobbybrady/Projects/pocket-blooms", HostID: "air",
			TargetBranch: "main", MergeMode: core.LandMerge, MaxConcurrency: 1,
			Notes: "A cosy garden game. Ship the seed loop before anything else.",
		},
		{ID: "p3", Slug: "idea", Name: "idea", Notes: "Not sure yet."},
	}
	f.allTickets = []core.Ticket{
		{ID: "t1", ProjectID: "p1", State: core.StateReady},
		{ID: "t2", ProjectID: "p1", State: core.StateReady},
		{ID: "t3", ProjectID: "p1", State: core.StateReview},
		{ID: "t4", ProjectID: "p2", State: core.StateBacklog},
	}
	return f
}

// TestProjectsShowsWhatIsInEach is the reason to look at this screen rather than the project
// list in Settings: what is queued, and where it runs.
func TestProjectsShowsWhatIsInEach(t *testing.T) {
	m := openProjects(t, projectFixture())
	view := m.View()

	for _, want := range []string{"gravy", "pocket-blooms", "idea", "air", "local"} {
		if !strings.Contains(view, want) {
			t.Errorf("view omits %q:\n%s", want, view)
		}
	}
	// Counts, so a project's row says what is in it.
	if !strings.Contains(view, "2 ready") || !strings.Contains(view, "1 review") {
		t.Errorf("ticket counts are missing:\n%s", view)
	}
	if !strings.Contains(view, "no tickets") {
		t.Errorf("a project with nothing in it does not say so:\n%s", view)
	}
}

// TestProjectWithoutARepositorySaysSo: finding out from a ticket that never starts is worse.
func TestProjectWithoutARepositorySaysSo(t *testing.T) {
	f := projectFixture()
	m := openProjects(t, f)

	m = selectProject(t, m, "idea")

	view := m.View()
	if !strings.Contains(view, "no repository yet") {
		t.Errorf("a repo-less project does not say so:\n%s", view)
	}
	if !strings.Contains(view, "nothing can run here") {
		t.Errorf("it does not say what that costs:\n%s", view)
	}
}

// TestProjectNotesAreVisibleAndEditable: notes are what planning reads, so they belong on screen
// rather than in a config file.
func TestProjectNotesAreVisibleAndEditable(t *testing.T) {
	f := projectFixture()
	m := openProjects(t, f)
	m = selectProject(t, m, "pocket-blooms")

	if !strings.Contains(m.View(), "cosy garden game") {
		t.Errorf("notes are not shown:\n%s", m.View())
	}

	m = send(t, m, key("e"))
	scr := m.screens[SectionProjects].(*projects)
	if scr.mode != projectEditingNotes {
		t.Fatal("e did not start editing notes")
	}
	// Editing starts from what is there, rather than making you retype it.
	if !strings.Contains(scr.input, "cosy garden game") {
		t.Errorf("editing started from %q, want the existing notes", scr.input)
	}

	m = typeKeys(t, m, " Then weather.")
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter saved nothing")
	}
	send(t, m, cmd())

	if len(f.savedProjects) == 0 {
		t.Fatal("no project was updated")
	}
	saved := f.savedProjects[len(f.savedProjects)-1]
	if !strings.Contains(saved.Notes, "Then weather.") {
		t.Errorf("the edit was not saved: %q", saved.Notes)
	}
	if !strings.Contains(saved.Notes, "cosy garden game") {
		t.Errorf("the edit replaced the notes instead of extending them: %q", saved.Notes)
	}
}

// TestNewProjectNeedsNoRepository is the case the screen exists for: deciding what to build
// usually starts before there is anywhere to build it.
func TestNewProjectNeedsNoRepository(t *testing.T) {
	f := projectFixture()
	m := openProjects(t, f)

	m = send(t, m, key("n"))
	m = typeKeys(t, m, "pocket blooms 2")
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter created nothing")
	}
	send(t, m, cmd())

	if len(f.added) != 1 {
		t.Fatalf("AddProject called %d times, want 1", len(f.added))
	}
	if f.added[0].Name != "pocket blooms 2" {
		t.Errorf("created %q", f.added[0].Name)
	}
	if f.added[0].Path != "" {
		t.Errorf("a repo-less project was given the path %q", f.added[0].Path)
	}
}

// TestProjectsTypingDoesNotTriggerGlobals: notes are prose, and prose contains q, p and digits.
func TestProjectsTypingDoesNotTriggerGlobals(t *testing.T) {
	m := openProjects(t, projectFixture())
	m = send(t, m, key("e"))

	before := m.projectIdx
	m = typeKeys(t, m, "quick plan for 7 things")

	if m.quitting {
		t.Fatal("a q in the notes quit the program")
	}
	if m.active != SectionProjects {
		t.Errorf("a digit in the notes jumped to %v", m.active)
	}
	if m.projectIdx != before {
		t.Error("a p in the notes cycled the project filter")
	}
}

// TestProjectsEnterOpensItsTickets rather than building a second ticket list here.
func TestProjectsEnterOpensItsTickets(t *testing.T) {
	m := openProjects(t, projectFixture())
	m.projectIdx = 1
	m.filter = "unrelated text"
	m = selectProject(t, m, "gravy")
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter did nothing")
	}
	m = send(t, m, cmd())
	if m.active != SectionBacklog {
		t.Errorf("enter went to %v, want the backlog", m.active)
	}
	if m.projectName() != "gravy" || m.filter != "" {
		t.Fatalf("wrong filters: project %q, text %q", m.projectName(), m.filter)
	}
	q := newQueue(core.StateBacklog)
	q.items = []api.TicketDetail{{Project: core.Project{Name: "gravy"}, Ticket: core.Ticket{Title: "Archive projects"}}, {Project: core.Project{Name: "pocket-blooms"}, Ticket: core.Ticket{Title: "Other work"}}}
	if visible := q.visible(m.viewContext()); len(visible) != 1 || visible[0].Ticket.Title != "Archive projects" {
		t.Fatalf("visible %+v", visible)
	}
}

// TestDeleteProjectNeedsItsNameTyped, because it takes the project's tickets, runs and history
// with it.
func TestDeleteProjectNeedsItsNameTyped(t *testing.T) {
	f := projectFixture()
	m := openProjects(t, f)
	m = selectProject(t, m, "idea")

	m = send(t, m, key("D"))
	if scr := m.screens[SectionProjects].(*projects); scr.mode != projectConfirmDelete {
		t.Fatal("D did not ask for confirmation")
	}

	// The wrong name does not delete.
	m = typeKeys(t, m, "gravy")
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd != nil {
		t.Fatal("a mismatched name deleted a project")
	}
	if len(f.deletedProjects) != 0 {
		t.Fatalf("deleted %v on a mismatched name", f.deletedProjects)
	}

	// The right one does.
	m = send(t, m, key("D"))
	m = typeKeys(t, m, "idea")
	m, cmd = sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("the correct name did not delete")
	}
	send(t, m, cmd())

	if len(f.deletedProjects) != 1 || f.deletedProjects[0] != "p3" {
		t.Errorf("deleted %v, want the selected project", f.deletedProjects)
	}
}

// TestDeleteGoesThroughTheService rather than leaving orphans.
//
// An orphaned ticket is not inert: the scheduler reads it every tick, fails to find its project,
// and stops scheduling for every project until somebody notices — which is exactly what a
// hand-written DELETE on the projects table caused.
func TestDeleteGoesThroughTheService(t *testing.T) {
	f := projectFixture()
	m := openProjects(t, f)
	m = selectProject(t, m, "idea")
	m = send(t, m, key("D"))
	m = typeKeys(t, m, "idea")
	m, cmd := sendCmd(t, m, key("enter"))
	send(t, m, cmd())

	if len(f.deletedProjects) == 0 {
		t.Fatal("nothing reached the service")
	}
}

// TestProjectSettingsLiveWithTheProject is the move: a project's own configuration is edited
// where the project is selected, not as six rows per project in one flat global list.
func TestProjectSettingsLiveWithTheProject(t *testing.T) {
	f := projectFixture()
	m := openProjects(t, f)
	m = send(t, m, projectsLoadedMsg{
		projects: f.projects, tickets: f.allTickets,
		hosts:  []string{"local", "air"},
		agents: []api.AgentOption{{ProviderID: "claude-code", Models: []string{"sonnet"}}},
	})
	m = selectProject(t, m, "gravy")

	m = send(t, m, key("c"))
	view := m.View()
	for _, want := range []string{"host", "buckets", "allowed commands", "validation", "target branch"} {
		if !strings.Contains(view, want) {
			t.Errorf("the project's settings omit %q:\n%s", want, view)
		}
	}
	// An empty allowlist refuses every command an agent tries, which is not obvious from a
	// blank line — and cost several runs before anyone noticed.
	if !strings.Contains(view, "refused every command") {
		t.Errorf("an empty allowlist does not say what it costs:\n%s", view)
	}
}

// TestEditingAProjectSettingSavesOnLeaving: a project with a half-typed host is not one the
// scheduler should see, so nothing is written until the config is closed.
func TestEditingAProjectSettingSavesOnLeaving(t *testing.T) {
	f := projectFixture()
	m := openProjects(t, f)
	m = send(t, m, projectsLoadedMsg{
		projects: f.projects, tickets: f.allTickets, hosts: []string{"local", "air"},
	})
	m = selectProject(t, m, "gravy")
	m = send(t, m, key("c"))

	// Move to "allowed commands" and set it.
	scr := m.screens[SectionProjects].(*projects)
	for scr.fields[scr.field].Label != "allowed commands" {
		m = send(t, m, key("j"))
	}
	m = send(t, m, key("enter"))
	m = typeKeys(t, m, "mix, cd")

	m = send(t, m, key("enter")) // accept the field
	if len(f.savedProjects) != 0 {
		t.Fatal("a field edit wrote the project before the config was closed")
	}

	m, cmd := sendCmd(t, m, key("esc")) // leave, which saves
	if cmd == nil {
		t.Fatal("leaving did not save")
	}
	send(t, m, cmd())

	if len(f.savedProjects) != 1 {
		t.Fatalf("saved %d projects, want 1", len(f.savedProjects))
	}
	got := f.savedProjects[0].Allowlist.Commands
	if len(got) != 2 || got[0].Match != "mix" || got[1].Match != "cd" {
		t.Errorf("allowlist = %+v, want mix and cd", got)
	}
}

// TestUnchangedProjectIsNotWritten avoids rewriting rows nobody edited.
func TestUnchangedProjectIsNotWritten(t *testing.T) {
	f := projectFixture()
	m := openProjects(t, f)
	m = selectProject(t, m, "gravy")
	m = send(t, m, key("c"))
	m, cmd := sendCmd(t, m, key("esc"))
	if cmd != nil {
		send(t, m, cmd())
	}
	if len(f.savedProjects) != 0 {
		t.Errorf("wrote %d projects without an edit", len(f.savedProjects))
	}
}

func TestParseAllowedCommands(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "mix, cd, elixir", want: 3},
		{in: "  go  ", want: 1},
		{in: "", want: 0},
		// A shell operator would be permitted verbatim, match nothing useful, and read like it
		// granted a pipeline.
		{in: "mix test | tee log", wantErr: true},
		{in: "rm -rf / && echo", wantErr: true},
	} {
		got, err := parseAllowedCommands(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAllowedCommands(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAllowedCommands(%q): %v", tc.in, err)
		}
		if len(got) != tc.want {
			t.Errorf("parseAllowedCommands(%q) = %d commands, want %d", tc.in, len(got), tc.want)
		}
	}
}

func TestCompleteBucketsField(t *testing.T) {
	agents := []api.AgentOption{
		{ProviderID: "claude-code", Models: []string{"opus", "sonnet", "haiku"}},
		{ProviderID: "codex", Models: []string{"default", "gpt-5.6-sol"}, Open: true},
	}
	buckets := []core.Route{"cheap", "implementation", "planning", "review"}

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		// The separator is not the interesting part, and forgetting it is the commonest way
		// to get this field wrong.
		{"a bucket name gets its = for free", "imp", "implementation="},
		{"unambiguous prefix", "r", "review="},
		{"a provider", "implementation=cl", "implementation=claude-code/"},
		{"a model", "implementation=claude-code/s", "implementation=claude-code/sonnet"},
		// splitLast keeps the separator; adding another produced a double space on every
		// fallback typed.
		{"a fallback keeps one space", "implementation=codex/gpt-5.6-sol cl", "implementation=codex/gpt-5.6-sol claude-code/"},
		{"a second bucket after a comma", "implementation=codex/default, rev", "implementation=codex/default, review="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, matches := completeField("buckets", tc.in, agents, nil, buckets)
			if got != tc.want {
				t.Errorf("completeField(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(matches) == 0 {
				t.Errorf("completeField(%q) offered no candidates", tc.in)
			}
		})
	}

	// And what it completes must parse, or completion is teaching a syntax the parser rejects.
	completed, _ := completeField("buckets", "implementation=claude-code/s", agents, nil, buckets)
	if _, err := parseProjectRoutes(completed, agents); err != nil {
		t.Errorf("completion produced %q, which the field itself refuses: %v", completed, err)
	}
}

func TestCompleteHostAndChoiceFields(t *testing.T) {
	hosts := []string{"local", "air"}
	if got, _ := completeField("host", "a", nil, hosts, nil); got != "air" {
		t.Errorf("host completion = %q, want air", got)
	}
	if got, _ := completeField("merge mode", "p", nil, hosts, nil); got != "pr" {
		t.Errorf("merge mode completion = %q, want pr", got)
	}
	if got, _ := completeField("parallel", "t", nil, hosts, nil); got != "true" {
		t.Errorf("parallel completion = %q, want true", got)
	}
	// A field with nothing to offer leaves what was typed alone.
	if got, _ := completeField("validation", "mix", nil, hosts, nil); got != "mix" {
		t.Errorf("validation completion changed %q", got)
	}
}
