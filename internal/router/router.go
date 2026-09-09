// Package router resolves a route to the provider and model that should run it.
//
// A route is an ordered list of choices — a preference order, not a set. The first one that is
// usable wins; the rest are the fallback, and they exist because a subscription can run out.
// Falling through to the next choice is the difference between "your queue kept moving on a
// cheaper model" and "everything stopped at 4pm".
package router

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// Store is what the router needs to know about cooldowns.
type Store interface {
	IsUnavailable(ctx context.Context, providerID, model string, now time.Time) (bool, error)
}

// Choices returns a route's ordered preferences.
type Choices func(core.Route) []core.Choice

// Usable reports whether this build can run a provider at all — it is registered, and the
// configuration has not disabled it.
type Usable func(providerID string) bool

// Router picks the first choice on a route that is not cooling down.
type Router struct {
	store   Store
	choices Choices
	usable  Usable
	now     func() time.Time
}

// New returns a router.
func New(s Store, choices Choices, usable Usable) *Router {
	return &Router{store: s, choices: choices, usable: usable, now: time.Now}
}

// WithClock overrides the clock, for tests.
func (r *Router) WithClock(now func() time.Time) *Router {
	r.now = now
	return r
}

// Resolve walks a route's choices in order and returns the first usable one.
//
// Every skip is recorded in Why. A queue that quietly moved to a different model is a queue
// whose output you cannot account for later, so the reason travels with the assignment.
func (r *Router) Resolve(ctx context.Context, route core.Route, c core.Constraints) (core.Choice, error) {
	choices, overridden := projectChoices(c, route)
	if !overridden {
		choices = r.choices(route)
	}
	if len(choices) == 0 {
		return core.Choice{}, fmt.Errorf("bucket %q has no agents configured", route)
	}

	var why []string
	var cooling []string
	if overridden {
		why = append(why, fmt.Sprintf("bucket %q is set on the project, so its %d choice(s) replace the global table",
			route, len(choices)))
	}

	for i, c := range choices {
		label := c.ProviderID + "/" + c.Model

		if r.usable != nil && !r.usable(c.ProviderID) {
			why = append(why, fmt.Sprintf("skipped %s: provider not available in this build", label))
			continue
		}

		down, err := r.store.IsUnavailable(ctx, c.ProviderID, c.Model, r.now())
		if err != nil {
			return core.Choice{}, fmt.Errorf("router: %q: %w", label, err)
		}
		if down {
			why = append(why, fmt.Sprintf("skipped %s: cooling down after a quota or rate-limit failure", label))
			cooling = append(cooling, label)
			continue
		}

		switch i {
		case 0:
			why = append(why, fmt.Sprintf("bucket %q resolved to %s, its first choice", route, label))
		default:
			// Naming the fallback explicitly: a run that silently used a different model is
			// one whose result nobody can account for afterwards.
			why = append(why, fmt.Sprintf("bucket %q fell back to %s, choice %d of %d",
				route, label, i+1, len(choices)))
		}
		c.Why = why
		return c, nil
	}

	// Nothing usable. The ticket stays Ready and the scheduler tries again next tick, which is
	// what makes a cooldown a pause rather than a failure.
	if len(cooling) > 0 {
		return core.Choice{}, fmt.Errorf("every agent for %q is cooling down (%s)",
			route, strings.Join(cooling, ", "))
	}
	return core.Choice{}, fmt.Errorf("no agent for %q is available in this build", route)
}

// projectChoices returns a project's own preferences for a bucket.
//
// An override replaces the global list rather than extending it: a project that pins "review" to
// a local model does not want the global cloud model as its fallback, which is the whole reason
// for pinning it. An entry with no choices is treated as absent — the TUI refuses to store one,
// and falling through beats parking every ticket on that bucket.
func projectChoices(c core.Constraints, route core.Route) ([]core.Choice, bool) {
	got, ok := c.Routes[route]
	if !ok || len(got) == 0 {
		return nil, false
	}
	return got, true
}
