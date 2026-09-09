package main

import (
	"fmt"
	"sync"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/scheduler"
)

// liveConfig holds the configuration the running daemon is using.
//
// The scheduler reads it from its own goroutine while the API writes it, so it is guarded.
type liveConfig struct {
	mu  sync.RWMutex
	cfg config.Config
}

func newLiveConfig(c config.Config) *liveConfig { return &liveConfig{cfg: c} }

func (l *liveConfig) get() config.Config {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.cfg
}

func (l *liveConfig) set(c config.Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg = c
}

// choices returns a bucket's ordered preferences, read live so a route table edited in the TUI
// takes effect on the next tick rather than on the next restart.
func (l *liveConfig) choices(route core.Route) []core.Choice {
	cfg := l.get()
	parsed, err := cfg.RouteChoices(route)
	if err != nil {
		return nil
	}
	return parsed
}

// usable reports whether this build can run a provider and the configuration has not disabled it.
func (l *liveConfig) usable(providerID string) bool {
	if _, built := registeredProviders[providerID]; !built {
		return false
	}
	return enabled(l.get(), providerID)
}

// applyConfig installs what can be changed while running, and names what cannot.
//
// Being honest about the second list is the point. Some values are baked into the object graph
// at startup — the size of the worker pool, which agent CLIs are registered, the timeouts an
// orchestrator was built with — and a settings screen that silently accepted them would leave a
// user believing a change took effect when it had not.
// sameHosts reports whether two host lists describe the same machines.
func sameHosts(a, b []config.Host) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func applyConfig(live *liveConfig, sched *scheduler.Scheduler, loaded config.Config) func(config.Config) []string {
	return func(c config.Config) []string {
		// Applied live: the router reads the route table on every resolve, and the scheduler
		// reads its caps on every tick.
		live.set(c)
		sched.WithRouteCaps(c.Concurrency.Routes)

		var pending []string
		if c.Concurrency.Workers != loaded.Concurrency.Workers {
			pending = append(pending, fmt.Sprintf(
				"concurrency.workers (%d saved, %d running)",
				c.Concurrency.Workers, loaded.Concurrency.Workers))
		}
		// Hosts are bound into the object graph at startup — the pool probes each one's
		// capabilities and the repository factory indexes them — so a machine added or
		// removed here does not join or leave a running daemon.
		if !sameHosts(c.Hosts, loaded.Hosts) {
			pending = append(pending, "hosts")
		}
		for id, p := range c.Providers {
			was, existed := loaded.Providers[id]
			if !existed || was.Command != p.Command || was.Enabled != p.Enabled {
				pending = append(pending, "providers."+id)
			}
		}
		if c.Timeouts != loaded.Timeouts {
			pending = append(pending, "timeouts")
		}
		if c.Retry != loaded.Retry {
			pending = append(pending, "retry")
		}
		if c.Context != loaded.Context {
			pending = append(pending, "context.token_budget")
		}
		if c.Retention != loaded.Retention {
			pending = append(pending, "retention")
		}
		if c.Notifications != loaded.Notifications {
			pending = append(pending, "notifications")
		}
		return pending
	}
}
