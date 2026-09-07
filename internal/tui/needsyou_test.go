package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/bobbybrady/gravy/internal/api"
	"github.com/bobbybrady/gravy/internal/core"
)

// queueFixture holds one of every reason M0 can raise, plus one this build does not know about.
func queueFixture() *fakeService {
	f := newFake()
	f.status.Attention = []api.AttentionItem{
		{ // oldest
			Attention: core.Attention{ID: "a1", Reason: core.ReasonMergeConflict, TicketID: "aaa11111",
				Payload: map[string]any{
					"files":    []any{"calc.go", "calc_test.go"},
					"worktree": "/home/bobby/.gravy/projects/testrepo/worktrees/gravy-aaa1",
				}},
			Project: projGravy, Ticket: core.Ticket{ID: "aaa11111", Title: "Rebase fell over"},
			Age: 26 * time.Hour,
		},
		{
			Attention: core.Attention{ID: "a2", Reason: core.ReasonValidationFailed, TicketID: "bbb22222",
				Payload: map[string]any{"summary": "test  FAILED (exit 1)", "attempts": float64(2)}},
			Project: projMojo, Ticket: core.Ticket{ID: "bbb22222", Title: "Flaky auth middleware"},
			Age: 3 * time.Hour,
		},
		{
			Attention: core.Attention{ID: "a3", Reason: core.ReasonHostUnavailable, TicketID: "ccc33333",
				Payload: map[string]any{"reason": "the daemon stopped while this run was in flight",
					"detail": "the run's process was already gone"}},
			Project: projGravy, Ticket: core.Ticket{ID: "ccc33333", Title: "Interrupted work"},
			Age: 40 * time.Minute,
		},
		{
			Attention: core.Attention{ID: "a4", Reason: core.ReasonReviewPending, TicketID: "ddd44444",
				Payload: map[string]any{"commit": "604f7f5f1122", "summary": "test ok build ok"}},
			Project: projGravy, Ticket: core.Ticket{ID: "ddd44444", Title: "Add a Subtract function"},
			Age: 4 * time.Minute,
		},
		{ // newest, and a reason this build has never heard of
			Attention: core.Attention{ID: "a5", Reason: core.ReasonAgentQuestion, TicketID: "eee55555",
				Payload: map[string]any{"question": "Which database should I target?"}},
			Project: projMojo, Ticket: core.Ticket{ID: "eee55555", Title: "Add migrations"},
			Age: time.Minute,
		},
	}
	return f
}

func openQueue(t *testing.T, f *fakeService) Model {
	t.Helper()
	m := boot(t, f, 96, 30)
	m = send(t, m, key("6"))
	m = send(t, m, enteredMsg{})
	return m
}

// TestQueueIsOldestFirst is AC6: nothing starves.
func TestQueueIsOldestFirst(t *testing.T) {
	m := openQueue(t, queueFixture())
	view := m.View()

	order := []string{"merge_conflict", "validation_failed", "host_unavailable", "review_pending"}
	last := -1
	for _, r := range order {
		i := strings.Index(view, r)
		if i < 0 {
			t.Fatalf("%q is not in the queue:\n%s", r, view)
		}
		if i < last {
			t.Errorf("%q appears out of age order", r)
		}
		last = i
	}
}

// TestEveryM0ReasonHasItsOwnActions is AC1.
func TestEveryM0ReasonHasItsOwnActions(t *testing.T) {
	tests := []struct {
		name    string
		downs   int
		context []string
		actions []string
	}{
		{"merge_conflict", 0,
			[]string{"calc.go", "worktree is preserved"},
			[]string{"c retry the land", "x reject"}},
		{"validation_failed", 1,
			[]string{"FAILED (exit 1)", "after 2 attempt"},
			[]string{"r send back with guidance", "x reject"}},
		{"host_unavailable", 2,
			[]string{"daemon stopped while this run was in flight"},
			[]string{"a acknowledge"}},
		{"review_pending", 3,
			[]string{"commit 604f7f5f"},
			[]string{"enter open the review"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := openQueue(t, queueFixture())
			for i := 0; i < tc.downs; i++ {
				m = send(t, m, key("j"))
			}
			view := m.View()
			// AC5: enough context to act without leaving the screen.
			for _, want := range tc.context {
				if !strings.Contains(view, want) {
					t.Errorf("detail omits %q:\n%s", want, view)
				}
			}
			for _, want := range tc.actions {
				if !strings.Contains(view, want) {
					t.Errorf("actions omit %q:\n%s", want, view)
				}
			}
		})
	}
}

// TestUnknownReasonDegradesReadably is AC1's adversarial half. A queue that crashes on an
// unfamiliar row is worse than one that shows it plainly.
func TestUnknownReasonDegradesReadably(t *testing.T) {
	m := openQueue(t, queueFixture())
	for i := 0; i < 4; i++ {
		m = send(t, m, key("j"))
	}
	view := m.View() // must not panic

	if !strings.Contains(view, "agent_question") {
		t.Errorf("the unknown reason is not listed at all:\n%s", view)
	}
	if !strings.Contains(view, "no detail view for this reason") {
		t.Errorf("the unknown reason gives no explanation:\n%s", view)
	}
	// Its payload is still shown, because that is the only thing that can help.
	if !strings.Contains(view, "Which database") {
		t.Errorf("the unknown reason's payload is hidden:\n%s", view)
	}
	if !strings.Contains(view, "a acknowledge") {
		t.Errorf("the unknown reason offers no way out:\n%s", view)
	}
}

