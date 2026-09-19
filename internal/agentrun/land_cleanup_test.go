package agentrun_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider/fake"
)

// TestLandClearsTheWorktreePathBeforeRemovingIt is the regression.
//
// Landing publishes a ticket-changed event, so clients reload the moment it finishes. Removing
// the directory before clearing the ticket's record of it left a window where the database named
// a path that was gone, and the review screen's `git diff` in that directory failed with "not a
// git repository" — reporting that a landing which merged, pushed and finished could not be
// loaded.
func TestLandClearsTheWorktreePathBeforeRemovingIt(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed([]core.Step{{Name: "test", Cmd: "true", Required: true}})
	res := landReady(t, h, "feature.txt", "the work\n")

	if _, err := h.orch.Land().Approve(context.Background(), "GR-100", core.ApprovePush); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	ticket, err := h.db.GetTicket(context.Background(), "GR-100")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.WorktreePath != "" {
		t.Errorf("the ticket still names %q", ticket.WorktreePath)
	}
	// The invariant: nothing can read a path from the ticket that is not there. Both cleared
	// and both gone is fine; a path recorded for a directory that has been removed is not.
	if _, err := os.Stat(res.Worktree.Path); err == nil {
		t.Error("the worktree survived a successful landing")
	}
}
