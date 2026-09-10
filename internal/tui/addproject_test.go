package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/api"
)

// typeKeys sends each character of s as its own key press, as a real keyboard would.
func typeKeys(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m = send(t, m, key(string(r)))
	}
	return m
}

// runCmd executes a command and feeds its message back, as Bubble Tea would.
func runCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	return send(t, m, cmd())
}

func TestAddProjectRegistersFromAnyScreen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		section Section
	}{
		{"from the dashboard", SectionDashboard},
		{"from the plan screen", SectionPlan},
		{"from the backlog", SectionBacklog},
		{"from review", SectionReview},
		{"from settings", SectionSettings},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newFake()
			m := boot(t, svc, 80, 24)
			m = send(t, m, key(tc.section.Key()))

			m = send(t, m, key("P"))
			if !m.adding.open {
				t.Fatal("P did not open the prompt")
			}
			m = typeKeys(t, m, "/tmp/repo")

			m, cmd := sendCmd(t, m, key("enter"))
			m = runCmd(t, m, cmd)

			if len(svc.added) != 1 {
				t.Fatalf("AddProject called %d times, want 1", len(svc.added))
			}
			if got := svc.added[0].Path; got != "/tmp/repo" {
				t.Errorf("registered %q, want /tmp/repo", got)
			}
			if m.adding.open {
				t.Error("the prompt stayed open after a successful registration")
			}
		})
	}
}

// TestAddProjectOwnsTheKeyboard is the adversarial case the binding exists to survive: the
// global keys are ordinary letters, and a path is ordinary letters.
func TestAddProjectOwnsTheKeyboard(t *testing.T) {
	svc := newFake()
	m := boot(t, svc, 80, 24)
	m = send(t, m, key("P"))

	// "q" quits, "p" cycles projects, "/" filters, "," opens Settings and "1".."7" jump
	// sections. Every one of them appears in this path, and none may do its global job here.
	const path = "/q/p/7/,/repo"
	m = typeKeys(t, m, path)

	if m.quitting {
		t.Fatal("typing a q in a path quit the program")
	}
	if m.active != SectionDashboard {
		t.Errorf("active section = %v, want the dashboard: a digit in a path jumped sections", m.active)
	}
	if m.filtering {
		t.Error("a / in a path started a filter")
	}
	if m.projectIdx != -1 {
		t.Error("a p in a path cycled the project filter")
	}
	if m.adding.path != path {
		t.Errorf("prompt holds %q, want %q", m.adding.path, path)
	}
}

func TestAddProjectFailureKeepsWhatWasTyped(t *testing.T) {
	svc := newFake()
	svc.actionErr = fmt.Errorf("/nope is not a git repository")
	m := boot(t, svc, 80, 24)

	m = send(t, m, key("P"))
	m = typeKeys(t, m, "/nope")
	m, cmd := sendCmd(t, m, key("enter"))
	m = runCmd(t, m, cmd)

	if !m.adding.open {
		t.Fatal("the prompt closed on failure, discarding the path")
	}
	if m.adding.path != "/nope" {
		t.Errorf("path = %q, want it preserved for editing", m.adding.path)
	}
	if !strings.Contains(m.adding.notice, "not a git repository") {
		t.Errorf("notice = %q, want the service's reason", m.adding.notice)
	}
	if m.adding.busy {
		t.Error("still busy after the call returned; a retry would be impossible")
	}
	// The reason has to be on screen, not just in the struct.
	if !strings.Contains(m.View(), "not a git repository") {
		t.Error("the failure is not rendered")
	}
}

// TestAddProjectDoubleEnterRegistersOnce guards the race: registering runs git against the
// repository, so it is slow enough for a second enter to arrive before the first returns.
func TestAddProjectDoubleEnterRegistersOnce(t *testing.T) {
	svc := newFake()
	m := boot(t, svc, 80, 24)
	m = send(t, m, key("P"))
	m = typeKeys(t, m, "/tmp/repo")

	m, first := sendCmd(t, m, key("enter"))
	m, second := sendCmd(t, m, key("enter"))
	if second != nil {
		t.Fatal("a second enter while busy issued a second AddProject")
	}
	m = runCmd(t, m, first)

	if len(svc.added) != 1 {
		t.Fatalf("AddProject called %d times, want 1", len(svc.added))
	}
	if m.adding.open {
		t.Error("prompt still open after success")
	}
}

