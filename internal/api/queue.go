package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/pot-roast-co/gravy/internal/core"
)

// TicketDetail is a ticket with what the queue screens need to show and order it.
type TicketDetail struct {
	Ticket  core.Ticket
	Project core.Project
	// DependsOn are the tickets that must reach Done first, with their current state, so a
	// blocked ticket can say what it is waiting for rather than just that it is waiting.
	DependsOn []core.Ticket
	// Blocked names why this ticket cannot become Ready yet. Empty means it can.
	Blocked string
}

// ListQueue returns tickets matching the filter, with their dependencies resolved.
func (l *Local) ListQueue(ctx context.Context, f TicketFilter) ([]TicketDetail, error) {
	tickets, err := l.ListTickets(ctx, f)
	if err != nil {
		return nil, err
	}

	projects := map[string]core.Project{}
	out := make([]TicketDetail, 0, len(tickets))
	for _, t := range tickets {
		d := TicketDetail{Ticket: t}

		p, ok := projects[t.ProjectID]
		if !ok {
			if p, err = l.db.GetProject(ctx, t.ProjectID); err != nil {
				return nil, err
			}
			projects[t.ProjectID] = p
		}
		d.Project = p

		deps, err := l.dependencies(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		d.DependsOn = deps
		d.Blocked = blockedBy(deps)
		out = append(out, d)
	}
	return out, nil
}

func (l *Local) dependencies(ctx context.Context, ticketID string) ([]core.Ticket, error) {
	ids, err := l.db.DepsOf(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	out := make([]core.Ticket, 0, len(ids))
	for _, id := range ids {
		dep, err := l.db.GetTicket(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, nil
}

// blockedBy names the unlanded dependencies, or returns empty when there are none.
func blockedBy(deps []core.Ticket) string {
	var waiting []string
	for _, d := range deps {
		if d.State != core.StateDone {
			waiting = append(waiting, fmt.Sprintf("%s (%s)", d.ID, d.State))
		}
	}
	if len(waiting) == 0 {
		return ""
	}
	return "waiting on " + strings.Join(waiting, ", ")
}

// UpdateTicket saves an edited ticket.
//
// It writes the editable fields only. State is moved through MoveTicket and position through
// ReorderTicket, so an edit cannot smuggle a transition past the state machine.
func (l *Local) UpdateTicket(ctx context.Context, t core.Ticket) error {
	current, err := l.db.GetTicket(ctx, t.ID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(t.Title) == "" {
		return fmt.Errorf("a ticket needs a title")
	}
	if t.Route != "" && !t.Route.Valid() {
		return fmt.Errorf("route %q is not one of %v", t.Route, core.AllRoutes)
	}

	current.Title = t.Title
	current.Body = t.Body
	current.Priority = t.Priority
	if t.Route != "" {
		current.Route = t.Route
	}
	if err := l.db.UpdateTicket(ctx, current); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventTicketChanged, ProjectID: current.ProjectID, TicketID: current.ID})
	return nil
}

// ReorderTicket moves a ticket between two neighbours.
func (l *Local) ReorderTicket(ctx context.Context, id, before, after string) error {
	if err := l.db.ReorderTicket(ctx, id, before, after); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventTicketChanged, TicketID: id})
	return nil
}

// DeleteTicket removes a ticket outright.
//
// This is not rejection: a rejected ticket is a decision worth keeping. Delete is for work that
// should never have been written down, which is why only Draft and Backlog tickets can be
// deleted — anything further along has a run, a worktree or a branch behind it.
func (l *Local) DeleteTicket(ctx context.Context, id string) error {
	t, err := l.db.GetTicket(ctx, id)
	if err != nil {
		return err
	}
	if t.State != core.StateBacklog && t.State != core.StateDraft {
		return fmt.Errorf("ticket %s is %s: reject it rather than deleting it", id, t.State)
	}
	if err := l.db.DeleteTicket(ctx, id); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventTicketChanged, ProjectID: t.ProjectID, TicketID: id})
	return nil
}
