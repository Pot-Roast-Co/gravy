package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// fixture reads a captured sample of real CLI output.
//
// These were captured from the actual binary during the GR-000 spike. Classification rules
// written against imagined output are how a classifier drifts, so the tests are written against
// what the CLI really emitted.
func fixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "docs", "fixtures", "claude-code", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func TestClassifyAgainstFixtures(t *testing.T) {
	p := New()

	tests := []struct {
		name     string
		exit     int
		stdout   string
		stderr   string
		want     provider.FailureClass
		wantRule string
	}{
		{
			name:     "successful run",
			exit:     0,
			stdout:   fixture(t, "result-success.json"),
			want:     provider.Success,
			wantRule: "clean exit",
		},
		{
			// Observed: exit 1, api_error_status 401, after ~188s of silent retries.
			name:     "unauthenticated",
			exit:     1,
			stdout:   fixture(t, "result-auth-401.json"),
			want:     provider.AuthExpired,
			wantRule: "http 401 unauthenticated",
		},
		{
			// A configuration error, deliberately NOT a provider outage: escalating it would
			// spend the expensive route on a request guaranteed to 404 again.
			name:     "unrecognized model",
			exit:     1,
			stdout:   fixture(t, "result-unrecognized-model.json"),
			stderr:   fixture(t, "stderr-unrecognized-model.txt"),
			want:     provider.TaskFailure,
			wantRule: "unrecognized model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Classify(tt.exit, tt.stdout, tt.stderr)
			if got.Class != tt.want {
				t.Errorf("class = %v, want %v (rule %q, evidence %q)",
					got.Class, tt.want, got.Rule, got.Evidence)
			}
			if got.Rule != tt.wantRule {
				t.Errorf("rule = %q, want %q", got.Rule, tt.wantRule)
			}
		})
	}
}

// TestClassifyOrdinaryBuildErrorIsTaskFailure is AC5's first half.
func TestClassifyOrdinaryBuildErrorIsTaskFailure(t *testing.T) {
	p := New()

	buildErrors := []string{
		"./main.go:12:2: undefined: fmt.Printline",
		"FAIL\tgithub.com/example/pkg\t0.012s",
		"error: cannot borrow `x` as mutable more than once at a time",
		"SyntaxError: Unexpected token '}'",
		"make: *** [build] Error 2",
	}
	for _, out := range buildErrors {
		got := p.Classify(1, out, "")
		if got.Class != provider.TaskFailure {
			t.Errorf("Classify(%q) = %v, want TaskFailure", out, got.Class)
		}
		if got.Class.IsQuotaCondition() {
			t.Errorf("an ordinary build error was classified as a quota condition: %q", out)
		}
	}
}

// TestQuotaAndRateLimitPatterns documents the wordings each rule is meant to catch.
func TestQuotaAndRateLimitPatterns(t *testing.T) {
	p := New()

	tests := []struct {
		output string
		want   provider.FailureClass
	}{
		{`{"api_error_status":429}`, provider.RateLimited},
		{"API Error: 429 Too Many Requests", provider.RateLimited},
		{"Usage limit reached for the five_hour window", provider.QuotaExhausted},
		// "quota exceeded" was matched here on a guess about how an exhausted window might
		// phrase itself. It is not the CLI's wording, and it collides with ordinary prose —
		// a ticket about billing, or a document about limits — so it now falls through to a
		// task failure, which is retried and visible rather than cooling the model down.
		{"quota exceeded", provider.TaskFailure},
		{`{"api_error_status":529}`, provider.ProviderUnavailable},
		{"API Error: 503 upstream unavailable", provider.ProviderUnavailable},
		{`{"type":"error","error":{"type":"overloaded_error"}}`, provider.ProviderUnavailable},
		{"[claude-code:authentication_error] {}", provider.AuthExpired},
	}
	for _, tt := range tests {
		if got := p.Classify(1, "", tt.output); got.Class != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.output, got.Class, tt.want)
		}
	}
}

// TestKilledRunClassifiesAsTimeout: LocalHost reports 128+SIGKILL for a run it terminated.
func TestKilledRunClassifiesAsTimeout(t *testing.T) {
	p := New()
	if got := p.Classify(137, "", ""); got.Class != provider.Timeout {
		t.Errorf("exit 137 = %v, want Timeout", got.Class)
	}
	if got := p.Classify(127, "", ""); got.Class != provider.ProviderUnavailable {
		t.Errorf("exit 127 = %v, want ProviderUnavailable", got.Class)
	}
}

// TestSubtypeSuccessIsNotTrusted is the F1 finding from the spike, as a test.
//
// The CLI reported subtype "success" alongside is_error true and api_error_status 404 on a hard
// failure. An adapter keying off subtype would report that run as successful; since result
// summaries are generated from the diff, the empty diff would then be described as "no changes"
// rather than surfacing the failure.
func TestSubtypeSuccessIsNotTrusted(t *testing.T) {
	raw := fixture(t, "result-unrecognized-model.json")
	if !strings.Contains(raw, `"subtype":"success"`) {
		t.Fatal("the fixture no longer contains subtype success; this test's premise has changed")
	}
	if !resultIsError(raw) {
		t.Error("resultIsError did not detect is_error on a fixture where subtype says success")
	}

	success := fixture(t, "result-success.json")
	if resultIsError(success) {
		t.Error("resultIsError reported a genuinely successful run as an error")
	}
}

