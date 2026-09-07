package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// fakeStore records which provider/model pairs are cooling down.
type fakeStore struct {
	down map[string]bool
	err  error
}

func (f *fakeStore) IsUnavailable(_ context.Context, providerID, model string, _ time.Time) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.down[providerID+"/"+model], nil
}

func choices(entries ...string) Choices {
	return func(core.Route) []core.Choice {
		out := make([]core.Choice, 0, len(entries))
		for _, e := range entries {
			p, m, _ := strings.Cut(e, "/")
			out = append(out, core.Choice{ProviderID: p, Model: m})
		}
		return out
	}
}

func always(string) bool { return true }

// TestFirstChoiceWins is the ordinary case: a route is a preference order.
func TestFirstChoiceWins(t *testing.T) {
	r := New(&fakeStore{down: map[string]bool{}},
		choices("claude-code/fable", "claude-code/opus"), always)

	got, err := r.Resolve(context.Background(), "astra", "local")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "fable" {
		t.Errorf("model = %q, want fable", got.Model)
	}
	if len(got.Why) == 0 || !strings.Contains(got.Why[0], "first choice") {
		t.Errorf("why = %v, want it to say this was the first choice", got.Why)
	}
}

// TestFallsThroughACooldown is the behaviour the whole package exists for: "fable, and opus if we
// run out of tokens".
func TestFallsThroughACooldown(t *testing.T) {
	r := New(&fakeStore{down: map[string]bool{"claude-code/fable": true}},
		choices("claude-code/fable", "claude-code/opus"), always)

	got, err := r.Resolve(context.Background(), "astra", "local")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Model != "opus" {
		t.Fatalf("model = %q, want the fallback opus", got.Model)
	}

	// Both the skip and the fallback are on the record: a run that silently used a different
	// model is one nobody can account for afterwards.
	joined := strings.Join(got.Why, " | ")
	if !strings.Contains(joined, "skipped claude-code/fable") {
		t.Errorf("why does not record the skip: %v", got.Why)
	}
	if !strings.Contains(joined, "cooling down") {
		t.Errorf("why does not say why it was skipped: %v", got.Why)
	}
	if !strings.Contains(joined, "fell back") || !strings.Contains(joined, "choice 2 of 2") {
		t.Errorf("why does not name the fallback: %v", got.Why)
	}
}

// TestEverythingCoolingDownIsAPauseNotAFailure. The scheduler leaves such a ticket Ready and
// tries again next tick, so the error has to say what is wrong rather than being swallowed.
func TestEverythingCoolingDownIsAPauseNotAFailure(t *testing.T) {
	r := New(&fakeStore{down: map[string]bool{
		"claude-code/fable": true, "claude-code/opus": true,
	}}, choices("claude-code/fable", "claude-code/opus"), always)

	_, err := r.Resolve(context.Background(), "astra", "local")
	if err == nil {
		t.Fatal("resolving with everything cooling down succeeded")
	}
	for _, want := range []string{"cooling down", "claude-code/fable", "claude-code/opus"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestUnusableProvidersAreSkipped keeps a route naming an agent this build lacks from stalling a
// bucket that has a perfectly good second choice.
func TestUnusableProvidersAreSkipped(t *testing.T) {
	r := New(&fakeStore{down: map[string]bool{}},
		choices("nonesuch/model", "claude-code/opus"),
		func(id string) bool { return id == "claude-code" })

	got, err := r.Resolve(context.Background(), "astra", "local")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderID != "claude-code" {
		t.Errorf("provider = %q, want the one this build has", got.ProviderID)
	}
	if !strings.Contains(strings.Join(got.Why, " "), "not available in this build") {
		t.Errorf("why does not explain the skip: %v", got.Why)
	}
}

// TestEmptyBucketSaysSo rather than resolving to something arbitrary.
func TestEmptyBucketSaysSo(t *testing.T) {
	r := New(&fakeStore{}, func(core.Route) []core.Choice { return nil }, always)

	_, err := r.Resolve(context.Background(), "astra", "local")
	if err == nil || !strings.Contains(err.Error(), "no agents configured") {
		t.Errorf("err = %v, want it to say the bucket is empty", err)
	}
}

// TestStoreFailureIsReported: guessing that a model is available when the cooldown table cannot
// be read would send work straight back into a quota wall.
func TestStoreFailureIsReported(t *testing.T) {
	r := New(&fakeStore{err: errors.New("database is locked")},
		choices("claude-code/opus"), always)

	if _, err := r.Resolve(context.Background(), "astra", "local"); err == nil {
		t.Fatal("a failed cooldown lookup resolved anyway")
	}
}

// TestCooldownExpiryIsTimeBased confirms the router asks the store with the current time, so a
// cooldown ends on its own rather than needing a restart.
func TestCooldownExpiryIsTimeBased(t *testing.T) {
	var asked time.Time
	store := &fakeStore{down: map[string]bool{}}
	r := New(clockStore{store, &asked}, choices("claude-code/opus"), always).
		WithClock(func() time.Time { return time.Unix(1700000000, 0) })

	if _, err := r.Resolve(context.Background(), "astra", "local"); err != nil {
		t.Fatal(err)
	}
	if !asked.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("store was asked about %v, want the router's clock", asked)
	}
}

type clockStore struct {
	inner *fakeStore
	asked *time.Time
}

func (c clockStore) IsUnavailable(ctx context.Context, p, m string, now time.Time) (bool, error) {
	*c.asked = now
	return c.inner.IsUnavailable(ctx, p, m, now)
}
