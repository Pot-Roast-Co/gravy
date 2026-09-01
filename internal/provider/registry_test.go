package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bobbybrady/gravy/internal/host"
	"github.com/bobbybrady/gravy/internal/provider"
	"github.com/bobbybrady/gravy/internal/provider/fake"
)

// TestRegistryRejectsDuplicates is AC1.
func TestRegistryRejectsDuplicates(t *testing.T) {
	r := provider.NewRegistry()

	if err := r.Register(fake.New("claude-code")); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Two adapters answering to one name would make routing depend on initialisation order.
	if err := r.Register(fake.New("claude-code")); err == nil {
		t.Error("a duplicate id was accepted")
	}
	if err := r.Register(fake.New("")); err == nil {
		t.Error("an empty id was accepted")
	}
	if err := r.Register(nil); err == nil {
		t.Error("a nil provider was accepted")
	}
}

func TestRegistryGetAndAll(t *testing.T) {
	r := provider.NewRegistry()
	for _, id := range []string{"codex", "claude-code", "another"} {
		if err := r.Register(fake.New(id)); err != nil {
			t.Fatal(err)
		}
	}

	p, err := r.Get("codex")
	if err != nil || p.ID() != "codex" {
		t.Errorf("Get(codex) = %v, %v", p, err)
	}
	if _, err := r.Get("nope"); err == nil {
		t.Error("Get accepted an unregistered id")
	}

	all := r.All()
	if len(all) != 3 {
		t.Fatalf("All returned %d providers, want 3", len(all))
	}
	// Sorted, so onboarding and the TUI render a stable list.
	want := []string{"another", "claude-code", "codex"}
	for i, p := range all {
		if p.ID() != want[i] {
			t.Errorf("All()[%d] = %q, want %q", i, p.ID(), want[i])
		}
	}
}

// TestDetectAll is AC4: an absent CLI is a state of the world, not an error.
func TestDetectAll(t *testing.T) {
	r := provider.NewRegistry()
	r.MustRegister(fake.New("installed", fake.WithAvailability(provider.Availability{
		Installed: true, Authenticated: true, Version: "1.2.3",
	})))
	r.MustRegister(fake.New("absent", fake.WithAvailability(provider.Availability{
		Installed: false, Detail: "codex is not on PATH; install it with: brew install codex",
	})))
	r.MustRegister(fake.New("unauthenticated", fake.WithAvailability(provider.Availability{
		Installed: true, Authenticated: false, Version: "2.0.0",
		Detail: "run `claude login` to authenticate",
	})))

	got := r.DetectAll(context.Background(), host.NewLocal("local", 1))
	if len(got) != 3 {
		t.Fatalf("DetectAll returned %d results, want 3", len(got))
	}

	byID := map[string]provider.Detection{}
	for _, d := range got {
		if d.Err != nil {
			t.Errorf("%s reported an error: %v", d.ProviderID, d.Err)
		}
		byID[d.ProviderID] = d
	}

	if !byID["installed"].Installed || byID["installed"].Version != "1.2.3" {
		t.Errorf("installed = %+v", byID["installed"])
	}
	if byID["absent"].Installed {
		t.Error("an absent provider reported as installed")
	}
	// The detail must tell the human what to do, since it is what onboarding shows them.
	if !strings.Contains(byID["absent"].Detail, "install") {
		t.Errorf("absent detail = %q, want actionable guidance", byID["absent"].Detail)
	}
	if byID["unauthenticated"].Authenticated {
		t.Error("an unauthenticated provider reported as authenticated")
	}
	if !strings.Contains(byID["unauthenticated"].Detail, "login") {
		t.Errorf("unauthenticated detail = %q, want actionable guidance", byID["unauthenticated"].Detail)
	}
}

func TestDetectAllOnEmptyRegistry(t *testing.T) {
	got := provider.NewRegistry().DetectAll(context.Background(), host.NewLocal("local", 1))
	if len(got) != 0 {
		t.Errorf("DetectAll on an empty registry returned %d results", len(got))
	}
}

// TestFakeScriptedOutcomes is AC5.
func TestFakeScriptedOutcomes(t *testing.T) {
	cost := 1.5
	p := fake.New("fake", fake.WithScripts(
		fake.Script{Outcome: provider.Outcome{
			Class: provider.Success, Turns: 3, TokensIn: 100, TokensOut: 200, CostUSD: &cost,
			Session: provider.SessionRef{ProviderID: "fake", ID: "sess-1"},
		}},
		fake.Script{Outcome: provider.Outcome{Class: provider.QuotaExhausted, Note: "scripted quota"}},
	))

	h, err := p.Run(context.Background(), nil, provider.AgentTask{RunID: "r1", Prompt: "do a thing"})
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(h)
	out, err := h.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if out.Class != provider.Success || out.Turns != 3 || out.CostUSD == nil || *out.CostUSD != cost {
		t.Errorf("first outcome = %+v", out)
	}

	// The second script is used for the second run.
	h2, _ := p.Run(context.Background(), nil, provider.AgentTask{RunID: "r2"})
	drainEvents(h2)
	out2, _ := h2.Wait()
	if out2.Class != provider.QuotaExhausted {
		t.Errorf("second outcome = %v, want QuotaExhausted", out2.Class)
	}

	// The last script repeats, so a test does not have to script every run it triggers.
	h3, _ := p.Run(context.Background(), nil, provider.AgentTask{RunID: "r3"})
	drainEvents(h3)
	out3, _ := h3.Wait()
	if out3.Class != provider.QuotaExhausted {
		t.Errorf("third outcome = %v, want the last script to repeat", out3.Class)
	}

	runs := p.Runs()
	if len(runs) != 3 || runs[0].Prompt != "do a thing" {
		t.Errorf("recorded runs = %+v", runs)
	}
}

