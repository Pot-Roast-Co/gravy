package agentrun

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

// TestPlanWorkspaceForAProjectWithNoRepository is the regression.
//
// A project with no repository is a supported, deliberate state — it is how a goal gets a home
// before anyone has decided what to build, and planning is the whole point of a project in that
// state. The planner passed the empty RepoPath straight to the adapter, which refused with
// "claude-code: no worktree path", so the one kind of project that exists only to be planned was
// the one kind that could not be.
func TestPlanWorkspaceForAProjectWithNoRepository(t *testing.T) {
	home := t.TempDir()
	o := New(nil, nil, nil, nil, nil, Config{Home: home, RunsDir: filepath.Join(home, "runs")},
		func() string { return "run-1" })
	p := o.Plan(nil, 0)
	h := host.NewLocal("local", 1)

	dir, err := p.workspace(h, core.Project{Slug: "fantasyhockeyaid"})
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		t.Fatal("empty workspace; the adapter refuses this with \"no worktree path\"")
	}
	if !strings.HasPrefix(dir, home) {
		t.Errorf("workspace %q is outside the gravy home", dir)
	}
	if !h.FS().Exists(dir) {
		t.Errorf("workspace %q was not created, so the agent cannot start in it", dir)
	}

	// Stable across turns: a planning conversation spans several, and notes left in it have to
	// still be there on the next one.
	again, err := p.workspace(h, core.Project{Slug: "fantasyhockeyaid"})
	if err != nil || again != dir {
		t.Errorf("workspace moved between turns: %q then %q (%v)", dir, again, err)
	}
}

// A project with a repository is still planned in it, untouched.
func TestPlanWorkspaceUsesTheRepositoryWhenThereIsOne(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	o := New(nil, nil, nil, nil, nil, Config{Home: home}, func() string { return "run-1" })

	dir, err := o.Plan(nil, 0).workspace(host.NewLocal("local", 1), core.Project{Slug: "gravy", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if dir != repo {
		t.Errorf("workspace = %q, want the project's repository %q", dir, repo)
	}
}

// Without a home there is nowhere to put it, and that is an error rather than an empty path
// handed to an adapter that will reject it less clearly.
func TestPlanWorkspaceWithoutAHomeIsAnError(t *testing.T) {
	o := New(nil, nil, nil, nil, nil, Config{}, func() string { return "run-1" })
	if _, err := o.Plan(nil, 0).workspace(host.NewLocal("local", 1), core.Project{Slug: "x"}); err == nil {
		t.Fatal("want an error naming the missing home")
	}
}

// TestPlanPromptSaysWhenThereIsNoRepository is the other half of planning a project that has
// nothing built yet.
//
// The workspace it runs in is an empty directory gravy just created. An agent not told that
// spends its turn shelling out to look around — and `ls a b 2>/dev/null; find . | head -5` is a
// chained command, which no prefix rule can authorise, so the turn dies on "permission denied"
// having learned nothing that was there to learn.
func TestPlanPromptSaysWhenThereIsNoRepository(t *testing.T) {
	o := New(nil, nil, nil, nil, nil, Config{}, func() string { return "run-1" })
	got := o.Plan(nil, 0).prompt(host.NewLocal("local", 1),
		core.Project{Slug: "fantasyhockeyaid", Name: "fantasyHockeyAid"}, nil, "I want to make this")

	if !strings.Contains(got, "no repository yet") {
		t.Fatalf("the planner is not told the project has no files:\n%s", got)
	}
	if !strings.Contains(got, "do not go looking") {
		t.Errorf("told there are no files but not to stop looking for them:\n%s", got)
	}
}

// A project with a repository is told nothing of the sort: it has files, and looking at them is
// the job.
func TestPlanPromptSaysNothingAboutMissingFilesWhenThereIsARepo(t *testing.T) {
	o := New(nil, nil, nil, nil, nil, Config{}, func() string { return "run-1" })
	got := o.Plan(nil, 0).prompt(host.NewLocal("local", 1),
		core.Project{Slug: "gravy", RepoPath: t.TempDir()}, nil, "what next")

	if strings.Contains(got, "no repository yet") {
		t.Errorf("told a project with a repository that it has none:\n%s", got)
	}
}

// Whatever the project, the planner is steered off chained shell commands, because a chain is
// refused whatever it contains.
func TestPlanPromptSteersAwayFromChainedCommands(t *testing.T) {
	o := New(nil, nil, nil, nil, nil, Config{}, func() string { return "run-1" })
	got := o.Plan(nil, 0).prompt(host.NewLocal("local", 1), core.Project{Slug: "gravy"}, nil, "hi")

	if !strings.Contains(got, "simple command") {
		t.Errorf("nothing steers the agent off chained shell commands:\n%s", got)
	}
	if !strings.Contains(got, "file tools") {
		t.Errorf("nothing points the agent at the file tools instead of the shell:\n%s", got)
	}
}
