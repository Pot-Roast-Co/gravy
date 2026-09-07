package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

func backlogFixture() *fakeService {
	f := newFake()
	f.status.Projects = []api.ProjectStatus{{Project: projGravy}}
	f.queue = []api.TicketDetail{
		{Ticket: core.Ticket{ID: "t1", Title: "Add Divide", Body: "handle the zero case", Position: 1024},
			Project: projGravy},
		{Ticket: core.Ticket{ID: "t2", Title: "Wire notifications", Body: "bell and OS", Position: 2048},
			Project: projGravy},
		{Ticket: core.Ticket{ID: "t3", Title: "Document the gate", Body: "merge semantics", Position: 3072},
			Project:   projGravy,
			DependsOn: []core.Ticket{{ID: "t1", State: core.StateBacklog}},
			Blocked:   "waiting on t1 (backlog)"},
	}
	return f
}

func openBacklog(t *testing.T, f *fakeService) Model {
	t.Helper()
	m := boot(t, f, 96, 28)
	m = send(t, m, key("2"))
	m = send(t, m, enteredMsg{})
	m = send(t, m, queueLoadedMsg{state: core.StateBacklog, items: f.queue})
	return m
}

// TestCreateLandsInBacklog is AC1: type and save, with the ticket in the backlog and nothing
// waiting on a round trip before the thought is recorded.
func TestCreateLandsInBacklog(t *testing.T) {
	f := backlogFixture()
	m := openBacklog(t, f)

	m = send(t, m, key("n"))
	if !strings.Contains(m.View(), "New ticket") {
		t.Fatalf("n did not open the form:\n%s", m.View())
	}
	for _, c := range "Add Modulo" {
		m = send(t, m, key(string(c)))
	}
	m = send(t, m, key("tab"))
	for _, c := range "with tests" {
		m = send(t, m, key(string(c)))
	}

	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter did not submit the form")
	}
	m = send(t, m, cmd())

	if len(f.created) != 1 {
		t.Fatalf("created = %+v, want one ticket", f.created)
	}
	got := f.created[0]
	if got.Title != "Add Modulo" || got.Body != "with tests" {
		t.Errorf("created %+v, want the typed title and body", got)
	}
	if got.Ready {
		t.Error("a new ticket went straight to Ready; it belongs in the backlog until queued")
	}
	if got.ProjectID != projGravy.ID {
		t.Errorf("project = %q, want the only registered project", got.ProjectID)
	}
	if !strings.Contains(m.View(), "created") {
		t.Errorf("the outcome was not reported:\n%s", m.View())
	}
}

// TestFormRefusesAnEmptyTitle keeps unusable rows out of the queue.
func TestFormRefusesAnEmptyTitle(t *testing.T) {
	f := backlogFixture()
	m := openBacklog(t, f)
	m = send(t, m, key("n"))

	m, cmd := sendCmd(t, m, key("enter"))
	if cmd != nil {
		t.Fatal("an empty ticket was submitted")
	}
	if !strings.Contains(m.View(), "needs a title") {
		t.Errorf("no explanation for the refusal:\n%s", m.View())
	}
	if len(f.created) != 0 {
		t.Errorf("created = %+v, want none", f.created)
	}
}

// TestReorderMovesExactlyOneRow is AC2. Fractional positions mean inserting between two tickets
// is a single row update, not a renumbering of the queue.
func TestReorderMovesExactlyOneRow(t *testing.T) {
	f := backlogFixture()
	m := openBacklog(t, f)
	m = send(t, m, key("j")) // t2

	m, cmd := sendCmd(t, m, key("K")) // move up
	if cmd == nil {
		t.Fatal("K produced no command")
	}
	m = send(t, m, cmd())

	if len(f.reordered) != 1 {
		t.Fatalf("reordered %d times, want exactly 1: %v", len(f.reordered), f.reordered)
	}
	got := f.reordered[0]
	if got[0] != "t2" {
		t.Errorf("moved %q, want t2", got[0])
	}
	// Moving up puts it above t1, which has no neighbour above it.
	if got[1] != "" || got[2] != "t1" {
		t.Errorf("neighbours = (%q, %q), want ('', 't1')", got[1], got[2])
	}
	if !strings.Contains(m.View(), "reordered") {
		t.Errorf("the reorder was not reported:\n%s", m.View())
	}
}

// TestBlockedTicketShowsWhatItWaitsFor is AC3's display half.
func TestBlockedTicketShowsWhatItWaitsFor(t *testing.T) {
	m := openBacklog(t, backlogFixture())
	view := m.View()
	if !strings.Contains(view, "waiting on t1") {
		t.Errorf("a dependency-blocked ticket does not say what it waits for:\n%s", view)
	}
}

