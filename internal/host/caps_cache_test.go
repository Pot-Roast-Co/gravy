package host

import (
	"fmt"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// TestCapsCache is the latency behind a three-second dashboard: Status probes every host on
// every call, and for an ssh host a probe is a whole connection.
func TestCapsCache(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func(n int) (core.Caps, error)
		wait  time.Duration
		want  int // probes after the second get
	}{
		{
			name:  "a success is reused",
			probe: func(int) (core.Caps, error) { return core.Caps{OS: "linux"}, nil },
			wait:  capsTTL - time.Second,
			want:  1,
		},
		{
			name:  "and re-probed once it is stale",
			probe: func(int) (core.Caps, error) { return core.Caps{OS: "linux"}, nil },
			wait:  capsTTL + time.Second,
			want:  2,
		},
		{
			name:  "a failure is reused only briefly",
			probe: func(int) (core.Caps, error) { return core.Caps{}, fmt.Errorf("host is asleep") },
			wait:  capsErrTTL - time.Second,
			want:  1,
		},
		{
			// A machine that was asleep should be picked up soon after it wakes, which is why
			// a failure is not held as long as a success.
			name:  "a failed host is retried sooner than a working one",
			probe: func(int) (core.Caps, error) { return core.Caps{}, fmt.Errorf("host is asleep") },
			wait:  capsErrTTL + time.Second,
			want:  2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1700000000, 0)
			c := capsCache{now: func() time.Time { return now }}

			calls := 0
			probe := func() (core.Caps, error) { calls++; return tc.probe(calls) }

			if _, err := c.get(probe); calls != 1 {
				t.Fatalf("first get made %d probes (%v), want 1", calls, err)
			}
			now = now.Add(tc.wait)
			_, _ = c.get(probe)
			if calls != tc.want {
				t.Errorf("probes = %d, want %d", calls, tc.want)
			}
		})
	}
}

// TestCapsCacheKeepsTheAnswer: a reused entry returns what was probed, not a zero value.
func TestCapsCacheKeepsTheAnswer(t *testing.T) {
	c := capsCache{}
	want := core.Caps{OS: "darwin", Arch: "arm64", Tools: map[string]string{"git": "2.39"}}
	for i := 0; i < 2; i++ {
		got, err := c.get(func() (core.Caps, error) { return want, nil })
		if err != nil {
			t.Fatal(err)
		}
		if got.OS != want.OS || got.Arch != want.Arch || got.Tools["git"] != "2.39" {
			t.Fatalf("get %d = %+v, want %+v", i, got, want)
		}
	}
}
