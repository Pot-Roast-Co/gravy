package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/store"
)

// atReview returns a service holding one ticket awaiting judgement, with an open queue entry.
func atReview(t *testing.T) (*Local, *store.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "gravy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	p := core.Project{ID: "p1", Slug: "proj", Name: "proj", RepoPath: "/repo",
		TargetBranch: "main", MergeMode: core.LandMerge, MaxConcurrency: 1, CreatedAt: time.Now()}
	if err := db.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	tk := core.Ticket{ID: "GR-1", ProjectID: "p1", Title: "do the thing",
		State: core.StateBacklog, Route: core.RouteImplementation, CreatedAt: time.Now()}
	if err := db.CreateTicket(ctx, tk); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []core.Event{
		core.EventMarkReady, core.EventAssign, core.EventStart,
		core.EventAgentFinished, core.EventValidationPassed, core.EventReviewed,
	} {
		if _, err := db.SetTicketState(ctx, "GR-1", ev); err != nil {
			t.Fatalf("advance with %s: %v", ev, err)
		}
	}
	if err := db.OpenAttention(ctx, core.Attention{ID: "a1", ProjectID: "p1", TicketID: "GR-1",
		Reason: core.ReasonReviewPending, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// A counter, not a constant: more than one CreateTicket in a test would otherwise collide
	// on the primary key and fail for a reason that has nothing to do with the test.
	var n int
	return NewLocal(db, nil, nil, func() string { n++; return fmt.Sprintf("id-%d", n) }), db
}

// TestRequestChangesCarriesTheNoteForward is AC5's real content: a retry with no new information
// usually produces the same output, so the note is the whole value of sending work back.
func TestRequestChangesCarriesTheNoteForward(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	if err := svc.RequestChanges(ctx, "GR-1", "handle the zero case"); err != nil {
		t.Fatalf("RequestChanges: %v", err)
	}

	got, err := db.GetTicket(ctx, "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Feedback != "handle the zero case" {
		t.Errorf("feedback = %q, want it stored on the ticket", got.Feedback)
	}
	if got.State != core.StateReady {
		t.Errorf("state = %q, want ready so the agent picks it up again", got.State)
	}
	// The worktree is deliberately preserved; the agent continues where it left off.
	open, err := db.ListOpenAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("queue still holds %+v after the ticket went back to the agent", open)
	}
}

// TestRequestChangesRefusesAnEmptyNote guards the same invariant from the other side.
func TestRequestChangesRefusesAnEmptyNote(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	if err := svc.RequestChanges(ctx, "GR-1", "   "); err == nil {
		t.Fatal("an empty note was accepted")
	}
	got, _ := db.GetTicket(ctx, "GR-1")
	if got.State != core.StateReview {
		t.Errorf("state = %q, want the ticket left where it was", got.State)
	}
}