func TestFakeEvents(t *testing.T) {
	p := fake.New("fake", fake.WithScripts(fake.Script{
		Events: []provider.Event{
			{Kind: provider.EventStarted},
			{Kind: provider.EventToolUse, Tool: "Read"},
			{Kind: provider.EventToolResult, Tool: "Read"},
			{Kind: provider.EventMessage, Text: "done"},
		},
		Outcome: provider.Outcome{Class: provider.Success},
	}))

	h, err := p.Run(context.Background(), nil, provider.AgentTask{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}

	var kinds []provider.EventKind
	for e := range h.Events() {
		kinds = append(kinds, e.Kind)
		if e.At.IsZero() {
			t.Error("event has no timestamp")
		}
	}
	want := []provider.EventKind{
		provider.EventStarted, provider.EventToolUse,
		provider.EventToolResult, provider.EventMessage,
	}
	if len(kinds) != len(want) {
		t.Fatalf("got %d events, want %d", len(kinds), len(want))
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, kinds[i], want[i])
		}
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
}

// TestFakeResume is AC5's session half: downstream tests must be able to prove a blocked ticket
// resumed its prior session rather than starting a fresh one.
func TestFakeResume(t *testing.T) {
	p := fake.New("fake")
	session := provider.SessionRef{ProviderID: "fake", ID: "sess-42"}

	h, err := p.Resume(context.Background(), nil, session, "the human says: use the other API")
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(h)
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}

	resumes := p.Resumes()
	if len(resumes) != 1 {
		t.Fatalf("got %d resumes, want 1", len(resumes))
	}
	if resumes[0].Session != session {
		t.Errorf("resumed session = %+v, want %+v", resumes[0].Session, session)
	}
	if !strings.Contains(resumes[0].Message, "the other API") {
		t.Errorf("injected message = %q", resumes[0].Message)
	}

	// An unusable reference must be refused rather than silently starting a fresh run, which
	// would lose the agent's context and look like a mysterious restart.
	if _, err := p.Resume(context.Background(), nil, provider.SessionRef{}, "hello"); err == nil {
		t.Error("Resume accepted an invalid session reference")
	}
}

func TestFakeBlockUntilKilled(t *testing.T) {
	p := fake.New("fake", fake.WithScripts(fake.Script{BlockUntilKilled: true}))

	h, err := p.Run(context.Background(), nil, provider.AgentTask{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(h)

	done := make(chan provider.Outcome, 1)
	go func() {
		out, _ := h.Wait()
		done <- out
	}()

	select {
	case <-done:
		t.Fatal("a blocking run finished on its own")
	case <-time.After(100 * time.Millisecond):
	}

	if err := h.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if !out.TimedOut {
			t.Errorf("killed run outcome = %+v, want TimedOut", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Kill did not end the run")
	}

	// Kill must be idempotent: the TUI's kill switch can race the timeout.
	if err := h.Kill(); err != nil {
		t.Errorf("second Kill: %v", err)
	}
}

func TestFakeContextCancellation(t *testing.T) {
	p := fake.New("fake", fake.WithScripts(fake.Script{BlockUntilKilled: true}))
	ctx, cancel := context.WithCancel(context.Background())

	h, err := p.Run(ctx, nil, provider.AgentTask{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(h)
	cancel()

	done := make(chan struct{})
	go func() { h.Wait(); close(done) }() //nolint:errcheck // outcome asserted elsewhere
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the context did not end the run")
	}
}

func TestFakeWaitError(t *testing.T) {
	sentinel := errors.New("scripted failure")
	p := fake.New("fake", fake.WithScripts(fake.Script{WaitErr: sentinel}))

	h, _ := p.Run(context.Background(), nil, provider.AgentTask{RunID: "r1"})
	drainEvents(h)
	if _, err := h.Wait(); !errors.Is(err, sentinel) {
		t.Errorf("Wait error = %v, want the scripted one", err)
	}
}

func TestFakeSatisfiesProvider(t *testing.T) {
	var _ provider.Provider = fake.New("fake")
}

// drainEvents consumes events in the background.
//
// It must not block the caller: Handle.Events is closed when the RUN ends, not when the last
// event is emitted, so draining synchronously before killing a deliberately-blocking run would
// deadlock. The fake's channel is buffered so a run never stalls on an absent consumer.
func drainEvents(h provider.Handle) {
	go func() {
		for range h.Events() {
		}
	}()
}
