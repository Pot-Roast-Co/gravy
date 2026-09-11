package host

import (
	"context"
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

// capsErrTTL is how long a failed probe is reused before another is worth trying.
//
// Much shorter than a success, so a machine that was asleep is picked up soon after it wakes.
// It no longer has to trade that off against latency: a stale entry is served immediately and
// refreshed behind the caller, so shortening it costs nobody a wait.
const capsErrTTL = 30 * time.Second

// capsRefreshTimeout bounds a background refresh.
//
// Longer than ssh's own ConnectTimeout so that what gets recorded is ssh's reason — "connection
// timed out", which names the machine — rather than our deadline, which would not.
const capsRefreshTimeout = 30 * time.Second

// capsCache remembers one host's last probe.
//
// The rule it exists to enforce: a caller never waits on a machine Gravy has already reached
// once. A fresh entry is returned as-is, and a stale one is returned immediately while a probe
// runs behind the caller. Only the very first probe for a host blocks, because until it lands
// there is genuinely nothing to report.
//
// That asymmetry is the whole point. Probing inline meant a computer being switched off cost
// every dashboard refresh a full connection timeout, once the short error TTL above had
// lapsed — a host that was merely absent could stall the parts of Gravy that had nothing to do
// with it.
type capsCache struct {
	mu   sync.Mutex
	caps core.Caps
	err  error
	at   time.Time
	// refreshing keeps concurrent stale reads to one background probe.
	refreshing bool
	// now is overridden in tests.
	now func() time.Time
}

func (c *capsCache) clock() func() time.Time {
	if c.now != nil {
		return c.now
	}
	return time.Now
}

// get returns the cached probe, refreshing behind the caller when what it holds is stale.
//
// probe takes its own context because a background refresh outlives the request that triggered
// it: handed the caller's, it would be cancelled the moment the dashboard finished drawing and
// the entry would never refresh.
func (c *capsCache) get(ctx context.Context, probe func(context.Context) (core.Caps, error)) (core.Caps, error) {
	c.mu.Lock()

	now := c.clock()
	if c.at.IsZero() {
		// Never probed: there is nothing to serve, so this caller waits. The lock is held
		// across the probe so that two callers arriving together make one connection rather
		// than two — the scheduler and a dashboard refresh land at the same moment all the time.
		defer c.mu.Unlock()
		c.caps, c.err = probe(ctx)
		c.at = now()
		return c.caps, c.err
	}

	ttl := capsTTL
	if c.err != nil {
		ttl = capsErrTTL
	}
	if now().Sub(c.at) >= ttl && !c.refreshing {
		c.refreshing = true
		go c.refresh(context.WithoutCancel(ctx), probe)
	}

	caps, err := c.caps, c.err
	c.mu.Unlock()
	return caps, err
}

// refresh re-probes and stores the result. It runs detached from any request.
func (c *capsCache) refresh(ctx context.Context, probe func(context.Context) (core.Caps, error)) {
	ctx, cancel := context.WithTimeout(ctx, capsRefreshTimeout)
	defer cancel()

	caps, err := probe(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps, c.err, c.at, c.refreshing = caps, err, c.clock()(), false
}

// recheck probes now and waits for the answer, whatever the cache holds.
//
// This is the human asking, from the TUI, whether a machine they have just switched on is back.
// Answering from a cache written while it was still off would make the button appear broken.
func (c *capsCache) recheck(ctx context.Context, probe func(context.Context) (core.Caps, error)) (core.Caps, error) {
	c.mu.Lock()
	c.refreshing = true
	c.mu.Unlock()

	caps, err := probe(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps, c.err, c.at, c.refreshing = caps, err, c.clock()(), false
	return caps, err
}

// state reports what the cache last learned, without touching the network.
func (c *capsCache) state() Reachability {
	c.mu.Lock()
	defer c.mu.Unlock()

	r := Reachability{CheckedAt: c.at, Checking: c.refreshing}
	if c.at.IsZero() {
		return r // never probed: neither online nor known to be off
	}
	if c.err != nil {
		r.Err = c.err.Error()
		return r
	}
	r.Online = true
	return r
}
