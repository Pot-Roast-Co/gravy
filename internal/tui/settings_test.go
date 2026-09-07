package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
)

func settingsFixture() *fakeService {
	f := newFake()
	cfg := config.Default()
	cfg.Concurrency.Routes = map[core.Route]int{core.RoutePlanning: 1}
	f.settings = api.Settings{Config: cfg, Path: "/home/bobby/.gravy/config.yaml"}
	return f
}

func testProjects() []core.Project {
	return []core.Project{{
		ID: "p1", Slug: "gravy", Name: "gravy", TargetBranch: "main",
		MergeMode: core.LandMerge, ParallelMode: true, MaxConcurrency: 3,
		Validation: []core.Step{{Name: "check", Cmd: "make check"}},
	}}
}

func openSettings(t *testing.T, f *fakeService) Model {
	t.Helper()
	m := boot(t, f, 92, 30)
	m = send(t, m, key("7"))
	m = send(t, m, settingsLoadedMsg{settings: f.settings, projects: testProjects()})
	return m
}

// focus moves the cursor onto the field with the given label.
func focus(t *testing.T, m Model, label string) Model {
	t.Helper()
	scr, ok := m.screens[SectionSettings].(*settings)
	if !ok {
		t.Fatal("settings screen is not registered")
	}
	for i, f := range scr.fields {
		if f.Label == label {
			for scr.cursor < i {
				m = send(t, m, key("j"))
			}
			for scr.cursor > i {
				m = send(t, m, key("k"))
			}
			return m
		}
	}
	t.Fatalf("no field labelled %q", label)
	return m
}

func typeInto(t *testing.T, m Model, text string) Model {
	t.Helper()
	m = send(t, m, key("enter"))
	scr := m.screens[SectionSettings].(*settings)
	for scr.buf != "" {
		m = send(t, m, key("backspace"))
	}
	for _, c := range text {
		m = send(t, m, key(string(c)))
	}
	return send(t, m, key("enter"))
}

// TestEveryConfigSectionIsEditable is the requirement stated plainly: the things that decide how
// Gravy behaves are all reachable without leaving the TUI.
func TestEveryConfigSectionIsEditable(t *testing.T) {
	m := openSettings(t, settingsFixture())
	scr := m.screens[SectionSettings].(*settings)

	sections := map[string]bool{}
	for _, f := range scr.fields {
		sections[f.Section] = true
	}
	for _, want := range []string{
		"Concurrency", "Bucket capacity", "Bucket agents", "Agents",
		"Timeouts", "Retry", "Retention", "Project gravy",
	} {
		if !sections[want] {
			t.Errorf("no editable fields in section %q", want)
		}
	}
	// Every field must be able to render and to be typed into.
	for _, f := range scr.fields {
		if f.Get == nil || f.Set == nil {
			t.Errorf("field %q is not editable", f.Label)
		}
	}
}

// TestEditingABucketCapSavesIt is the setting the whole bucket idea rests on.
func TestEditingABucketCapSavesIt(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)

	// The capacity fields and the agent fields share route names, so target the first.
	m = focus(t, m, "implementation")
	m = typeInto(t, m, "4")

	m, cmd := sendCmd(t, m, key("s"))
	if cmd == nil {
		t.Fatal("s produced no save")
	}
	m = send(t, m, cmd())

	// A successful save clears the unsaved-changes state, or the screen keeps warning about
	// an edit that is already on disk.
	if !strings.Contains(m.View(), "saved") || strings.Contains(m.View(), "unsaved") {
		t.Errorf("the save was not reflected:\n%s", m.View())
	}
	if len(f.saved) != 1 {
		t.Fatalf("saved %d configs, want 1", len(f.saved))
	}
	if got := f.saved[0].Concurrency.Routes[core.RouteImplementation]; got != 4 {
		t.Errorf("implementation cap = %d, want 4", got)
	}
	// The cap that was already set must survive an unrelated edit.
	if got := f.saved[0].Concurrency.Routes[core.RoutePlanning]; got != 1 {
		t.Errorf("planning cap = %d, want the existing 1", got)
	}
}

// TestBlankCapMeansUncapped: removing a limit has to be possible, not just raising it.
func TestBlankCapMeansUncapped(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)
	m = focus(t, m, "planning")
	m = typeInto(t, m, "")

	m, cmd := sendCmd(t, m, key("s"))
	m = send(t, m, cmd())
	if strings.Contains(m.View(), "failed") {
		t.Fatalf("clearing a cap failed:\n%s", m.View())
	}

	if _, still := f.saved[0].Concurrency.Routes[core.RoutePlanning]; still {
		t.Error("clearing a cap left it in the config")
	}
}

