package agentrun

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/provider/fake"
)

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

	_, err := providerModel{orch: o, providerID: "fake", model: "sol"}.
		Complete(context.Background(), "review this")
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

	_, err := providerModel{orch: o, providerID: "fake", model: "m"}.
		Complete(context.Background(), "review this")
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

	out, err := providerModel{orch: o, providerID: "fake", model: "m"}.
		Complete(context.Background(), "review this")
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
