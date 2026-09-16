package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"path/filepath"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/store"
)

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "gravy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// offHost is a machine that has been switched off: it is recorded as unreachable, and any
// attempt to probe it hangs rather than failing, which is what a connection to an absent
// machine actually does.
type offHost struct {
	host.Host
	id       string
	probes   chan struct{}
	online   bool
	blockFor time.Duration
}

func (h *offHost) ID() string { return h.id }

func (h *offHost) Capabilities(context.Context) (core.Caps, error) {
	if h.online {
		return core.Caps{OS: "darwin"}, nil
	}
	return core.Caps{}, fmt.Errorf("ssh: connect to host %s port 22: Connection timed out", h.id)
}

func (h *offHost) Reachability() host.Reachability {
	r := host.Reachability{CheckedAt: time.Unix(1700000000, 0), Online: h.online}
	if !h.online {
		r.Err = "ssh: connect to host " + h.id + " port 22: Connection timed out"
	}
	return r
}

func (h *offHost) Recheck(ctx context.Context) (core.Caps, error) {
	if h.probes != nil {
		h.probes <- struct{}{}
	}
	select {
	case <-time.After(h.blockFor):
	case <-ctx.Done():
	}
	return h.Capabilities(ctx)
}

func (h *offHost) Slots() (int, int) { return 0, 2 }

// TestStatusReportsAnOffHostWithoutWaitingForIt is the rule a switched-off computer must obey:
// it may be absent, and it may not be slow for everyone else.
//
// Status is drawn on every refresh and lists every configured host, so probing them inline
// meant one machine being off cost the whole screen a connection timeout — repeatedly, because
// a failed probe is only cached briefly.
func TestStatusReportsAnOffHostWithoutWaitingForIt(t *testing.T) {
	db := openTestDB(t)
	off := &offHost{id: "yeet", blockFor: time.Hour}
	l := NewLocal(db, nil, []host.Host{off}, func() string { return "id" })

	done := make(chan SystemStatus, 1)
	go func() {
		st, err := l.Status(context.Background(), ProjectFilter{})
		if err != nil {
			t.Error(err)
		}
		done <- st
	}()

	select {
	case st := <-done:
		if len(st.Hosts) != 1 {
			t.Fatalf("hosts = %d, want 1", len(st.Hosts))
		}
		got := st.Hosts[0]
		if got.Online {
			t.Error("a host that is off reports Online")
		}
		if got.Unreachable == "" {
			t.Error("no reason given for an unreachable host; the human is left guessing whether Gravy is at fault")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Status blocked on a host that is switched off")
	}
}

// TestReconnectHostProbesAndReportsWhatItFound covers both answers the human can get from the
// reconnect they asked for: the machine is back, or it is still off and they should go and
// look at it rather than at Gravy.
func TestReconnectHostProbesAndReportsWhatItFound(t *testing.T) {
	db := openTestDB(t)
	off := &offHost{id: "yeet", probes: make(chan struct{}, 4)}
	l := NewLocal(db, nil, []host.Host{off}, func() string { return "id" })

	hs, err := l.ReconnectHost(context.Background(), "yeet")
	if err != nil {
		t.Fatalf("ReconnectHost: %v", err)
	}
	if len(off.probes) != 1 {
		t.Errorf("probes = %d, want the reconnect to actually connect", len(off.probes))
	}
	if hs.Online {
		t.Error("reported online while the machine is still off")
	}

	// The human switches it on and asks again.
	off.online = true
	hs, err = l.ReconnectHost(context.Background(), "yeet")
	if err != nil {
		t.Fatalf("ReconnectHost after power-on: %v", err)
	}
	if !hs.Online {
		t.Error("a machine that is now answering still reports off; the reconnect looks broken")
	}

	if _, err := l.ReconnectHost(context.Background(), "nope"); err == nil {
		t.Error("reconnecting a host that is not configured succeeded")
	}
}
