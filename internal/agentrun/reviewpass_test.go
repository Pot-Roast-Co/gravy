package agentrun

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/provider/fake"
)

// fixedRoute is a resolver that always answers with one agent, for the tests that are about
// what the reviewer does with an answer rather than how the bucket was chosen.
func fixedRoute(providerID, model string) RouteResolver {
	return func(context.Context, core.Route, core.Constraints) (core.Choice, error) {
		return core.Choice{ProviderID: providerID, Model: model}, nil
	}
}

// reviewOrch builds the minimum an advisory review needs: one host and one scripted provider.
func reviewOrch(t *testing.T, script fake.Script) *Orchestrator {
	t.Helper()
	o := New(nil, nil, nil, nil, nil, Config{RunTimeout: time.Minute, RunsDir: t.TempDir()},
		func() string { return "run-1" })
	o.RegisterHost(host.NewLocal("test", 1))
	o.RegisterProvider(fake.New("fake", fake.WithScripts(script)))
	return o
}

// TestReviewerReportsProviderErrors is the bug a real run surfaced.
//
// Codex refused a configured model with a 400. Collecting every event's text swept that error
// into the answer, so the parser rejected it and the human was told "the review model's answer
// could not be read: no JSON object in the answer" — while the actual cause, sitting in the run
// log, was "the 'sol' model is not supported when using Codex with a ChatGPT account".
func TestReviewerReportsProviderErrors(t *testing.T) {
	o := reviewOrch(t, fake.Script{
		Events: []provider.Event{{
			Kind: provider.EventError,
			Text: `{"status":400,"message":"The 'sol' model is not supported"}`,
		}},
		Outcome: provider.Outcome{Class: provider.TaskFailure, Note: "task_failure (no rule matched)"},
	})

	_, err := providerModel{orch: o, resolve: fixedRoute("fake", "sol")}.
		Complete(context.Background(), core.Project{}, "review this")
	if err == nil {
		t.Fatal("a refused request returned an answer")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("the provider's reason is not reported: %v", err)
	}
}

// TestReviewerFallsBackToTheOutcome: a provider that fails silently still owes a reason.
func TestReviewerFallsBackToTheOutcome(t *testing.T) {
	o := reviewOrch(t, fake.Script{
		Outcome: provider.Outcome{Class: provider.TaskFailure, Note: "killed after the turn cap"},
	})

	_, err := providerModel{orch: o, resolve: fixedRoute("fake", "m")}.
		Complete(context.Background(), core.Project{}, "review this")
	if err == nil {
		t.Fatal("a failed run returned an answer")
	}
	if !strings.Contains(err.Error(), "turn cap") {
		t.Errorf("the outcome's note is not reported: %v", err)
	}
}

// TestReviewerKeepsTheAnswer: an ordinary answer still comes back whole.
func TestReviewerKeepsTheAnswer(t *testing.T) {
	o := reviewOrch(t, fake.Script{
		Events: []provider.Event{
			{Kind: provider.EventThinking, Text: "considering"},
			{Kind: provider.EventMessage, Text: `{"overall":"pass"}`},
		},
		Outcome: provider.Outcome{Class: provider.Success},
	})

	out, err := providerModel{orch: o, resolve: fixedRoute("fake", "m")}.
		Complete(context.Background(), core.Project{}, "review this")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(out, `"overall":"pass"`) {
		t.Errorf("the answer was lost: %q", out)
	}
	// Thinking is not the answer, and folding it in is how prose ends up in the JSON.
	if strings.Contains(out, "considering") {
		t.Errorf("non-message events leaked into the answer: %q", out)
	}
}

// TestMissingCLIDoesNotCoolTheModel is a fleet-wide outage caused by one machine.
//
// A Mac without claude installed failed with "cli not found", which classified as
// provider_unavailable and put claude-code/haiku into cooldown everywhere — so work that would
// have run perfectly well on Linux stopped too. The model was never the problem.
func TestMissingCLIDoesNotCoolTheModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		out   provider.Outcome
		cools bool
	}{
		{
			name:  "a machine without the program",
			out:   provider.Outcome{Class: provider.ProviderUnavailable, Note: `provider_unavailable (rule "cli not found")`},
			cools: false,
		},
		{
			name:  "a provider that is genuinely down",
			out:   provider.Outcome{Class: provider.ProviderUnavailable, Note: `provider_unavailable (rule "service unavailable")`},
			cools: true,
		},
		{
			name:  "a rate limit is about the model",
			out:   provider.Outcome{Class: provider.RateLimited, Note: `rate_limited (rule "http 429")`},
			cools: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := !hostLocalFailure(tc.out); got != tc.cools {
				t.Errorf("cools = %v, want %v for %q", got, tc.cools, tc.out.Note)
			}
		})
	}
}

// TestTheReviewerUsesTheProjectsBucket is the bug: a project could pin its review bucket in the
// TUI and the pass ran on the global model anyway, because the review model was resolved once at
// startup and baked into the orchestrator.
func TestTheReviewerUsesTheProjectsBucket(t *testing.T) {
	p := fake.New("fake", fake.WithScripts(fake.Script{
		Events:  []provider.Event{{Kind: provider.EventMessage, Text: `{"overall":"pass"}`}},
		Outcome: provider.Outcome{Class: provider.Success},
	}))
	o := New(nil, nil, nil, nil, nil, Config{RunTimeout: time.Minute, RunsDir: t.TempDir()},
		func() string { return "run-1" })
	o.RegisterHost(host.NewLocal("test", 1))
	o.RegisterProvider(p)

	var asked core.Route
	var got core.Constraints
	m := providerModel{orch: o, resolve: func(_ context.Context, route core.Route, c core.Constraints) (core.Choice, error) {
		asked, got = route, c
		if choices, ok := c.Routes[route]; ok && len(choices) > 0 {
			return choices[0], nil
		}
		return core.Choice{ProviderID: "fake", Model: "global"}, nil
	}}

	project := core.Project{Routes: map[core.Route][]core.Choice{
		core.RouteReview: {{ProviderID: "fake", Model: "pinned"}},
	}}
	if _, err := m.Complete(context.Background(), project, "review this"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if asked != core.RouteReview {
		t.Errorf("resolved bucket %q, want the review bucket", asked)
	}
	if len(got.Routes) == 0 {
		t.Error("the review was resolved without the project's bucket table")
	}
	runs := p.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Model != "pinned" {
		t.Errorf("the review ran on %q, want the project's pinned model", runs[0].Model)
	}
}

// TestAnUnresolvableReviewBucketIsReported: the review is advisory, so a bucket that resolves to
// nothing must produce a reason a human can read rather than a silent pass.
func TestAnUnresolvableReviewBucketIsReported(t *testing.T) {
	o := reviewOrch(t, fake.Script{Outcome: provider.Outcome{Class: provider.Success}})
	m := providerModel{orch: o, resolve: func(context.Context, core.Route, core.Constraints) (core.Choice, error) {
		return core.Choice{}, fmt.Errorf("every agent is cooling down")
	}}

	_, err := m.Complete(context.Background(), core.Project{}, "review this")
	if err == nil {
		t.Fatal("an unresolvable bucket returned an answer")
	}
	if !strings.Contains(err.Error(), "cooling down") {
		t.Errorf("the reason is not reported: %v", err)
	}
}