// TestBlockedTicketCannotBeQueued is AC3's enforcement half, as the screen sees it: the refusal
// arrives with its reason rather than as a silent no-op.
func TestBlockedTicketCannotBeQueued(t *testing.T) {
	f := backlogFixture()
	f.moveErr = fmt.Errorf("cannot mark t3 ready: waiting on t1 (backlog)")
	m := openBacklog(t, f)
	for i := 0; i < 2; i++ {
		m = send(t, m, key("j")) // t3
	}

	m, cmd := sendCmd(t, m, key(" "))
	if cmd == nil {
		t.Fatal("space produced no command")
	}
	m = send(t, m, cmd())

	if !strings.Contains(m.View(), "waiting on t1") {
		t.Errorf("the refusal does not say why:\n%s", m.View())
	}
}

// TestBulkMoveOverASelection is AC4.
func TestBulkMoveOverASelection(t *testing.T) {
	f := backlogFixture()
	m := openBacklog(t, f)

	m = send(t, m, key("x")) // select t1
	m = send(t, m, key("j"))
	m = send(t, m, key("x")) // select t2
	if !strings.Contains(m.View(), "2 selected") {
		t.Errorf("the selection is not shown:\n%s", m.View())
	}

	m, cmd := sendCmd(t, m, key(" "))
	if cmd == nil {
		t.Fatal("space produced no command")
	}
	m = send(t, m, cmd())

	if len(f.moved) != 2 {
		t.Fatalf("moved %d tickets, want 2: %v", len(f.moved), f.moved)
	}
	for _, mv := range f.moved {
		if mv[1] != string(core.EventMarkReady) {
			t.Errorf("moved with %q, want mark_ready", mv[1])
		}
	}
	if !strings.Contains(m.View(), "queued 2 ticket") {
		t.Errorf("the bulk move was not reported:\n%s", m.View())
	}
}

// TestDeleteConfirmsFirst is AC5.
func TestDeleteConfirmsFirst(t *testing.T) {
	f := backlogFixture()
	m := openBacklog(t, f)

	m, cmd := sendCmd(t, m, key("D"))
	if cmd != nil {
		t.Fatal("D deleted with no confirmation")
	}
	if !strings.Contains(m.View(), "delete t1 permanently") {
		t.Fatalf("D did not confirm:\n%s", m.View())
	}
	m, cmd = sendCmd(t, m, key("n"))
	if cmd != nil || len(f.deleted) != 0 {
		t.Fatal("declining still deleted the ticket")
	}

	m = send(t, m, key("D"))
	m, cmd = sendCmd(t, m, key("y"))
	if cmd == nil {
		t.Fatal("confirming did not delete")
	}
	send(t, m, cmd())
	if len(f.deleted) != 1 || f.deleted[0] != "t1" {
		t.Errorf("deleted = %v, want t1", f.deleted)
	}
}

// TestFilterMatchesTitleAndBody is AC6. A queue you can only search by title is one you re-read
// instead of searching.
func TestFilterMatchesTitleAndBody(t *testing.T) {
	m := openBacklog(t, backlogFixture())

	m = send(t, m, key("/"))
	for _, c := range "zero case" { // only in t1's body
		m = send(t, m, key(string(c)))
	}
	m = send(t, m, key("enter"))

	view := m.View()
	if !strings.Contains(view, "Add Divide") {
		t.Errorf("the body match was filtered out:\n%s", view)
	}
	if strings.Contains(view, "Wire notifications") {
		t.Errorf("a non-matching ticket survived the filter:\n%s", view)
	}
	if !strings.Contains(view, "Backlog (1)") {
		t.Errorf("the count does not follow the filter:\n%s", view)
	}
}

// TestEditSavesTheChangedFields covers `e`.
func TestEditSavesTheChangedFields(t *testing.T) {
	f := backlogFixture()
	m := openBacklog(t, f)

	m = send(t, m, key("e"))
	if !strings.Contains(m.View(), "Edit t1") {
		t.Fatalf("e did not open the edit form:\n%s", m.View())
	}
	for i := 0; i < len("Add Divide"); i++ {
		m = send(t, m, key("backspace"))
	}
	for _, c := range "Add Divide safely" {
		m = send(t, m, key(string(c)))
	}
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter did not save")
	}
	send(t, m, cmd())

	if len(f.updated) != 1 || f.updated[0].Title != "Add Divide safely" {
		t.Errorf("updated = %+v, want the retyped title", f.updated)
	}
}

// TestReadyCannotGoBackwards: the state machine has no edge from Ready to Backlog, so the screen
// says so rather than appearing to do nothing.
func TestReadyCannotGoBackwards(t *testing.T) {
	f := backlogFixture()
	m := boot(t, f, 96, 28)
	m = send(t, m, key("3"))
	m = send(t, m, queueLoadedMsg{state: core.StateReady, items: f.queue})

	m, cmd := sendCmd(t, m, key(" "))
	if cmd == nil {
		t.Fatal("space produced no command")
	}
	m = send(t, m, cmd())

	if !strings.Contains(m.View(), "cannot go back") {
		t.Errorf("moving a Ready ticket back is not explained:\n%s", m.View())
	}
	if len(f.moved) != 0 {
		t.Errorf("a Ready ticket was moved anyway: %v", f.moved)
	}
}
