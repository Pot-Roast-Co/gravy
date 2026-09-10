package tui

import (
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

func TestNotificationDestinationUsesCurrentState(t *testing.T) {
	for _, state := range []core.State{core.StateReview, core.StateNeedsYou, core.StateBlocked} {
		st := api.SystemStatus{Attention: []api.AttentionItem{{Attention: core.Attention{TicketID: "ticket"}, Ticket: core.Ticket{State: state}}}}
		want := SectionNeedsYou
		if state == core.StateReview {
			want = SectionReview
		}
		if got := notificationDestination(st, "ticket"); got != want {
			t.Fatalf("%s: %v", state, got)
		}
	}
}

func TestDaemonSoundSuppressesClientBell(t *testing.T) {
	m := New(nil).WithDaemonSound()
	updated, _ := m.Update(bellSettingMsg{enabled: true})
	if updated.(Model).bell {
		t.Fatal("duplicate terminal bell enabled")
	}
}

func TestLiveNotificationNavigatesWithFreshState(t *testing.T) {
	m := New(nil)
	st := api.SystemStatus{Attention: []api.AttentionItem{{Attention: core.Attention{TicketID: "ticket"}, Ticket: core.Ticket{State: core.StateNeedsYou}}}}
	updated, cmd := m.Update(ticketDestinationMsg{id: "ticket", status: st})
	if len(updated.(Model).status.Attention) != 1 {
		t.Fatal("snapshot was not refreshed")
	}
	msg := cmd().(gotoMsg)
	if msg.section != SectionNeedsYou || msg.focus != "ticket" {
		t.Fatalf("wrong destination: %+v", msg)
	}
}
