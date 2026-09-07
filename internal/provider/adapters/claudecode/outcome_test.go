package claudecode

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// parseResult builds a result event from a JSON line, as the stream reader would.
func parseResult(t *testing.T, line string) *streamEvent {
	t.Helper()
	var se streamEvent
	if err := json.Unmarshal([]byte(line), &se); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	return &se
}

// TestDeniedRunIsNotSuccess is the regression test for the worst bug found in this adapter.
//
// The real CLI reported a run as successful — exit 0, is_error false, subtype "success" — while
// every tool call in it had been denied and the requested file was never created. Gravy would
// have recorded Success, generated a summary from the resulting empty diff, and put "no changes"
// in the review queue. The agent was not idle; it was blocked, and no success field said so.
//
// This cannot be caught by the integration test: --permission-mode acceptEdits now prevents the
// denial from happening at all, so a live run never reaches this state. The only way to hold the
// behaviour in place is to assert it directly.
func TestDeniedRunIsNotSuccess(t *testing.T) {
	p := New()
	result := parseResult(t, `{"type":"result","subtype":"success","is_error":false,"num_turns":2,`+
		`"result":"I need permission to write to the file.",`+
		`"permission_denials":[{"tool_name":"Write","tool_use_id":"toolu_01",`+
		`"tool_input":{"file_path":"/w/hello.txt","content":"hello"}}]}`)

	out := computeOutcome(p, host.ExitStatus{Code: 0}, result, "", "", "sess-1")

	if out.Class == provider.Success {
		t.Fatal("a run whose every tool call was denied was reported as Success; " +
			"this sends an empty diff to review described as \"no changes\"")
	}
	if out.Class != provider.TaskFailure {
		t.Errorf("class = %v, want TaskFailure", out.Class)
	}
	if len(out.Denials) != 1 || out.Denials[0].Tool != "Write" {
		t.Errorf("denials = %+v, want the refused Write", out.Denials)
	}
	// The note must name what was refused, or a human cannot tell why the run failed.
	if got := out.Note; got == "" || !contains(got, "Write") || !contains(got, "/w/hello.txt") {
		t.Errorf("note = %q, want it to name the refused call", got)
	}
}

// TestSubtypeSuccessWithIsErrorIsNotSuccess is the F1 finding held in place.
//
// A hard 404 and a 401 were both observed arriving as subtype "success" alongside is_error true.
func TestSubtypeSuccessWithIsErrorIsNotSuccess(t *testing.T) {
	p := New()
	result := parseResult(t, `{"type":"result","subtype":"success","is_error":true,`+
		`"api_error_status":404,"terminal_reason":"api_error",`+
		`"result":"There's an issue with the selected model (no-such-model-9000)."}`)

	// Exit code 0 with is_error true is the pathological combination.
	out := computeOutcome(p, host.ExitStatus{Code: 0}, result, "", "", "sess-1")

	if out.Class == provider.Success {
		t.Fatal("a run with is_error true was reported as Success on the strength of subtype")
	}
	if out.Class.IsQuotaCondition() {
		t.Errorf("class = %v; a model configuration error must not cool down a provider", out.Class)
	}
}

func TestSuccessfulRunIsSuccess(t *testing.T) {
	p := New()
	result := parseResult(t, `{"type":"result","subtype":"success","is_error":false,"num_turns":4,`+
		`"session_id":"from-result","total_cost_usd":0.0353,`+
		`"usage":{"input_tokens":10,"output_tokens":500,"cache_read_input_tokens":90,"cache_creation_input_tokens":100}}`)

	out := computeOutcome(p, host.ExitStatus{Code: 0}, result, "", "", "sess-1")

	if out.Class != provider.Success {
		t.Fatalf("class = %v (%s), want Success", out.Class, out.Note)
	}
	if len(out.Denials) != 0 {
		t.Errorf("denials = %+v, want none", out.Denials)
	}
	if out.Turns != 4 {
		t.Errorf("turns = %d, want 4", out.Turns)
	}
	// Input tokens include cache reads and creations; those are real consumption.
	if out.TokensIn != 200 {
		t.Errorf("tokens in = %d, want 200 (10 + 90 + 100)", out.TokensIn)
	}
	if out.TokensOut != 500 {
		t.Errorf("tokens out = %d, want 500", out.TokensOut)
	}
	if out.CostUSD == nil || *out.CostUSD != 0.0353 {
		t.Errorf("cost = %v", out.CostUSD)
	}
	// The result's own session id wins, since it is what the CLI will accept for --resume.
	if out.Session.ID != "from-result" {
		t.Errorf("session = %q, want the id reported by the CLI", out.Session.ID)
	}
	if out.Session.ProviderID != ID {
		t.Errorf("session provider = %q, want %q", out.Session.ProviderID, ID)
	}
}

func TestTimeoutOutcome(t *testing.T) {
	p := New()
	out := computeOutcome(p, host.ExitStatus{
		Code: 137, Signaled: true, TimedOut: true, Duration: 2 * time.Second,
	}, nil, "", "", "sess-1")

	if out.Class != provider.Timeout {
		t.Errorf("class = %v, want Timeout", out.Class)
	}
	if !out.TimedOut {
		t.Error("TimedOut was not propagated")
	}
	if out.Note == "" {
		t.Error("a timed-out run has no note explaining itself")
	}
	// A timeout must be resumable: the session is how a retry keeps its context.
	if !out.Session.Valid() {
		t.Error("no session recorded for a timed-out run")
	}
}

// TestOutcomeWithoutResultEvent: a run killed before emitting its result still gets judged.
func TestOutcomeWithoutResultEvent(t *testing.T) {
	p := New()
	out := computeOutcome(p, host.ExitStatus{Code: 1}, nil, "", "some failure", "sess-1")

	if out.Class != provider.TaskFailure {
		t.Errorf("class = %v, want TaskFailure", out.Class)
	}
	if out.Session.ID != "sess-1" {
		t.Errorf("session = %q, want the pre-assigned id", out.Session.ID)
	}
}

// TestAuthFailureOutcome: a 401 must classify as AuthExpired so it escalates rather than
// burning the self-correction budget on a retry that cannot succeed.
func TestAuthFailureOutcome(t *testing.T) {
	p := New()
	result := parseResult(t, `{"type":"result","subtype":"success","is_error":true,`+
		`"api_error_status":401,"result":"Failed to authenticate. API Error: 401 API key is invalid."}`)

	out := computeOutcome(p, host.ExitStatus{Code: 1}, result, "", "", "sess-1")

	if out.Class != provider.AuthExpired {
		t.Errorf("class = %v, want AuthExpired", out.Class)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
