package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

const attentionColumns = `id, project_id, ticket_id, run_id, reason, payload, resolved, created_at`

// OpenAttention adds an entry to the Needs You queue.
func (d *DB) OpenAttention(ctx context.Context, a core.Attention) error {
	if !a.Reason.Valid() {
		return fmt.Errorf("open attention %q: unknown reason %q", a.ID, a.Reason)
	}
	payload, err := marshalJSON(a.Payload)
	if err != nil {
		return err
	}
	_, err = d.exec(ctx, `INSERT INTO attention (`+attentionColumns+`)
		VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.ProjectID, nullString(a.TicketID), nullString(a.RunID), string(a.Reason),
		payload, boolToInt(a.Resolved), unixOrZero(a.CreatedAt))
	if err != nil {
		return fmt.Errorf("open attention %q: %w", a.ID, err)
	}
	return nil
}

// ResolveAttention marks an entry handled. It is idempotent in effect but reports a missing row.
func (d *DB) ResolveAttention(ctx context.Context, id string) error {
	res, err := d.exec(ctx, `UPDATE attention SET resolved = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("resolve attention %q: %w", id, err)
	}
	return requireOneRow(res, "attention", id)
}

// ResolveAttentionForTicket marks every open entry for a ticket handled and reports how many it
// closed.
//
// An attention row means "this ticket needs a human". Once the ticket leaves the state that
// raised one — approved, rejected, sent back for another attempt — the row is stale, and a queue
// that lists work nobody needs to act on fails the same way as one that omits work that is
// waiting: it stops being trustworthy, so it stops being read (PRODUCT.md §8).
func (d *DB) ResolveAttentionForTicket(ctx context.Context, ticketID string) (int, error) {
	res, err := d.exec(ctx,
		`UPDATE attention SET resolved = 1 WHERE resolved = 0 AND ticket_id = ?`, ticketID)
	if err != nil {
		return 0, fmt.Errorf("resolve attention for ticket %q: %w", ticketID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("resolve attention for ticket %q: %w", ticketID, err)
	}
	return int(n), nil
}

// ListOpenAttention returns the unresolved Needs You queue, oldest first, which is the order
// the human works through it.
func (d *DB) ListOpenAttention(ctx context.Context) ([]core.Attention, error) {
	return d.queryAttention(ctx,
		`SELECT `+attentionColumns+` FROM attention WHERE resolved = 0 ORDER BY created_at ASC`)
}

// ListOpenAttentionForProject returns a single project's unresolved entries.
func (d *DB) ListOpenAttentionForProject(ctx context.Context, projectID string) ([]core.Attention, error) {
	return d.queryAttention(ctx,
		`SELECT `+attentionColumns+` FROM attention
		 WHERE resolved = 0 AND project_id = ? ORDER BY created_at ASC`, projectID)
}

func (d *DB) queryAttention(ctx context.Context, query string, args ...any) ([]core.Attention, error) {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list attention: %w", err)
	}
	defer rows.Close()

	var out []core.Attention
	for rows.Next() {
		var (
			a               core.Attention
			ticketID, runID sql.NullString
			reason, payload string
			resolved        int
			createdAt       int64
		)
		if err := rows.Scan(&a.ID, &a.ProjectID, &ticketID, &runID, &reason, &payload,
			&resolved, &createdAt); err != nil {
			return nil, fmt.Errorf("list attention: %w", err)
		}
		a.TicketID = ticketID.String
		a.RunID = runID.String
		a.Reason = core.AttentionReason(reason)
		a.Resolved = resolved != 0
		a.CreatedAt = timeOrZero(createdAt)
		if err := unmarshalJSON(payload, &a.Payload); err != nil {
			return nil, fmt.Errorf("attention %q payload: %w", a.ID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list attention: %w", err)
	}
	return out, nil
}

// SetProviderUnavailable records a cooldown for a provider/model, replacing any existing one.
func (d *DB) SetProviderUnavailable(ctx context.Context, a core.ProviderAvailability) error {
	_, err := d.exec(ctx, `INSERT INTO provider_availability (provider_id, model, class, until, note)
		VALUES (?,?,?,?,?)
		ON CONFLICT(provider_id, model) DO UPDATE SET
			class=excluded.class, until=excluded.until, note=excluded.note`,
		a.ProviderID, a.Model, a.Class.String(), unixOrZero(a.Until), nullString(a.Note))
	if err != nil {
		return fmt.Errorf("set availability for %s/%s: %w", a.ProviderID, a.Model, err)
	}
	return nil
}

// ClearProviderAvailability removes a cooldown, for example after a successful re-auth.
func (d *DB) ClearProviderAvailability(ctx context.Context, providerID, model string) error {
	_, err := d.exec(ctx,
		`DELETE FROM provider_availability WHERE provider_id = ? AND model = ?`, providerID, model)
	if err != nil {
		return fmt.Errorf("clear availability for %s/%s: %w", providerID, model, err)
	}
	return nil
}

// ListUnavailable returns cooldowns still in force at the given time. Expired rows are left in
// place; they are harmless and their history is useful when explaining a past routing decision.
func (d *DB) ListUnavailable(ctx context.Context, now time.Time) ([]core.ProviderAvailability, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT provider_id, model, class, until, note FROM provider_availability
		 WHERE until > ? ORDER BY provider_id, model`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("list availability: %w", err)
	}
	defer rows.Close()

	var out []core.ProviderAvailability
	for rows.Next() {
		var (
			a     core.ProviderAvailability
			class string
			until int64
			note  sql.NullString
		)
		if err := rows.Scan(&a.ProviderID, &a.Model, &class, &until, &note); err != nil {
			return nil, fmt.Errorf("list availability: %w", err)
		}
		a.Class = parseFailureClass(class)
		a.Until = timeOrZero(until)
		a.Note = note.String
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list availability: %w", err)
	}
	return out, nil
}

// IsUnavailable reports whether a provider/model is cooling down at the given time.
func (d *DB) IsUnavailable(ctx context.Context, providerID, model string, now time.Time) (bool, error) {
	var until int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT until FROM provider_availability WHERE provider_id = ? AND model = ?`,
		providerID, model).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check availability for %s/%s: %w", providerID, model, err)
	}
	return until > now.Unix(), nil
}
