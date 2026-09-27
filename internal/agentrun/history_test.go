package agentrun_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider/fake"
	"github.com/pot-roast-co/gravy/internal/review"
)

// historyLander exercises the same API boundary as the daemon's lander adapter.
type historyLander struct{ lander *agentrun.Lander }

func (l historyLander) Approve(ctx context.Context, id string, how core.Approval) (core.State, error) {
	r, err := l.lander.Approve(ctx, id, how)
	return r.State, err
}
func (l historyLander) Continue(ctx context.Context, id string, how core.Approval) (core.State, error) {
	r, err := l.lander.Continue(ctx, id, how)
	return r.State, err
}

func TestHistoryFromCreatedToLanded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool
		how   core.Approval
		park  bool
	}{
		{"queued then merged", false, core.ApproveLocal, false},
		{"created ready then merged manually", true, core.ApproveHandOff, false},
		{"parked landing continued", false, core.ApproveLocal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			script := successScript()
			cost := 0.25
			script.Outcome.CostUSD = &cost
			h := newHarness(t, []fake.Script{script}, agentrun.Config{RunTimeout: time.Minute})
			p, _ := h.seed([]core.Step{{Name: "test", Cmd: "true", Required: true}})
			h.orch.WithReviewer(review.New(passingReviewModel{}, 0))
			svc := api.NewLocal(h.db, nil, nil, h.ids.next).WithLander(historyLander{h.orch.Land()})
			ticket, err := svc.CreateTicket(ctx, api.CreateTicketReq{ProjectID: p.ID, Title: "history", Ready: tc.ready})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.ready {
				if _, err := svc.MoveTicket(ctx, ticket.ID, core.EventMarkReady); err != nil {
					t.Fatal(err)
				}
			}
			h.work.set(map[string]string{"history.txt": "one line\n"})
			a := h.assignment()
			a.TicketID = ticket.ID
			res, err := h.orch.Run(ctx, a)
			if err != nil || res.FinalState != core.StateReview {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			if tc.park {
				writeFile(t, res.Worktree.Path, "history.txt", "dirty\n")
			}
			state, err := svc.Approve(ctx, ticket.ID, tc.how)
			if err != nil {
				t.Fatal(err)
			}
			if tc.park {
				if state != core.StateNeedsYou {
					t.Fatalf("state = %s", state)
				}
				writeFile(t, res.Worktree.Path, "history.txt", "one line\n")
				state, err = svc.Continue(ctx, ticket.ID, tc.how)
			}
			if tc.how == core.ApproveHandOff {
				if state != core.StateHandedOff {
					t.Fatalf("state = %s", state)
				}
				state, err = svc.MarkMerged(ctx, ticket.ID)
			}
			if err != nil || state != core.StateDone {
				t.Fatalf("land = %s, %v", state, err)
			}

			type event struct {
				kind  core.ActivityKind
				actor string
			}
			want := []event{
				{core.KindCreated, core.ActorHuman}, {core.KindQueued, core.ActorHuman},
				{core.KindFetch, core.ActorGravy}, {core.KindWorktree, core.ActorGravy},
				{core.KindPrompt, core.ActorGravy},
				{core.KindAgentStart, core.AgentActor("fake", "m")},
				{core.KindAgentExit, core.AgentActor("fake", "m")},
				{core.KindCommit, core.ActorGravy},
				{core.KindValidationStep, core.ActorGravy}, {core.KindValidationStep, core.ActorGravy},
				{core.KindSummary, core.ActorGravy}, {core.KindReview, core.ActorGravy},
				{core.KindVerdict, core.ActorGravy}, {core.KindReview, core.ActorGravy},
				{core.KindHandoff, core.ActorGravy}, {core.KindApproved, core.ActorHuman},
				{core.KindLanding, core.ActorGravy}, {core.KindFetch, core.ActorGravy},
				{core.KindWorktree, core.ActorGravy}, {core.KindWorktree, core.ActorGravy},
				{core.KindValidationStep, core.ActorGravy}, {core.KindValidationStep, core.ActorGravy},
			}
			if tc.how == core.ApproveHandOff {
				want = append(want, event{core.KindHandoff, core.ActorGravy}, event{core.KindLanded, core.ActorHuman})
			} else {
				want = append(want, event{core.KindLanded, core.ActorGravy})
			}
			if tc.park {
				prefix := append([]event{}, want[:15]...)
				prefix = append(prefix, event{core.KindApproved, core.ActorHuman}, event{core.KindLanding, core.ActorGravy}, event{core.KindFetch, core.ActorGravy}, event{core.KindParked, core.ActorGravy})
				want[15].kind = core.KindContinued
				want = append(prefix, want[15:]...)
			}
			rows, err := svc.ListHistory(ctx, ticket.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]event, len(rows))
			var runID string
			for i, row := range rows {
				got[i] = event{row.Kind, row.Actor}
				switch row.Kind {
				case core.KindAgentStart:
					runID = row.RunID
					assertPayload(t, row, map[string]any{"provider": "fake", "model": "m", "host": "local", "attempt": float64(1)})
				case core.KindAgentExit:
					assertPayload(t, row, map[string]any{"class": "success", "turns": float64(3), "tokens_in": float64(100), "tokens_out": float64(50), "cost_usd": 0.25})
				case core.KindCommit:
					assertPayload(t, row, map[string]any{"files": float64(1), "insertions": float64(1), "deletions": float64(0)})
					if row.Payload["hash"] == "" || row.Payload["hash"] == nil {
						t.Error("commit has no hash")
					}
				case core.KindVerdict:
					assertPayload(t, row, map[string]any{"overall": "pass", "findings": float64(0)})
				}
				if i >= 15 && (runID == "" || row.RunID != runID) {
					t.Errorf("landing row %s run = %q, want %q", row.Kind, row.RunID, runID)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("history = %v\nwant = %v", got, want)
			}
		})
	}
}

func assertPayload(t *testing.T, row core.Activity, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if !reflect.DeepEqual(row.Payload[key], value) {
			t.Errorf("%s payload[%s] = %#v, want %#v", row.Kind, key, row.Payload[key], value)
		}
	}
}