// TestPermissionDenialSummary checks what a human would be shown for a refused call.
func TestPermissionDenialSummary(t *testing.T) {
	tests := []struct {
		denial provider.PermissionDenial
		want   string
	}{
		{provider.PermissionDenial{Tool: "Write", Input: map[string]any{"file_path": "/w/hello.txt"}}, "Write: /w/hello.txt"},
		{provider.PermissionDenial{Tool: "Bash", Input: map[string]any{"command": "rm -rf /"}}, "Bash: rm -rf /"},
		{provider.PermissionDenial{Tool: "WebFetch", Input: map[string]any{"url": "https://example.com"}}, "WebFetch: https://example.com"},
		{provider.PermissionDenial{Tool: "Mystery"}, "Mystery"},
	}
	for _, tt := range tests {
		if got := tt.denial.Summary(); got != tt.want {
			t.Errorf("Summary() = %q, want %q", got, tt.want)
		}
	}
}

// TestAllowedToolsFromAllowlist guards the fix for an agent that could not run the commands its
// own work is judged by.
//
// Observed against the real CLI: acceptEdits permits file edits but gates shell, so `go test`
// was denied. The agent wrote correct code, reported that it needed approval, and the run was
// classified a task failure — which then burned a self-correction retry doing the same thing at
// twice the cost.
func TestAllowedToolsFromAllowlist(t *testing.T) {
	got := allowedTools(core.Allowlist{Commands: []core.Pattern{
		{Match: "go test ./...", Note: "validation"},
		{Match: "go build ./...", Note: "validation"},
		{Match: "   ", Note: "blank is skipped"},
	}})

	// The CLI's own read tools lead, then the shell grants in the order declared. The read
	// tools are there because a shell rule cannot authorise a chained command, so an agent with
	// no other way to read a file is denied the moment it writes `ls a; find b`.
	want := []string{
		"Read", "Glob", "Grep",
		"Bash(go test ./...)", "Bash(go test ./... *)",
		"Bash(go build ./...)", "Bash(go build ./... *)",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if len(allowedTools(core.Allowlist{})) != 0 {
		t.Error("an empty allowlist produced patterns")
	}
}

// TestAllowedToolsStaysNarrow: the wildcard permits appended flags, never a different command.
//
// A pattern like "go *" would let the agent run `go run` on anything, which is a different
// permission entirely from "run the project's test suite".
func TestAllowedToolsStaysNarrow(t *testing.T) {
	for _, p := range allowedTools(core.Allowlist{Commands: []core.Pattern{{Match: "go test ./..."}}}) {
		if p == "Bash(go *)" || p == "Bash(*)" {
			t.Errorf("allowlist widened to %q, which permits unrelated commands", p)
		}
		// The read tools are not shell and are checked by their own test; this one is about
		// what a declared command is allowed to turn into.
		if !strings.HasPrefix(p, "Bash(") {
			continue
		}
		if !strings.HasPrefix(p, "Bash(go test ./...") {
			t.Errorf("pattern %q does not start with the declared command", p)
		}
	}
}

// TestQuotaWordsInTheTranscriptAreNotAQuotaFailure is a real incident.
//
// A run editing a ROADMAP entry about *rate limiting* was classified as rate limited. It had
// exited cleanly after 35 turns having done the work, at 9% of the five-hour window. The result
// was a discarded run, a fleet-wide cooldown on the model, and a ticket parked as
// "validation_failed" — for a phrase in a file the agent wrote.
func TestQuotaWordsInTheTranscriptAreNotAQuotaFailure(t *testing.T) {
	p := New()

	for _, tc := range []struct {
		name   string
		stdout string
	}{
		{
			name:   "a file about rate limiting",
			stdout: `{"type":"assistant","text":"Moved the Rate limit finding to fixed in ROADMAP.md"}`,
		},
		{
			// The CLI reports healthy quota telemetry on every run.
			name:   "healthy quota telemetry",
			stdout: `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour"}}`,
		},
		{
			name:   "a ticket body quoting a limit",
			stdout: `{"type":"user","text":"add a per-identifier login throttle; the quota exceeded case must 429"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Classify(0, tc.stdout, "")
			if got.Class.IsQuotaCondition() {
				t.Errorf("classified as %v (rule %q) — the provider reported no error",
					got.Class, got.Rule)
			}
		})
	}
}

// TestRealRateLimitIsStillCaught, so the guard has not made the rule decorative.
func TestRealRateLimitIsStillCaught(t *testing.T) {
	p := New()
	for _, out := range []string{
		`{"type":"result","is_error":true,"api_error_status":429}`,
		`API Error: 429 Too Many Requests`,
	} {
		got := p.Classify(1, out, "")
		if got.Class != provider.RateLimited {
			t.Errorf("Classify(%q) = %v, want RateLimited", out, got.Class)
		}
	}
}
