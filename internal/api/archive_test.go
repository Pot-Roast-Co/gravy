package api

import (
	"context"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/store"
)

// landerStub stands in for the merge gate, making the state transitions a real landing makes so
// a test can see the dashboard on the other side of an approval.
type landerStub struct {
	db      *store.DB
	landed  []string
	failErr error
}

func (l *landerStub) Approve(ctx context.Context, ticketID string, _ core.Approval) (core.State, error) {
	if l.failErr != nil {
		return "", l.failErr
	}
	for _, ev := range []core.Event{core.EventApprove, core.EventLanded} {
		if _, err := l.db.SetTicketState(ctx, ticketID, ev); err != nil {
			return "", err
		}
	}
	if _, err := l.db.ResolveAttentionForTicket(ctx, ticketID); err != nil {
		return "", err
	}
	l.landed = append(l.landed, ticketID)
	return core.StateDone, nil
}

func (l *landerStub) Continue(ctx context.Context, ticketID string, how core.Approval) (core.State, error) {
	return l.Approve(ctx, ticketID, how)
}

// TestArchivingDoesNotStrandWorkInReview is the acceptance case that decides whether archiving
// is a working-set change or a kill switch.
//
// There are two ways to get this wrong and they pull in opposite directions. Filter archived
// projects out of everything the snapshot carries, and a ticket sitting in Review when somebody
// archived its project disappears mid-decision: still in the database, still holding a worktree
// and a branch, with no screen that will show it and no way to approve or reject it. Keep the
// project in the Projects collection instead, and archiving does not take the repository out of
// the working set at all — it comes back to the dashboard and the `p` cycle the moment anything
// is in flight, which is the one thing the caller asked archiving to stop.
//
// Both are avoidable, because the Needs You queue spans every project on its own: the project
// leaves the Projects collection immediately, and its in-flight ticket stays in Attention,
// approvable and landable, until it reaches Done or Rejected.
func TestArchivingDoesNotStrandWorkInReview(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()
	lander := &landerStub{db: db}
	svc = svc.WithLander(lander)

	// A queued ticket alongside the one in Review: archiving must stop the queue without
	// touching the decision waiting on a human.
	queued := core.Ticket{ID: "GR-2", ProjectID: "p1", Title: "next", State: core.StateBacklog,
		Route: core.RouteImplementation, CreatedAt: time.Now()}
	if err := db.CreateTicket(ctx, queued); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetTicketState(ctx, "GR-2", core.EventMarkReady); err != nil {
		t.Fatal(err)
	}

	if err := svc.ArchiveProject(ctx, "p1", true); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}

	st, err := svc.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Projects) != 0 {
		t.Fatalf("projects = %+v, want an archived project out of the default working set even "+
			"with work in flight", st.Projects)
	}
	if len(st.Ready) != 0 {
		t.Errorf("ready = %+v, want an archived project's queue off the dashboard", st.Ready)
	}
	// The in-flight decision is exactly what does not disappear.
	if len(st.Attention) != 1 || st.Attention[0].Attention.TicketID != "GR-1" {
		t.Fatalf("Needs You = %+v, want the pending review still in it", st.Attention)
	}
	if st.Attention[0].Project.ID != "p1" || !st.Attention[0].Project.Archived {
		t.Errorf("attention project = %+v, want the archived project resolved onto the row",
			st.Attention[0].Project)
	}

	// Asking for archived projects is how you see the project itself, and its held queue.
	withArchived, err := svc.Status(ctx, ProjectFilter{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withArchived.Projects) != 1 || withArchived.Projects[0].Project.ID != "p1" {
		t.Fatalf("IncludeArchived projects = %+v, want the archived project",
			withArchived.Projects)
	}
	if !withArchived.Projects[0].Project.Archived {
		t.Error("the snapshot reports the project as not archived")
	}
	if got := withArchived.Projects[0].Blocked; got != "project archived" {
		t.Errorf("blocked = %q, want the queue to say why it is idle", got)
	}
	if len(withArchived.Ready) != 1 || withArchived.Ready[0].Held != "project archived" {
		t.Errorf("IncludeArchived ready = %+v, want the queued ticket held and saying why",
			withArchived.Ready)
	}

	// The evidence a reviewer judges on is still assembled for an archived project.
	if _, err := svc.GetReview(ctx, "GR-1"); err != nil {
		t.Fatalf("GetReview on an archived project: %v", err)
	}

	// Approvable and landable: the whole point.
	if _, err := svc.Approve(ctx, "GR-1", core.ApprovePush); err != nil {
		t.Fatalf("Approve in an archived project: %v", err)
	}
	if len(lander.landed) != 1 {
		t.Errorf("landed %v, want GR-1 to have gone through the gate", lander.landed)
	}
	got, err := db.GetTicket(ctx, "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.StateDone {
		t.Errorf("state = %q, want done", got.State)
	}

	// The landing resolved the last thing that needed a human, so now the project is absent
	// from the snapshot entirely rather than merely absent from its Projects collection.
	st, err = svc.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Projects) != 0 {
		t.Errorf("projects = %+v, want the archived project gone once its work is done", st.Projects)
	}
	if len(st.Ready) != 0 {
		t.Errorf("ready = %+v, want an archived project's queue off the dashboard", st.Ready)
	}
	if len(st.Attention) != 0 {
		t.Errorf("Needs You = %+v, want nothing left once the review landed", st.Attention)
	}

	// Nothing was thrown away, and a caller that asks still sees all of it.
	all, err := svc.Status(ctx, ProjectFilter{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Projects) != 1 || all.Projects[0].Counts[core.StateDone] != 1 {
		t.Errorf("IncludeArchived = %+v, want the project with its landed ticket", all.Projects)
	}

	// Unarchiving is the whole undo.
	if err := svc.ArchiveProject(ctx, "p1", false); err != nil {
		t.Fatal(err)
	}
	st, err = svc.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Projects) != 1 || st.Projects[0].Blocked != "" {
		t.Errorf("after unarchiving, projects = %+v, want the project back and unblocked", st.Projects)
	}
	if len(st.Ready) != 1 || st.Ready[0].Held != "" {
		t.Errorf("after unarchiving, ready = %+v, want the queue released", st.Ready)
	}
}

