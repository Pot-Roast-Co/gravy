package provider

import (
	"fmt"
	"regexp"
	"strings"
)

// Classification is a judged run outcome plus the evidence for it.
//
// The evidence is not decoration. Classification is string and exit-code matching against CLI
// output that changes on the vendor's schedule, so misclassification is a question of when, not
// if — and a wrong call is only diagnosable if the reason was recorded.
type Classification struct {
	Class FailureClass
	// Rule names the matcher rule that fired, or is empty when none did.
	Rule string
	// Evidence is the text that matched, trimmed for display.
	Evidence string
	// Defaulted reports that no rule matched and the safe default was applied.
	Defaulted bool
}

// Note renders the classification for the run's failure_note column.
func (c Classification) Note() string {
	switch {
	case c.Defaulted:
		return fmt.Sprintf("%s (no rule matched; defaulted)", c.Class)
	case c.Evidence != "":
		return fmt.Sprintf("%s (rule %q matched: %s)", c.Class, c.Rule, c.Evidence)
	default:
		return fmt.Sprintf("%s (rule %q)", c.Class, c.Rule)
	}
}

// Rule maps an observed exit code and/or output pattern to a failure class.
//
// Adapters declare rules rather than write classification logic, so that adding a provider is a
// table of patterns rather than a fresh opportunity to get the quota invariant wrong.
type Rule struct {
	// Name identifies the rule in recorded evidence.
	Name string
	// Class is what to report when this rule matches.
	Class FailureClass
	// ExitCodes, when non-empty, requires the exit code to be one of these.
	ExitCodes []int
	// Pattern, when set, must match the combined stdout and stderr.
	Pattern *regexp.Regexp
}

func (r Rule) matches(exit int, combined string) (string, bool) {
	if len(r.ExitCodes) > 0 {
		ok := false
		for _, c := range r.ExitCodes {
			if c == exit {
				ok = true
				break
			}
		}
		if !ok {
			return "", false
		}
	}
	if r.Pattern == nil {
		return fmt.Sprintf("exit %d", exit), true
	}
	m := r.Pattern.FindString(combined)
	if m == "" {
		return "", false
	}
	return truncate(strings.TrimSpace(m), 200), true
}

// Matcher classifies runs against an ordered rule table.
type Matcher struct {
	rules []Rule
}

// NewMatcher returns a Matcher. Rules are evaluated in order and the first match wins, so more
// specific rules belong earlier.
func NewMatcher(rules ...Rule) *Matcher {
	return &Matcher{rules: rules}
}

// Classify judges a finished run.
//
// An exit code of zero is Success. Otherwise the rules are tried in order, and if none matches
// the result is TaskFailure.
//
// That default is the single most important line in this package. An unrecognised failure means
// the agent had a bad run — never that the provider is unavailable. A false QuotaExhausted cools
// down a working model and escalates the work up the fallback chain toward the most expensive
// one, which is precisely the outcome routing exists to prevent, and it does so silently.
// Failing toward "bad run" is cheap and visible; failing toward "provider is down" is expensive
// and invisible.
func (m *Matcher) Classify(exit int, stdout, stderr string) Classification {
	combined := stdout + "\n" + stderr

	for _, r := range m.rules {
		if evidence, ok := r.matches(exit, combined); ok {
			return Classification{Class: r.Class, Rule: r.Name, Evidence: evidence}
		}
	}

	if exit == 0 {
		return Classification{Class: Success, Rule: "clean exit"}
	}
	return Classification{
		Class:     TaskFailure,
		Evidence:  truncate(strings.TrimSpace(lastLines(stderr, 3)), 200),
		Defaulted: true,
	}
}

// Rules returns the matcher's rules, for tests and for explaining a classification in the UI.
func (m *Matcher) Rules() []Rule { return m.rules }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// lastLines returns the final n non-empty lines, which is where CLIs put the actual error.
func lastLines(s string, n int) string {
	var kept []string
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			kept = append([]string{lines[i]}, kept...)
		}
	}
	return strings.Join(kept, "\n")
}
