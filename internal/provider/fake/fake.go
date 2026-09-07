// Package fake provides a scriptable Provider for testing everything downstream of the provider
// layer without running a real agent CLI.
//
// The scheduler, orchestrator, validation loop and TUI all need a provider that behaves exactly
// as a test demands — succeeding, failing a specific way, emitting a chosen sequence of events,
// or being resumed. Driving a real CLI for that would be slow, costly, and non-deterministic.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// Script defines what one run does.
type Script struct {
	// Events are emitted in order before the run finishes.
	Events []provider.Event
	// EventDelay is slept between events, for tests that need a run to be observably in
	// flight. Zero emits them as fast as the consumer reads.
	EventDelay time.Duration
	// Outcome is what Wait reports.
	Outcome provider.Outcome
	// WaitErr, when set, is returned by Wait instead of Outcome.
	WaitErr error
	// BlockUntilKilled makes the run hang until Kill or context cancellation, for testing
	// timeouts and the kill switch.
	BlockUntilKilled bool
}

// Provider is a scriptable fake.
type Provider struct {
	id string

	mu sync.Mutex
	// scripts are consumed in order; the last one repeats once exhausted.
	scripts []Script
	next    int

	availability provider.Availability
	models       []provider.Model
	matcher      *provider.Matcher

	// runs records every task the fake was asked to run, for assertions.
	runs []provider.AgentTask
	// resumes records every resume, so tests can prove a blocked ticket resumed its session
	// rather than starting fresh.
	resumes []Resume
}

// Resume records a resumed session.
type Resume struct {
	Session provider.SessionRef
	Message string
}

// Option configures the fake.
type Option func(*Provider)

// WithScripts sets the scripted runs, consumed in order.
func WithScripts(s ...Script) Option { return func(p *Provider) { p.scripts = s } }

// WithAvailability sets what Detect reports.
func WithAvailability(a provider.Availability) Option {
	return func(p *Provider) { p.availability = a }
}

// WithModels sets the reported models.
func WithModels(m ...provider.Model) Option { return func(p *Provider) { p.models = m } }

// WithMatcher sets the classification rules.
func WithMatcher(m *provider.Matcher) Option { return func(p *Provider) { p.matcher = m } }

// New returns a fake provider that succeeds by default.
func New(id string, opts ...Option) *Provider {
	p := &Provider{
		id:           id,
		availability: provider.Availability{Installed: true, Authenticated: true, Version: "0.0.0-fake"},
		models:       []provider.Model{{ID: "fake-model", Name: "Fake Model"}},
		matcher:      provider.NewMatcher(),
	}
	for _, o := range opts {
		o(p)
	}
	if len(p.scripts) == 0 {
		p.scripts = []Script{{Outcome: provider.Outcome{Class: provider.Success}}}
	}
	return p
}

// ID returns the provider id.
func (p *Provider) ID() string { return p.id }

// Detect reports the configured availability.
func (p *Provider) Detect(context.Context, host.Host) (provider.Availability, error) {
	return p.availability, nil
}

// Models returns the configured models.
func (p *Provider) Models(context.Context) ([]provider.Model, error) { return p.models, nil }

// Classify applies the configured matcher.
func (p *Provider) Classify(exit int, stdout, stderr string) provider.Classification {
	return p.matcher.Classify(exit, stdout, stderr)
}

// Run starts a scripted run.
func (p *Provider) Run(ctx context.Context, _ host.Host, t provider.AgentTask) (provider.Handle, error) {
	p.mu.Lock()
	p.runs = append(p.runs, t)
	script := p.nextScript()
	p.mu.Unlock()
	return start(ctx, script), nil
}

// Resume continues a session, recording that it happened.
func (p *Provider) Resume(ctx context.Context, _ host.Host, s provider.SessionRef, msg string) (provider.Handle, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("resume: invalid session reference %+v", s)
	}
	p.mu.Lock()
	p.resumes = append(p.resumes, Resume{Session: s, Message: msg})
	script := p.nextScript()
	p.mu.Unlock()
	return start(ctx, script), nil
}

// nextScript returns the next script, repeating the last once exhausted. Callers hold p.mu.
func (p *Provider) nextScript() Script {
	s := p.scripts[p.next]
	if p.next < len(p.scripts)-1 {
		p.next++
	}
	return s
}

// Runs returns the tasks the fake was asked to run.
func (p *Provider) Runs() []provider.AgentTask {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.AgentTask(nil), p.runs...)
}

// Resumes returns the resumes the fake was asked to perform.
func (p *Provider) Resumes() []Resume {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Resume(nil), p.resumes...)
}

// handle is a scripted run in progress.
type handle struct {
	events chan provider.Event
	done   chan struct{}

	killOnce sync.Once
	killed   chan struct{}

	mu      sync.Mutex
	outcome provider.Outcome
	err     error
}

func start(ctx context.Context, s Script) *handle {
	h := &handle{
		events: make(chan provider.Event, len(s.Events)+1),
		done:   make(chan struct{}),
		killed: make(chan struct{}),
	}

	go func() {
		defer close(h.done)
		defer close(h.events)

		for _, e := range s.Events {
			if e.At.IsZero() {
				e.At = time.Now()
			}
			select {
			case h.events <- e:
			case <-h.killed:
				h.finishKilled()
				return
			case <-ctx.Done():
				h.finishKilled()
				return
			}
			if s.EventDelay > 0 {
				select {
				case <-time.After(s.EventDelay):
				case <-h.killed:
					h.finishKilled()
					return
				case <-ctx.Done():
					h.finishKilled()
					return
				}
			}
		}

		if s.BlockUntilKilled {
			select {
			case <-h.killed:
			case <-ctx.Done():
			}
			h.finishKilled()
			return
		}

		h.mu.Lock()
		h.outcome, h.err = s.Outcome, s.WaitErr
		h.mu.Unlock()
	}()

	return h
}

func (h *handle) finishKilled() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outcome = provider.Outcome{Class: provider.Timeout, Note: "killed", TimedOut: true}
}

// Events streams the scripted events.
func (h *handle) Events() <-chan provider.Event { return h.events }

// Wait blocks until the scripted run finishes.
func (h *handle) Wait() (provider.Outcome, error) {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.outcome, h.err
}

// Kill ends the run early.
func (h *handle) Kill() error {
	h.killOnce.Do(func() { close(h.killed) })
	return nil
}
