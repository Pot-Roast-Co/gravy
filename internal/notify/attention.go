package notify

import (
	"fmt"

	"github.com/pot-roast-co/gravy/internal/core"
)

// ForAttention renders the notification for one attention item.
//
// It lives here rather than at the call sites because attention is raised from the orchestrator,
// the lander and the daemon, and three copies of this wording would drift. The body names the
// project and the ticket: an alert that says only "Gravy needs you" makes the human open the TUI
// to find out which repository stopped, which is the work the alert was meant to save.
func ForAttention(reason core.AttentionReason, project, ticket string) (title, body string, urgency Urgency) {
	subject := ticket
	if project != "" && ticket != "" {
		subject = project + " — " + ticket
	} else if project != "" {
		subject = project
	}

	switch reason {
	case core.ReasonReviewPending:
		// The ordinary case, and the one that matters most under the serial default: a ticket
		// in Review holds its whole repository until it is approved.
		return "Gravy · ready for review", subject, Normal

	case core.ReasonAgentQuestion:
		return "Gravy · an agent is asking", subject, Normal

	case core.ReasonPermissionReq:
		// The agent is stopped dead until this is answered.
		return "Gravy · permission needed", subject, Critical

	case core.ReasonValidationFailed:
		return "Gravy · validation failed", subject, Critical

	case core.ReasonMergeConflict:
		return "Gravy · merge conflict", subject, Critical

	case core.ReasonProviderAuth:
		// Nothing will run until the human re-authenticates, so this outranks a single
		// ticket's problem.
		return "Gravy · agent needs signing in", subject, Critical

	case core.ReasonTicketCritique:
		// A suggestion, not a blockage. It can wait for the next time the TUI is open.
		return "Gravy · ticket suggestions", subject, Low

	case core.ReasonHostUnavailable:
		return "Gravy · host unavailable", subject, Critical
	}

	// An unknown reason still tells the human something stopped, rather than staying silent
	// because this switch has not caught up with a new one.
	return "Gravy needs you", fmt.Sprintf("%s (%s)", subject, reason), Normal
}
