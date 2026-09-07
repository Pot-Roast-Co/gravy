package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pot-roast-co/gravy/internal/core"
)

const ticketColumns = `id, project_id, title, body, state, priority, position, route,
	requirements, host_override, worktree_path, branch, retry_count, feedback,
	created_at, updated_at`

// CreateTicket inserts a ticket. When Position is zero it is placed at the end of its project's
// queue.
func (d *DB) CreateTicket(ctx context.Context, t core.Ticket) error {
	req, err := marshalJSON(t.Requirements)
	if err != nil {
		return err
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	if t.Position == 0 {
		var max sql.NullFloat64
		if err := d.sql.QueryRowContext(ctx,
			`SELECT MAX(position) FROM tickets WHERE project_id = ?`, t.ProjectID,
		).Scan(&max); err != nil {
			return fmt.Errorf("create ticket %q: %w", t.ID, err)
		}
		t.Position = max.Float64 + positionGap
	}

	_, err = d.sql.ExecContext(ctx, `INSERT INTO tickets (`+ticketColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.ProjectID, t.Title, t.Body, string(t.State), t.Priority, t.Position,
		string(t.Route), req, nullString(t.HostOverride), nullString(t.WorktreePath),
		nullString(t.Branch), t.RetryCount, t.Feedback,
		unixOrZero(t.CreatedAt), unixOrZero(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("create ticket %q: %w", t.ID, err)
	}
	return nil
}

// positionGap is the spacing between appended tickets. Reordering averages neighbours, so a
// wide gap leaves plenty of room to subdivide before float precision becomes a concern.
const positionGap = 1024.0

// GetTicket returns a ticket by id.
func (d *DB) GetTicket(ctx context.Context, id string) (core.Ticket, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = ?`, id)
	t, err := scanTicket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Ticket{}, notFound("ticket", id)
	}
	return t, err
}

// ListTickets returns a project's tickets in queue order.
func (d *DB) ListTickets(ctx context.Context, projectID string) ([]core.Ticket, error) {
	return d.queryTickets(ctx,
		`SELECT `+ticketColumns+` FROM tickets WHERE project_id = ?
		 ORDER BY priority DESC, position ASC, created_at ASC`, projectID)
}

// ListTicketsByState returns every ticket in a state across all projects, in scheduling order.
// This is the query the scheduler runs each tick.
func (d *DB) ListTicketsByState(ctx context.Context, state core.State) ([]core.Ticket, error) {
	return d.queryTickets(ctx,
		`SELECT `+ticketColumns+` FROM tickets WHERE state = ?
		 ORDER BY priority DESC, position ASC, created_at ASC`, string(state))
}

// CountActiveTickets returns how many of a project's tickets are in flight.
//
// Serial mode consults this: a project is available only at zero. The states counted are
// core.IsActive's, listed here rather than inferred so the SQL and core cannot disagree —
// TestActiveStatesMatchCore pins them together.
func (d *DB) CountActiveTickets(ctx context.Context, projectID string) (int, error) {
	states := activeStates()
	args := make([]any, 0, len(states)+1)
	args = append(args, projectID)
	placeholders := ""
	for i, s := range states {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, string(s))
	}

	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tickets WHERE project_id = ? AND state IN (`+placeholders+`)`,
		args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active tickets for project %q: %w", projectID, err)
	}
	return n, nil
}

// activeStates returns the states core considers in flight.
func activeStates() []core.State {
	var out []core.State
	for _, s := range core.AllStates {
		if core.IsActive(s) {
			out = append(out, s)
		}
	}
	return out
}

// UpdateTicket replaces a ticket's mutable fields and bumps updated_at.
func (d *DB) UpdateTicket(ctx context.Context, t core.Ticket) error {
	req, err := marshalJSON(t.Requirements)
	if err != nil {
		return err
	}
	res, err := d.exec(ctx, `UPDATE tickets SET
		title=?, body=?, state=?, priority=?, position=?, route=?, requirements=?,
		host_override=?, worktree_path=?, branch=?, retry_count=?, feedback=?,
		updated_at=unixepoch()
		WHERE id=?`,
		t.Title, t.Body, string(t.State), t.Priority, t.Position, string(t.Route), req,
		nullString(t.HostOverride), nullString(t.WorktreePath), nullString(t.Branch),
		t.RetryCount, t.Feedback, t.ID)
	if err != nil {
		return fmt.Errorf("update ticket %q: %w", t.ID, err)
	}
	return requireOneRow(res, "ticket", t.ID)
}

// SetTicketState applies an event to a ticket through the core state machine and persists the
// result. An illegal transition is rejected before anything is written.
//
// Routing every state change through here is what keeps the transition table authoritative
// rather than advisory.
func (d *DB) SetTicketState(ctx context.Context, id string, ev core.Event) (core.State, error) {
	var next core.State
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var cur string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM tickets WHERE id = ?`, id).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return notFound("ticket", id)
			}
			return fmt.Errorf("read ticket %q state: %w", id, err)
		}
		to, err := core.Transition(core.State(cur), ev)
		if err != nil {
			return fmt.Errorf("ticket %q: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET state = ?, updated_at = unixepoch() WHERE id = ?`,
			string(to), id); err != nil {
			return fmt.Errorf("update ticket %q state: %w", id, err)
		}
		next = to
		return nil
	})
	return next, err
}