// TestBadInputIsRefusedWithAReason keeps an unusable value out of the file.
func TestBadInputIsRefusedWithAReason(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)
	m = focus(t, m, "workers")
	m = typeInto(t, m, "banana")

	view := m.View()
	if !strings.Contains(view, "positive whole number") {
		t.Errorf("no explanation for the refusal:\n%s", view)
	}
	// Still editing, so the value can be corrected rather than lost.
	scr := m.screens[SectionSettings].(*settings)
	if !scr.editing {
		t.Error("a refused edit closed the field instead of letting it be fixed")
	}
	if scr.cfg.Concurrency.Workers == 0 {
		t.Error("a refused edit was applied anyway")
	}
}

// TestRouteChoicesAreValidatedOnEntry: a typo must be caught here, not at run time when a ticket
// is already waiting on it.
func TestRouteChoicesAreValidatedOnEntry(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)

	// The second field with this label is the agent list.
	scr := m.screens[SectionSettings].(*settings)
	seen := 0
	for i, fl := range scr.fields {
		if fl.Label == "review" {
			seen++
			if seen == 2 {
				scr.cursor = i
				break
			}
		}
	}
	m = typeInto(t, m, "not-a-choice")
	if !scr.editing {
		t.Error("a malformed provider/model was accepted")
	}
}

// TestProjectFieldsSaveThroughTheirOwnCall: projects live in the database, not the config file.
func TestProjectFieldsSaveThroughTheirOwnCall(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)

	m = focus(t, m, "validation")
	m = typeInto(t, m, "test:go test ./...; lint:golangci-lint run")

	m, cmd := sendCmd(t, m, key("s"))
	m = send(t, m, cmd())
	if strings.Contains(m.View(), "failed") {
		t.Fatalf("saving the project failed:\n%s", m.View())
	}

	if len(f.projects) != 1 {
		t.Fatalf("updated %d projects, want 1", len(f.projects))
	}
	steps := f.projects[0].Validation
	if len(steps) != 2 || steps[0].Name != "test" || steps[1].Cmd != "golangci-lint run" {
		t.Errorf("validation steps = %+v", steps)
	}
}

// TestUntouchedProjectsAreNotWritten avoids rewriting rows nobody edited.
func TestUntouchedProjectsAreNotWritten(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)
	m = focus(t, m, "workers")
	m = typeInto(t, m, "6")

	m, cmd := sendCmd(t, m, key("s"))
	send(t, m, cmd())

	if len(f.projects) != 0 {
		t.Errorf("wrote %d projects for a config-only edit", len(f.projects))
	}
}

// TestPendingRestartIsSurfaced is the honesty requirement: a setting that was saved but is not
// in effect must say so, or you believe something untrue about your own daemon.
func TestPendingRestartIsSurfaced(t *testing.T) {
	f := settingsFixture()
	f.settings.PendingRestart = []string{"concurrency.workers (8 saved, 4 running)"}
	m := openSettings(t, f)

	view := m.View()
	if !strings.Contains(view, "still uses the old values") {
		t.Errorf("a pending restart is not surfaced:\n%s", view)
	}
	if !strings.Contains(view, "concurrency.workers") {
		t.Errorf("the pending setting is not named:\n%s", view)
	}
	if !strings.Contains(view, "gravy serve") {
		t.Errorf("no instruction for how to apply it:\n%s", view)
	}
}

// TestUnsavedChangesAreVisible: an edit that looks applied but is not saved is a trap.
func TestUnsavedChangesAreVisible(t *testing.T) {
	m := openSettings(t, settingsFixture())
	if !strings.Contains(m.View(), "saved") {
		t.Fatalf("clean state is not shown:\n%s", m.View())
	}

	m = focus(t, m, "workers")
	m = typeInto(t, m, "6")
	if !strings.Contains(m.View(), "unsaved changes") {
		t.Errorf("an edited-but-unsaved config does not say so:\n%s", m.View())
	}
}

// TestTypingASettingDoesNotQuit is the keyboard-capture rule, which a command like
// "golangci-lint run" would otherwise trip over.
func TestTypingASettingDoesNotQuit(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)
	m = focus(t, m, "validation")
	m = send(t, m, key("enter"))

	scr := m.screens[SectionSettings].(*settings)
	for scr.buf != "" {
		m = send(t, m, key("backspace"))
	}
	for _, c := range "q1 make quick" {
		var cmd = func() interface{} { return nil }
		_ = cmd
		m = send(t, m, key(string(c)))
	}
	if !strings.Contains(m.View(), "q1 make quick") {
		t.Errorf("the field did not receive the keystrokes:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "Settings") {
		t.Errorf("typing left the settings screen:\n%s", m.View())
	}
}

// TestSaveFailureIsReported rather than looking like success.
func TestSaveFailureIsReported(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)
	m = focus(t, m, "workers")
	m = typeInto(t, m, "6")

	f.actionErr = fmt.Errorf("disk is full")
	m, cmd := sendCmd(t, m, key("s"))
	m = send(t, m, cmd())

	if !strings.Contains(m.View(), "save failed") || !strings.Contains(m.View(), "disk is full") {
		t.Errorf("a failed save is not reported:\n%s", m.View())
	}
}
