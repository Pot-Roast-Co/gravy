package provider

import (
	"regexp"
	"strings"
	"testing"
)

// claudeLikeRules approximates a real adapter's table, seeded from the GR-000 spike fixtures.
func claudeLikeRules() *Matcher {
	return NewMatcher(
		Rule{
			Name:    "tagged auth error",
			Class:   AuthExpired,
			Pattern: regexp.MustCompile(`\[claude-code:(authentication_error|invalid_api_key)\]`),
		},
		Rule{
			Name:    "http 401",
			Class:   AuthExpired,
			Pattern: regexp.MustCompile(`(?i)API Error: 401|api key is invalid`),
		},
		Rule{
			Name:    "quota exhausted",
			Class:   QuotaExhausted,
			Pattern: regexp.MustCompile(`(?i)usage limit reached|quota exceeded`),
		},
		Rule{
			Name:    "rate limited",
			Class:   RateLimited,
			Pattern: regexp.MustCompile(`(?i)API Error: 429|rate limit`),
		},
		Rule{
			// An unrecognised model is a configuration error, not a provider outage.
			// Escalating it up the fallback chain would burn the expensive route on a
			// request guaranteed to 404 again.
			Name:    "unrecognized model",
			Class:   TaskFailure,
			Pattern: regexp.MustCompile(`\[claude-code:unrecognized_model\]`),
		},
	)
}

// TestUnmatchedErrorIsTaskFailure is AC2, and the most important test in this package.
//
// Classification matches strings and exit codes against CLI output that changes on the vendor's
// schedule, so an unrecognised failure is not a rare edge case — it is what happens every time a
// provider rewords an error message. The safe direction is "the agent had a bad run": cheap,
// visible, and retried within budget.
//
// The unsafe direction is QuotaExhausted. That cools down a model that is working perfectly and
// escalates the work up the fallback chain toward the most expensive one, silently — which is
// the exact outcome the routing layer exists to prevent. So an unmatched error must never, under
// any circumstances, classify as a quota condition.
func TestUnmatchedErrorIsTaskFailure(t *testing.T) {
	m := claudeLikeRules()

	unmatched := []string{
		"panic: runtime error: invalid memory address",
		"error: something entirely new that no rule anticipated",
		"Error: ENOENT: no such file or directory",
		"the model refused to continue",
		"",                                    // no output at all
		"API Error: 503 upstream unavailable", // plausibly provider-side, still unmatched
	}

	for _, output := range unmatched {
		got := m.Classify(1, "", output)
		if got.Class != TaskFailure {
			t.Errorf("Classify(1, %q) = %v, want TaskFailure", output, got.Class)
		}
		if got.Class.IsQuotaCondition() {
			t.Errorf("Classify(1, %q) produced a quota condition (%v); this silently escalates "+
				"work to the most expensive model", output, got.Class)
		}
		if !got.Defaulted {
			t.Errorf("Classify(1, %q) did not record that it defaulted", output)
		}
	}
}

// TestUnknownIsNeverReturned: the enum has an Unknown member, but Classify resolves it rather
// than handing an ambiguous value to callers who might route on it.
func TestUnknownIsNeverReturned(t *testing.T) {
	m := claudeLikeRules()
	for _, exit := range []int{0, 1, 2, 127, 137} {
		got := m.Classify(exit, "", "nothing any rule recognises")
		if got.Class == Unknown {
			t.Errorf("Classify(%d, ...) returned Unknown; it must resolve to a routable class", exit)
		}
	}
}

func TestClassifyMatches(t *testing.T) {
	m := claudeLikeRules()

	tests := []struct {
		name     string
		exit     int
		stdout   string
		stderr   string
		want     FailureClass
		wantRule string
	}{
		{"clean exit", 0, "all done", "", Success, "clean exit"},
		{"tagged auth", 1, "", "[claude-code:authentication_error] {}", AuthExpired, "tagged auth error"},
		{"401 text", 1, "", "Failed to authenticate. API Error: 401 API key is invalid.", AuthExpired, "http 401"},
		{"quota", 1, "", "Usage limit reached for this window", QuotaExhausted, "quota exhausted"},
		{"rate limit", 1, "", "API Error: 429 too many requests", RateLimited, "rate limited"},
		{"unrecognized model", 1, "", "[claude-code:unrecognized_model] {\"model\":\"nope\"}", TaskFailure, "unrecognized model"},
		{"matches stdout too", 1, "Usage limit reached", "", QuotaExhausted, "quota exhausted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.Classify(tt.exit, tt.stdout, tt.stderr)
			if got.Class != tt.want {
				t.Errorf("class = %v, want %v", got.Class, tt.want)
			}
			if got.Rule != tt.wantRule {
				t.Errorf("rule = %q, want %q", got.Rule, tt.wantRule)
			}
		})
	}
}

