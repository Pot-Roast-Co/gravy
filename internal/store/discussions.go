package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pot-roast-co/gravy/internal/core"
)

const discussionColumns = `id, ticket_id, run_id, state, session, agent, messages, proposal,
	created_at, updated_at`

// SaveDiscussion writes a change discussion, inserting it or replacing what is there.
//
// One method rather than create and update because every caller above this package holds the
// whole discussion: a turn appends a message and replaces the draft in the same breath, and
// splitting that into two writes would leave a window in which the transcript and the proposal
// disagree.
func (d *DB) SaveDiscussion(ctx context.Context, cd core.ChangeDiscussion) error {
	if cd.ID == "" {
		return fmt.Errorf("save discussion: no id")
	}
	if !cd.State.Valid() {
		return fmt.Errorf("save discussion %q: unknown state %q", cd.ID, cd.State)
	}
	messages, err := marshalJSON(cd.Messages)
	if err != nil {
		return err
	}
	proposal, err := marshalJSON(cd.Proposal)
	if err != nil {
		return err
	}
	_, err = d.exec(ctx, `INSERT INTO change_discussions (`+discussionColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			run_id=excluded.run_id, state=excluded.state, session=excluded.session,
			agent=excluded.agent, messages=excluded.messages, proposal=excluded.proposal,
			updated_at=excluded.updated_at`,
		cd.ID, cd.TicketID, cd.RunID, string(cd.State), cd.Session, cd.Agent, messages, proposal,
		unixOrZero(cd.CreatedAt), unixOrZero(cd.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save discussion %q: %w", cd.ID, err)
	}
	return nil
}

// GetDiscussion returns one discussion by id.
func (d *DB) GetDiscussion(ctx context.Context, id string) (core.ChangeDiscussion, error) {
	row := d.sql.QueryRowContext(ctx,
		`SELECT `+discussionColumns+` FROM change_discussions WHERE id = ?`, id)
	cd, err := scanDiscussion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ChangeDiscussion{}, notFound("discussion", id)
	}
	return cd, err
}

// OpenDiscussionFor returns the ticket's open discussion, newest first.
//
// ErrNotFound means the ticket has no conversation in progress, which is the ordinary case and
// not a failure: the caller starts one.
func (d *DB) OpenDiscussionFor(ctx context.Context, ticketID string) (core.ChangeDiscussion, error) {
	row := d.sql.QueryRowContext(ctx,
		`SELECT `+discussionColumns+` FROM change_discussions
		 WHERE ticket_id = ? AND state = ?
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		ticketID, string(core.DiscussionOpen))
	cd, err := scanDiscussion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ChangeDiscussion{}, notFound("open discussion for ticket", ticketID)
	}
	return cd, err
}

// ListDiscussions returns every discussion a ticket has had, oldest first.
func (d *DB) ListDiscussions(ctx context.Context, ticketID string) ([]core.ChangeDiscussion, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+discussionColumns+` FROM change_discussions
		 WHERE ticket_id = ? ORDER BY created_at ASC, rowid ASC`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("list discussions for %q: %w", ticketID, err)
	}
	defer rows.Close()

	var out []core.ChangeDiscussion
	for rows.Next() {
		cd, err := scanDiscussion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list discussions for %q: %w", ticketID, err)
	}
	return out, nil
}

func scanDiscussion(s scanner) (core.ChangeDiscussion, error) {
	var (
		cd                  core.ChangeDiscussion
		state               string
		messages, proposal  string
		createdAt, updateAt int64
	)
	if err := s.Scan(&cd.ID, &cd.TicketID, &cd.RunID, &state, &cd.Session, &cd.Agent,
		&messages, &proposal, &createdAt, &updateAt); err != nil {
		return core.ChangeDiscussion{}, err
	}
	cd.State = core.DiscussionState(state)
	cd.CreatedAt = timeOrZero(createdAt)
	cd.UpdatedAt = timeOrZero(updateAt)
	if err := unmarshalJSON(messages, &cd.Messages); err != nil {
		return core.ChangeDiscussion{}, fmt.Errorf("discussion %q messages: %w", cd.ID, err)
	}
	if err := unmarshalJSON(proposal, &cd.Proposal); err != nil {
		return core.ChangeDiscussion{}, fmt.Errorf("discussion %q proposal: %w", cd.ID, err)
	}
	return cd, nil
}

// AddChangeInstruction records an instruction a human confirmed.
//
// Append-only. An instruction is a decision that was made, and a later round revising it does
// not make the earlier promise untrue — which is the whole reason the preservation constraints
// of round one still apply in round three.
func (d *DB) AddChangeInstruction(ctx context.Context, ci core.ChangeInstruction) error {
	if ci.ID == "" {
		return fmt.Errorf("add change instruction: no id")
	}
	ci = ci.Normalized()
	preserve, err := marshalJSON(ci.Preserve)
	if err != nil {
		return err
	}
	verify, err := marshalJSON(ci.Verify)
	if err != nil {
		return err
	}
	_, err = d.exec(ctx, `INSERT INTO change_instructions
		(id, ticket_id, discussion_id, correction, preserve, verify, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		ci.ID, ci.TicketID, ci.DiscussionID, ci.Correction, preserve, verify,
		unixOrZero(ci.AgreedAt))
	if err != nil {
		return fmt.Errorf("add change instruction %q: %w", ci.ID, err)
	}
	return nil
}

// ListChangeInstructions returns a ticket's agreed instructions, oldest first.
//
// Oldest first because that is the order a prompt has to render them in: the last one is the
// correction to make, and the ones before it are the constraints it must not cost.
func (d *DB) ListChangeInstructions(ctx context.Context, ticketID string) ([]core.ChangeInstruction, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, ticket_id, discussion_id, correction, preserve, verify, created_at
		 FROM change_instructions WHERE ticket_id = ? ORDER BY created_at ASC, rowid ASC`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("list change instructions for %q: %w", ticketID, err)
	}
	defer rows.Close()

	var out []core.ChangeInstruction
	for rows.Next() {
		var (
			ci               core.ChangeInstruction
			preserve, verify string
			createdAt        int64
		)
		if err := rows.Scan(&ci.ID, &ci.TicketID, &ci.DiscussionID, &ci.Correction,
			&preserve, &verify, &createdAt); err != nil {
			return nil, fmt.Errorf("list change instructions for %q: %w", ticketID, err)
		}
		ci.AgreedAt = timeOrZero(createdAt)
		if err := unmarshalJSON(preserve, &ci.Preserve); err != nil {
			return nil, fmt.Errorf("change instruction %q preserve: %w", ci.ID, err)
		}
		if err := unmarshalJSON(verify, &ci.Verify); err != nil {
			return nil, fmt.Errorf("change instruction %q verify: %w", ci.ID, err)
		}
		out = append(out, ci)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list change instructions for %q: %w", ticketID, err)
	}
	return out, nil
}
