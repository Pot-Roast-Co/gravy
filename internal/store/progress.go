package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pot-roast-co/gravy/internal/core"
)

const progressColumns = `id, ticket_id, run_id, at, phase, detail`

// AddProgress appends one entry to a ticket's journal.
//
// Append-only: an entry records what was happening at a moment, and a later moment does not make
// an earlier one untrue. Nothing updates or deletes one.
func (d *DB) AddProgress(ctx context.Context, p core.Progress) error {
	if p.ID == "" {
		return fmt.Errorf("add progress for ticket %q: no id", p.TicketID)
	}
	_, err := d.exec(ctx, `INSERT INTO ticket_progress (`+progressColumns+`) VALUES (?,?,?,?,?,?)`,
		p.ID, p.TicketID, nullString(p.RunID), unixOrZero(p.At), string(p.Phase), p.Detail)
	if err != nil {
		return fmt.Errorf("add progress for ticket %q: %w", p.TicketID, err)
	}
	return nil
}

// ListProgress returns a ticket's journal, oldest first.
//
// Ordered by rowid within a second: timestamps are stored in whole seconds like every other time
// in this schema, and a run narrates several phases faster than that. Insertion order is the
// order they happened, because the orchestrator is the only writer for a ticket in flight.
func (d *DB) ListProgress(ctx context.Context, ticketID string) ([]core.Progress, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+progressColumns+` FROM ticket_progress
		 WHERE ticket_id = ? ORDER BY at ASC, rowid ASC`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("list progress for ticket %q: %w", ticketID, err)
	}
	defer rows.Close()

	var out []core.Progress
	for rows.Next() {
		p, err := scanProgress(rows)
		if err != nil {
			return nil, fmt.Errorf("list progress for ticket %q: %w", ticketID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list progress for ticket %q: %w", ticketID, err)
	}
	return out, nil
}

// LatestProgress returns the newest entry for a ticket.
//
// Its own query rather than the tail of ListProgress: the dashboard reads this for every running
// ticket on every refresh, and reading a whole run's journal to show one line would grow with the
// length of the run.
//
// ErrNotFound means the ticket has no journal yet, which is the ordinary case for a ticket that
// was assigned a moment ago rather than a failure.
func (d *DB) LatestProgress(ctx context.Context, ticketID string) (core.Progress, error) {
	row := d.sql.QueryRowContext(ctx,
		`SELECT `+progressColumns+` FROM ticket_progress
		 WHERE ticket_id = ? ORDER BY at DESC, rowid DESC LIMIT 1`, ticketID)
	p, err := scanProgress(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Progress{}, notFound("progress for ticket", ticketID)
	}
	return p, err
}

func scanProgress(s scanner) (core.Progress, error) {
	var (
		p     core.Progress
		runID sql.NullString
		phase string
		at    int64
	)
	if err := s.Scan(&p.ID, &p.TicketID, &runID, &at, &phase, &p.Detail); err != nil {
		return core.Progress{}, err
	}
	p.RunID = runID.String
	p.Phase = core.ProgressPhase(phase)
	p.At = timeOrZero(at)
	return p, nil
}