// TestClassificationRecordsEvidence is AC3.
func TestClassificationRecordsEvidence(t *testing.T) {
	m := claudeLikeRules()
	got := m.Classify(1, "", "Failed to authenticate. API Error: 401 API key is invalid.")

	if got.Rule != "http 401" {
		t.Fatalf("rule = %q", got.Rule)
	}
	if got.Evidence == "" {
		t.Fatal("no evidence recorded; a misclassification would be undiagnosable")
	}
	if !strings.Contains(got.Evidence, "401") {
		t.Errorf("evidence = %q, want the matched text", got.Evidence)
	}
	note := got.Note()
	if !strings.Contains(note, "http 401") || !strings.Contains(note, "auth_expired") {
		t.Errorf("Note() = %q, want the rule and the class", note)
	}

	// A defaulted classification says so, so nobody mistakes it for a positive match.
	def := m.Classify(1, "", "something nobody has seen before")
	if !strings.Contains(def.Note(), "defaulted") {
		t.Errorf("defaulted Note() = %q, want it to say so", def.Note())
	}
}

func TestRuleOrderingFirstMatchWins(t *testing.T) {
	m := NewMatcher(
		Rule{Name: "specific", Class: AuthExpired, Pattern: regexp.MustCompile(`API Error: 401`)},
		Rule{Name: "general", Class: TaskFailure, Pattern: regexp.MustCompile(`API Error`)},
	)
	got := m.Classify(1, "", "API Error: 401")
	if got.Rule != "specific" {
		t.Errorf("rule = %q, want the earlier, more specific rule", got.Rule)
	}
}

func TestExitCodeRules(t *testing.T) {
	m := NewMatcher(
		Rule{Name: "sigkill", Class: Timeout, ExitCodes: []int{137}},
		Rule{Name: "not found", Class: ProviderUnavailable, ExitCodes: []int{127}},
	)
	if got := m.Classify(137, "", ""); got.Class != Timeout {
		t.Errorf("exit 137 = %v, want Timeout", got.Class)
	}
	if got := m.Classify(127, "", ""); got.Class != ProviderUnavailable {
		t.Errorf("exit 127 = %v, want ProviderUnavailable", got.Class)
	}
	// An exit code rule must not fire on a different code.
	if got := m.Classify(1, "", ""); got.Class != TaskFailure {
		t.Errorf("exit 1 = %v, want the TaskFailure default", got.Class)
	}
}

// TestExitCodeAndPatternAreAnded: a rule with both constraints requires both, or a rule meant to
// be narrow would fire far more widely than its author intended.
func TestExitCodeAndPatternAreAnded(t *testing.T) {
	m := NewMatcher(Rule{
		Name:      "quota on exit 2 only",
		Class:     QuotaExhausted,
		ExitCodes: []int{2},
		Pattern:   regexp.MustCompile(`limit`),
	})
	if got := m.Classify(2, "", "limit reached"); got.Class != QuotaExhausted {
		t.Errorf("both conditions met = %v, want QuotaExhausted", got.Class)
	}
	if got := m.Classify(1, "", "limit reached"); got.Class != TaskFailure {
		t.Errorf("pattern matched but exit code did not: %v, want TaskFailure", got.Class)
	}
	if got := m.Classify(2, "", "unrelated"); got.Class != TaskFailure {
		t.Errorf("exit code matched but pattern did not: %v, want TaskFailure", got.Class)
	}
}

func TestEmptyMatcherStillClassifies(t *testing.T) {
	m := NewMatcher()
	if got := m.Classify(0, "", ""); got.Class != Success {
		t.Errorf("exit 0 with no rules = %v, want Success", got.Class)
	}
	if got := m.Classify(1, "", "boom"); got.Class != TaskFailure {
		t.Errorf("exit 1 with no rules = %v, want TaskFailure", got.Class)
	}
}

func TestEvidenceIsTruncated(t *testing.T) {
	m := NewMatcher(Rule{Name: "greedy", Class: TaskFailure, Pattern: regexp.MustCompile(`(?s).*`)})
	got := m.Classify(1, "", strings.Repeat("x", 5000))
	if len(got.Evidence) > 220 {
		t.Errorf("evidence is %d chars; it must be bounded for storage and display", len(got.Evidence))
	}
}
