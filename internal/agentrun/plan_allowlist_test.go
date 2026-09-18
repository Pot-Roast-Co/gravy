package agentrun

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// TestPlanAllowlistIsNotEmpty is the regression.
//
// The planner's AgentTask carried no allowlist at all, which is not the same as a restrictive
// one: with no allowlist the adapter sends no --allowedTools, the agent falls back to the CLI's
// own default, and every shell call is denied. Three planning turns in a row died on
// "permission denied" for ls, sed and grep, and the Plan screen was unusable on any project.
func TestPlanAllowlistIsNotEmpty(t *testing.T) {
	a := planAllowlist()
	if len(a.Commands) == 0 {
		t.Fatal("the planner would be sent no --allowedTools, and denied every command it runs")
	}
}

// The commands the planner actually reached for, and was refused.
func TestPlanAllowlistCoversReadingARepository(t *testing.T) {
	have := map[string]bool{}
	for _, c := range planAllowlist().Commands {
		have[c.Match] = true
	}
	for _, want := range []string{"ls", "cat", "head", "sed", "grep", "find", "echo", "git log"} {
		if !have[want] {
			t.Errorf("a planner cannot read a repository without %q", want)
		}
	}
}

// Planning reads and proposes; it never edits. It must not inherit the project's build and test
// commands just because the run path does.
func TestPlanAllowlistGrantsNoBuildCommands(t *testing.T) {
	for _, c := range planAllowlist().Commands {
		for _, forbidden := range []string{"make", "go build", "go test", "rm", "curl"} {
			if c.Match == forbidden {
				t.Errorf("planning was granted %q", forbidden)
			}
		}
	}
}

// The run path keeps its own, wider allowlist: a ticket run does have to build and test.
func TestEffectiveAllowlistStillCarriesTheProject(t *testing.T) {
	p := core.Project{
		Allowlist:  core.Allowlist{Commands: []core.Pattern{{Match: "make"}}},
		Validation: []core.Step{{Name: "check", Cmd: "make check"}},
	}
	var got []string
	for _, c := range effectiveAllowlist(p).Commands {
		got = append(got, c.Match)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"make", "make check", "ls", "grep"} {
		if !strings.Contains(joined, want) {
			t.Errorf("effectiveAllowlist lost %q: %v", want, got)
		}
	}
}