func TestAddProjectEscCancels(t *testing.T) {
	svc := newFake()
	m := boot(t, svc, 80, 24)
	m = send(t, m, key("P"))
	m = typeKeys(t, m, "/tmp/repo")
	m = send(t, m, key("esc"))

	if m.adding.open {
		t.Fatal("esc did not close the prompt")
	}
	if len(svc.added) != 0 {
		t.Errorf("esc registered %d projects, want 0", len(svc.added))
	}
	// Reopening starts clean rather than resurrecting an abandoned attempt.
	m = send(t, m, key("P"))
	if m.adding.path != "" {
		t.Errorf("reopened holding %q, want empty", m.adding.path)
	}
}

func TestAddProjectEmptyPathIsRefused(t *testing.T) {
	svc := newFake()
	m := boot(t, svc, 80, 24)
	m = send(t, m, key("P"))

	m, cmd := sendCmd(t, m, key("enter"))
	if cmd != nil {
		t.Fatal("an empty path called AddProject")
	}
	if !m.adding.open {
		t.Error("the prompt closed on an empty path")
	}
	if m.adding.notice == "" {
		t.Error("an empty path was refused silently")
	}
}

func TestAddProjectBackspaceTrimsByRune(t *testing.T) {
	svc := newFake()
	m := boot(t, svc, 80, 24)
	m = send(t, m, key("P"))
	m = typeKeys(t, m, "/tmp/café")
	m = send(t, m, key("backspace"))

	if got, want := m.adding.path, "/tmp/caf"; got != want {
		t.Errorf("path = %q, want %q: backspace must trim a rune, not a byte", got, want)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"bare tilde", "~", home},
		{"tilde path", "~/Projects/gravy", filepath.Join(home, "Projects", "gravy")},
		{"absolute is untouched", "/tmp/repo", "/tmp/repo"},
		{"relative is untouched", "repo", "repo"},
		{"empty is untouched", "", ""},
		// Only a leading "~/" is a home reference. A directory genuinely named "~foo" or
		// "a~b" must survive, and "~user" is a shell expansion Gravy does not implement.
		{"tilde user is untouched", "~someone/repo", "~someone/repo"},
		{"embedded tilde is untouched", "/tmp/a~b", "/tmp/a~b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandHome(tc.in)
			if err != nil {
				t.Fatalf("expandHome(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAddProjectExpandsHomeOnSubmit checks the expansion reaches the service, not just the
// helper: a "~" arriving at os.Stat verbatim fails naming a directory called "~".
func TestAddProjectExpandsHomeOnSubmit(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	svc := newFake()
	m := boot(t, svc, 80, 24)
	m = send(t, m, key("P"))
	m = typeKeys(t, m, "~/repo")

	m, cmd := sendCmd(t, m, key("enter"))
	m = runCmd(t, m, cmd)

	if len(svc.added) != 1 {
		t.Fatalf("AddProject called %d times, want 1", len(svc.added))
	}
	if got, want := svc.added[0].Path, filepath.Join(home, "repo"); got != want {
		t.Errorf("registered %q, want %q", got, want)
	}
	if m.adding.open {
		t.Error("the prompt stayed open after a successful registration")
	}
}

// TestAddProjectIsDiscoverable covers the first-run path: an empty dashboard must name the key
// rather than sending someone out to a shell.
func TestAddProjectIsDiscoverable(t *testing.T) {
	svc := newFake()
	svc.status.Projects = nil
	m := boot(t, svc, 80, 24)

	m = send(t, m, agentsDetectedMsg{agents: []api.AgentStatus{
		{ProviderID: "claude-code", Installed: true, Authenticated: true},
	}})
	view := m.View()
	if strings.Contains(view, "gravy project add") {
		t.Error("the first-run screen still sends the user to the CLI")
	}
	if !strings.Contains(view, "open setup") {
		t.Errorf("the first-run screen does not offer the key:\n%s", view)
	}

	// And it is in the generated help, which is the same struct Update dispatches on.
	help := send(t, m, key("?"))
	if h := help.View(); !strings.Contains(h, "add a project") {
		t.Errorf("help overlay omits the binding:\n%s", h)
	}
}

// TestAddProjectDoesNotShadowScreenKeys is the other half of the shadowing risk: the frame
// checks global bindings first, so lowercase p and the screens' own letters must still work.
func TestAddProjectDoesNotShadowScreenKeys(t *testing.T) {
	svc := newFake()
	m := boot(t, svc, 80, 24)

	// Lowercase p still cycles the project filter.
	m = send(t, m, key("p"))
	if m.adding.open {
		t.Fatal("lowercase p opened the add-project prompt")
	}
	if m.projectIdx != 0 {
		t.Errorf("projectIdx = %d, want 0: p no longer cycles projects", m.projectIdx)
	}
}

func TestAddProjectFitsNarrowTerminals(t *testing.T) {
	for _, w := range []int{20, 40, 80, 200} {
		svc := newFake()
		m := boot(t, svc, w, 24)
		m = send(t, m, key("P"))
		m = typeKeys(t, m, "/tmp/repo")

		for i, ln := range strings.Split(m.View(), "\n") {
			if got := lipgloss.Width(ln); got > w {
				t.Errorf("width %d: line %d is %d cells: %q", w, i, got, ln)
			}
		}
	}
}

// completeTree builds a directory tree to complete against and returns its root.
func completeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{
		"projects/gravy", "projects/gravy-old", "projects/mission-mojo",
		"projects/.hidden", "solo",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A regular file must never be offered: a project is a directory.
	if err := os.WriteFile(filepath.Join(root, "projects", "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink to a directory is still a directory to complete into.
	if err := os.Symlink(filepath.Join(root, "solo"), filepath.Join(root, "projects", "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return root
}

func TestCompletePath(t *testing.T) {
	root := completeTree(t)
	sep := string(filepath.Separator)

	for _, tc := range []struct {
		name    string
		typed   string
		want    string
		matches []string
	}{
		{
			name:    "unique match lands inside it",
			typed:   root + "/projects/mis",
			want:    root + "/projects/mission-mojo" + sep,
			matches: []string{"mission-mojo"},
		},
		{
			name:    "ambiguous stops at the common prefix",
			typed:   root + "/projects/gra",
			want:    root + "/projects/gravy",
			matches: []string{"gravy", "gravy-old"},
		},
		{
			name:    "trailing slash lists the directory",
			typed:   root + "/projects/",
			want:    root + "/projects/",
			matches: []string{"gravy", "gravy-old", "linked", "mission-mojo"},
		},
		{
			name:    "a symlinked directory completes",
			typed:   root + "/projects/link",
			want:    root + "/projects/linked" + sep,
			matches: []string{"linked"},
		},
		{
			name:    "a dot offers the hidden directory",
			typed:   root + "/projects/.",
			want:    root + "/projects/.hidden" + sep,
			matches: []string{".hidden"},
		},
		{
			name:  "no match leaves the path alone",
			typed: root + "/projects/zzz",
			want:  root + "/projects/zzz",
		},
		{
			name:  "a missing directory is not an error",
			typed: root + "/nope/thing",
			want:  root + "/nope/thing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, matches := completePath(tc.typed)
			if got != tc.want {
				t.Errorf("completePath(%q) = %q, want %q", tc.typed, got, tc.want)
			}
			if len(matches) != len(tc.matches) {
				t.Fatalf("matches = %v, want %v", matches, tc.matches)
			}
			for i := range matches {
				if matches[i] != tc.matches[i] {
					t.Errorf("matches = %v, want %v", matches, tc.matches)
					break
				}
			}
		})
	}
}

// TestCompletePathOffersOnlyDirectories is the case that would otherwise complete to a path
// AddProject can only reject.
func TestCompletePathOffersOnlyDirectories(t *testing.T) {
	root := completeTree(t)
	_, matches := completePath(root + "/projects/")
	for _, m := range matches {
		if m == "notes.md" {
			t.Fatalf("a regular file was offered: %v", matches)
		}
	}
}

// TestCompletePathHidesDotfilesUntilAsked keeps a home directory completable.
func TestCompletePathHidesDotfilesUntilAsked(t *testing.T) {
	root := completeTree(t)
	_, matches := completePath(root + "/projects/")
	for _, m := range matches {
		if m == ".hidden" {
			t.Fatalf(".hidden offered before a dot was typed: %v", matches)
		}
	}
}

// TestCompletePathKeepsTheTilde: rewriting what someone typed into /home/... mid-edit is a
// jarring thing for a text field to do.
func TestCompletePathKeepsTheTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if _, err := os.Stat(filepath.Join(home, "Projects")); err != nil {
		t.Skip("no ~/Projects on this machine")
	}
	got, _ := completePath("~/Project")
	if !strings.HasPrefix(got, "~/") {
		t.Errorf("completePath(%q) = %q, want the tilde preserved", "~/Project", got)
	}
	if strings.HasPrefix(got, home) {
		t.Errorf("completion expanded the tilde in the field: %q", got)
	}
}

func TestLongestCommonPrefix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
		want  string
	}{
		{"none", nil, ""},
		{"one", []string{"gravy"}, "gravy"},
		{"shared", []string{"gravy", "gravy-old"}, "gravy"},
		{"partial", []string{"gravy", "grand"}, "gra"},
		{"nothing shared", []string{"gravy", "mojo"}, ""},
		{"shorter first", []string{"go", "gopher"}, "go"},
		// Multibyte: a byte-wise trim would split the é and leave invalid UTF-8 in the field.
		{"multibyte", []string{"café-one", "café-two"}, "café-"},
		{"multibyte divergent", []string{"café", "cafx"}, "caf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := longestCommonPrefix(tc.names); got != tc.want {
				t.Errorf("longestCommonPrefix(%v) = %q, want %q", tc.names, got, tc.want)
			}
		})
	}
}

// TestTabCompletesInThePrompt drives it through the frame, as a keyboard would.
func TestTabCompletesInThePrompt(t *testing.T) {
	root := completeTree(t)
	svc := newFake()
	m := boot(t, svc, 100, 30)
	m = send(t, m, key("P"))
	m = typeKeys(t, m, root+"/projects/mis")
	m = send(t, m, key("tab"))

	want := root + "/projects/mission-mojo" + string(filepath.Separator)
	if m.adding.path != want {
		t.Errorf("path = %q, want %q", m.adding.path, want)
	}

	// An ambiguous completion names what it stopped between, on screen. Reopen first: a P
	// typed into an open prompt is a literal P, which is exactly what it should be.
	m = send(t, m, key("esc"))
	m = send(t, m, key("P"))
	m = typeKeys(t, m, root+"/projects/gra")
	m = send(t, m, key("tab"))
	if m.adding.path != root+"/projects/gravy" {
		t.Errorf("path = %q, want the common prefix", m.adding.path)
	}
	view := m.View()
	if !strings.Contains(view, "gravy-old") {
		t.Errorf("the ambiguous candidates are not shown:\n%s", view)
	}
}

// TestTabWithNoMatchSaysSo: a tab that silently does nothing reads as a broken key.
func TestTabWithNoMatchSaysSo(t *testing.T) {
	svc := newFake()
	m := boot(t, svc, 100, 30)
	m = send(t, m, key("P"))
	m = typeKeys(t, m, "/nonexistent-zzz/x")
	m = send(t, m, key("tab"))

	if m.adding.notice == "" {
		t.Error("a tab that matched nothing said nothing")
	}
	if m.adding.path != "/nonexistent-zzz/x" {
		t.Errorf("path = %q, want it left alone", m.adding.path)
	}
}

// TestTypingClearsStaleMatches: candidates from an earlier tab must not linger under a path
// they no longer describe.
func TestTypingClearsStaleMatches(t *testing.T) {
	root := completeTree(t)
	svc := newFake()
	m := boot(t, svc, 100, 30)
	m = send(t, m, key("P"))
	m = typeKeys(t, m, root+"/projects/gra")
	m = send(t, m, key("tab"))
	if len(m.adding.matches) == 0 {
		t.Fatal("expected candidates after tab")
	}

	m = typeKeys(t, m, "v")
	if len(m.adding.matches) != 0 {
		t.Errorf("typing left %v on screen", m.adding.matches)
	}
	m = send(t, m, key("tab"))
	m = send(t, m, key("backspace"))
	if len(m.adding.matches) != 0 {
		t.Errorf("backspace left %v on screen", m.adding.matches)
	}
}

// TestSectionKeysAreTheLifecycle pins the number row to the order work actually moves in, and
// keeps Settings off it.
func TestSectionKeysAreTheLifecycle(t *testing.T) {
	want := []struct {
		key   string
		title string
	}{
		{"1", "Dashboard"},
		{"2", "Plan"},
		{"3", "Projects"},
		{"4", "Backlog"},
		{"5", "Ready"},
		{"6", "Running"},
		{"7", "Review"},
		{"8", "Needs You"},
		{",", "Settings"},
	}
	if len(AllSections) != len(want) {
		t.Fatalf("%d sections, want %d", len(AllSections), len(want))
	}
	for i, s := range AllSections {
		if s.Key() != want[i].key || s.Title() != want[i].title {
			t.Errorf("section %d = %q %q, want %q %q", i, s.Key(), s.Title(), want[i].key, want[i].title)
		}
	}
}

// TestSettingsKeyDoesNotShadowSave is why Settings is on "," and not "s": the frame checks
// global keys first, so a global "s" would break saving in the screen it opens.
func TestSettingsKeyDoesNotShadowSave(t *testing.T) {
	svc := newFake()
	m := openSettings(t, svc)
	if m.active != SectionSettings {
		t.Fatalf("%q did not open Settings", SettingsKey)
	}

	// "s" must reach the Settings screen, not the frame.
	before := len(svc.saved)
	m, cmd := sendCmd(t, m, key("s"))
	if m.active != SectionSettings {
		t.Error("s navigated away from Settings instead of saving")
	}
	if cmd == nil {
		t.Fatal("s produced no command: it never reached the Settings screen")
	}
	send(t, m, cmd())
	if len(svc.saved) == before {
		t.Error("s did not reach the Settings screen as save")
	}
}

// TestSettingsIsOffTheRowButAdvertised: the number row is the ticket lifecycle, so Settings does
// not compete for space in it — but a key nobody can find is a key nobody uses, so it is named in
// the status bar beside the other one that is always available.
func TestSettingsIsOffTheHeaderButFindable(t *testing.T) {
	m := boot(t, newFake(), 100, 30)
	view := m.View()

	header := strings.Split(view, "\n")[0]
	if strings.Contains(header, "Settings") {
		t.Errorf("Settings is still competing for header space:\n%s", header)
	}
	for _, want := range []string{"Dashboard", "Needs You"} {
		if !strings.Contains(header, want) {
			t.Errorf("the header lost %q:\n%s", want, header)
		}
	}

	status := strings.Split(view, "\n")
	last := status[len(status)-1]
	if !strings.Contains(last, SettingsKey+" settings") {
		t.Errorf("the status bar does not advertise Settings:\n%s", last)
	}
	if !strings.Contains(last, "? help") {
		t.Errorf("the status bar lost the help hint:\n%s", last)
	}

	// And it still works.
	m = send(t, m, key(SettingsKey))
	if m.active != SectionSettings {
		t.Errorf("%q did not open Settings", SettingsKey)
	}
}

// TestBellRingsWhenWorkNeedsYou is the notification that was never audible.
//
// The daemon has no terminal: its "bell" was a byte written into its own log file. The client
// that does have a terminal rings it instead.
func TestBellRingsWhenWorkNeedsYou(t *testing.T) {
	f := newFake()
	f.status.Attention = nil
	m := boot(t, f, 100, 30)
	m = send(t, m, bellSettingMsg{enabled: true})

	// Arriving to an existing queue is not three things happening now.
	if m.newAttention(api.SystemStatus{Attention: nil}) {
		t.Error("rang with nothing new")
	}

	grew := api.SystemStatus{Attention: []api.AttentionItem{{}}}
	if !m.newAttention(grew) {
		t.Error("did not ring when the queue grew")
	}

	// Resolving an item also changes the queue, and a bell for work leaving it is a bell for
	// something the human just did.
	m.attention = 2
	if m.newAttention(api.SystemStatus{Attention: []api.AttentionItem{{}}}) {
		t.Error("rang when the queue shrank")
	}
}

// TestBellIsSilentWhenNotificationsAreOff respects the config rather than deciding for itself.
func TestBellIsSilentWhenNotificationsAreOff(t *testing.T) {
	f := newFake()
	m := boot(t, f, 100, 30)
	m = send(t, m, bellSettingMsg{enabled: false})

	if m.newAttention(api.SystemStatus{Attention: []api.AttentionItem{{}, {}, {}}}) {
		t.Error("rang with notifications switched off")
	}
}

// TestCompleteABareRepositoryName: nobody thinks of their project as "~/Projects/mission-mojo".
// They think of it as mission-mojo, and typing the first half of a path you already know the end
// of is work the machine should be doing.
func TestCompleteABareRepositoryName(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"mission-mojo", "pocket-blooms-ios", "unrelated"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Point the search at the fixture rather than the real home directory.
	old := searchRoots
	searchRoots = []string{root}
	t.Cleanup(func() { searchRoots = old })

	// A name in the middle, because a repository rarely starts with the word you remember it by.
	got, matches := completePath("blooms")
	want := filepath.Join(root, "pocket-blooms-ios") + string(filepath.Separator)
	if got != want {
		t.Errorf("completePath(\"blooms\") = %q, want %q", got, want)
	}
	if len(matches) != 1 {
		t.Errorf("matches = %v, want one", matches)
	}

	// Ambiguity leaves what was typed alone rather than picking one.
	got, matches = completePath("o")
	if got != "o" {
		t.Errorf("an ambiguous name was replaced with %q", got)
	}
	if len(matches) < 2 {
		t.Errorf("matches = %v, want the candidates", matches)
	}

	// A path is still a path: anything with a separator uses the directory walk.
	if got, _ := completePath(root + "/mission"); got != filepath.Join(root, "mission-mojo")+string(filepath.Separator) {
		t.Errorf("a typed path stopped completing: %q", got)
	}
}
