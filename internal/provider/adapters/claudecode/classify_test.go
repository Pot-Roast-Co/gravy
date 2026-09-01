package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bobbybrady/gravy/internal/provider"
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
		{"quota exceeded", provider.QuotaExhausted},
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
