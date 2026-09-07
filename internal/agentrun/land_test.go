package agentrun_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider/fake"
)

// landReady drives a ticket to Review so landing can be exercised, writing the given file into
// the worktree as the agent's work.
func landReady(t *testing.T, h *harness, file, body string) agentrun.Result {
	t.Helper()
	res, err := runWithAgentWork(t, h, file, body)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinalState != core.StateReview {
		t.Fatalf("ticket is %s, want review", res.FinalState)
	}
	return res
}

// TestLandCleanCase is AC1 and AC2: a squash commit on target, pushed, ticket Done, worktree
// gone — and no re-validation, because the branch was already on top of target.
func TestLandCleanCase(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed([]core.Step{{Name: "test", Cmd: "true", Required: true}})
	res := landReady(t, h, "feature.txt", "the work\n")

	land, err := h.orch.Land().Approve(context.Background(), "GR-100")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if land.State != core.StateDone {
		t.Fatalf("state = %s, want done", land.State)
	}
	if land.MergeCommit == "" {
		t.Error("no merge commit recorded")
	}
	if !land.Pushed {
		t.Error("the work was not pushed to the remote")
	}
	// AC2: nothing to re-validate when the target has not moved.
	if land.Revalidated {
		t.Error("re-validation ran although the branch was already on top of target")
	}

	// The squash commit is on target, in the main copy and upstream.
	log := gitCmd(t, h.h, h.repoPath, "log", "--oneline", "-1", "main")
	if !strings.Contains(log, "do the thing") {
		t.Errorf("target branch does not carry the ticket's squash commit: %s", log)
	}
	files := gitCmd(t, h.h, h.repoPath, "show", "--name-only", "--format=", "main")
	if !strings.Contains(files, "feature.txt") {
		t.Errorf("the merge does not contain the agent's work: %s", files)
	}
	remote := h.targetLog("--oneline", "-1", "main")
	if !strings.Contains(remote, "do the thing") {
		t.Errorf("the remote did not receive the merge: %s", remote)
	}

	// The worktree is removed once the work has landed.
	if _, err := os.Stat(res.Worktree.Path); !os.IsNotExist(err) {
		t.Error("the worktree survived a successful landing")
	}
	tk, _ := h.db.GetTicket(context.Background(), "GR-100")
	if tk.State != core.StateDone {
		t.Errorf("persisted state = %s, want done", tk.State)
	}
}

// TestReplayedRebaseRevalidates is AC3, and the point of the merge gate.
//
// Work that was green when reviewed can be red once the target moves underneath it.
// Green-against-yesterday is not evidence about today, so a rebase that replayed commits must
// re-run validation and refuse to merge when it fails.
func TestReplayedRebaseRevalidates(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	// Validation passes only while the sentinel the target will later remove still exists.
	h.seed([]core.Step{{Name: "test", Cmd: "test -f contract.txt", Required: true}})

	// The repo starts with the sentinel, so the ticket validates green.
	h.landOnTarget(func(dir string) {
		writeFile(t, dir, "contract.txt", "the interface the work depends on\n")
	}, "add the contract")
	gitCmd(t, h.h, h.repoPath, "pull", "-q", "origin", "main")

	landReady(t, h, "feature.txt", "work that depends on the contract\n")

	// While the ticket sat in review, someone removed what it depended on.
	h.landOnTarget(func(dir string) {
		if err := os.Remove(filepath.Join(dir, "contract.txt")); err != nil {
			t.Fatal(err)
		}
	}, "remove the contract")

	land, err := h.orch.Land().Approve(context.Background(), "GR-100")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if !land.Revalidated {
		t.Fatal("the target moved and commits were replayed, but validation was not re-run")
	}
	if land.State != core.StateNeedsYou {
		t.Fatalf("state = %s, want needs_you: work that is now red must not merge", land.State)
	}

	// Nothing reached the target.
	files := gitCmd(t, h.h, h.repoPath, "show", "--name-only", "--format=", "main")
	if strings.Contains(files, "feature.txt") {
		t.Error("red work was merged into the target branch")
	}

	open, err := h.db.ListOpenAttention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Reason != core.ReasonValidationFailed {
		t.Fatalf("attention = %+v, want validation_failed", open)
	}
}

