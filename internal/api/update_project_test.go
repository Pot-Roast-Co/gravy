package api

import (
	"context"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// TestUpdateProjectSavesEveryEditableField is the bug a real session hit: a bucket override typed
// into the Projects screen saved without error and was gone on the next load, because
// UpdateProject copied only some of the fields the screen edits.
func TestUpdateProjectSavesEveryEditableField(t *testing.T) {
	l, repo, _ := setupFixture(t, "go.mod")
	ctx := context.Background()

	p, err := l.AddProject(ctx, AddProjectReq{Path: repo})
	if err != nil {
		t.Fatal(err)
	}

	p.Routes = map[core.Route][]core.Choice{
		core.RouteImplementation: {{ProviderID: "codex", Model: "gpt-6-astra"}},
	}
	p.Allowlist = core.Allowlist{Commands: []core.Pattern{{Match: "mix *", Note: "an Elixir project"}}}
	p.Notes = "the nuns are armed"
	p.TargetBranch = "trunk"
	p.Validation = []core.Step{{Name: "test", Cmd: "go test ./...", Required: true}}
	if err := l.UpdateProject(ctx, p); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	got, err := l.db.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Routes[core.RouteImplementation]); n != 1 {
		t.Errorf("stored buckets = %v, want the project's own implementation override", got.Routes)
	}
	if len(got.Allowlist.Commands) != 1 {
		t.Errorf("stored allowlist = %+v, want the edited commands", got.Allowlist)
	}
	if got.Notes != "the nuns are armed" {
		t.Errorf("stored notes = %q, want the edited notes", got.Notes)
	}
	if got.TargetBranch != "trunk" || len(got.Validation) != 1 {
		t.Errorf("stored project = %+v, want the edited branch and validation", got)
	}
}

// TestUpdateProjectRefusesAHostWithoutTheClone: moving a project to another machine does not move
// its repository, so a host that does not have it must be refused at the edit.
func TestUpdateProjectRefusesAHostWithoutTheClone(t *testing.T) {
	l, repo, _ := setupFixture(t, "go.mod")
	ctx := context.Background()

	p, err := l.AddProject(ctx, AddProjectReq{Path: repo})
	if err != nil {
		t.Fatal(err)
	}

	was := p.HostID
	p.HostID = "nowhere"
	if err := l.UpdateProject(ctx, p); err == nil {
		t.Fatal("a project was moved to a host that does not exist")
	}

	got, err := l.db.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostID != was {
		t.Errorf("stored host = %q, want %q — unchanged after a refused edit", got.HostID, was)
	}
}
