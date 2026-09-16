package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pot-roast-co/gravy/internal/core"
)

const projectColumns = `id, slug, name, repo_path, target_branch, merge_mode, requirements,
	validation, allowlist, routes, parallel_mode, max_concurrency, created_at, host_id, notes,
	archived`

// ProjectFilter narrows a project listing. The zero value is the working set: everything that
// has not been archived.
type ProjectFilter struct {
	// IncludeArchived adds finished projects back in, for the screens and commands that are
	// deliberately looking at history rather than at this week's work.
	IncludeArchived bool
}

// CreateProject inserts a project.
func (d *DB) CreateProject(ctx context.Context, p core.Project) error {
	req, err := marshalJSON(p.Requirements)
	if err != nil {
		return err
	}
	val, err := marshalJSON(p.Validation)
	if err != nil {
		return err
	}
	allow, err := marshalJSON(p.Allowlist)
	if err != nil {
		return err
	}
	routes, err := marshalJSON(p.Routes)
	if err != nil {
		return err
	}

	_, err = d.exec(ctx, `INSERT INTO projects (`+projectColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Slug, p.Name, p.RepoPath, p.TargetBranch, string(p.MergeMode),
		req, val, allow, routes, boolToInt(p.ParallelMode), p.MaxConcurrency,
		unixOrZero(p.CreatedAt), p.HostID, p.Notes, boolToInt(p.Archived))
	if err != nil {
		return fmt.Errorf("create project %q: %w", p.Slug, err)
	}
	return nil
}

// GetProject returns a project by id.
func (d *DB) GetProject(ctx context.Context, id string) (core.Project, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Project{}, notFound("project", id)
	}
	return p, err
}

// GetProjectBySlug returns a project by its slug, which is what the CLI takes.
func (d *DB) GetProjectBySlug(ctx context.Context, slug string) (core.Project, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE slug = ?`, slug)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Project{}, notFound("project", slug)
	}
	return p, err
}

// ListProjects returns projects ordered by slug for stable display.
//
// Archived projects are left out unless the filter asks for them: the default is the working
// set, because a list that only ever grows stops being the answer to "what am I working on".
func (d *DB) ListProjects(ctx context.Context, f ProjectFilter) ([]core.Project, error) {
	where := ` WHERE archived = 0`
	if f.IncludeArchived {
		where = ``
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects`+where+` ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []core.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return out, nil
}

// UpdateProject replaces a project's mutable fields.
//
// Deliberately not archived: taking a project out of the working set is its own recorded act
// (SetProjectArchived), never a side effect of an edit that happened to carry a stale flag.
func (d *DB) UpdateProject(ctx context.Context, p core.Project) error {
	req, err := marshalJSON(p.Requirements)
	if err != nil {
		return err
	}
	val, err := marshalJSON(p.Validation)
	if err != nil {
		return err
	}
	allow, err := marshalJSON(p.Allowlist)
	if err != nil {
		return err
	}
	routes, err := marshalJSON(p.Routes)
	if err != nil {
		return err
	}

	res, err := d.exec(ctx, `UPDATE projects SET
		slug=?, name=?, repo_path=?, target_branch=?, merge_mode=?, requirements=?,
		validation=?, allowlist=?, routes=?, parallel_mode=?, max_concurrency=?, host_id=?,
		notes=?
		WHERE id=?`,
		p.Slug, p.Name, p.RepoPath, p.TargetBranch, string(p.MergeMode), req, val, allow,
		routes, boolToInt(p.ParallelMode), p.MaxConcurrency, p.HostID, p.Notes, p.ID)
	if err != nil {
		return fmt.Errorf("update project %q: %w", p.ID, err)
	}
	return requireOneRow(res, "project", p.ID)
}

// SetProjectArchived takes a project out of the working set, or puts it back.
//
// Nothing else is touched: the project's tickets, runs, summaries and history stay exactly as
// they are, which is the whole point of archiving rather than deleting.
func (d *DB) SetProjectArchived(ctx context.Context, id string, archived bool) error {
	res, err := d.exec(ctx, `UPDATE projects SET archived = ? WHERE id = ?`, boolToInt(archived), id)
	if err != nil {
		return fmt.Errorf("archive project %q: %w", id, err)
	}
	return requireOneRow(res, "project", id)
}

// DeleteProject removes a project. Its tickets, runs, summaries and attention rows go with it
// via ON DELETE CASCADE.
func (d *DB) DeleteProject(ctx context.Context, id string) error {
	res, err := d.exec(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project %q: %w", id, err)
	}
	return requireOneRow(res, "project", id)
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanProject(s scanner) (core.Project, error) {
	var (
		p                       core.Project
		mergeMode               string
		req, val, allow, routes string
		parallel, archived      int
		createdAt               int64
	)
	if err := s.Scan(&p.ID, &p.Slug, &p.Name, &p.RepoPath, &p.TargetBranch, &mergeMode,
		&req, &val, &allow, &routes, &parallel, &p.MaxConcurrency, &createdAt,
		&p.HostID, &p.Notes, &archived); err != nil {
		return core.Project{}, err
	}
	p.MergeMode = core.LandMode(mergeMode)
	p.ParallelMode = parallel != 0
	p.Archived = archived != 0
	p.CreatedAt = timeOrZero(createdAt)

	if err := unmarshalJSON(req, &p.Requirements); err != nil {
		return core.Project{}, fmt.Errorf("project %q requirements: %w", p.ID, err)
	}
	if err := unmarshalJSON(val, &p.Validation); err != nil {
		return core.Project{}, fmt.Errorf("project %q validation: %w", p.ID, err)
	}
	if err := unmarshalJSON(allow, &p.Allowlist); err != nil {
		return core.Project{}, fmt.Errorf("project %q allowlist: %w", p.ID, err)
	}
	if err := unmarshalJSON(routes, &p.Routes); err != nil {
		return core.Project{}, fmt.Errorf("project %q routes: %w", p.ID, err)
	}
	return p, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// requireOneRow turns a no-op UPDATE or DELETE into ErrNotFound, so a caller acting on a row
// that has since been deleted finds out rather than silently succeeding.
func requireOneRow(res sql.Result, kind, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s %q: %w", kind, id, err)
	}
	if n == 0 {
		return notFound(kind, id)
	}
	return nil
}
