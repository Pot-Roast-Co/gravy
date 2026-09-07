package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bobbybrady/gravy/internal/core"
)

const runColumns = `id, ticket_id, host_id, provider_id, model, session_ref, state,
	failure_class, failure_note, pid, turns, tokens_in, tokens_out, cost_usd,
	verdict, started_at, ended_at`

// CreateRun records the start of a run.
func (d *DB) CreateRun(ctx context.Context, r core.Run) error {
	_, err := d.exec(ctx, `INSERT INTO runs (`+runColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.TicketID, r.HostID, r.ProviderID, r.Model, nullString(r.SessionRef),
		string(r.State), r.FailureClass.String(), nullString(r.FailureNote), r.PID,
		r.Turns, r.TokensIn, r.TokensOut, r.CostUSD, r.Verdict,
		unixOrZero(r.StartedAt), endedAt(r))
	if err != nil {
		return fmt.Errorf("create run %q: %w", r.ID, err)
	}
	return nil
}

func endedAt(r core.Run) any {
	if r.EndedAt == nil || r.EndedAt.IsZero() {
		return nil
	}
	return r.EndedAt.Unix()
}

// GetRun returns a run by id.
func (d *DB) GetRun(ctx context.Context, id string) (core.Run, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Run{}, notFound("run", id)
	}
	return r, err
}

// UpdateRun replaces a run's mutable fields, which is how a finished run records its outcome.
func (d *DB) UpdateRun(ctx context.Context, r core.Run) error {
	res, err := d.exec(ctx, `UPDATE runs SET
		session_ref=?, state=?, failure_class=?, failure_note=?, pid=?, turns=?,
		tokens_in=?, tokens_out=?, cost_usd=?, verdict=?, ended_at=?
		WHERE id=?`,
		nullString(r.SessionRef), string(r.State), r.FailureClass.String(),
		nullString(r.FailureNote), r.PID, r.Turns, r.TokensIn, r.TokensOut, r.CostUSD,
		r.Verdict, endedAt(r), r.ID)
	if err != nil {
		return fmt.Errorf("update run %q: %w", r.ID, err)
	}
	return requireOneRow(res, "run", r.ID)
}

// ListRunsForTicket returns a ticket's runs, newest first.
func (d *DB) ListRunsForTicket(ctx context.Context, ticketID string) ([]core.Run, error) {
	return d.queryRuns(ctx,
		`SELECT `+runColumns+` FROM runs WHERE ticket_id = ? ORDER BY started_at DESC`, ticketID)
}

// ListUnfinishedRuns returns runs that never recorded an end.
//
// This is what startup reconciliation reads: a daemon that died leaves runs open, and their
// recorded pid is how the new daemon decides whether the process is still alive or orphaned.
func (d *DB) ListUnfinishedRuns(ctx context.Context) ([]core.Run, error) {
	return d.queryRuns(ctx,
		`SELECT `+runColumns+` FROM runs WHERE ended_at IS NULL ORDER BY started_at ASC`)
}

func (d *DB) queryRuns(ctx context.Context, query string, args ...any) ([]core.Run, error) {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []core.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return out, nil
}

func scanRun(s scanner) (core.Run, error) {
	var (
		r                                     core.Run
		state                                 string
		sessionRef, failureClass, failureNote sql.NullString
		pid, turns, tokensIn, tokensOut       sql.NullInt64
		cost                                  sql.NullFloat64
		startedAt                             int64
		endedAt                               sql.NullInt64
	)
	if err := s.Scan(&r.ID, &r.TicketID, &r.HostID, &r.ProviderID, &r.Model, &sessionRef,
		&state, &failureClass, &failureNote, &pid, &turns, &tokensIn, &tokensOut, &cost,
		&r.Verdict, &startedAt, &endedAt); err != nil {
		return core.Run{}, err
	}
	r.State = core.State(state)
	r.SessionRef = sessionRef.String
	r.FailureNote = failureNote.String
	r.FailureClass = parseFailureClass(failureClass.String)
	r.PID = int(pid.Int64)
	r.Turns = int(turns.Int64)
	r.TokensIn = int(tokensIn.Int64)
	r.TokensOut = int(tokensOut.Int64)
	if cost.Valid {
		c := cost.Float64
		r.CostUSD = &c
	}
	r.StartedAt = timeOrZero(startedAt)
	if endedAt.Valid {
		t := timeOrZero(endedAt.Int64)
		r.EndedAt = &t
	}
	return r, nil
}

