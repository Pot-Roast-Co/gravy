package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// blockingService answers one method only when released, so a test can hold a call open.
//
// The embedded interface is nil: anything else this test calls would panic, which is the point —
// it documents exactly which methods the test relies on.
type blockingService struct {
	Service
	started chan struct{}
	release chan struct{}
}

func (b *blockingService) ListProjects(context.Context, ProjectFilter) ([]core.Project, error) {
	close(b.started)
	<-b.release
	return nil, nil
}

func (b *blockingService) ListAttention(context.Context) ([]core.Attention, error) {
	return nil, nil
}

// TestASlowCallDoesNotBlockTheNextOne is the freeze a real session hit.
//
// Calls used to share one connection under a mutex, so a planning turn — an agent run, minutes
// long — held the transport and the TUI's next refresh waited behind it. The screen stopped
// until the planner answered, which reads as a hung application rather than a slow request.
func TestASlowCallDoesNotBlockTheNextOne(t *testing.T) {
	dir, err := os.MkdirTemp("", "gv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, SocketName)

	svc := &blockingService{started: make(chan struct{}), release: make(chan struct{})}
	srv := NewServer(svc, path, nil)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	c, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	slow := make(chan error, 1)
	go func() { _, err := c.ListProjects(context.Background(), ProjectFilter{}); slow <- err }()
	<-svc.started

	// The second call must complete while the first is still in flight.
	quick := make(chan error, 1)
	go func() { _, err := c.ListAttention(context.Background()); quick <- err }()

	select {
	case err := <-quick:
		if err != nil {
			t.Fatalf("the second call failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a second call was blocked behind a slow one — the transport is serialising")
	}

	close(svc.release)
	if err := <-slow; err != nil {
		t.Fatalf("the slow call failed: %v", err)
	}
}
