package host

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// waitFor polls until cond holds, so a test can observe a refresh that runs behind the caller
// without sleeping for a fixed guess at how long it takes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestCapsCache is the latency behind a three-second dashboard: Status probes every host on
// every call, and for an ssh host a probe is a whole connection.
func TestCapsCache(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func(n int64) (core.Caps, error)
		wait  time.Duration
		want  int64 // probes after the second get settles
	}{
		{
			name:  "a success is reused",
			probe: func(int64) (core.Caps, error) { return core.Caps{OS: "linux"}, nil },
			wait:  capsTTL - time.Second,
			want:  1,
		},
		{
			name:  "and re-probed once it is stale",
			probe: func(int64) (core.Caps, error) { return core.Caps{OS: "linux"}, nil },
			wait:  capsTTL + time.Second,
			want:  2,
		},
		{
			name:  "a failure is reused only briefly",
			probe: func(int64) (core.Caps, error) { return core.Caps{}, fmt.Errorf("host is asleep") },
			wait:  capsErrTTL - time.Second,
			want:  1,
		},
		{
			// A machine that was asleep should be picked up soon after it wakes, which is why
			// a failure is not held as long as a success.
			name:  "a failed host is retried sooner than a working one",
			probe: func(int64) (core.Caps, error) { return core.Caps{}, fmt.Errorf("host is asleep") },
			wait:  capsErrTTL + time.Second,
			want:  2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			now := time.Unix(1700000000, 0)
			readNow := func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			}
			c := capsCache{now: readNow}

			var calls atomic.Int64
			probe := func(context.Context) (core.Caps, error) { return tc.probe(calls.Add(1)) }

			if _, err := c.get(context.Background(), probe); calls.Load() != 1 {
				t.Fatalf("first get made %d probes (%v), want 1", calls.Load(), err)
			}
			mu.Lock()
			now = now.Add(tc.wait)
			mu.Unlock()

			_, _ = c.get(context.Background(), probe)
			if tc.want > 1 {
				waitFor(t, "the background refresh", func() bool { return calls.Load() == tc.want })
			}
			if got := calls.Load(); got != tc.want {
				t.Errorf("probes = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCapsCacheKeepsTheAnswer: a reused entry returns what was probed, not a zero value.
func TestCapsCacheKeepsTheAnswer(t *testing.T) {
	c := capsCache{}
	want := core.Caps{OS: "darwin", Arch: "arm64", Tools: map[string]string{"git": "2.39"}}
	for i := range 2 {
		got, err := c.get(context.Background(), func(context.Context) (core.Caps, error) { return want, nil })
		if err != nil {
			t.Fatal(err)
		}
		if got.OS != want.OS || got.Arch != want.Arch || got.Tools["git"] != "2.39" {
			t.Fatalf("get %d = %+v, want %+v", i, got, want)
		}
	}
}

// TestCapsCacheServesAStaleEntryWithoutWaiting is the bug that let one switched-off computer
// stall the whole app.
//
// A failed probe is only held for capsErrTTL, so the dashboard re-probed an absent machine
// every half minute — inline, on the request path, for as long as ssh takes to give up. The
// caller must get the last known answer immediately and let the probe happen behind it.
func TestCapsCacheServesAStaleEntryWithoutWaiting(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1700000000, 0)
	readNow := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	c := capsCache{now: readNow}

	// The first probe answers at once; every later one hangs, standing in for a machine that
	// has been switched off and will not refuse the connection, only fail to answer it.
	release := make(chan struct{})
	var probes atomic.Int64
	probe := func(ctx context.Context) (core.Caps, error) {
		if probes.Add(1) == 1 {
			return core.Caps{OS: "linux"}, nil
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return core.Caps{}, fmt.Errorf("connection timed out")
	}

	if _, err := c.get(context.Background(), probe); err != nil {
		t.Fatalf("first get: %v", err)
	}

	mu.Lock()
	now = now.Add(capsTTL + time.Second)
	mu.Unlock()

	done := make(chan core.Caps, 1)
	go func() {
		caps, _ := c.get(context.Background(), probe)
		done <- caps
	}()

	select {
	case caps := <-done:
		if caps.OS != "linux" {
			t.Errorf("stale get = %+v, want the last known capabilities", caps)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("get blocked on a host that was not answering; one absent machine stalls every caller")
	}

	close(release)
}

// TestCapsCacheRecheckWaitsForTheTruth: the reconnect a human asks for must not be answered
// from a cache written while the machine was still off, or the button looks broken.
func TestCapsCacheRecheckWaitsForTheTruth(t *testing.T) {
	c := capsCache{}

	if _, err := c.get(context.Background(), func(context.Context) (core.Caps, error) {
		return core.Caps{}, fmt.Errorf("connection timed out")
	}); err == nil {
		t.Fatal("first get succeeded, want the probe failure")
	}
	if r := c.state(); r.Online || !r.Off() {
		t.Errorf("state = %+v, want a host recorded as off", r)
	}

	// The machine is switched on. Recheck must report that, not the cached failure.
	caps, err := c.recheck(context.Background(), func(context.Context) (core.Caps, error) {
		return core.Caps{OS: "darwin"}, nil
	})
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if caps.OS != "darwin" {
		t.Errorf("recheck = %+v, want the machine that is now answering", caps)
	}
	if r := c.state(); !r.Online {
		t.Errorf("state = %+v, want online after a successful recheck", r)
	}
}
