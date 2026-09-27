package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

// ticketWithWork registers the fixture repository as a project and puts a ticket in Review with
// a real worktree on a real branch.
func ticketWithWork(t *testing.T) (*Local, string, string, string) {
	t.Helper()
	l, repo, _ := setupFixture(t, "go.mod")
	ctx := context.Background()
	p, err := l.AddProject(ctx, AddProjectReq{Path: repo, Name: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	branch := "gravy/aaaa1111-some-work"
	if _, code, err := runGit(ctx, host.NewLocal("local", 1), repo, "worktree", "add", "-b", branch, wt); err != nil || code != 0 {
		t.Fatalf("worktree add: %d %v", code, err)
	}
	if err := l.db.CreateTicket(ctx, core.Ticket{ID: "T1", ProjectID: p.ID, Title: "some work",
		State: core.StateReview, Route: core.RouteImplementation, WorktreePath: wt, Branch: branch,
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return l, repo, wt, branch
}

// TestRejectDiscardsTheBranchToo is the regression: rejecting removed the worktree but left the
// ticket's branch in the repository for good, and nothing else ever deletes one.
func TestRejectDiscardsTheBranchToo(t *testing.T) {
	l, repo, wt, branch := ticketWithWork(t)
	ctx := context.Background()

	if err := l.Reject(ctx, "T1"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("the worktree survived the rejection")
	}
	if _, code, _ := runGit(ctx, host.NewLocal("local", 1), repo, "rev-parse", "--verify", "--quiet",
		"refs/heads/"+branch); code == 0 {
		t.Errorf("branch %s survived the rejection", branch)
	}
}

// A ticket that never ran has a branch name but no branch, and rejects cleanly.
func TestRejectWithoutABranchIsNotAnError(t *testing.T) {
	l, repo, _ := setupFixture(t, "go.mod")
	ctx := context.Background()
	p, err := l.AddProject(ctx, AddProjectReq{Path: repo, Name: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.db.CreateTicket(ctx, core.Ticket{ID: "T2", ProjectID: p.ID, Title: "never ran",
		State: core.StateReview, Route: core.RouteImplementation, Branch: "gravy/bbbb2222-never-made",
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := l.Reject(ctx, "T2"); err != nil {
		t.Errorf("Reject: %v", err)
	}
}

// TestDeleteProjectCleansUpItsWorkButNotTheRepository: deleting a project used to cascade its
// records away and leave its tickets' worktrees and branches behind, which nothing would ever
// delete after. The repository itself must be left alone.
func TestDeleteProjectCleansUpItsWorkButNotTheRepository(t *testing.T) {
	l, repo, wt, branch := ticketWithWork(t)
	ctx := context.Background()
	tk, _ := l.db.GetTicket(ctx, "T1")

	if err := l.DeleteProject(ctx, tk.ProjectID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("the ticket's worktree survived the project")
	}
	h := host.NewLocal("local", 1)
	if _, code, _ := runGit(ctx, h, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); code == 0 {
		t.Error("the ticket's branch survived the project")
	}
	if _, code, _ := runGit(ctx, h, repo, "rev-parse", "--verify", "--quiet", "refs/heads/main"); code != 0 {
		t.Error("deleting the project touched the repository's own branch")
	}
	if _, err := l.db.GetProject(ctx, tk.ProjectID); err == nil {
		t.Error("the project is still registered")
	}
}

// A project with an agent at work in it is not deleted out from under the agent.
func TestDeleteProjectRefusesWorkInFlight(t *testing.T) {
	l, _, _, _ := ticketWithWork(t)
	ctx := context.Background()
	tk, _ := l.db.GetTicket(ctx, "T1")
	tk.State = core.StateRunning
	if err := l.db.UpdateTicket(ctx, tk); err != nil {
		t.Fatal(err)
	}

	if err := l.DeleteProject(ctx, tk.ProjectID); err == nil {
		t.Fatal("a project was deleted with an agent running in it")
	}
	if _, err := l.db.GetProject(ctx, tk.ProjectID); err != nil {
		t.Error("the refused delete removed the project anyway")
	}
}
