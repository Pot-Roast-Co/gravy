package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
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
	m = send(t, m, key(SectionBacklog.Key()))
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

func TestReadySendToBacklog(t *testing.T) {
	f := backlogFixture()
	m := boot(t, f, 120, 28)
	m = send(t, m, key(SectionReady.Key()))
	m = send(t, m, queueLoadedMsg{state: core.StateReady, items: f.queue})
	if !strings.Contains(m.View(), "space send to backlog") {
		t.Fatal(m.View())
	}
	m = send(t, m, key("x"))
	m = send(t, m, key("j"))
	m = send(t, m, key("x"))
	m, cmd := sendCmd(t, m, key(" "))
	if cmd == nil {
		t.Fatal("space produced no command")
	}
	m = send(t, m, cmd())
	if len(f.moved) != 2 {
		t.Fatalf("moved: %v", f.moved)
	}
	for _, mv := range f.moved {
		if mv[1] != string(core.EventReturnToBacklog) {
			t.Fatalf("move: %v", mv)
		}
	}
	if !strings.Contains(m.View(), "sent to backlog 2") {
		t.Fatal(m.View())
	}
}

func TestReadyRejectSelection(t *testing.T) {
	for _, binding := range []string{"r", "D"} {
		t.Run(binding, func(t *testing.T) {
			f := backlogFixture()
			m := boot(t, f, 140, 28)
			m = send(t, m, key(SectionReady.Key()))
			m = send(t, m, queueLoadedMsg{state: core.StateReady, items: f.queue})
			if !strings.Contains(m.View(), "r reject") || strings.Contains(m.View(), "D delete") {
				t.Fatal(m.View())
			}
			m = send(t, m, key("x"))
			m = send(t, m, key("j"))
			m = send(t, m, key("x"))
			m, cmd := sendCmd(t, m, key(binding))
			if cmd != nil || !strings.Contains(m.View(), "reject 2 selected tickets") {
				t.Fatal(m.View())
			}
			m, cmd = sendCmd(t, m, key("n"))
			if cmd != nil || len(f.rejected) != 0 {
				t.Fatal("cancel rejected tickets")
			}
			m = send(t, m, key(binding))
			m, cmd = sendCmd(t, m, key("y"))
			if cmd == nil {
				t.Fatal("confirmation produced no command")
			}
			m = send(t, m, cmd())
			if strings.Join(f.rejected, ",") != "t1,t2" || len(f.deleted) != 0 {
				t.Fatalf("rejected %v, deleted %v", f.rejected, f.deleted)
			}
			if !strings.Contains(m.View(), "rejected 2") {
				t.Fatal(m.View())
			}
		})
	}
}

// TestQueueNoticeDoesNotSquatOnTheFooter is what a stuck "saved" costs.
//
// The footer is where the keys are documented, so a confirmation left sitting in it means the
// screen stops telling you what you can do — and right after saving a ticket, the next thing you
// want is how to queue it.
func TestQueueNoticeDoesNotSquatOnTheFooter(t *testing.T) {
	f := populated()
	m := boot(t, f, 100, 30)
	m = send(t, m, key(SectionBacklog.Key()))

	// A finished action leaves its confirmation.
	m = send(t, m, queueDoneMsg{verb: "saved"})
	if !strings.Contains(m.View(), "saved") {
		t.Fatal("the confirmation was never shown")
	}
	if !strings.Contains(m.View(), "space queue it") {
		t.Fatal("the notice hid the available actions")
	}

	// The next key restores the keys.
	m = send(t, m, key("j"))
	view := m.View()
	if strings.Contains(view, "saved") {
		t.Errorf("the confirmation outlived the key that followed it:\n%s", view)
	}
	if !strings.Contains(view, "space queue it") {
		t.Errorf("the footer does not say how to queue a ticket:\n%s", view)
	}
}

func TestManualTicketPasteAndProjectSelection(t *testing.T) {
	f := backlogFixture()
	second := core.Project{ID: "second", Slug: "second", Name: "Second"}
	f.status.Projects = append(f.status.Projects, api.ProjectStatus{Project: second})
	m := openBacklog(t, f)
	m = send(t, m, key("n"))
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Add café☕")})
	m = send(t, m, key("backspace"))
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd != nil {
		t.Fatal("created without choosing a project")
	}
	if !strings.Contains(m.View(), "Add café") {
		t.Fatal("lost form on validation error")
	}
	for i := 0; i < 3; i++ {
		m = send(t, m, key("tab"))
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(projectName(second))})
	m, cmd = sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal(m.View())
	}
	send(t, m, cmd())
	if len(f.created) != 1 || f.created[0].Title != "Add café" || f.created[0].ProjectID != "second" || f.created[0].Ready {
		t.Fatalf("%+v", f.created)
	}
}
