package agentrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/bobbybrady/gravy/internal/core"
)

// SimplePrompt is the M0 context builder: the ticket, the project's conventions, and the
// previous attempt's failure.
//
// GR-015 replaces it with the real thing — capped excerpts of project documentation and the
// result summaries of dependency tickets, under a token budget. It sits behind PromptBuilder so
// that swap is one line rather than a rewrite of the orchestrator.
type SimplePrompt struct{}

// Build assembles the prompt for one attempt.
func (SimplePrompt) Build(_ context.Context, t core.Ticket, p core.Project, attempt Attempt) (string, error) {
	var b strings.Builder

	b.WriteString("You are implementing one ticket in an existing repository.\n\n")
	fmt.Fprintf(&b, "## Ticket %s: %s\n\n", t.ID, t.Title)
	if strings.TrimSpace(t.Body) != "" {
		b.WriteString(t.Body)
		b.WriteString("\n\n")
	}

	b.WriteString("## Working agreement\n\n")
	b.WriteString("- You are working in an isolated git worktree. Everything you need is here.\n")
	b.WriteString("- Make the change the ticket asks for, and nothing else.\n")
	b.WriteString("- Do not commit; that is handled for you.\n")

	if len(p.Validation) > 0 {
		b.WriteString("- Your work is validated by these commands, which must pass:\n")
		for _, s := range p.Validation {
			required := "required"
			if !s.Required {
				required = "advisory"
			}
			fmt.Fprintf(&b, "    - %s: `%s` (%s)\n", s.Name, s.Cmd, required)
		}
	}

	// The failure from the previous attempt is the entire value of a retry: an agent asked to
	// try again with no new information will usually produce the same output.
	if attempt.Number > 1 && strings.TrimSpace(attempt.PriorFailure) != "" {
		fmt.Fprintf(&b, "\n## Attempt %d — the previous attempt failed\n\n", attempt.Number)
		b.WriteString("Fix the cause rather than working around it:\n\n```\n")
		b.WriteString(attempt.PriorFailure)
		b.WriteString("\n```\n")
	}

	return b.String(), nil
}

var _ PromptBuilder = SimplePrompt{}
