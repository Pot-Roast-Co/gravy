package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/provider"
)

// adapterSamples are one event of each kind as each adapter produces it, in the shapes their
// parsers build (see internal/provider/adapters/*/stream.go). They are written the way the
// orchestrator writes them — a provider.Event through encoding/json — because that is the only
// form StreamLogs ever sees.
var adapterSamples = []struct {
	adapter string
	ev      provider.Event
	want    string
	tool    string
}{
	// claude-code
	{"claude-code", provider.Event{Kind: provider.EventStarted, Text: "session s-1",
		Fields: map[string]any{"session_id": "s-1", "model": "claude-sonnet-5"}},
		"started session s-1 (claude-sonnet-5)", ""},
	{"claude-code", provider.Event{Kind: provider.EventMessage, Text: "I'll start by reading the tests.\n"},
		"I'll start by reading the tests.", ""},
	{"claude-code", provider.Event{Kind: provider.EventToolUse, Tool: "Edit", Text: "Edit internal/foo/bar.go",
		Fields: map[string]any{"file_path": "internal/foo/bar.go", "old_string": "a", "new_string": "b"}},
		"Edit internal/foo/bar.go", "Edit"},
	{"claude-code", provider.Event{Kind: provider.EventToolUse, Tool: "Bash", Text: "Bash make check",
		Fields: map[string]any{"command": "make check", "description": "run the gate"}},
		"Bash make check", "Bash"},
	{"claude-code", provider.Event{Kind: provider.EventToolResult},
		"done", ""},
	{"claude-code", provider.Event{Kind: provider.EventThinking, Fields: map[string]any{"estimated_tokens": 812}},
		"… thinking", ""},
	{"claude-code", provider.Event{Kind: provider.EventUsage,
		Fields: map[string]any{"message_id": "msg_1", "input_tokens": 19256, "output_tokens": 4,
			"uncached_input_tokens": 10, "cache_read_input_tokens": 16245, "cache_creation_input_tokens": 3001}},
		"usage: 19256 tokens in, 4 out", ""},
	{"claude-code", provider.Event{Kind: provider.EventRateLimit,
		Fields: map[string]any{"status": "allowed_warning", "resetsAt": 1760000000}},
		"rate limit reported by claude-code: allowed_warning", ""},
	{"claude-code", provider.Event{Kind: provider.EventError, Text: "API Error: 401 invalid x-api-key",
		Fields: map[string]any{"is_error": true, "api_error_status": 401}},
		"error: API Error: 401 invalid x-api-key", ""},
	{"claude-code", provider.Event{Kind: provider.EventFinished, Text: "All done. The long final message.",
		Fields: map[string]any{"num_turns": 7, "subtype": "success", "terminal_reason": "completed"}},
		"finished after 7 turns (completed)", ""},

	// codex
	{"codex", provider.Event{Kind: provider.EventStarted, Text: "thread th-9", Fields: map[string]any{"thread_id": "th-9"}},
		"started thread th-9", ""},
	{"codex", provider.Event{Kind: provider.EventMessage, Text: "Tests pass."},
		"Tests pass.", ""},
	{"codex", provider.Event{Kind: provider.EventToolUse, Tool: "Bash", Text: "go test ./..."},
		"Bash go test ./...", "Bash"},
	{"codex", provider.Event{Kind: provider.EventToolUse, Tool: "Edit", Text: "update internal/foo/bar.go"},
		"Edit update internal/foo/bar.go", "Edit"},
	{"codex", provider.Event{Kind: provider.EventToolResult, Tool: "Bash", Text: "go test ./...",
		Fields: map[string]any{"output": "ok", "exit_code": 0}},
		"Bash → go test ./...", "Bash"},
	{"codex", provider.Event{Kind: provider.EventThinking, Text: "**Planning the change**\nA long paragraph."},
		"… thinking", ""},
	{"codex", provider.Event{Kind: provider.EventUsage,
		Fields: map[string]any{"input_tokens": 1200, "cached_tokens": 800, "output_tokens": 340}},
		"usage: 1200 tokens in, 340 out", ""},
	{"codex", provider.Event{Kind: provider.EventError, Text: "stream disconnected before completion"},
		"error: stream disconnected before completion", ""},

	// copilot: Fields is the event's whole data object, and tool arguments are nested.
	{"copilot", provider.Event{Kind: provider.EventStarted, Fields: map[string]any{"sessionId": "cp-3", "selectedModel": "gpt-5"}},
		"started session cp-3 (gpt-5)", ""},
	{"copilot", provider.Event{Kind: provider.EventMessage, Text: "Done.", Fields: map[string]any{"content": "Done."}},
		"Done.", ""},
	{"copilot", provider.Event{Kind: provider.EventToolUse, Tool: "edit", Text: "edit",
		Fields: map[string]any{"toolName": "edit", "toolCallId": "c1",
			"arguments": map[string]any{"path": "internal/foo/bar.go", "old_str": "a"}}},
		"edit internal/foo/bar.go", "edit"},
	{"copilot", provider.Event{Kind: provider.EventToolUse, Tool: "report_intent", Text: "report_intent",
		Fields: map[string]any{"toolName": "report_intent", "toolCallId": "c2",
			"arguments": map[string]any{"intent": "Exploring codebase"}}},
		"report_intent Exploring codebase", "report_intent"},
	{"copilot", provider.Event{Kind: provider.EventToolResult, Tool: "edit", Text: "File updated.\nsecond line",
		Fields: map[string]any{"toolCallId": "c1", "success": true}},
		"edit → File updated.", "edit"},
	{"copilot", provider.Event{Kind: provider.EventThinking, Text: "reasoning…", Fields: map[string]any{"content": "reasoning…"}},
		"… thinking", ""},
	{"copilot", provider.Event{Kind: provider.EventUsage, Fields: map[string]any{"inputTokens": 900, "outputTokens": 45, "model": "gpt-5"}},
		"usage: 900 tokens in, 45 out", ""},
	{"copilot", provider.Event{Kind: provider.EventError, Text: "Rate limit exceeded",
		Fields: map[string]any{"errorType": "rate_limit", "message": "Rate limit exceeded"}},
		"error: Rate limit exceeded", ""},
	{"copilot", provider.Event{Kind: provider.EventFinished},
		"finished", ""},
}