// TestReplayedRebaseThatStaysGreenMerges: re-validation is a gate, not an obstacle.
func TestReplayedRebaseThatStaysGreenMerges(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed([]core.Step{{Name: "test", Cmd: "true", Required: true}})
	landReady(t, h, "feature.txt", "the work\n")

	// The target moves in a way that does not conflict and does not break anything.
	h.landOnTarget(func(dir string) {
		writeFile(t, dir, "unrelated.txt", "someone else's work\n")
	}, "unrelated change")

	land, err := h.orch.Land().Approve(context.Background(), "GR-100")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !land.Revalidated {
		t.Error("the target moved but validation was not re-run")
	}
	if land.State != core.StateDone {
		t.Fatalf("state = %s, want done", land.State)
	}
}

// TestConflictPreservesWorktree is AC4: Gravy stops and asks, attempting nothing further.
func TestConflictPreservesWorktree(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed(nil)

	res := landReady(t, h, "README.md", "the agent's version of the readme\n")

	// The target changes the same file, so the rebase cannot replay.
	h.landOnTarget(func(dir string) {
		writeFile(t, dir, "README.md", "someone else's version of the readme\n")
	}, "conflicting change")

	land, err := h.orch.Land().Approve(context.Background(), "GR-100")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if land.State != core.StateNeedsYou {
		t.Fatalf("state = %s, want needs_you", land.State)
	}
	if len(land.ConflictFiles) == 0 || land.ConflictFiles[0] != "README.md" {
		t.Errorf("conflicting files = %v, want [README.md]", land.ConflictFiles)
	}

	// The worktree must be preserved and usable: a human is about to resolve this by hand.
	if _, err := os.Stat(res.Worktree.Path); err != nil {
		t.Fatalf("the worktree was removed on conflict: %v", err)
	}
	status := gitCmd(t, h.h, res.Worktree.Path, "status", "--porcelain=v2", "--branch")
	if strings.Contains(status, "rebase") {
		t.Errorf("the worktree was left mid-rebase:\n%s", status)
	}
	if dirty := strings.TrimSpace(gitCmd(t, h.h, res.Worktree.Path, "status", "--porcelain")); dirty != "" {
		t.Errorf("the worktree is dirty after the aborted rebase:\n%s", dirty)
	}

	// Nothing was merged, and the attention row carries what a human needs.
	files := gitCmd(t, h.h, h.repoPath, "show", "--name-only", "--format=", "main")
	if strings.Contains(files, "the agent's version") {
		t.Error("conflicting work reached the target")
	}
	open, _ := h.db.ListOpenAttention(context.Background())
	if len(open) != 1 || open[0].Reason != core.ReasonMergeConflict {
		t.Fatalf("attention = %+v, want merge_conflict", open)
	}
	if open[0].Payload["worktree"] == nil {
		t.Error("the attention payload does not say where the worktree is")
	}
}

// TestResolveThenContinue is AC5.
func TestResolveThenContinue(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed(nil)
	res := landReady(t, h, "README.md", "the agent's version\n")

	h.landOnTarget(func(dir string) {
		writeFile(t, dir, "README.md", "someone else's version\n")
	}, "conflicting change")

	land, err := h.orch.Land().Approve(context.Background(), "GR-100")
	if err != nil {
		t.Fatal(err)
	}
	if land.State != core.StateNeedsYou {
		t.Fatalf("state = %s, want needs_you", land.State)
	}

	// The human resolves it in their own tools, in the preserved worktree.
	//
	// They rebase and fix the conflict, which is what actually makes the branch mergeable.
	// Committing a reconciled version on top would not: the original conflicting commit is
	// still in the branch, so Gravy's rebase would hit the same conflict again.
	rebase := gitCmdAllowFail(t, h.h, res.Worktree.Path, "rebase", "origin/main")
	if !strings.Contains(rebase, "CONFLICT") && !strings.Contains(rebase, "conflict") {
		t.Logf("rebase output: %s", rebase)
	}
	writeFile(t, res.Worktree.Path, "README.md", "the reconciled version\n")
	gitCmd(t, h.h, res.Worktree.Path, "add", "README.md")
	gitCmd(t, h.h, res.Worktree.Path, "rebase", "--continue")

	again, err := h.orch.Land().Continue(context.Background(), "GR-100")
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if again.State != core.StateDone {
		t.Fatalf("state after continue = %s, want done", again.State)
	}

	content := gitCmd(t, h.h, h.repoPath, "show", "main:README.md")
	if !strings.Contains(content, "reconciled") {
		t.Errorf("the target does not carry the resolved content: %q", content)
	}
}