// TestArchivedProjectStillReportsWhatIsRunning is the case a fix for the Projects collection is
// most likely to take down with it.
//
// Taking the project out of the working set means dropping it from that one collection. It does
// not mean skipping the pass that collects what is in flight inside it: an agent that is part way
// through writing code in a project somebody just archived, with no row on any screen, is a
// process nobody can see, wait for, or decide about. Archiving is not a kill switch, and a run
// that cannot be observed is worse than one that was killed outright.
func TestArchivedProjectStillReportsWhatIsRunning(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	inFlight := core.Ticket{ID: "GR-3", ProjectID: "p1", Title: "mid-flight",
		State: core.StateBacklog, Route: core.RouteImplementation, CreatedAt: time.Now()}
	if err := db.CreateTicket(ctx, inFlight); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []core.Event{core.EventMarkReady, core.EventAssign, core.EventStart} {
		if _, err := db.SetTicketState(ctx, "GR-3", ev); err != nil {
			t.Fatalf("advance with %s: %v", ev, err)
		}
	}

	if err := svc.ArchiveProject(ctx, "p1", true); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}

	st, err := svc.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Projects) != 0 {
		t.Errorf("projects = %+v, want the archived project out of the working set", st.Projects)
	}
	if len(st.Running) != 1 || st.Running[0].Ticket.ID != "GR-3" {
		t.Fatalf("running = %+v, want the in-flight ticket still reported", st.Running)
	}
	if st.Running[0].Project.ID != "p1" {
		t.Errorf("running project = %+v, want the archived project resolved onto the row",
			st.Running[0].Project)
	}
	if st.Running[0].Activity == "" {
		t.Error("running row has no activity, so the row says nothing about what is happening")
	}
}

// TestArchiveIsNotAFieldUpdateProjectWrites: the explicit method exists so that taking a project
// out of the working set is a recorded act and not something that happens on the way past.
//
// The Projects screen loads a project, a human edits its notes for a while, and somebody else
// archives it from the CLI in the meantime. Saving those notes must not carry the stale flag
// back in and quietly unarchive the project.
func TestArchiveIsNotAFieldUpdateProjectWrites(t *testing.T) {
	l, repo, _ := setupFixture(t, "go.mod")
	ctx := context.Background()

	p, err := l.AddProject(ctx, AddProjectReq{Path: repo})
	if err != nil {
		t.Fatal(err)
	}
	stale := p // loaded before the archive, so Archived is false on this copy

	if err := l.ArchiveProject(ctx, p.ID, true); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}

	stale.Notes = "edited from a screen holding the old flag"
	if err := l.UpdateProject(ctx, stale); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	got, err := l.db.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Archived {
		t.Error("saving an edit unarchived the project")
	}
	if got.Notes != stale.Notes {
		t.Errorf("notes = %q, want the edit saved", got.Notes)
	}
}

// TestListProjectsDefaultsToTheWorkingSet, with everything that needs the whole list still
// getting it. A ticket does not stop existing because its repository is finished.
func TestListProjectsDefaultsToTheWorkingSet(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	if err := db.CreateProject(ctx, core.Project{ID: "p2", Slug: "other", Name: "other",
		RepoPath: "/repo2", TargetBranch: "main", MergeMode: core.LandMerge,
		MaxConcurrency: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ArchiveProject(ctx, "p1", true); err != nil {
		t.Fatal(err)
	}

	working, err := svc.ListProjects(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(working) != 1 || working[0].ID != "p2" {
		t.Errorf("default listing = %+v, want only the working set", working)
	}
	all, err := svc.ListProjects(ctx, ProjectFilter{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("IncludeArchived listing = %d, want 2", len(all))
	}

	// The unfiltered ticket listing reads every project, or an archived project's tickets
	// would drop out of history the moment it left the working set.
	tickets, err := svc.ListTickets(ctx, TicketFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 || tickets[0].ID != "GR-1" {
		t.Errorf("tickets = %+v, want the archived project's ticket still listed", tickets)
	}
}