func TestRenderEventPerAdapter(t *testing.T) {
	for _, tc := range adapterSamples {
		t.Run(tc.adapter+"/"+string(tc.ev.Kind)+"/"+tc.want, func(t *testing.T) {
			tc.ev.At = time.Now()
			tc.ev.Raw = `{"type":"whatever the CLI sent"}`
			raw, err := json.Marshal(tc.ev)
			if err != nil {
				t.Fatal(err)
			}
			kind, tool, text := renderEvent(string(raw), tc.adapter)
			if kind != tc.ev.Kind {
				t.Errorf("kind = %q, want %q", kind, tc.ev.Kind)
			}
			if tool != tc.tool {
				t.Errorf("tool = %q, want %q", tool, tc.tool)
			}
			if text != tc.want {
				t.Errorf("text = %q, want %q", text, tc.want)
			}
		})
	}
}

// TestRenderEventKeepsWhatItCannotRead: a line that is not an event is shown as it is, never
// dropped and never rendered as an empty sentence.
func TestRenderEventKeepsWhatItCannotRead(t *testing.T) {
	for _, raw := range []string{
		`not json at all`,
		`{"truncated":`,
		`{"Kind":"","Text":"no kind"}`,
		`{"Kind":"some_future_kind","Text":"x"}`,
	} {
		kind, tool, text := renderEvent(raw, "codex")
		if kind != "" || tool != "" || text != raw {
			t.Errorf("renderEvent(%q) = (%q, %q, %q), want the raw line with no kind", raw, kind, tool, text)
		}
	}
}

// TestStreamLogsRendersEventsOverTheWire is the ticket's done-when: a remote client reads
// sentences with their kind, not JSON, for every adapter's events — and raw agent output
// passes through untouched.
func TestStreamLogsRendersEventsOverTheWire(t *testing.T) {
	c, local, _ := served(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := local.logs.Open("run-events")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.WriteAgent(`{"type":"assistant","raw":true}`); err != nil {
		t.Fatal(err)
	}
	for _, s := range adapterSamples {
		if err := w.WriteEvent(s.ev); err != nil {
			t.Fatal(err)
		}
	}

	ch, stop, err := c.StreamLogs(ctx, "run-events")
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	defer stop()

	next := func() LogLine {
		t.Helper()
		select {
		case l := <-ch:
			return l
		case <-time.After(3 * time.Second):
			t.Fatal("stream went quiet")
			return LogLine{}
		}
	}

	if l := next(); l.Kind != "" || l.Text != `{"type":"assistant","raw":true}` {
		t.Errorf("agent line = %+v, want it untouched", l)
	}
	for _, s := range adapterSamples {
		l := next()
		// The run has no row, so a rate-limit line cannot name its provider.
		want := s.want
		if s.ev.Kind == provider.EventRateLimit {
			want = "rate limit reported by the provider: allowed_warning"
		}
		if l.Kind != s.ev.Kind || l.Tool != s.tool || l.Text != want {
			t.Errorf("%s %s: got kind=%q tool=%q text=%q, want kind=%q tool=%q text=%q",
				s.adapter, s.ev.Kind, l.Kind, l.Tool, l.Text, s.ev.Kind, s.tool, want)
		}
	}
}
