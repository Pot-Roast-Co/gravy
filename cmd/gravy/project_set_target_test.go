package main

import (
	"context"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// projectEditor answers the two calls set-target makes and records what it was asked to save.
type projectEditor struct {
	api.Service
	projects []core.Project
	saved    *core.Project
}

func (e *projectEditor) ListProjects(_ context.Context, _ api.ProjectFilter) ([]core.Project, error) {
	return e.projects, nil
}

func (e *projectEditor) UpdateProject(_ context.Context, p core.Project) error {
	e.saved = &p
	return nil
}

func TestSetTargetBranchReportsWhatItChanged(t *testing.T) {
	e := &projectEditor{projects: []core.Project{
		{ID: "1", Slug: "gravy", TargetBranch: "feat/planning-projects-and-remote-hosts"},
	}}

	slug, was, changed, err := setTargetBranch(context.Background(), e, "gravy", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || slug != "gravy" || was != "feat/planning-projects-and-remote-hosts" {
		t.Fatalf("slug=%q was=%q changed=%v", slug, was, changed)
	}
	if e.saved == nil || e.saved.TargetBranch != "main" {
		t.Fatalf("saved = %+v", e.saved)
	}
}

// The edit carries the rest of the project with it. Writing a fresh core.Project here would
// silently blank the validation steps, allowlist and notes of whatever it was pointed at.
func TestSetTargetBranchKeepsEverythingElse(t *testing.T) {
	original := core.Project{
		ID: "1", Slug: "gravy", Name: "gravy", TargetBranch: "old",
		RepoPath: "/home/bobby/Projects/gravy", HostID: "local", Notes: "keep me",
		Validation:     []core.Step{{Name: "check", Cmd: "make check", Required: true}},
		MergeMode:      core.LandMerge,
		MaxConcurrency: 3,
	}
	e := &projectEditor{projects: []core.Project{original}}

	if _, _, _, err := setTargetBranch(context.Background(), e, "gravy", "main"); err != nil {
		t.Fatal(err)
	}
	got := *e.saved
	if got.TargetBranch != "main" {
		t.Fatalf("target = %q", got.TargetBranch)
	}
	got.TargetBranch = original.TargetBranch
	if got.Notes != original.Notes || got.MaxConcurrency != original.MaxConcurrency ||
		got.RepoPath != original.RepoPath || got.HostID != original.HostID ||
		len(got.Validation) != len(original.Validation) || got.MergeMode != original.MergeMode {
		t.Fatalf("set-target changed more than the branch:\n got %+v\nwant %+v", got, original)
	}
}

// Setting the branch it already has writes nothing at all.
func TestSetTargetBranchNoOpDoesNotSave(t *testing.T) {
	e := &projectEditor{projects: []core.Project{{ID: "1", Slug: "gravy", TargetBranch: "main"}}}

	_, _, changed, err := setTargetBranch(context.Background(), e, "gravy", "main")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("reported a change")
	}
	if e.saved != nil {
		t.Fatalf("saved %+v on a no-op", e.saved)
	}
}

func TestSetTargetBranchUnknownProject(t *testing.T) {
	e := &projectEditor{projects: []core.Project{{ID: "1", Slug: "gravy", TargetBranch: "main"}}}
	if _, _, _, err := setTargetBranch(context.Background(), e, "nope", "main"); err == nil {
		t.Fatal("want an error for an unknown project")
	}
}