// TestRejectClosesTheTicketAndTheQueueEntry is AC6, minus the worktree removal, which needs a
// host this test deliberately does not give it.
func TestRejectClosesTheTicketAndTheQueueEntry(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	if err := svc.Reject(ctx, "GR-1"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	got, _ := db.GetTicket(ctx, "GR-1")
	if got.State != core.StateRejected {
		t.Errorf("state = %q, want rejected", got.State)
	}
	open, _ := db.ListOpenAttention(ctx)
	if len(open) != 0 {
		t.Errorf("a rejected ticket is still in Needs You: %+v", open)
	}
}

// TestApproveNeedsALander is the invariant that nothing merges by accident: a service built
// without the gate cannot land work, it fails loudly.
func TestApproveNeedsALander(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	err := svc.Approve(ctx, "GR-1")
	if err == nil {
		t.Fatal("a service with no lander approved and landed work")
	}
	if !strings.Contains(err.Error(), "cannot land") {
		t.Errorf("error = %v, want it to say why", err)
	}
	got, _ := db.GetTicket(ctx, "GR-1")
	if got.State != core.StateReview {
		t.Errorf("state = %q, want the ticket untouched", got.State)
	}
}

// TestGetReviewWithoutAWorktreeStillAnswers: a ticket with nothing checked out is a real state,
// not an error the reviewer should be shown.
func TestGetReviewWithoutAWorktreeStillAnswers(t *testing.T) {
	svc, _ := atReview(t)

	rb, err := svc.GetReview(context.Background(), "GR-1")
	if err != nil {
		t.Fatalf("GetReview: %v", err)
	}
	if rb.Ticket.ID != "GR-1" || rb.Project.Slug != "proj" {
		t.Errorf("bundle did not resolve ticket and project: %+v", rb)
	}
	if len(rb.Diff.Files) != 0 {
		t.Errorf("diff = %+v, want empty with no worktree", rb.Diff.Files)
	}
}

// TestReadyIsAllowedWhileADependencyIsUnlanded, with the reason still on the record.
//
// GR-026 AC3 refused this. The reasoning was that such a ticket would look eligible while the
// scheduler passed over it — but the scheduler holds a dependent until its dependency is Done
// and says so, and the queue screens draw the "waiting on ..." line from ListQueue. Refusing
// only meant that a plan arriving as a chain of four tickets had to be queued one at a time, as
// each predecessor landed.
func TestReadyIsAllowedWhileADependencyIsUnlanded(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	dep := core.Ticket{ID: "DEP-1", ProjectID: "p1", Title: "first", State: core.StateBacklog,
		Route: core.RouteImplementation, CreatedAt: time.Now()}
	blocked := core.Ticket{ID: "GR-2", ProjectID: "p1", Title: "second", State: core.StateBacklog,
		Route: core.RouteImplementation, CreatedAt: time.Now()}
	for _, tk := range []core.Ticket{dep, blocked} {
		if err := db.CreateTicket(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddDep(ctx, "GR-2", "DEP-1"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.MoveTicket(ctx, "GR-2", core.EventMarkReady); err != nil {
		t.Fatalf("MoveTicket: %v", err)
	}
	got, _ := db.GetTicket(ctx, "GR-2")
	if got.State != core.StateReady {
		t.Fatalf("state = %q, want the ticket queued", got.State)
	}

	// Queued is not the same as startable, and the screens have to be able to say which. A
	// Ready ticket with an unlanded dependency that reported nothing would be the failure the
	// old refusal was guarding against.
	details, err := svc.ListQueue(ctx, TicketFilter{State: core.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range details {
		if d.Ticket.ID != "GR-2" {
			continue
		}
		found = true
		if !strings.Contains(d.Blocked, "DEP-1") {
			t.Errorf("blocked = %q, want it to name the dependency", d.Blocked)
		}
	}
	if !found {
		t.Error("the queued ticket is not in the Ready queue")
	}
}

// TestDeleteOnlyRemovesUnstartedWork: delete is for work that should never have been written
// down. Anything further along has a run, a worktree or a branch behind it, and rejecting is the
// decision worth recording.
func TestDeleteOnlyRemovesUnstartedWork(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	if err := svc.DeleteTicket(ctx, "GR-1"); err == nil {
		t.Error("a ticket in review was deleted outright")
	}
	if _, err := db.GetTicket(ctx, "GR-1"); err != nil {
		t.Errorf("the ticket was removed anyway: %v", err)
	}

	fresh := core.Ticket{ID: "GR-9", ProjectID: "p1", Title: "never mind",
		State: core.StateBacklog, Route: core.RouteImplementation, CreatedAt: time.Now()}
	if err := db.CreateTicket(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteTicket(ctx, "GR-9"); err != nil {
		t.Fatalf("DeleteTicket on a backlog ticket: %v", err)
	}
	if _, err := db.GetTicket(ctx, "GR-9"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the backlog ticket survived deletion: %v", err)
	}
}

// TestListQueueResolvesDependencies covers what the queue screens render.
func TestListQueueResolvesDependencies(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	for _, tk := range []core.Ticket{
		{ID: "A", ProjectID: "p1", Title: "first", State: core.StateBacklog, Route: core.RouteImplementation, CreatedAt: time.Now()},
		{ID: "B", ProjectID: "p1", Title: "second", State: core.StateBacklog, Route: core.RouteImplementation, CreatedAt: time.Now()},
	} {
		if err := db.CreateTicket(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddDep(ctx, "B", "A"); err != nil {
		t.Fatal(err)
	}

	items, err := svc.ListQueue(ctx, TicketFilter{State: core.StateBacklog})
	if err != nil {
		t.Fatal(err)
	}
	var b *TicketDetail
	for i := range items {
		if items[i].Ticket.ID == "B" {
			b = &items[i]
		}
	}
	if b == nil {
		t.Fatalf("B is not in the backlog listing: %+v", items)
	}
	if len(b.DependsOn) != 1 || b.DependsOn[0].ID != "A" {
		t.Errorf("dependencies = %+v, want A", b.DependsOn)
	}
	if !strings.Contains(b.Blocked, "A") {
		t.Errorf("blocked = %q, want it to name A", b.Blocked)
	}
	if b.Project.Slug != "proj" {
		t.Errorf("project not resolved: %+v", b.Project)
	}
}
