package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

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
	m = send(t, m, key(SectionSettings.Key()))
	m = send(t, m, settingsLoadedMsg{settings: f.settings, projects: testProjects()})
	return m
}

// focus moves the cursor onto the first field with the given label.
func focus(t *testing.T, m Model, label string) Model {
	t.Helper()
	return focusIn(t, m, "", label)
}

// focusIn moves the cursor onto a field, disambiguating by section when labels repeat.
func focusIn(t *testing.T, m Model, section, label string) Model {
	t.Helper()
	scr, ok := m.screens[SectionSettings].(*settings)
	if !ok {
		t.Fatal("settings screen is not registered")
	}
	for i, f := range scr.fields {
		if f.Label == label && (section == "" || f.Section == section) {
			for scr.cursor < i {
				m = send(t, m, key("j"))
			}
			for scr.cursor > i {
				m = send(t, m, key("k"))
			}
			return m
		}
	}
	t.Fatalf("no field labelled %q in section %q", label, section)
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
		// Global configuration only. A project's own settings moved to the Projects screen,
		// where there is a selected project to attach them to — six rows per project in this
		// flat list was unreadable past two or three of them.
		"Concurrency", "Buckets", "Bucket planning", "Bucket implementation", "Agents",
		"Timeouts", "Retry", "Retention",
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

	m = focusIn(t, m, "Bucket implementation", "capacity")
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
	m = focusIn(t, m, "Bucket planning", "capacity")
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
	m = focusIn(t, m, "Bucket review", "agents")
	m = typeInto(t, m, "not-a-choice")

	scr := m.screens[SectionSettings].(*settings)
	if !scr.editing {
		t.Error("a malformed provider/model was accepted")
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
	m = focus(t, m, "workers")
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
	// Checked on the active section: Settings is deliberately off the header row, so its
	// title is not in the view even while it is the screen you are on.
	if m.active != SectionSettings {
		t.Errorf("typing left the settings screen for %v", m.active)
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

// TestCreateYourOwnBucket is the point of user-defined names: buckets are what you call them,
// not a list compiled into Gravy.
func TestCreateYourOwnBucket(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)

	m = focus(t, m, "new bucket")
	m = typeInto(t, m, "astra")

	scr := m.screens[SectionSettings].(*settings)
	if _, ok := scr.cfg.Routes[core.Route("astra")]; !ok {
		t.Fatalf("astra was not created: %v", scr.cfg.Routes)
	}
	// It gains its own editable fields immediately, without a save or a reload.
	var sections int
	for _, fl := range scr.fields {
		if fl.Section == "Bucket astra" {
			sections++
		}
	}
	if sections != 3 {
		t.Errorf("astra has %d fields, want agents, capacity and delete", sections)
	}

	// And it can be given an agent and a cap like any other.
	m = focusIn(t, m, "Bucket astra", "agents")
	m = typeInto(t, m, "claude-code/fable, claude-code/opus")
	m = focusIn(t, m, "Bucket astra", "capacity")
	m = typeInto(t, m, "1")

	m, cmd := sendCmd(t, m, key("s"))
	m = send(t, m, cmd())

	saved := f.saved[0]
	if got := saved.Routes[core.Route("astra")]; len(got) != 2 || got[0] != "claude-code/fable" {
		t.Errorf("astra's agents = %v", got)
	}
	if got := saved.Concurrency.Routes[core.Route("astra")]; got != 1 {
		t.Errorf("astra's capacity = %d, want 1", got)
	}
}

// TestBucketNamesAreChecked keeps a name that would be ambiguous out of the config.
func TestBucketNamesAreChecked(t *testing.T) {
	m := openSettings(t, settingsFixture())
	m = focus(t, m, "new bucket")

	for _, bad := range []string{"two words", "claude/opus", "a,b"} {
		m = typeInto(t, m, bad)
		scr := m.screens[SectionSettings].(*settings)
		if !scr.editing {
			t.Errorf("%q was accepted as a bucket name", bad)
		}
		m = send(t, m, key("esc"))
		m = focus(t, m, "new bucket")
	}

	// And a duplicate is refused rather than silently replacing what is there.
	m = typeInto(t, m, "planning")
	if !strings.Contains(m.View(), "already a bucket") {
		t.Errorf("a duplicate name was not refused:\n%s", m.View())
	}
}

// TestDeletingABucketNeedsItsName: removing a bucket orphans any ticket asking for it, so it is
// not a single keystroke.
func TestDeletingABucketNeedsItsName(t *testing.T) {
	f := settingsFixture()
	m := openSettings(t, f)

	m = focusIn(t, m, "Bucket planning", "delete")
	m = typeInto(t, m, "plannin")
	scr := m.screens[SectionSettings].(*settings)
	if _, gone := scr.cfg.Routes[core.RoutePlanning]; !gone {
		t.Error("a near-miss deleted the bucket")
	}

	m = send(t, m, key("esc"))
	m = focusIn(t, m, "Bucket planning", "delete")
	m = typeInto(t, m, "planning")

	scr = m.screens[SectionSettings].(*settings)
	if _, still := scr.cfg.Routes[core.RoutePlanning]; still {
		t.Error("typing the name exactly did not remove the bucket")
	}
	if _, still := scr.cfg.Concurrency.Routes[core.RoutePlanning]; still {
		t.Error("the bucket's capacity outlived the bucket")
	}
}

// TestRouteRefusesAnAgentThisBuildCannotRun is the check that would have caught "codex/sol".
//
// It parses perfectly as provider/model and is still wrong: no such model exists. The only way
// anyone found out was a ticket reaching review with a 400 from the provider attached to it.
func TestRouteRefusesAnAgentThisBuildCannotRun(t *testing.T) {
	agents := []api.AgentOption{
		{ProviderID: "claude-code", Models: []string{"opus", "sonnet", "haiku"}},
		{ProviderID: "codex", Models: []string{"default"}},
	}

	for _, tc := range []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "a real pair", in: "claude-code/sonnet"},
		{name: "several real pairs", in: "codex/default, claude-code/opus"},
		{name: "the model that started this", in: "codex/sol", wantErr: `codex has no model "sol"`},
		{name: "an unknown provider", in: "gpt/4", wantErr: `no agent called "gpt"`},
		{name: "still catches the shape", in: "claude-code", wantErr: "not \"provider/model\""},
		{name: "empty is no route at all", in: "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseChoices(tc.in, agents)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("parseChoices(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseChoices(%q) = nil, want %q", tc.in, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseChoices(%q) = %q, want it to mention %q", tc.in, err, tc.wantErr)
			}
		})
	}
}

