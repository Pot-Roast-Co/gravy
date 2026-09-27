package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pot-roast-co/gravy/internal/core"
)

// The table is still ticket_progress and the kind column is still phase: the history grew out of
// the progress journal, and renaming either would buy nothing but a migration that rewrites
// every row.
const activityColumns = `id, ticket_id, run_id, at, phase, actor, detail, payload`

// AddActivity appends one entry to a ticket's history.
//
// Append-only: an entry records what happened at a moment, and a later moment does not make an
// earlier one untrue. Nothing updates or deletes one.
func (d *DB) AddActivity(ctx context.Context, a core.Activity) error {
	if a.ID == "" {
		return fmt.Errorf("add activity for ticket %q: no id", a.TicketID)
	}
	actor := a.Actor
	if actor == "" {
		actor = core.ActorGravy
	}
	payload := "{}"
	if len(a.Payload) > 0 {
		var err error
		if payload, err = marshalJSON(a.Payload); err != nil {
			return fmt.Errorf("add activity for ticket %q: %w", a.TicketID, err)
		}
	}
	_, err := d.exec(ctx, `INSERT INTO ticket_progress (`+activityColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.TicketID, nullString(a.RunID), unixOrZero(a.At), string(a.Kind), actor, a.Detail, payload)
	if err != nil {
		return fmt.Errorf("add activity for ticket %q: %w", a.TicketID, err)
	}
	return nil
}

// ListHistory returns a ticket's history, oldest first.
//
// Ordered by rowid within a second: timestamps are stored in whole seconds like every other time
// in this schema, and a run narrates several steps faster than that. Insertion order is the
// order they happened, because a ticket has one writer at a time: the human acting on it, or the
// orchestrator running it.
func (d *DB) ListHistory(ctx context.Context, ticketID string) ([]core.Activity, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+activityColumns+` FROM ticket_progress
		 WHERE ticket_id = ? ORDER BY at ASC, rowid ASC`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("list history for ticket %q: %w", ticketID, err)
	}
	defer rows.Close()

	var out []core.Activity
	for rows.Next() {
		a, err := scanActivity(rows)
		if err != nil {
			return nil, fmt.Errorf("list history for ticket %q: %w", ticketID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list history for ticket %q: %w", ticketID, err)
	}
	return out, nil
}

// LatestActivity returns the newest entry for a ticket.
//
// Its own query rather than the tail of ListHistory: the dashboard reads this for every running
// ticket on every refresh, and reading a whole run's history to show one line would grow with
// the length of the run.
//
// ErrNotFound means the ticket has no history yet, which is the ordinary case for a ticket that
// was assigned a moment ago rather than a failure.
func (d *DB) LatestActivity(ctx context.Context, ticketID string) (core.Activity, error) {
	row := d.sql.QueryRowContext(ctx,
		`SELECT `+activityColumns+` FROM ticket_progress
		 WHERE ticket_id = ? ORDER BY at DESC, rowid DESC LIMIT 1`, ticketID)
	a, err := scanActivity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Activity{}, notFound("activity for ticket", ticketID)
	}
	return a, err
}

func scanActivity(s scanner) (core.Activity, error) {
	var (
		a       core.Activity
		runID   sql.NullString
		kind    string
		payload string
		at      int64
	)
	if err := s.Scan(&a.ID, &a.TicketID, &runID, &at, &kind, &a.Actor, &a.Detail, &payload); err != nil {
		return core.Activity{}, err
	}
	a.RunID = runID.String
	a.Kind = core.ActivityKind(kind)
	a.At = timeOrZero(at)
	if err := unmarshalJSON(payload, &a.Payload); err != nil {
		return core.Activity{}, err
	}
	if a.Payload == nil {
		a.Payload = map[string]any{}
	}
	return a, nil
}
