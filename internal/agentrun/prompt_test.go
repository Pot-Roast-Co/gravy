package agentrun_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bobbybrady/gravy/internal/agentrun"
	"github.com/bobbybrady/gravy/internal/core"
)

// TestFeedbackReachesTheNextAttempt is the half of "request changes" that matters.
//
// Sending work back records a note; if that note does not reach the agent, the retry is the same
// prompt that already failed, and it will usually produce the same output.
func TestFeedbackReachesTheNextAttempt(t *testing.T) {
	ticket := core.Ticket{
		ID: "GR-1", Title: "Add Multiply", Body: "with a table-driven test",
		Feedback: "handle integer overflow explicitly",
	}
	project := core.Project{
		Slug:       "proj",
		Validation: []core.Step{{Name: "test", Cmd: "go test ./...", Required: true}},
	}

	got, err := agentrun.SimplePrompt{}.Build(context.Background(), ticket, project, agentrun.Attempt{Number: 2})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "handle integer overflow explicitly") {
		t.Errorf("the reviewer's note is not in the prompt:\n%s", got)
	}
	if !strings.Contains(got, "reviewer sent this back") {
		t.Errorf("the note is not framed as review feedback:\n%s", got)
	}
}

// TestNoFeedbackNoSection keeps a first attempt's prompt clean.
func TestNoFeedbackNoSection(t *testing.T) {
	got, err := agentrun.SimplePrompt{}.Build(context.Background(),
		core.Ticket{ID: "GR-1", Title: "Add Multiply"}, core.Project{}, agentrun.Attempt{Number: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "reviewer sent this back") {
		t.Errorf("a ticket with no feedback got a feedback section:\n%s", got)
	}
}
