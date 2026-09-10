package host

import (
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// capsTTL is how long a capability probe is reused.
//
// A machine does not grow a toolchain between two dashboard ticks, and Status probes every host
// on every call: with two ssh hosts that was two full connections — login shell, tool probe,
// teardown — before the screen could draw, about three seconds. Minutes of staleness in a list
// of installed tools costs nothing; seconds of latency on every refresh costs the UI.
const capsTTL = 5 * time.Minute

// capsErrTTL is how long a failed probe is reused.
//
// Much shorter than a success, and for the opposite reason: a machine that was asleep should be
// picked up soon after it wakes. But not zero — an unreachable host must not make every refresh
// wait for a connection timeout.
const capsErrTTL = 30 * time.Second

// capsCache remembers one host's last probe.
type capsCache struct {
	mu   sync.Mutex
	caps core.Caps
	err  error
	at   time.Time
	// now is overridden in tests.
	now func() time.Time
}

// get returns the cached probe, running one when what it holds is too old.
//
// The probe runs while the lock is held, so two callers arriving together make one connection
// rather than two. That is the point: the scheduler and a dashboard refresh land at the same
// moment all the time.
func (c *capsCache) get(probe func() (core.Caps, error)) (core.Caps, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now
	if c.now != nil {
		now = c.now
	}

	ttl := capsTTL
	if c.err != nil {
		ttl = capsErrTTL
	}
	if !c.at.IsZero() && now().Sub(c.at) < ttl {
		return c.caps, c.err
	}

	c.caps, c.err = probe()
	c.at = now()
	return c.caps, c.err
}
