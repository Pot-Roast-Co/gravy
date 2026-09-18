package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/api"
)

type updateRecorder struct {
	api.Service
	got  chan api.UpdateStatus
	sets int
}

func (r *updateRecorder) SetUpdate(u api.UpdateStatus) {
	r.sets++
	select {
	case r.got <- u:
	default:
	}
}

func TestWatchForUpdatesPublishesWhatItFinds(t *testing.T) {
	rec := &updateRecorder{got: make(chan api.UpdateStatus, 1)}
	d := New("", rec, idleRunner{}, nil, func() string { return "x" }, nil)
	d.updateCheck = func(context.Context) (api.UpdateStatus, error) {
		return api.UpdateStatus{Latest: "v0.1.4", Available: true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go d.watchForUpdates(ctx)

	select {
	case got := <-rec.got:
		if got.Latest != "v0.1.4" || !got.Available {
			t.Fatalf("published %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the check never reached the service")
	}
	cancel()
}

// A failed check publishes nothing at all, rather than a zero status that would clear a newer
// answer obtained yesterday.
func TestWatchForUpdatesIgnoresFailures(t *testing.T) {
	rec := &updateRecorder{got: make(chan api.UpdateStatus, 1)}
	d := New("", rec, idleRunner{}, nil, func() string { return "x" }, nil)
	d.updateCheck = func(context.Context) (api.UpdateStatus, error) {
		return api.UpdateStatus{}, errors.New("no network")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go d.watchForUpdates(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()

	if rec.sets != 0 {
		t.Errorf("a failed check wrote a status %d times", rec.sets)
	}
}

// Cancelling stops it: the check must never be something a shutdown waits on.
func TestWatchForUpdatesStopsWithTheContext(t *testing.T) {
	rec := &updateRecorder{got: make(chan api.UpdateStatus, 1)}
	d := New("", rec, idleRunner{}, nil, func() string { return "x" }, nil)
	d.updateCheck = func(ctx context.Context) (api.UpdateStatus, error) {
		return api.UpdateStatus{Latest: "v0.1.4"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.watchForUpdates(ctx); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchForUpdates outlived its context")
	}
}