// TestMergeConflictContinueRetriesTheLand is AC7's actionable half.
func TestMergeConflictContinueRetriesTheLand(t *testing.T) {
	f := queueFixture()
	m := openQueue(t, f)

	m, cmd := sendCmd(t, m, key("c"))
	if cmd == nil {
		t.Fatal("c produced no command on a merge conflict")
	}
	m = send(t, m, cmd())

	if len(f.continued) != 1 || f.continued[0] != "aaa11111" {
		t.Errorf("continued = %v, want the conflicted ticket", f.continued)
	}
	if !strings.Contains(m.View(), "landing retried") {
		t.Errorf("the retry was not reported:\n%s", m.View())
	}
}

// TestSendBackCarriesGuidance covers validation_failed's main action.
func TestSendBackCarriesGuidance(t *testing.T) {
	f := queueFixture()
	m := openQueue(t, f)
	m = send(t, m, key("j")) // validation_failed

	m = send(t, m, key("r"))
	if !strings.Contains(m.View(), "what needs to change") {
		t.Fatalf("r did not prompt:\n%s", m.View())
	}
	for _, c := range "stub the clock" {
		m = send(t, m, key(string(c)))
	}
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter did not submit")
	}
	m = send(t, m, cmd())

	if got := f.changes["bbb22222"]; got != "stub the clock" {
		t.Errorf("feedback = %q, want it attached to the parked ticket", got)
	}
	if !strings.Contains(m.View(), "sent back to the agent") {
		t.Errorf("the outcome was not reported:\n%s", m.View())
	}
}

// TestAcknowledgeResolvesTheItem is AC4's action.
func TestAcknowledgeResolvesTheItem(t *testing.T) {
	f := queueFixture()
	m := openQueue(t, f)
	m = send(t, m, key("j"))
	m = send(t, m, key("j")) // host_unavailable

	m, cmd := sendCmd(t, m, key("a"))
	if cmd == nil {
		t.Fatal("a produced no command")
	}
	m = send(t, m, cmd())

	if len(f.resolved) != 1 || f.resolved[0] != "a3" {
		t.Errorf("resolved = %v, want the acknowledged item", f.resolved)
	}
	if !strings.Contains(m.View(), "acknowledged") {
		t.Errorf("the acknowledgement was not reported:\n%s", m.View())
	}
}

// TestReviewPendingJumpsToReview is AC1 for the commonest reason, and the link that makes the
// queue a starting point rather than a dead end.
func TestReviewPendingJumpsToReview(t *testing.T) {
	f := queueFixture()
	m := openQueue(t, f)
	for i := 0; i < 3; i++ {
		m = send(t, m, key("j"))
	}
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("enter produced no command on review_pending")
	}
	m = send(t, m, cmd())
	if !strings.Contains(m.View(), SectionReview.Title()) {
		t.Errorf("enter did not open the review screen:\n%s", m.View())
	}
}

// TestHeldQueueNamesTheBlockingTicket is AC8: an idle queue is never unexplained.
func TestHeldQueueNamesTheBlockingTicket(t *testing.T) {
	f := queueFixture()
	f.status.Projects = []api.ProjectStatus{{
		Project: projGravy,
		Blocked: "serialized; ddd44444 awaiting your review (review)",
	}}
	m := openQueue(t, f)
	view := m.View()

	if !strings.Contains(view, "Held queues") {
		t.Errorf("held queues are not surfaced:\n%s", view)
	}
	if !strings.Contains(view, "ddd44444") {
		t.Errorf("the blocking ticket is not named:\n%s", view)
	}
}

// TestEmptyQueueMakesTheClaim is the state the product is named for.
func TestEmptyQueueMakesTheClaim(t *testing.T) {
	m := openQueue(t, newFake())
	view := m.View()
	if !strings.Contains(view, "Nothing needs you") {
		t.Errorf("empty queue is not stated:\n%s", view)
	}
	if !strings.Contains(view, "Gravy does not need you") {
		t.Errorf("the empty queue does not make the product's claim:\n%s", view)
	}
}

// TestFilterNarrowsTheQueue covers filtering by reason and project through the frame's own `/`.
func TestFilterNarrowsTheQueue(t *testing.T) {
	m := openQueue(t, queueFixture())
	m = send(t, m, key("/"))
	for _, c := range "merge" {
		m = send(t, m, key(string(c)))
	}
	m = send(t, m, key("enter"))

	view := m.View()
	if !strings.Contains(view, "merge_conflict") {
		t.Errorf("the filter removed the matching row:\n%s", view)
	}
	if strings.Contains(view, "validation_failed") {
		t.Errorf("the filter kept a non-matching row:\n%s", view)
	}
	if !strings.Contains(view, "Needs You (1)") {
		t.Errorf("the count does not follow the filter:\n%s", view)
	}
}