// TestRouteErrorNamesTheAlternatives: being told "no" without being told what would work is how
// someone ends up guessing a second wrong model.
func TestRouteErrorNamesTheAlternatives(t *testing.T) {
	agents := []api.AgentOption{{ProviderID: "codex", Models: []string{"default"}}}
	_, err := parseChoices("codex/sol", agents)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("the error does not say what would work: %v", err)
	}
}

// TestRouteAcceptsAnythingWhenModelsAreUnknown: a provider that cannot enumerate its models is
// not evidence that a model is wrong — an API-key login can name models the adapter has no list
// for.
func TestRouteAcceptsAnythingWhenModelsAreUnknown(t *testing.T) {
	agents := []api.AgentOption{{ProviderID: "codex"}}
	if _, err := parseChoices("codex/gpt-5-codex", agents); err != nil {
		t.Errorf("refused a model it has no basis to judge: %v", err)
	}
}

// TestRouteValidationIsSilentWithoutAgents: a client that cannot enumerate agents must not
// refuse a configuration it has no basis to judge.
func TestRouteValidationIsSilentWithoutAgents(t *testing.T) {
	if _, err := parseChoices("anything/at-all", nil); err != nil {
		t.Errorf("refused without knowing what this build has: %v", err)
	}
}

// TestRouteAcceptsAnUnlistedModelWhenTheListIsOpen is the correction.
//
// The codex adapter's model list is transcribed from an interactive picker by hand, so refusing
// a name missing from it would break the day OpenAI ships a model — which is exactly what a
// stale note about ChatGPT-account logins nearly caused here. "gpt-5.6-sol" was verified working
// on such a login while the adapter still claimed no model could be named.
func TestRouteAcceptsAnUnlistedModelWhenTheListIsOpen(t *testing.T) {
	agents := []api.AgentOption{
		{ProviderID: "codex", Models: []string{"default", "gpt-5.6-sol"}, Open: true},
		{ProviderID: "claude-code", Models: []string{"opus", "sonnet"}},
	}

	for _, in := range []string{"codex/gpt-5.6-sol", "codex/whatever-ships-next", "codex/default"} {
		if _, err := parseChoices(in, agents); err != nil {
			t.Errorf("parseChoices(%q) = %v, want nil: the list is advisory", in, err)
		}
	}

	// A closed list still refuses, so the check has not become decorative.
	if _, err := parseChoices("claude-code/nonesuch", agents); err == nil {
		t.Error("a closed list accepted an unknown model")
	}
}

