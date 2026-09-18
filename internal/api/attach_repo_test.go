package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/store"
)

// gitRepo makes a real repository with a default branch and one commit.
func gitRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	h := host.NewLocal("local", 1)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", branch}, {"add", "."},
		{"-c", "user.name=T", "-c", "user.email=t@example.test", "commit", "-m", "initial"},
	} {
		if _, code, err := runGit(context.Background(), h, dir, args...); err != nil || code != 0 {
			t.Fatalf("git %v: %d %v", args, code, err)
		}
	}
	return dir
}

func projectWithoutARepo(t *testing.T) (*Local, *store.DB, core.Project) {
	t.Helper()
	l, _, _ := setupFixture(t, "go.mod")
	p, err := l.AddProject(context.Background(), AddProjectReq{Name: "fantasyHockeyAid"})
	if err != nil {
		t.Fatal(err)
	}
	return l, nil, p
}

// TestAttachRepositoryIsTheGoalBecomingAProject is the regression.
//
// Planning a project with no repository is supported and produces implementation tickets. Those
// tickets cannot start — the scheduler refuses them for having nowhere to work — and the only
// route to giving the project a repository was to delete it and register it again, which took
// the plan with it.
func TestAttachRepositoryIsTheGoalBecomingAProject(t *testing.T) {
	l, _, p := projectWithoutARepo(t)
	if p.RepoPath != "" {
		t.Fatalf("fixture already has a repository: %q", p.RepoPath)
	}
	repo := gitRepo(t, "main")

	saved, err := l.AttachRepository(context.Background(), p.ID, repo)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RepoPath != repo {
		t.Fatalf("RepoPath = %q, want %q", saved.RepoPath, repo)
	}
	// The two things a project could not have without a repository.
	if saved.TargetBranch != "main" {
		t.Errorf("TargetBranch = %q, want the repository's default", saved.TargetBranch)
	}
	if len(saved.Allowlist.Commands) == 0 {
		t.Error("no allowlist was detected, so every command the agent runs is refused")
	}

	// And it is persisted, not just returned.
	all, err := l.ListProjects(context.Background(), ProjectFilter{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	var reloaded core.Project
	for _, x := range all {
		if x.ID == p.ID {
			reloaded = x
		}
	}
	if reloaded.RepoPath != repo {
		t.Errorf("reloaded RepoPath = %q, want %q", reloaded.RepoPath, repo)
	}
}

// Moving an existing checkout is still refused: that is what would orphan the worktrees,
// branches and runs already recorded against it.
func TestAttachRepositoryRefusesToMoveAnExistingOne(t *testing.T) {
	l, _, p := projectWithoutARepo(t)
	first := gitRepo(t, "main")
	if _, err := l.AttachRepository(context.Background(), p.ID, first); err != nil {
		t.Fatal(err)
	}

	second := gitRepo(t, "main")
	_, err := l.AttachRepository(context.Background(), p.ID, second)
	if err == nil {
		t.Fatal("moving a project's checkout was allowed")
	}
	if !strings.Contains(err.Error(), "already has a repository") {
		t.Errorf("error = %v, want it to name the reason", err)
	}
}

// A human's own choice is never overwritten by detection.
func TestAttachRepositoryKeepsAChosenTargetBranch(t *testing.T) {
	l, _, p := projectWithoutARepo(t)
	p.TargetBranch = "release"
	if err := l.UpdateProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	saved, err := l.AttachRepository(context.Background(), p.ID, gitRepo(t, "main"))
	if err != nil {
		t.Fatal(err)
	}
	if saved.TargetBranch != "release" {
		t.Errorf("TargetBranch = %q; detection overwrote a chosen value", saved.TargetBranch)
	}
}

// A directory that is not a repository is refused here rather than at the first git command of
// the first run.
func TestAttachRepositoryRefusesANonRepository(t *testing.T) {
	l, _, p := projectWithoutARepo(t)
	if _, err := l.AttachRepository(context.Background(), p.ID, t.TempDir()); err == nil {
		t.Fatal("attached a directory that is not a git repository")
	}
	if _, err := l.AttachRepository(context.Background(), p.ID, "/definitely/not/here"); err == nil {
		t.Fatal("attached a directory that does not exist")
	}
}

// TestReadyWorkSaysWhyItIsNotStarting is the other half.
//
// The scheduler refuses a project with no repository before it looks at anything else, and used
// to do it silently. Seven planned tickets sat in Ready looking like work about to start, with
// nothing anywhere saying the one thing standing in the way. Status promises otherwise in its
// own comment: an idle queue is always explained, never merely idle.
func TestReadyWorkSaysWhyItIsNotStarting(t *testing.T) {
	l, _, p := projectWithoutARepo(t)
	ctx := context.Background()

	ready, err := l.CreateTicket(ctx, CreateTicketReq{
		ProjectID: p.ID, Title: "Scaffold the app", Ready: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	st, err := l.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, q := range st.Ready {
		if q.Ticket.ID != ready.ID {
			continue
		}
		found = true
		if q.Held == "" {
			t.Fatal("a ticket that cannot start is listed with no explanation")
		}
		if !strings.Contains(q.Held, "no repository") {
			t.Errorf("Held = %q, want it to name the missing repository", q.Held)
		}
		// And it says what to do about it, since the answer is one command.
		if !strings.Contains(q.Held, "set-repo") {
			t.Errorf("Held = %q, want it to name the way out", q.Held)
		}
	}
	if !found {
		t.Fatal("the ready ticket is not in Status at all, which is worse than unexplained")
	}
}

// Once it has a repository the explanation goes away, rather than becoming a permanent label.
func TestReadyWorkStopsExplainingOnceAttached(t *testing.T) {
	l, _, p := projectWithoutARepo(t)
	ctx := context.Background()
	if _, err := l.CreateTicket(ctx, CreateTicketReq{ProjectID: p.ID, Title: "Scaffold", Ready: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AttachRepository(ctx, p.ID, gitRepo(t, "main")); err != nil {
		t.Fatal(err)
	}

	st, err := l.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range st.Ready {
		if strings.Contains(q.Held, "no repository") {
			t.Errorf("still says %q after a repository was attached", q.Held)
		}
	}
}
