package agentrun

import (
	"context"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/provider/fake"
)

func discussTurn(t *testing.T) core.DiscussionTurn {
	t.Helper()
	return core.DiscussionTurn{
		Ticket:  core.Ticket{ID: "GR-1", Title: "Recommend picks", WorktreePath: t.TempDir()},
		Project: core.Project{Slug: "gravy"},
		Message: "try again",
	}
}

var refusedChain = []provider.PermissionDenial{{Tool: "Bash",
	Input: map[string]any{"command": "git log -3 && gh pr view --json title,body"}}}

// TestARefusedLookDoesNotLoseTheAnswer is the discussion that failed on screen: the agent tried
// a chained `git log && gh pr view`, was refused, answered anyway — and the human was shown
// "the discussion failed: permission denied" instead of the answer.
func TestARefusedLookDoesNotLoseTheAnswer(t *testing.T) {
	o := reviewOrch(t, fake.Script{
		Events: []provider.Event{{Kind: provider.EventMessage,
			Text: "Narrow the ticket to ESPN projections.\n\n```json\n" +
				`{"correction":"narrow to ESPN","preserve":["consensus rank"],"verify":[]}` + "\n```"}},
		Outcome: provider.Outcome{Class: provider.TaskFailure, Denials: refusedChain,
			Note: `task_failure (rule "permission denied" matched: Bash: git log -3 && gh pr view)`},
	})

	got, err := o.Discuss(fixedRoute("fake", "m")).Discuss(context.Background(), discussTurn(t))
	if err != nil {
		t.Fatalf("a turn that answered was reported as failed: %v", err)
	}
	if got.Proposal.Correction != "narrow to ESPN" {
		t.Errorf("proposal = %+v, want the agent's answer", got.Proposal)
	}
}

// TestARefusedTurnThatSaidNothingStillFails: being lenient about refusals must not turn an agent
// that was blocked from everything into an empty success.
func TestARefusedTurnThatSaidNothingStillFails(t *testing.T) {
	o := reviewOrch(t, fake.Script{
		Outcome: provider.Outcome{Class: provider.TaskFailure, Denials: refusedChain,
			Note: `task_failure (rule "permission denied")`},
	})

	if _, err := o.Discuss(fixedRoute("fake", "m")).Discuss(context.Background(), discussTurn(t)); err == nil {
		t.Fatal("a refused turn with no answer was reported as a success")
	}
}

// TestAFailedTurnStillFails: only refusals are forgiven. A crash is still a crash.
func TestAFailedTurnStillFails(t *testing.T) {
	for _, out := range []provider.Outcome{
		{Class: provider.TaskFailure, ExitCode: 1, Denials: refusedChain, Note: "exit 1"},
		{Class: provider.TaskFailure, Note: "no rule matched"},
		{Class: provider.Timeout, TimedOut: true, Denials: refusedChain, Note: "killed"},
	} {
		o := reviewOrch(t, fake.Script{
			Events:  []provider.Event{{Kind: provider.EventMessage, Text: "partial thoughts"}},
			Outcome: out,
		})
		_, err := o.Discuss(fixedRoute("fake", "m")).Discuss(context.Background(), discussTurn(t))
		if err == nil || !strings.Contains(err.Error(), "the discussion failed") {
			t.Errorf("outcome %+v: err = %v, want the discussion to fail", out, err)
		}
	}
}

// The agent is told why a chain costs it the turn, as the planner already is.
func TestDiscussionPromptSteersAwayFromChainedCommands(t *testing.T) {
	p := discussionPrompt(discussTurn(t), "hi")
	if !strings.Contains(p, "simple command") || !strings.Contains(p, "gh") {
		t.Errorf("the discussion is not steered off chained and gh commands:\n%s", p)
	}
}
