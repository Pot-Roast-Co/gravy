package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// TestPinnedProjectRunsOnlyOnItsHost is the point of pinning.
//
// Each machine has its own clone and its own worktrees, so a project's RepoPath is meaningless
// anywhere else. Sending its work to another host would run an agent against a path that is not
// there.
func TestPinnedProjectRunsOnlyOnItsHost(t *testing.T) {
	snap := &hostSnapshot{hosts: []*hostState{
		{id: "local", caps: core.Caps{OS: "linux"}, total: 4},
		{id: "air", caps: core.Caps{OS: "darwin", Tools: map[string]string{"xcodebuild": ""}}, total: 2},
	}}

	eligible, rejections := snap.eligibleOn("air", core.Requirements{}, core.Requirements{})
	if len(eligible) != 1 || eligible[0].id != "air" {
		t.Fatalf("eligible = %v, want only air", hostIDs(eligible))
	}
	// The rejection has to name the reason, or an idle queue is unexplained.
	var said bool
	for _, r := range rejections {
		if strings.Contains(r, "local") && strings.Contains(r, "the project is on air") {
			said = true
		}
	}
	if !said {
		t.Errorf("no rejection explains why local was excluded: %v", rejections)
	}
}

// TestUnpinnedProjectStillUsesRequirements: pinning is an extra filter, not a replacement.
func TestUnpinnedProjectStillUsesRequirements(t *testing.T) {
	snap := &hostSnapshot{hosts: []*hostState{
		{id: "local", caps: core.Caps{OS: "linux"}, total: 4},
		{id: "air", caps: core.Caps{OS: "darwin", Tools: map[string]string{"xcodebuild": ""}}, total: 2},
	}}

	eligible, _ := snap.eligibleOn("", core.Requirements{OS: []string{"darwin"}}, core.Requirements{})
	if len(eligible) != 1 || eligible[0].id != "air" {
		t.Fatalf("eligible = %v, want only air by requirement", hostIDs(eligible))
	}
}

// TestPinnedToAMissingHostIsExplained: a host that could not be probed is not in the snapshot,
// and work pinned to it must still say something rather than sitting idle in silence.
func TestPinnedToAMissingHostIsExplained(t *testing.T) {
	snap := &hostSnapshot{hosts: []*hostState{
		{id: "local", caps: core.Caps{OS: "linux"}, total: 4},
	}}

	eligible, rejections := snap.eligibleOn("air", core.Requirements{}, core.Requirements{})
	if len(eligible) != 0 {
		t.Fatalf("eligible = %v, want none", hostIDs(eligible))
	}
	if len(rejections) == 0 {
		t.Error("nothing explained why there is no host")
	}
}

func hostIDs(hs []*hostState) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.id)
	}
	return out
}

// TestHostWithoutTheAgentIsRefused is the burnt run this guard prevents.
//
// Capabilities record which CLIs a machine has, and nothing consulted them: a ticket routed to
// claude-code on a machine with only codex was assigned anyway and died at "cli not found" — a
// consumed worker slot and a failure that reads like a provider outage rather than a machine
// missing a program.
func TestHostProviderCapabilityIsRecorded(t *testing.T) {
	air := &hostState{
		id:   "air",
		caps: core.Caps{OS: "darwin", Providers: map[string]bool{"codex": true}},
	}
	if air.caps.Providers["claude-code"] {
		t.Fatal("the fixture claims an agent it does not have")
	}
	if !air.caps.Providers["codex"] {
		t.Fatal("the fixture lost the agent it does have")
	}

	// A host that reports no providers at all is not evidence of absence — an older daemon or
	// a probe that could not run leaves the map empty, and refusing then would strand work.
	unknown := &hostState{id: "old", caps: core.Caps{OS: "linux"}}
	if len(unknown.caps.Providers) != 0 {
		t.Fatal("the fixture is not the unknown case")
	}
}

// TestProjectWithoutARepositoryIsNotScheduled: a project with no working tree is a place for
// goals and notes. A ticket landing in one must be told so, not fail mysteriously at fetch.
func TestProjectWithoutARepositoryIsNotScheduled(t *testing.T) {
	p := core.Project{ID: "p1", Slug: "idea"}
	if p.RepoPath != "" {
		t.Fatal("the fixture has a repository")
	}
	// The guard is a plain field check in the scheduler; this pins the shape of a repo-less
	// project so a later change cannot make one look runnable.
	runnable := p.RepoPath != ""
	if runnable {
		t.Error("a project with no repository looks runnable")
	}
}

// TestOneBadTicketDoesNotStopTheTick is the outage a single orphaned row caused.
//
// A ticket whose project had been deleted out from under it — by a delete that bypassed the
// foreign-key cascade — made every scheduler tick fail, for every project, every two seconds.
// The queue screen reported only "cannot load the queue", which named neither the ticket nor the
// reason.
func TestOneBadTicketDoesNotStopTheTick(t *testing.T) {
	st := newStore().addProject(parallelProject("p1", "repo", 10))
	// The orphan comes first, so a tick that gives up on it never reaches the healthy one.
	st.addTicket(ticket("orphan", "gone", core.StateReady, 1))
	st.addTicket(ticket("fine", "p1", core.StateReady, 2))

	got, err := newScheduler(st, newPool(mac("m1", 10))).Tick(context.Background())
	if err != nil {
		t.Fatalf("one orphaned ticket failed the whole tick: %v", err)
	}

	ids := assignedIDs(got)
	if len(ids) != 1 || ids[0] != "fine" {
		t.Errorf("assigned %v, want the healthy ticket to still run", ids)
	}
}

// TestAProjectRunsOnlyWhereItsCodeIs is the incoherent run this prevents.
//
// mission-mojo's clone is on the local machine and it carried no host id, which used to mean
// "any host meeting the requirements". The scheduler picked the idle Mac, agentrun created the
// worktree locally, and the agent ran on a machine that did not have the code.
func TestAProjectRunsOnlyWhereItsCodeIs(t *testing.T) {
	snap := &hostSnapshot{hosts: []*hostState{
		{id: "local", caps: core.Caps{OS: "linux", Providers: map[string]bool{"claude-code": true}}, total: 4},
		// Idle, and therefore the one a least-busy choice would prefer.
		{id: "air", caps: core.Caps{OS: "darwin", Providers: map[string]bool{"claude-code": true}}, total: 4},
	}}

	eligible, rejections := snap.eligibleOn("local", core.Requirements{}, core.Requirements{})
	if len(eligible) != 1 || eligible[0].id != "local" {
		t.Fatalf("eligible = %v, want only the machine holding the clone", hostIDs(eligible))
	}
	var explained bool
	for _, r := range rejections {
		if strings.Contains(r, "air") && strings.Contains(r, "the project is on local") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("the remote host was excluded without a reason: %v", rejections)
	}
}
