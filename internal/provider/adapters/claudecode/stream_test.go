package claudecode

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/provider"
)

// TestParseRealStream runs the parser over a stream captured from the real CLI.
func TestParseRealStream(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "docs", "fixtures", "claude-code", "stream-tool-use.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	var events []provider.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		events = append(events, parseLine(sc.Text())...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	byKind := map[provider.EventKind]int{}
	var tools []string
	for _, e := range events {
		byKind[e.Kind]++
		if e.Kind == provider.EventToolUse {
			tools = append(tools, e.Tool)
		}
		if e.Raw == "" {
			t.Errorf("event %v has no raw line; a parsing gap would be undiagnosable", e.Kind)
		}
		if e.At.IsZero() {
			t.Errorf("event %v has no timestamp", e.Kind)
		}
	}

	// The captured run read a file, edited it, and reported back.
	if byKind[provider.EventStarted] != 1 {
		t.Errorf("got %d started events, want 1", byKind[provider.EventStarted])
	}
	if len(tools) != 2 || tools[0] != "Read" || tools[1] != "Edit" {
		t.Errorf("tool uses = %v, want [Read Edit]", tools)
	}
	if byKind[provider.EventToolResult] != 2 {
		t.Errorf("got %d tool results, want 2", byKind[provider.EventToolResult])
	}
	if byKind[provider.EventMessage] == 0 {
		t.Error("no message events were parsed")
	}
	if byKind[provider.EventThinking] == 0 {
		t.Error("no thinking events; these are the liveness signal for a stalled run")
	}
	// Quota telemetry arrives on healthy runs and is worth capturing now.
	if byKind[provider.EventRateLimit] != 1 {
		t.Errorf("got %d rate limit events, want 1", byKind[provider.EventRateLimit])
	}
	if byKind[provider.EventFinished] != 1 {
		t.Errorf("got %d finished events, want 1", byKind[provider.EventFinished])
	}
}

func TestParseSessionIDFromInit(t *testing.T) {
	line := `{"type":"system","subtype":"init","session_id":"11111111-2222-3333-4444-555555555555","model":"claude-haiku-4-5-20251001","cwd":"/WORKTREE"}`
	events := parseLine(line)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Kind != provider.EventStarted {
		t.Errorf("kind = %v, want started", e.Kind)
	}
	if e.Fields["session_id"] != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("session_id = %v", e.Fields["session_id"])
	}
	if e.Fields["model"] != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %v", e.Fields["model"])
	}
}

func TestParseRateLimitEvent(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1788235200,` +
		`"unifiedWindows":{"five_hour":{"utilization":0.68,"resetsAt":1788235200}}},"session_id":"x"}`
	events := parseLine(line)
	if len(events) != 1 || events[0].Kind != provider.EventRateLimit {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Fields["status"] != "allowed" {
		t.Errorf("status = %v, want allowed", events[0].Fields["status"])
	}
	// The exact reset time is what lets a cooldown have a real expiry rather than a guess.
	if events[0].Fields["resetsAt"] == nil {
		t.Error("resetsAt was not captured")
	}
}

func TestParseToolUseSummary(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash",` +
		`"input":{"command":"go test ./...","description":"run tests"}}]}}`
	events := parseLine(line)
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	e := events[0]
	if e.Tool != "Bash" {
		t.Errorf("tool = %q, want Bash", e.Tool)
	}
	// The one-line summary is what the Running view shows, so it must name the command.
	if !strings.Contains(e.Text, "go test ./...") {
		t.Errorf("text = %q, want it to include the command", e.Text)
	}
}

func TestParseErrorResult(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":true,"api_error_status":404,` +
		`"terminal_reason":"api_error","result":"model not found","num_turns":1}`
	events := parseLine(line)
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	// subtype says success; is_error says otherwise. The event must reflect the failure.
	if events[0].Kind != provider.EventError {
		t.Errorf("kind = %v, want error despite subtype success", events[0].Kind)
	}
	if events[0].Fields["api_error_status"] != 404 {
		t.Errorf("api_error_status = %v", events[0].Fields["api_error_status"])
	}
}

// TestParseMalformedLinesAreIgnored: an unparseable line must not fail a run.
//
// Failing a run because a log line was unrecognised would turn a cosmetic CLI change into a
// broken pipeline.
func TestParseMalformedLinesAreIgnored(t *testing.T) {
	for _, line := range []string{
		"", "   ", "not json at all", "{broken json", "[]", "null",
		`{"type":"something_new_from_a_future_version","field":1}`,
		`{"type":"assistant","message":{"content":"not an array"}}`,
	} {
		if got := parseLine(line); len(got) != 0 {
			t.Errorf("parseLine(%q) produced %d events, want none", line, len(got))
		}
	}
}

func TestTerminalResult(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.5,` +
		`"session_id":"abc","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5}}`
	se := terminalResult(line)
	if se == nil {
		t.Fatal("terminalResult returned nil for a result line")
	}
	if se.NumTurns != 3 || se.SessionID != "abc" {
		t.Errorf("parsed = %+v", se)
	}
	if se.TotalCostUSD == nil || *se.TotalCostUSD != 0.5 {
		t.Errorf("cost = %v", se.TotalCostUSD)
	}
	if terminalResult(`{"type":"assistant"}`) != nil {
		t.Error("terminalResult matched a non-result line")
	}
}

func TestNewUUIDIsValid(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newUUID()
		if len(id) != 36 {
			t.Fatalf("uuid %q is %d chars, want 36", id, len(id))
		}
		// The CLI validates --session-id as a UUID, so the shape must be right.
		if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			t.Fatalf("uuid %q is not dash-separated correctly", id)
		}
		if id[14] != '4' {
			t.Fatalf("uuid %q is not version 4", id)
		}
		if c := id[19]; c != '8' && c != '9' && c != 'a' && c != 'b' {
			t.Fatalf("uuid %q has the wrong variant nibble %q", id, c)
		}
		if seen[id] {
			t.Fatalf("duplicate uuid %q", id)
		}
		seen[id] = true
	}
}

// TestParsePermissionDenials covers the trap that a refused run reports itself as successful.
//
// Observed from the real CLI: exit 0, is_error false, subtype "success", and a permission_denials
// array holding the one Write the task needed. Nothing in the success fields distinguishes that
// from a run that genuinely had nothing to do — only the denials array does.
func TestParsePermissionDenials(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"num_turns":2,` +
		`"result":"I need permission to write to the file.",` +
		`"permission_denials":[{"tool_name":"Write","tool_use_id":"toolu_01",` +
		`"tool_input":{"file_path":"/w/hello.txt","content":"hello"}}]}`

	se := terminalResult(line)
	if se == nil {
		t.Fatal("terminalResult returned nil")
	}
	if se.IsError {
		t.Fatal("the fixture's premise has changed: is_error is now true")
	}
	if len(se.PermissionDenials) != 1 {
		t.Fatalf("got %d denials, want 1", len(se.PermissionDenials))
	}
	d := se.PermissionDenials[0]
	if d.ToolName != "Write" {
		t.Errorf("tool = %q, want Write", d.ToolName)
	}
	if d.ToolInput["file_path"] != "/w/hello.txt" {
		t.Errorf("tool input did not survive parsing: %+v", d.ToolInput)
	}
}
