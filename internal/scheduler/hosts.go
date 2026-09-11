package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

// hostState is one host's capabilities and load at the moment of a tick.
type hostState struct {
	id    string
	caps  core.Caps
	used  int
	total int
}

// hostSnapshot is the pool as it stood when the tick began.
//
// Taken once per tick so that every ticket in the same tick is judged against the same state:
// re-reading capabilities per ticket would let a host's slot count change mid-decision and make
// the resulting assignments impossible to reproduce or explain.
type hostSnapshot struct {
	hosts []*hostState
}

func (s *Scheduler) snapshotHosts(ctx context.Context) (*hostSnapshot, error) {
	all := s.hosts.Hosts()
	snap := &hostSnapshot{hosts: make([]*hostState, 0, len(all))}

	for _, h := range all {
		caps, err := s.hosts.Caps(ctx, h)
		if err != nil {
			// A host whose capabilities cannot be read is not usable, but it must not stop
			// scheduling on the others.
			continue
		}
		used, total := h.Slots()
		snap.hosts = append(snap.hosts, &hostState{id: h.ID(), caps: caps, used: used, total: total})
	}
	sort.Slice(snap.hosts, func(i, j int) bool { return snap.hosts[i].id < snap.hosts[j].id })
	return snap, nil
}

// claim records that a host took an assignment during this tick, so later tickets in the same
// tick see the updated load.
func (s *hostSnapshot) claim(id string) {
	for _, h := range s.hosts {
		if h.id == id {
			h.used++
			return
		}
	}
}

// eligibleOn filters hosts against a project's host pin and both sets of requirements, returning
// the survivors and a note for each rejection.
//
// Ticket requirements add to the project's rather than replacing them: a ticket that needs an
// extra tool still needs everything the project needs.
//
// A project cannot run anywhere but its own machine: its clone is there and its RepoPath is
// meaningless on any other. The rejection says so, because "nothing is happening" with no reason
// is the failure the explainability rule exists to prevent.
//
// An empty hostID pins nothing, which is now only reachable by a row written before projects
// were stamped with their host. It used to be the default, and it meant a local project's agent
// could be sent to a remote machine while its worktree was created here — the run and the code
// on different computers.
func (s *hostSnapshot) eligibleOn(hostID string, project, ticket core.Requirements) ([]*hostState, []string) {
	var (
		out        []*hostState
		rejections []string
	)
	for _, h := range s.hosts {
		if hostID != "" && h.id != hostID {
			rejections = append(rejections, fmt.Sprintf(
				"host %s excluded: the project is on %s", h.id, hostID))
			continue
		}
		if reason, ok := satisfies(h, project); !ok {
			rejections = append(rejections, fmt.Sprintf("host %s excluded: %s (project requirement)", h.id, reason))
			continue
		}
		if reason, ok := satisfies(h, ticket); !ok {
			rejections = append(rejections, fmt.Sprintf("host %s excluded: %s (ticket requirement)", h.id, reason))
			continue
		}
		out = append(out, h)
	}
	return out, rejections
}

// satisfies reports whether a host meets a set of requirements, and why not when it does not.
func satisfies(h *hostState, req core.Requirements) (string, bool) {
	if len(req.OS) > 0 {
		matched := false
		for _, os := range req.OS {
			if os == h.caps.OS {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Sprintf("runs %s, needs one of %s", h.caps.OS, strings.Join(req.OS, ", ")), false
		}
	}
	for tool := range req.Tools {
		if _, ok := h.caps.Tools[tool]; !ok {
			return fmt.Sprintf("does not have %s", tool), false
		}
	}
	return "", true
}

// leastBusy returns the idle host if there is one, otherwise the least loaded with a free slot.
//
// Ties break on id so that a tick is reproducible: two identical idle hosts must not produce
// different assignments on different runs.
func leastBusy(hosts []*hostState) *hostState {
	var best *hostState
	for _, h := range hosts {
		if h.used >= h.total {
			continue
		}
		if best == nil || h.used < best.used || (h.used == best.used && h.id < best.id) {
			best = h
		}
	}
	return best
}

// StaticPool is a HostPool over a fixed set of hosts.
//
// Static refers to the set of hosts, not to what is known about them: the membership is fixed
// at startup, while each host's capabilities are read from that host's own cache on every ask,
// so a machine switched on later becomes usable without a restart.
type StaticPool struct {
	hosts []host.Host
	// caps and probeErr are what the startup probe found. They are the report on how starting
	// up went — which hosts answered, and why the others did not — rather than the working
	// copy: Caps asks the host, because this pair stops being true the moment a machine's
	// state changes.
	caps     map[string]core.Caps
	probeErr map[string]error
}

// NewStaticPool probes each host once and caches the result.
//
// The probes run concurrently because a remote one is a whole ssh round trip that does not fail
// until its connect timeout elapses. Serially, every sleeping machine added its own timeout to
// the total, and the total is paid on daemon startup before the socket is bound — which is how
// two hosts being asleep turned into Gravy appearing not to start at all.
func NewStaticPool(ctx context.Context, hosts ...host.Host) (*StaticPool, error) {
	p := &StaticPool{hosts: hosts, caps: map[string]core.Caps{}, probeErr: map[string]error{}}

	// Results land in a pre-sized slice rather than the maps directly: no mutex, and the maps
	// are still filled in host order, so a pool is the same however the probes interleave.
	type probe struct {
		caps core.Caps
		err  error
	}
	probes := make([]probe, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probes[i].caps, probes[i].err = h.Capabilities(ctx)
		}()
	}
	wg.Wait()

	for i, h := range hosts {
		if probes[i].err != nil {
			// A machine that is asleep, off, or not reachable right now must not stop Gravy
			// starting: the local work has nothing to do with it. The failure is remembered so
			// that a ticket pinned to that host is told why nothing is happening, rather than
			// the host quietly not existing.
			p.probeErr[h.ID()] = probes[i].err
			continue
		}
		p.caps[h.ID()] = probes[i].caps
	}
	// Every host failing is different: it means the local machine could not be probed either,
	// which is a real problem rather than a sleeping laptop.
	if len(p.caps) == 0 && len(hosts) > 0 {
		return nil, fmt.Errorf("scheduler: no host could be probed: %w", p.probeErr[hosts[0].ID()])
	}
	return p, nil
}

// ProbeErrors reports the hosts that could not be probed at startup, and why.
func (p *StaticPool) ProbeErrors() map[string]error { return p.probeErr }

// Hosts returns the pool's hosts.
func (p *StaticPool) Hosts() []host.Host { return p.hosts }

// Caps returns the host's capabilities, as the host last learned them.
//
// This asks the host rather than replaying what the startup probe found, because the two stop
// agreeing the moment a machine's state changes. A laptop that was off when the daemon started
// would otherwise stay unusable until the daemon was restarted — including immediately after
// the human switched it on and asked Gravy to reconnect, which is exactly when they expect
// work to start flowing again.
//
// It is still a cache read, not a connection: the host answers from memory and refreshes
// behind the caller, so a tick never waits on a machine that is not there.
func (p *StaticPool) Caps(ctx context.Context, h host.Host) (core.Caps, error) {
	caps, err := h.Capabilities(ctx)
	if err != nil {
		return core.Caps{}, fmt.Errorf("host %q could not be reached: %w", h.ID(), err)
	}
	return caps, nil
}
