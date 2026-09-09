package notify

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

func TestForAttention(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reason  core.AttentionReason
		project string
		ticket  string
		wantIn  string
		urgency Urgency
	}{
		{
			name:   "review is the ordinary case",
			reason: core.ReasonReviewPending, project: "mission-mojo", ticket: "Close the re-parenting hole",
			wantIn: "review", urgency: Normal,
		},
		{
			// The agent is stopped dead until this is answered.
			name:   "permission stops an agent",
			reason: core.ReasonPermissionReq, project: "gravy", ticket: "add mul",
			wantIn: "permission", urgency: Critical,
		},
		{
			// Nothing runs until the human signs in, which outranks one ticket's problem.
			name:   "auth stops everything",
			reason: core.ReasonProviderAuth, project: "gravy", ticket: "add mul",
			wantIn: "signing in", urgency: Critical,
		},
		{
			// A suggestion, not a blockage.
			name:   "critique can wait",
			reason: core.ReasonTicketCritique, project: "gravy", ticket: "add mul",
			wantIn: "suggestion", urgency: Low,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title, body, urgency := ForAttention(tc.reason, tc.project, tc.ticket)
			if !strings.Contains(strings.ToLower(title), tc.wantIn) {
				t.Errorf("title = %q, want it to mention %q", title, tc.wantIn)
			}
			if urgency != tc.urgency {
				t.Errorf("urgency = %v, want %v", urgency, tc.urgency)
			}
			// An alert that does not say which repository stopped makes the human open the
			// TUI to find out, which is the work the alert was meant to save.
			for _, want := range []string{tc.project, tc.ticket} {
				if !strings.Contains(body, want) {
					t.Errorf("body = %q, want it to name %q", body, want)
				}
			}
		})
	}
}

// TestForAttentionCoversEveryReason: a reason added without a case here still tells the human
// something stopped, rather than being silent because this switch has not caught up.
func TestForAttentionCoversEveryReason(t *testing.T) {
	for _, r := range core.AllAttentionReasons {
		title, body, _ := ForAttention(r, "proj", "ticket")
		if title == "" || body == "" {
			t.Errorf("reason %q produced an empty notification", r)
		}
	}
	title, body, _ := ForAttention(core.AttentionReason("something_new"), "proj", "ticket")
	if title == "" || !strings.Contains(body, "something_new") {
		t.Errorf("an unknown reason degraded to %q / %q", title, body)
	}
}

func TestForAttentionWithoutAProject(t *testing.T) {
	_, body, _ := ForAttention(core.ReasonHostUnavailable, "", "a stranded ticket")
	if body != "a stranded ticket" {
		t.Errorf("body = %q, want just the ticket", body)
	}
}
