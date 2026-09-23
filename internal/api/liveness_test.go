package api

import (
	"context"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/config"
)

// lastEvents is a Liveness with fixed answers.
type lastEvents map[string]time.Time

func (m lastEvents) LastEvent(ticketID string) (time.Time, bool) {
	at, ok := m[ticketID]
	return at, ok
}

// TestStatusReportsLastOutput: Status says how long each running agent has been silent, and past
// the configured threshold says so in words — without the ticket moving anywhere, because
// quiet is reported, never acted on.
func TestStatusReportsLastOutput(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	cases := []struct {
		name       string
		last       lastEvents
		stall      time.Duration
		want       time.Duration
		wantSuffix string
	}{
		{"no live agent", lastEvents{}, 5 * time.Minute, 0, ""},
		{"recent output", lastEvents{"GR-2": now.Add(-20 * time.Second)}, 5 * time.Minute, 20 * time.Second, ""},
		{"quiet past the threshold", lastEvents{"GR-2": now.Add(-(6*time.Minute + 12*time.Second))}, 5 * time.Minute,
			6*time.Minute + 12*time.Second, " — no output for 6m"},
		{"threshold switched off", lastEvents{"GR-2": now.Add(-time.Hour)}, 0, time.Hour, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := atReview(t)
			assigned(t, db, "GR-2")
			var cfg config.Config
			cfg.Timeouts.Stall = config.Duration(tc.stall)
			svc = svc.WithSettings(t.TempDir(), cfg, nil).WithLiveness(tc.last)
			svc.now = func() time.Time { return now }

			st, err := svc.Status(context.Background(), ProjectFilter{})
			if err != nil {
				t.Fatal(err)
			}
			for _, rt := range st.Running {
				if rt.Ticket.ID != "GR-2" {
					continue
				}
				if rt.LastOutput != tc.want {
					t.Errorf("LastOutput = %s, want %s", rt.LastOutput, tc.want)
				}
				if want := activityFor(rt.Ticket.State) + tc.wantSuffix; rt.Activity != want {
					t.Errorf("Activity = %q, want %q", rt.Activity, want)
				}
				return
			}
			t.Fatalf("GR-2 is not in Running: %+v", st.Running)
		})
	}
}
