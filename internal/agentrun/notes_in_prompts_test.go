package agentrun

import (
	"context"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

const projectNotes = "A fantasy hockey draft assistant. Start with the draft board; scoring comes later."

// TestTicketPromptCarriesTheProjectNotes is half the regression.
//
// Project notes were written to the database, read back, and shown in the TUI — and never put in
// front of an agent. A ticket body says what to change; the notes are the only place that says
// what the thing being changed is meant to be.
func TestTicketPromptCarriesTheProjectNotes(t *testing.T) {
	got, err := SimplePrompt{}.Build(context.Background(),
		core.Ticket{ID: "GR-1", Title: "Add a draft board"},
		core.Project{Slug: "fantasyhockeyaid", Notes: projectNotes},
		Attempt{Number: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "draft board; scoring comes later") {
		t.Fatalf("the prompt never mentions what the project is for:\n%s", got)
	}
}

// A project with no notes gets no empty heading, which would read as "nobody said".
func TestTicketPromptWithoutNotesSaysNothing(t *testing.T) {
	got, err := SimplePrompt{}.Build(context.Background(),
		core.Ticket{ID: "GR-1", Title: "Add a draft board"},
		core.Project{Slug: "gravy"}, Attempt{Number: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "What this project is for") {
		t.Errorf("empty notes produced a heading:\n%s", got)
	}
}

// TestPlanPromptCarriesTheProjectNotes is the other half, and the one that prompted this.
//
// A project with no repository has no documents and an empty backlog, so the planner was working
// from the project name alone — which is why it could not see the note the human had written
// about what they wanted to build.
func TestPlanPromptCarriesTheProjectNotes(t *testing.T) {
	o := New(nil, nil, nil, nil, nil, Config{}, func() string { return "run-1" })
	got := o.Plan(nil, 0).prompt(host.NewLocal("local", 1),
		core.Project{Slug: "fantasyhockeyaid", Name: "fantasyHockeyAid", Notes: projectNotes},
		nil, "I want to make this, can you see the note")

	if !strings.Contains(got, "draft board; scoring comes later") {
		t.Fatalf("the planner is still working from the project name alone:\n%s", got)
	}
	// The question still arrives, and after the context rather than buried before it.
	if !strings.Contains(got, "can you see the note") {
		t.Fatal("the planner lost the human's question")
	}
	if strings.Index(got, "draft board; scoring") > strings.Index(got, "can you see the note") {
		t.Error("notes landed after the request; context belongs before the ask")
	}
}