// DeleteTicket removes a ticket and, by cascade, its runs, summary and attention rows.
func (d *DB) DeleteTicket(ctx context.Context, id string) error {
	res, err := d.exec(ctx, `DELETE FROM tickets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete ticket %q: %w", id, err)
	}
	return requireOneRow(res, "ticket", id)
}

// ReorderTicket moves a ticket between two neighbours, identified by id. Either may be empty to
// mean "the end of the queue".
//
// Exactly one row is updated: the new position is the average of its neighbours', so inserting
// between two tickets never renumbers the queue.
func (d *DB) ReorderTicket(ctx context.Context, id, before, after string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tickets WHERE id = ?`, id).
			Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return notFound("ticket", id)
			}
			return fmt.Errorf("reorder ticket %q: %w", id, err)
		}

		lo, err := neighbourPosition(ctx, tx, before, projectID)
		if err != nil {
			return err
		}
		hi, err := neighbourPosition(ctx, tx, after, projectID)
		if err != nil {
			return err
		}

		var pos float64
		switch {
		case before == "" && after == "":
			return fmt.Errorf("reorder ticket %q: neither neighbour given", id)
		case before == "": // moving to the head, ahead of `after`
			pos = hi - positionGap
		case after == "": // moving to the tail, behind `before`
			pos = lo + positionGap
		default:
			if lo >= hi {
				return fmt.Errorf("reorder ticket %q: %q is not before %q", id, before, after)
			}
			pos = lo + (hi-lo)/2
			// Float64 has ~15 significant digits; a gap this small means the queue has been
			// subdivided thousands of times in one spot and needs renumbering, which is a
			// deliberate operation rather than something to do silently mid-reorder.
			if pos <= lo || pos >= hi {
				return fmt.Errorf("reorder ticket %q: no room between positions %v and %v", id, lo, hi)
			}
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE tickets SET position = ?, updated_at = unixepoch() WHERE id = ?`, pos, id)
		if err != nil {
			return fmt.Errorf("reorder ticket %q: %w", id, err)
		}
		return requireOneRow(res, "ticket", id)
	})
}

func neighbourPosition(ctx context.Context, tx *sql.Tx, id, projectID string) (float64, error) {
	if id == "" {
		return 0, nil
	}
	var pos float64
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT position, project_id FROM tickets WHERE id = ?`, id).
		Scan(&pos, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, notFound("ticket", id)
	}
	if err != nil {
		return 0, fmt.Errorf("read neighbour %q: %w", id, err)
	}
	if owner != projectID {
		return 0, fmt.Errorf("neighbour %q belongs to a different project", id)
	}
	return pos, nil
}

func (d *DB) queryTickets(ctx context.Context, query string, args ...any) ([]core.Ticket, error) {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	defer rows.Close()

	var out []core.Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	return out, nil
}

func scanTicket(s scanner) (core.Ticket, error) {
	var (
		t                              core.Ticket
		state, route, req              string
		hostOverride, worktree, branch sql.NullString
		createdAt, updatedAt           int64
	)
	if err := s.Scan(&t.ID, &t.ProjectID, &t.Title, &t.Body, &state, &t.Priority, &t.Position,
		&route, &req, &hostOverride, &worktree, &branch, &t.RetryCount, &t.Feedback,
		&createdAt, &updatedAt); err != nil {
		return core.Ticket{}, err
	}
	t.State = core.State(state)
	t.Route = core.Route(route)
	t.HostOverride = hostOverride.String
	t.WorktreePath = worktree.String
	t.Branch = branch.String
	t.CreatedAt = timeOrZero(createdAt)
	t.UpdatedAt = timeOrZero(updatedAt)

	if err := unmarshalJSON(req, &t.Requirements); err != nil {
		return core.Ticket{}, fmt.Errorf("ticket %q requirements: %w", t.ID, err)
	}
	return t, nil
}

// nullString stores an empty string as SQL NULL, matching the nullable columns in the schema.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AddDep records that ticket depends on dependsOn.
func (d *DB) AddDep(ctx context.Context, ticketID, dependsOn string) error {
	if ticketID == dependsOn {
		return fmt.Errorf("ticket %q cannot depend on itself", ticketID)
	}
	_, err := d.exec(ctx,
		`INSERT OR IGNORE INTO ticket_deps (ticket_id, depends_on) VALUES (?, ?)`,
		ticketID, dependsOn)
	if err != nil {
		return fmt.Errorf("add dependency %q -> %q: %w", ticketID, dependsOn, err)
	}
	return nil
}

// RemoveDep removes a dependency.
func (d *DB) RemoveDep(ctx context.Context, ticketID, dependsOn string) error {
	_, err := d.exec(ctx,
		`DELETE FROM ticket_deps WHERE ticket_id = ? AND depends_on = ?`, ticketID, dependsOn)
	if err != nil {
		return fmt.Errorf("remove dependency %q -> %q: %w", ticketID, dependsOn, err)
	}
	return nil
}

// DepsOf returns the ids a ticket depends on.
func (d *DB) DepsOf(ctx context.Context, ticketID string) ([]string, error) {
	return d.queryIDs(ctx,
		`SELECT depends_on FROM ticket_deps WHERE ticket_id = ? ORDER BY depends_on`, ticketID)
}

// DependentsOf returns the ids that depend on a ticket, which is what gets re-evaluated when it
// reaches Done.
func (d *DB) DependentsOf(ctx context.Context, ticketID string) ([]string, error) {
	return d.queryIDs(ctx,
		`SELECT ticket_id FROM ticket_deps WHERE depends_on = ? ORDER BY ticket_id`, ticketID)
}

func (d *DB) queryIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query ids: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("query ids: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query ids: %w", err)
	}
	return out, nil
}