// parseFailureClass maps a stored class name back to its value.
//
// An unrecognised name resolves to core.Unknown, which core.Effective in turn treats as a task
// failure — never as a quota condition. Failing this way round is the whole point: a wrong quota
// call silently escalates work to the most expensive model.
func parseFailureClass(s string) core.FailureClass {
	for c := core.Success; c <= core.Unknown; c++ {
		if c.String() == s {
			return c
		}
	}
	return core.Unknown
}

// AddValidation records the result of one validation step.
func (d *DB) AddValidation(ctx context.Context, id, runID, step string, exitCode int, durationMS int64, logPath string) error {
	_, err := d.exec(ctx,
		`INSERT INTO validations (id, run_id, step, exit_code, duration_ms, log_path)
		 VALUES (?,?,?,?,?,?)`,
		id, runID, step, exitCode, durationMS, logPath)
	if err != nil {
		return fmt.Errorf("add validation %q for run %q: %w", step, runID, err)
	}
	return nil
}

// Validation is one recorded validation step result.
type Validation struct {
	ID         string
	RunID      string
	Step       string
	ExitCode   int
	DurationMS int64
	LogPath    string
}

// ListValidations returns a run's validation results in the order they were recorded.
func (d *DB) ListValidations(ctx context.Context, runID string) ([]Validation, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, run_id, step, exit_code, duration_ms, log_path
		 FROM validations WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, fmt.Errorf("list validations for run %q: %w", runID, err)
	}
	defer rows.Close()

	var out []Validation
	for rows.Next() {
		var v Validation
		if err := rows.Scan(&v.ID, &v.RunID, &v.Step, &v.ExitCode, &v.DurationMS, &v.LogPath); err != nil {
			return nil, fmt.Errorf("list validations for run %q: %w", runID, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list validations for run %q: %w", runID, err)
	}
	return out, nil
}

// PutSummary stores or replaces a ticket's result summary.
func (d *DB) PutSummary(ctx context.Context, s core.Summary) error {
	commits, err := marshalJSON(s.Commits)
	if err != nil {
		return err
	}
	files, err := marshalJSON(s.Files)
	if err != nil {
		return err
	}
	assumptions, err := marshalJSON(s.Assumptions)
	if err != nil {
		return err
	}
	_, err = d.exec(ctx, `INSERT INTO summaries
		(ticket_id, run_id, branch, commits, files, narrative, assumptions, created_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(ticket_id) DO UPDATE SET
			run_id=excluded.run_id, branch=excluded.branch, commits=excluded.commits,
			files=excluded.files, narrative=excluded.narrative,
			assumptions=excluded.assumptions, created_at=excluded.created_at`,
		s.TicketID, s.RunID, s.Branch, commits, files, s.Narrative, assumptions,
		unixOrZero(s.CreatedAt))
	if err != nil {
		return fmt.Errorf("put summary for ticket %q: %w", s.TicketID, err)
	}
	return nil
}

// GetSummary returns a ticket's summary.
func (d *DB) GetSummary(ctx context.Context, ticketID string) (core.Summary, error) {
	var (
		s                           core.Summary
		commits, files, assumptions string
		createdAt                   int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT ticket_id, run_id, branch, commits, files, narrative, assumptions, created_at
		 FROM summaries WHERE ticket_id = ?`, ticketID).
		Scan(&s.TicketID, &s.RunID, &s.Branch, &commits, &files, &s.Narrative, &assumptions, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Summary{}, notFound("summary", ticketID)
	}
	if err != nil {
		return core.Summary{}, fmt.Errorf("get summary for ticket %q: %w", ticketID, err)
	}
	s.CreatedAt = timeOrZero(createdAt)
	if err := unmarshalJSON(commits, &s.Commits); err != nil {
		return core.Summary{}, err
	}
	if err := unmarshalJSON(files, &s.Files); err != nil {
		return core.Summary{}, err
	}
	if err := unmarshalJSON(assumptions, &s.Assumptions); err != nil {
		return core.Summary{}, err
	}
	return s, nil
}