// TestNothingMergesWithoutApproval is AC8, and the invariant the whole product rests on.
//
// Landing is reachable only from Review, and only by an explicit human action. No configuration
// flag changes that, which is why the check lives in the state machine rather than in a
// conditional somebody could later relax.
func TestNothingMergesWithoutApproval(t *testing.T) {
	for _, state := range []core.State{
		core.StateReady, core.StateAssigned, core.StateRunning,
		core.StateValidating, core.StateReviewing, core.StateNeedsYou, core.StateBlocked,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
			h.seed(nil)
			runWithAgentWork(t, h, "feature.txt", "work\n")

			// Force the ticket into the state under test.
			if err := forceState(t, h, "GR-100", state); err != nil {
				t.Skipf("cannot reach %s directly: %v", state, err)
			}

			before := gitCmd(t, h.h, h.repoPath, "rev-parse", "main")
			_, err := h.orch.Land().Approve(context.Background(), "GR-100")
			if err == nil {
				t.Fatalf("a ticket in %s was landed without passing through review", state)
			}
			after := gitCmd(t, h.h, h.repoPath, "rev-parse", "main")
			if before != after {
				t.Errorf("the target branch moved for a ticket in %s", state)
			}
		})
	}
}

// forceState writes a ticket's state directly, bypassing the machine, to set up a test.
func forceState(t *testing.T, h *harness, id string, state core.State) error {
	t.Helper()
	_, err := h.db.SQL().ExecContext(context.Background(),
		`UPDATE tickets SET state = ? WHERE id = ?`, string(state), id)
	return err
}

// TestApproveRequiresReview: the error names the actual state rather than failing obscurely.
func TestApproveRequiresReview(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed(nil)

	_, err := h.orch.Land().Approve(context.Background(), "GR-100")
	if err == nil {
		t.Fatal("a Ready ticket was approved")
	}
	if !strings.Contains(err.Error(), "ready") {
		t.Errorf("error = %v, want it to name the ticket's actual state", err)
	}
}

// TestDependentBecomesEligibleOnlyAfterDone is AC6.
func TestDependentBecomesEligibleOnlyAfterDone(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed(nil)
	ctx := context.Background()

	dependent := core.Ticket{
		ID: "GR-101", ProjectID: "p1", Title: "depends on the first",
		State: core.StateReady, Route: core.RouteImplementation,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := h.db.CreateTicket(ctx, dependent); err != nil {
		t.Fatal(err)
	}
	if err := h.db.AddDep(ctx, "GR-101", "GR-100"); err != nil {
		t.Fatal(err)
	}

	landReady(t, h, "feature.txt", "the work\n")

	deps, err := h.db.DepsOf(ctx, "GR-101")
	if err != nil || len(deps) != 1 {
		t.Fatalf("deps = %v, %v", deps, err)
	}
	blocker, err := h.db.GetTicket(ctx, deps[0])
	if err != nil {
		t.Fatal(err)
	}
	if blocker.State == core.StateDone {
		t.Fatal("the dependency reached Done before it was approved")
	}

	if _, err := h.orch.Land().Approve(ctx, "GR-100"); err != nil {
		t.Fatal(err)
	}
	blocker, _ = h.db.GetTicket(ctx, "GR-100")
	if blocker.State != core.StateDone {
		t.Fatalf("dependency state = %s, want done", blocker.State)
	}
}

var _ = filepath.Join

// TestApproveClearsTheQueue is the other half of TestReviewEntersNeedsYouQueue.
//
// A row that outlives the judgement it asked for turns Needs You into a list of work already
// done, which is the same loss of trust as omitting work that is waiting.
func TestApproveClearsTheQueue(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed([]core.Step{{Name: "test", Cmd: "true", Required: true}})
	landReady(t, h, "feature.txt", "the work\n")

	ctx := context.Background()
	open, err := h.db.ListOpenAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Reason != core.ReasonReviewPending {
		t.Fatalf("before approval the queue = %+v, want one review_pending", open)
	}

	if _, err := h.orch.Land().Approve(ctx, "GR-100"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if open, err = h.db.ListOpenAttention(ctx); err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("queue still holds %+v after the ticket was approved and landed", open)
	}
}
