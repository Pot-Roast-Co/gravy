package agentrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

func TestParsePlan(t *testing.T) {
	for _, tc := range []struct {
		name      string
		text      string
		wantProse string
		wantCount int
		check     func(t *testing.T, ts []core.PlannedTicket)
	}{
		{
			name:      "prose only, still discussing",
			text:      "What does done look like for this?",
			wantProse: "What does done look like for this?",
		},
		{
			name: "a proposal with prose above it",
			text: "Here is what I suggest.\n\n```json\n" +
				`{"tickets":[{"title":"Add mul","body":"multiply two ints","route":"cheap","depends_on":[]}]}` +
				"\n```",
			wantProse: "Here is what I suggest.",
			wantCount: 1,
			check: func(t *testing.T, ts []core.PlannedTicket) {
				if ts[0].Title != "Add mul" || ts[0].Route != core.RouteCheap {
					t.Errorf("ticket = %+v", ts[0])
				}
			},
		},
		{
			name: "a linked set keeps its dependencies",
			text: "```json\n" +
				`{"tickets":[{"title":"schema"},{"title":"api","depends_on":[0]}]}` +
				"\n```",
			wantCount: 2,
			check: func(t *testing.T, ts []core.PlannedTicket) {
				if len(ts[1].DependsOn) != 1 || ts[1].DependsOn[0] != 0 {
					t.Errorf("dependencies lost: %+v", ts[1])
				}
			},
		},
		{
			name:      "a missing route defaults rather than emptying",
			text:      "```json\n" + `{"tickets":[{"title":"x"}]}` + "\n```",
			wantCount: 1,
			check: func(t *testing.T, ts []core.PlannedTicket) {
				if ts[0].Route != core.RouteImplementation {
					t.Errorf("route = %q, want the implementation default", ts[0].Route)
				}
			},
		},
		// A planner that revises itself mid-message leaves both blocks behind; the later one
		// is what it settled on.
		{
			name: "the last block wins",
			text: "```json\n" + `{"tickets":[{"title":"first"}]}` + "\n```\n" +
				"On reflection:\n\n```json\n" + `{"tickets":[{"title":"second"}]}` + "\n```",
			wantCount: 1,
			check: func(t *testing.T, ts []core.PlannedTicket) {
				if ts[0].Title != "second" {
					t.Errorf("title = %q, want the revised proposal", ts[0].Title)
				}
			},
		},
		{
			name:      "malformed json keeps the prose readable",
			text:      "Here you go.\n\n```json\n{not json at all}\n```",
			wantProse: "Here you go.\n\n```json\n{not json at all}\n```",
		},
		{
			name:      "an empty ticket list is not a proposal",
			text:      "Nothing to do.\n\n```json\n" + `{"tickets":[]}` + "\n```",
			wantProse: "Nothing to do.\n\n```json\n" + `{"tickets":[]}` + "\n```",
		},
		{
			name:      "a ticket with no title is dropped",
			text:      "```json\n" + `{"tickets":[{"title":"  "}]}` + "\n```",
			wantProse: "```json\n" + `{"tickets":[{"title":"  "}]}` + "\n```",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prose, tickets := parsePlan(tc.text)
			if len(tickets) != tc.wantCount {
				t.Fatalf("got %d tickets, want %d", len(tickets), tc.wantCount)
			}
			if tc.wantProse != "" && prose != tc.wantProse {
				t.Errorf("prose = %q, want %q", prose, tc.wantProse)
			}
			if tc.check != nil {
				tc.check(t, tickets)
			}
		})
	}
}

// TestParsePlanDoesNotLeakTheBlockIntoProse: the human reads the prose, and a raw json blob in
// it is noise they did not ask for.
func TestParsePlanDoesNotLeakTheBlockIntoProse(t *testing.T) {
	prose, tickets := parsePlan("Sounds good.\n\n```json\n" +
		`{"tickets":[{"title":"x","body":"y"}]}` + "\n```")

	if len(tickets) != 1 {
		t.Fatalf("got %d tickets, want 1", len(tickets))
	}
	if strings.Contains(prose, "tickets") || strings.Contains(prose, "```") {
		t.Errorf("the proposal block leaked into the prose: %q", prose)
	}
}

func TestRenderBacklog(t *testing.T) {
	if got := renderBacklog(nil); !strings.Contains(got, "Empty") {
		t.Errorf("an empty backlog renders as %q", got)
	}
	got := renderBacklog([]core.Ticket{
		{Title: "add mul", State: core.StateReady},
		{Title: "fix the thing", State: core.StateBacklog},
	})
	for _, want := range []string{"add mul", "fix the thing", string(core.StateReady)} {
		if !strings.Contains(got, want) {
			t.Errorf("backlog listing omits %q:\n%s", want, got)
		}
	}
}

// TestPromptAnswersRatherThanInterrogates pins the behaviour, not the wording.
//
// The first version of this prompt said that "grilling back is the point" and that a proposal
// should be sent "when you have" one. Asked what to work on next, the planner replied with three
// clarifying questions and no proposal — technically obedient, and useless: the human asked a
// question and got homework. Grilling is the human's move to make against something concrete.
func TestPromptAnswersRatherThanInterrogates(t *testing.T) {
	p := &Planner{docBudget: 1000}
	got := p.prompt(promptHost{}, core.Project{Name: "gravy", RepoPath: "/repo"}, nil, "")

	for _, want := range []string{
		"Do not open with questions",
		"state that assumption",
		"End every message with a fenced json block",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt no longer instructs %q:\n%s", want, got)
		}
	}
	// The instruction that caused the regression must not come back.
	for _, unwanted := range []string{"grilling back is the point", "When you have a concrete proposal"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("prompt still contains %q, which defers instead of answering", unwanted)
		}
	}
}

