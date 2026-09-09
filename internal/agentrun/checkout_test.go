package agentrun_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/provider/fake"
)

func TestEditorReviewIncludesEarlierAttempts(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed(nil)
	res := landReady(t, h, "first.txt", "first attempt\n")
	writeFile(t, res.Worktree.Path, "second.txt", "second attempt\n")
	gitCmd(t, h.h, res.Worktree.Path, "add", ".")
	gitCmd(t, h.h, res.Worktree.Path, "commit", "-m", "second attempt")
	path, err := h.orch.Checkouts().ReviewCheckout(context.Background(), "GR-100")
	if err != nil {
		t.Fatal(err)
	}
	status := gitCmd(t, h.h, path, "status", "--porcelain")
	if !strings.Contains(status, "first.txt") || !strings.Contains(status, "second.txt") {
		t.Fatalf("editor omitted an attempt: %s", status)
	}
}
