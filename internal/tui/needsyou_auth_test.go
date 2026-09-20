package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

func authQueue() *fakeService {
	f := newFake()
	f.status.Attention = []api.AttentionItem{{
		Attention: core.Attention{
			ID: "a1", Reason: core.ReasonProviderAuth, TicketID: "e00665fd",
			Payload: map[string]any{"class": "auth_expired", "model": "opus"},
		},
		Project: projGravy,
		Ticket:  core.Ticket{ID: "e00665fd", Title: "Record a per-ticket progress journal"},
	}}
	return f
}

// TestExpiredLoginSaysWhatToDo: an expired login used to fall through to the generic row, whose
// detail reads "this version of Gravy has no detail view for this reason" and whose only action
// was acknowledge.
func TestExpiredLoginSaysWhatToDo(t *testing.T) {
	m := openQueue(t, authQueue())
	view := m.View()

	if strings.Contains(view, "no detail view") {
		t.Errorf("an expired login still renders as an unknown reason:\n%s", view)
	}
	for _, want := range []string{"login has expired", "claude", "t try again"} {
		if !strings.Contains(view, want) {
			t.Errorf("the row omits %q:\n%s", want, view)
		}
	}
}

// TestRetryRequeuesTheTicket: signing in and pressing t is the way out, and it puts the work back
// in the queue rather than only dismissing the row.
func TestRetryRequeuesTheTicket(t *testing.T) {
	f := authQueue()
	m := openQueue(t, f)

	_, cmd := sendCmd(t, m, key("t"))
	if cmd == nil {
		t.Fatal("t produced no command on an expired login")
	}
	cmd()

	if len(f.requeued) != 1 || f.requeued[0] != "e00665fd" {
		t.Fatalf("requeued = %v, want the parked ticket", f.requeued)
	}
}

// Acknowledge is not offered here: it resolves the row and leaves the ticket parked, which is
// how this work became unreachable in the first place.
func TestExpiredLoginDoesNotOfferAcknowledgeAlone(t *testing.T) {
	view := openQueue(t, authQueue()).View()
	if strings.Contains(view, "a acknowledge") {
		t.Errorf("acknowledge is offered for a reason that would strand the ticket:\n%s", view)
	}
}

// A reason this build does not know about still parks a ticket, so it gets a way out too.
func TestUnknownReasonCanStillBeRetried(t *testing.T) {
	f := newFake()
	f.status.Attention = []api.AttentionItem{{
		Attention: core.Attention{ID: "a1", Reason: core.AttentionReason("from_the_future"), TicketID: "x1"},
		Project:   projGravy,
		Ticket:    core.Ticket{ID: "x1", Title: "something new"},
	}}

	view := openQueue(t, f).View()
	if !strings.Contains(view, "t try again") {
		t.Errorf("an unknown reason offers no way to put the work back:\n%s", view)
	}
}