// TestPromptCarriesTheProjectDocuments is the other half of what the screen promises: a plan
// argued from this repository's own conventions, not from generic advice.
func TestPromptCarriesTheProjectDocuments(t *testing.T) {
	p := &Planner{docBudget: 1000}
	got := p.prompt(
		promptHost{files: map[string]string{"/repo/CLAUDE.md": "always use tabs"}},
		core.Project{Name: "gravy", RepoPath: "/repo"},
		[]core.Ticket{{Title: "existing work", State: core.StateReady}},
		"",
	)
	for _, want := range []string{"always use tabs", "CLAUDE.md", "existing work"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt omits %q:\n%s", want, got)
		}
	}
}

// promptHost is a host whose only real method is FS, which is all prompt building uses.
type promptHost struct {
	host.Host
	files map[string]string
}

func (h promptHost) FS() host.FS { return promptFS(h.files) }

type promptFS map[string]string

func (f promptFS) ReadFile(path string) ([]byte, error) {
	b, ok := f[filepath.ToSlash(path)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(b), nil
}
func (f promptFS) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }
func (f promptFS) Stat(string) (os.FileInfo, error)            { return nil, os.ErrNotExist }
func (f promptFS) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (f promptFS) RemoveAll(string) error                      { return os.ErrPermission }
func (f promptFS) Exists(path string) bool                     { _, ok := f[path]; return ok }

// TestResumeNeverSendsAnEmptyMessage is the bug behind a "task_failure (no rule matched)" on the
// second press of "what should be next".
//
// The first turn puts the standing question in the prompt. A resume sends only the human's
// message — and an empty one asks the provider nothing, so it answers with nothing: no message
// events, an outcome that defaults to a task failure, and a turn that looks broken.
func TestResumeNeverSendsAnEmptyMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want string
	}{
		{"empty resumes with the standing question", "", defaultAsk},
		{"blank resumes with the standing question", "   \n ", defaultAsk},
		{"a real question is sent as written", "what about tests?", "what about tests?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := strings.TrimSpace(tc.msg)
			if msg == "" {
				msg = defaultAsk
			}
			if msg != tc.want {
				t.Errorf("resume message = %q, want %q", msg, tc.want)
			}
			if msg == "" {
				t.Error("a resume would ask the provider nothing")
			}
		})
	}
}

func TestActivityLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event provider.Event
		want  string
	}{
		{"start", provider.Event{Kind: provider.EventStarted}, "reading the project"},
		{"thinking", provider.Event{Kind: provider.EventThinking}, "thinking"},
		{
			name:  "a file read names the file",
			event: provider.Event{Kind: provider.EventToolUse, Tool: "Read", Fields: map[string]any{"file_path": "CLAUDE.md"}},
			want:  "reading CLAUDE.md",
		},
		{
			name:  "a tool with no subject still says something",
			event: provider.Event{Kind: provider.EventToolUse, Tool: "Glob"},
			want:  "running Glob",
		},
		// The answer belongs in the transcript, not in the activity line.
		{"messages are not activity", provider.Event{Kind: provider.EventMessage, Text: "here is my plan"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := activityLine(tc.event); got != tc.want {
				t.Errorf("activityLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionCarriesItsProvider(t *testing.T) {
	for _, tc := range []struct {
		name       string
		providerID string
		id         string
		encoded    string
	}{
		{"a normal session", "claude-code", "3da16b2a-ca8f", "claude-code:3da16b2a-ca8f"},
		{"codex", "codex", "abc", "codex:abc"},
		{"no id is no session", "claude-code", "", ""},
		{"no provider is no session", "", "abc", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeSession(tc.providerID, tc.id)
			if got != tc.encoded {
				t.Fatalf("encodeSession(%q, %q) = %q, want %q", tc.providerID, tc.id, got, tc.encoded)
			}
			if tc.encoded == "" {
				return
			}
			p, id := decodeSession(got)
			if p != tc.providerID || id != tc.id {
				t.Errorf("decodeSession(%q) = %q, %q, want %q, %q", got, p, id, tc.providerID, tc.id)
			}
		})
	}
}

// TestDecodeSessionRefusesToGuess is the safety property: an id whose agent is unknown starts a
// fresh conversation rather than being resumed against whichever agent the bucket resolves to
// now. After a quota cooldown falls through to the next choice, that is a different agent, and
// the id means nothing to it.
func TestDecodeSessionRefusesToGuess(t *testing.T) {
	for _, in := range []string{
		"",                // no session
		"bare-session-id", // recorded before the provider travelled with it
		":leading-colon",  // no provider
		"trailing-colon:", // no id
	} {
		if p, id := decodeSession(in); p != "" || id != "" {
			t.Errorf("decodeSession(%q) = %q, %q, want both empty", in, p, id)
		}
	}
}