func TestProjectRoutesRoundTrip(t *testing.T) {
	agents := []api.AgentOption{
		{ProviderID: "codex", Models: []string{"default", "gpt-5.6-sol"}, Open: true},
		{ProviderID: "claude-code", Models: []string{"opus", "sonnet"}},
	}

	for _, tc := range []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "one bucket", in: "implementation=codex/gpt-5.6-sol"},
		{name: "a fallback list", in: "implementation=codex/gpt-5.6-sol claude-code/sonnet"},
		{name: "several buckets", in: "implementation=codex/default, review=claude-code/sonnet"},
		{name: "empty is no override", in: "   "},
		{name: "missing the route", in: "codex/default", wantErr: `not "route=provider/model"`},
		{name: "a bucket with no agents", in: "implementation=", wantErr: "has no agents"},
		{name: "an unknown model on a closed list", in: "review=claude-code/nope", wantErr: "has no model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProjectRoutes(tc.in, agents)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseProjectRoutes(%q) = %v, want %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProjectRoutes(%q): %v", tc.in, err)
			}
			// What is parsed must render back to something that parses again, or editing a
			// project's agents twice loses them.
			again, err := parseProjectRoutes(formatProjectRoutes(got), agents)
			if err != nil {
				t.Fatalf("re-parsing what was rendered: %v", err)
			}
			if len(again) != len(got) {
				t.Errorf("round trip changed %d buckets into %d", len(got), len(again))
			}
		})
	}
}

// TestProjectHostMustExist: a project pointed at a machine that is not configured has nowhere to
// run, and would say so only once a ticket was waiting on it.
func TestProjectHostMustExist(t *testing.T) {
	f := newFake()
	f.settings.Config.Hosts = []config.Host{{ID: "air", Target: "air", Workers: 2}}
	m := openSettings(t, f)

	scr := m.screens[SectionSettings].(*settings)
	if !knownHost(scr, "air") {
		t.Error("a configured host is not recognised")
	}
	// The machine running the daemon is always available and is never in the file.
	if !knownHost(scr, "local") {
		t.Error("the local machine is not recognised as a host")
	}
	if knownHost(scr, "nonesuch") {
		t.Error("an unconfigured host was accepted")
	}
}

// TestReconnectingAHostThatCameBack is the way back for a machine that was switched off.
//
// Gravy records an unreachable host as off and stops waiting on it, which is what keeps one
// absent computer from slowing everything else down. The cost of that is that nothing will
// notice on its own the moment it is switched on again, so the human needs a way to say so.
func TestReconnectingAHostThatCameBack(t *testing.T) {
	f := settingsFixture()
	f.settings.Config.Hosts = []config.Host{{ID: "yeet", Target: "yeet", Workers: 1}}
	f.status = api.SystemStatus{Hosts: []api.HostStatus{{
		ID:          "yeet",
		Online:      false,
		Unreachable: "ssh: connect to host yeetcity port 22: Connection timed out",
		CheckedAt:   time.Unix(1700000000, 0),
	}}}
	// The machine is on by the time the human presses the key.
	f.reconnectTo = api.HostStatus{ID: "yeet", Online: true}

	m := openSettings(t, f)
	m = send(t, m, refreshedMsg{})

	m = focusIn(t, m, "Host yeet", "reconnect")

	// The host's line says it is off, and says why, before anyone asks it to reconnect.
	scr := m.screens[SectionSettings].(*settings)
	if state := scr.hostState("yeet"); !strings.HasPrefix(state, "off —") {
		t.Errorf("yeet reads %q, want it reported as off with ssh's reason", state)
	}
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter on reconnect did nothing")
	}
	m = send(t, m, cmd())

	if len(f.reconnected) != 1 || f.reconnected[0] != "yeet" {
		t.Fatalf("reconnected = %v, want one probe of yeet", f.reconnected)
	}
	if body := m.View(); !strings.Contains(body, "yeet is back") {
		t.Errorf("the screen does not report that yeet came back:\n%s", body)
	}
}

// TestReconnectingAHostThatIsStillOff: still off is an answer, not an error. The human needs to
// be told to go and look at the machine rather than at Gravy.
func TestReconnectingAHostThatIsStillOff(t *testing.T) {
	f := settingsFixture()
	f.settings.Config.Hosts = []config.Host{{ID: "yeet", Target: "yeet", Workers: 1}}
	f.reconnectTo = api.HostStatus{ID: "yeet", Online: false, Unreachable: "ssh: connect to host yeetcity port 22: Connection timed out"}

	m := openSettings(t, f)
	m = focusIn(t, m, "Host yeet", "reconnect")
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter on reconnect did nothing")
	}
	m = send(t, m, cmd())

	body := m.View()
	if !strings.Contains(body, "still off") {
		t.Errorf("a failed reconnect is not reported as the machine still being off:\n%s", body)
	}
	if !strings.Contains(body, "Connection timed out") {
		t.Errorf("ssh's own reason is not shown, so the human cannot tell what to fix:\n%s", body)
	}
}
