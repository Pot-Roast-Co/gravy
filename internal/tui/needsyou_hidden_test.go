package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

func attentionFor(project core.Project, ticketID string) api.AttentionItem {
	return api.AttentionItem{
		Attention: core.Attention{ID: "a1", Reason: core.ReasonReviewPending, TicketID: ticketID},
		Project:   project,
		Ticket:    core.Ticket{ID: ticketID, Title: "Discuss and agree on review changes"},
	}
}

// TestNeedsYouDoesNotClaimNothingWhileHiding is the regression.
//
// The status bar counts the whole fleet; this screen filters by project and by search. When they
// disagreed the screen said "Nothing needs you. If it is not here, Gravy does not need you."
// while the status bar said "needs you 1", and the human went looking for work the screen was
// hiding from them.
func TestNeedsYouDoesNotClaimNothingWhileHiding(t *testing.T) {
	s := &needsYou{}
	other := core.Project{ID: "p2", Name: "mission-mojo", Slug: "mission-mojo"}
	ctx := ViewContext{
		Theme: DefaultTheme(), Width: 100, Height: 24,
		Project: "gravy", // scoped elsewhere than the item
		Status:  api.SystemStatus{Attention: []api.AttentionItem{attentionFor(other, "79cbc6e4")}},
	}

	view := s.View(ctx)
	if strings.Contains(view, "Nothing needs you.") {
		t.Errorf("claimed nothing needs you while hiding an item:\n%s", view)
	}
	if !strings.Contains(view, "hidden") {
		t.Errorf("did not say anything was hidden:\n%s", view)
	}
	if !strings.Contains(view, "gravy") {
		t.Errorf("did not name the scope doing the hiding:\n%s", view)
	}
}

// A text filter hides items too, and the way to clear it belongs on the screen.
func TestNeedsYouNamesTheFilterThatIsHiding(t *testing.T) {
	s := &needsYou{}
	gravy := core.Project{ID: "p1", Name: "gravy", Slug: "gravy"}
	ctx := ViewContext{
		Theme: DefaultTheme(), Width: 100, Height: 24,
		Filter: "zzz-matches-nothing",
		Status: api.SystemStatus{Attention: []api.AttentionItem{attentionFor(gravy, "79cbc6e4")}},
	}

	view := s.View(ctx)
	if strings.Contains(view, "Nothing needs you.") {
		t.Errorf("claimed nothing needs you while a filter hid an item:\n%s", view)
	}
	if !strings.Contains(view, "esc") {
		t.Errorf("did not say how to clear the filter:\n%s", view)
	}
}

// With genuinely nothing open, the reassuring message is the right one and stays.
func TestNeedsYouStillSaysNothingWhenThereIsNothing(t *testing.T) {
	s := &needsYou{}
	ctx := ViewContext{Theme: DefaultTheme(), Width: 100, Height: 24, Project: "gravy"}

	view := s.View(ctx)
	if !strings.Contains(view, "Nothing needs you.") {
		t.Errorf("lost the empty state when there is truly nothing:\n%s", view)
	}
	if strings.Contains(view, "hidden") {
		t.Errorf("invented hidden work:\n%s", view)
	}
}

func TestPluralReadsCorrectly(t *testing.T) {
	if got := plural(1, "item is", "items are"); got != "1 item is" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(3, "item is", "items are"); got != "3 items are" {
		t.Errorf("plural(3) = %q", got)
	}
}
