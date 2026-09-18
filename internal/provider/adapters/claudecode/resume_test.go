package claudecode

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// TestResumeCarriesTheAllowlist is the regression.
//
// A resumed turn sent no --allowedTools at all, which is not a restrictive allowlist but the
// absence of one: the CLI falls back to its own default and denies every shell command. On the
// Plan screen that meant the opening question worked and every "try again" after it died on
// "permission denied" for ls, sed and grep.
func TestResumeCarriesTheAllowlist(t *testing.T) {
	p := New()
	allow := core.Allowlist{Commands: []core.Pattern{{Match: "ls"}, {Match: "sed"}}}
	args := p.resumeArgs(provider.SessionRef{ProviderID: ID, ID: "sess-1"}, "try again", allow)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--allowedTools") {
		t.Fatalf("a resumed turn sends no grants at all:\n%v", args)
	}
	for _, want := range []string{"Bash(ls)", "Bash(ls *)", "Bash(sed)", "Bash(sed *)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("resume args omit %q:\n%v", want, args)
		}
	}
	// The session and message still go, obviously.
	if !strings.Contains(joined, "--resume sess-1") || !strings.Contains(joined, "try again") {
		t.Errorf("resume lost its session or message:\n%v", args)
	}
}

// An empty allowlist sends no flag rather than an empty one, which the CLI rejects.
func TestResumeWithNoGrantsSendsNoFlag(t *testing.T) {
	args := New().resumeArgs(provider.SessionRef{ProviderID: ID, ID: "s"}, "hi", core.Allowlist{})
	if strings.Contains(strings.Join(args, " "), "--allowedTools") {
		t.Errorf("sent an empty --allowedTools:\n%v", args)
	}
}

// Run and Resume must grant the same things, or a conversation changes what it may do halfway
// through for reasons no one can see.
func TestRunAndResumeGrantTheSameCommands(t *testing.T) {
	allow := core.Allowlist{Commands: []core.Pattern{{Match: "grep"}, {Match: "head"}}}
	want := allowedTools(allow)

	resume := strings.Join(New().resumeArgs(provider.SessionRef{ProviderID: ID, ID: "s"}, "m", allow), " ")
	for _, pattern := range want {
		if !strings.Contains(resume, pattern) {
			t.Errorf("resume does not grant %q that a first turn does", pattern)
		}
	}
}
