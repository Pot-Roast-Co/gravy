package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestProjectArchivedRoundTrip is the migration's acceptance: the flag survives a reopen, rows
// written before the column existed read as not archived, and the default listing is the working
// set while everything else still finds an archived project.
func TestProjectArchivedRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravy.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// A row written the way one was before 0007 ran: no archived column in the insert, so the
	// column's default is what decides how every project already in a real database reads.
	if _, err := db.exec(ctx, `INSERT INTO projects
		(id, slug, name, repo_path, target_branch, merge_mode, requirements, validation,
		 allowlist, routes, parallel_mode, max_concurrency, created_at, host_id, notes)
		VALUES ('p2','mojo','mojo','/repos/mojo','main','merge','{}','[]','{}','{}',0,1,0,'','')`); err != nil {
		t.Fatalf("insert a pre-migration row: %v", err)
	}

	old, err := db.GetProject(ctx, "p2")
	if err != nil {
		t.Fatal(err)
	}
	if old.Archived {
		t.Error("a project written without the column reads as archived; the migration's default is wrong")
	}

	if err := db.SetProjectArchived(ctx, "p1", true); err != nil {
		t.Fatalf("SetProjectArchived: %v", err)
	}

	// Reopened, because a flag that only lives in this connection is not archived at all.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err = Open(ctx, path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	got, err := db.GetProject(ctx, "p1")
	if err != nil {
		t.Fatalf("an archived project must still be readable: %v", err)
	}
	if !got.Archived {
		t.Error("archived did not survive the reopen")
	}
	// Archiving keeps everything. A project that came back without its validation steps would
	// be a delete with extra steps.
	if len(got.Validation) != 1 || got.RepoPath != "/repos/gravy" {
		t.Errorf("archiving changed the project: %+v", got)
	}

	working, err := db.ListProjects(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(working) != 1 || working[0].ID != "p2" {
		t.Errorf("default listing = %+v, want only the unarchived project", working)
	}
	all, err := db.ListProjects(ctx, ProjectFilter{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("IncludeArchived listing = %d projects, want 2", len(all))
	}

	// UpdateProject is not the way to flip this. The Projects screen edits notes from a copy it
	// loaded before the archive, and saving that copy must not quietly unarchive the project.
	stale := got
	stale.Archived = false
	stale.Notes = "edited from a screen holding the old flag"
	if err := db.UpdateProject(ctx, stale); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	after, err := db.GetProject(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !after.Archived {
		t.Error("UpdateProject unarchived the project as a side effect of saving an edit")
	}
	if after.Notes != stale.Notes {
		t.Errorf("notes = %q, want the edit to have been saved", after.Notes)
	}

	if err := db.SetProjectArchived(ctx, "p1", false); err != nil {
		t.Fatal(err)
	}
	if back, err := db.GetProject(ctx, "p1"); err != nil || back.Archived {
		t.Errorf("unarchiving left %+v, %v", back, err)
	}
	if err := db.SetProjectArchived(ctx, "nope", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetProjectArchived on a missing row = %v, want ErrNotFound", err)
	}
}
