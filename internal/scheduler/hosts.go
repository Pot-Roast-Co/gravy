package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/host"
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

// eligible filters hosts against project and ticket requirements, returning the survivors and a
// note for each rejection.
//
// Ticket requirements add to the project's rather than replacing them: a ticket that needs an
// extra tool still needs everything the project needs.
func (s *hostSnapshot) eligible(project, ticket core.Requirements) ([]*hostState, []string) {
	var (
		out        []*hostState
		rejections []string
	)
	for _, h := range s.hosts {
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

// StaticPool is a HostPool over a fixed set of hosts with cached capabilities.
//
// Capability probing shells out to detect tools, and the scheduler ticks far more often than a
// machine's capabilities change, so they are read once and reused.
type StaticPool struct {
	hosts []host.Host
	caps  map[string]core.Caps
}

// NewStaticPool probes each host once and caches the result.
func NewStaticPool(ctx context.Context, hosts ...host.Host) (*StaticPool, error) {
	p := &StaticPool{hosts: hosts, caps: map[string]core.Caps{}}
	for _, h := range hosts {
		caps, err := h.Capabilities(ctx)
		if err != nil {
			return nil, fmt.Errorf("scheduler: capabilities of host %q: %w", h.ID(), err)
		}
		p.caps[h.ID()] = caps
	}
	return p, nil
}

// Hosts returns the pool's hosts.
func (p *StaticPool) Hosts() []host.Host { return p.hosts }

// Caps returns cached capabilities.
func (p *StaticPool) Caps(_ context.Context, h host.Host) (core.Caps, error) {
	caps, ok := p.caps[h.ID()]
	if !ok {
		return core.Caps{}, fmt.Errorf("scheduler: no cached capabilities for host %q", h.ID())
	}
	return caps, nil
}
