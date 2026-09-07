package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/pot-roast-co/gravy/internal/host"
)

// Registry holds the known providers, keyed by id.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// Register adds a provider.
//
// A duplicate id is an error rather than a silent replacement: two adapters answering to the
// same name would make routing non-deterministic, and which one won would depend on
// initialisation order.
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return fmt.Errorf("register provider: nil provider")
	}
	id := p.ID()
	if id == "" {
		return fmt.Errorf("register provider: empty id")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[id]; exists {
		return fmt.Errorf("register provider %q: already registered", id)
	}
	r.providers[id] = p
	return nil
}

// MustRegister registers a provider and panics on failure. For package initialisation only.
func (r *Registry) MustRegister(p Provider) {
	if err := r.Register(p); err != nil {
		panic(err)
	}
}

// Get returns a provider by id.
func (r *Registry) Get(id string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	if !ok {
		return nil, fmt.Errorf("provider %q is not registered", id)
	}
	return p, nil
}

// All returns every registered provider, ordered by id so output is stable.
func (r *Registry) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]Provider, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.providers[id])
	}
	return out
}

// Detection pairs a provider id with what was found on a host.
type Detection struct {
	ProviderID string
	Availability
	// Err is set when detection itself failed, as distinct from the provider being absent.
	Err error
}

// DetectAll probes every registered provider on a host.
//
// A provider whose CLI is missing is reported as not installed, not as an error: an absent
// optional tool is an ordinary state of the world, and onboarding needs to show the whole
// picture rather than stopping at the first gap.
func (r *Registry) DetectAll(ctx context.Context, h host.Host) []Detection {
	providers := r.All()
	out := make([]Detection, len(providers))

	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			d := Detection{ProviderID: p.ID()}
			av, err := p.Detect(ctx, h)
			if err != nil {
				d.Err = err
				d.Detail = err.Error()
			} else {
				d.Availability = av
			}
			out[i] = d
		}(i, p)
	}
	wg.Wait()
	return out
}
