package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// recordHuman appends a human's action to a ticket's history.
//
// Written after the action has happened, never before: a row saying a ticket was approved when
// the approval was refused is a history that lies. And best effort, for the same reason the
// orchestrator's narration is — the action has already been taken, and failing it because its
// record could not be written would undo something the human did in order to keep a note of it.
// The write drops the caller's cancellation so a client hanging up mid-reply cannot cost the row.
//
// Recording is all it does. It never opens attention and never touches the ticket's state.
func (l *Local) recordHuman(ctx context.Context, ticketID, runID string, kind core.ActivityKind, payload map[string]any, format string, args ...any) {
	a := core.Activity{
		ID:       l.newID(),
		TicketID: ticketID,
		RunID:    runID,
		At:       l.now(),
		Kind:     kind,
		Actor:    core.ActorHuman,
		Detail:   fmt.Sprintf(format, args...),
		Payload:  payload,
	}
	if err := l.db.AddActivity(context.WithoutCancel(ctx), a); err != nil {
		slog.Warn("could not record history", "ticket", ticketID, "kind", kind, "error", err)
	}
}

// legacyProgress is a history entry in the wire shape ListProgress had before the journal became
// the history. A client built then decodes Phase and nothing else tells it what the row was, so
// the alias must still send it; Kind, Actor and Payload ride along for a decoder that ignores
// fields it does not know.
type legacyProgress struct {
	ID       string
	TicketID string
	RunID    string
	At       time.Time
	Phase    core.ActivityKind
	Detail   string
	Kind     core.ActivityKind
	Actor    string
	Payload  map[string]any
}

// asLegacyProgress answers the ListProgress alias. Every row goes through, new kinds included —
// an old client shows an unfamiliar phase as text, which beats a history with holes in it.
func asLegacyProgress(rows []core.Activity) []legacyProgress {
	out := make([]legacyProgress, len(rows))
	for i, a := range rows {
		out[i] = legacyProgress{
			ID: a.ID, TicketID: a.TicketID, RunID: a.RunID, At: a.At, Phase: a.Kind,
			Detail: a.Detail, Kind: a.Kind, Actor: a.Actor, Payload: a.Payload,
		}
	}
	return out
}

// flattenLine collapses a human's note onto one capped line, so the entry stays one sentence
// however long the note was. The payload keeps it whole.
func flattenLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// moveKind names the history entry for a ticket moved by hand.
//
// Most moves have a kind of their own. The rest — a ticket withdrawn to the backlog, a draft
// submitted — are still a human's decision about the ticket, and are recorded as a move naming
// the event rather than being left out.
func moveKind(ev core.Event) (core.ActivityKind, string) {
	switch ev {
	case core.EventMarkReady:
		return core.KindQueued, "queued"
	case core.EventReject:
		return core.KindRejected, "rejected"
	case core.EventRequeue:
		return core.KindRequeued, "requeued"
	case core.EventRequestChanges:
		return core.KindChangesRequested, "sent back for changes"
	case core.EventKill:
		return core.KindKilled, "killed"
	default:
		return core.KindMoved, "moved by " + string(ev)
	}
}
