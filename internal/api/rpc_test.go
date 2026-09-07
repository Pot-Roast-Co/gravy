package api

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/runlog"
	"github.com/pot-roast-co/gravy/internal/store"
)

// served starts a server over a real store and returns a connected client.
func served(t *testing.T) (*Client, *Local, string) {
	t.Helper()
	ctx := context.Background()

	local, _ := atReview(t)
	// A short path: unix sockets are limited to ~104 bytes, and t.TempDir() under a long
	// TMPDIR is enough to exceed it.
	dir, err := os.MkdirTemp("", "gv")
	_ = err
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, SocketName)

	logs := runlog.New(filepath.Join(dir, "runs"))
	local = local.WithLogs(logs)

	srv := NewServer(local, path, nil)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srvCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(srvCtx) }()
	t.Cleanup(func() { cancel(); <-done })

	c, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, local, path
}

// TestRoundTripEveryMethod is AC2: every method crosses a real unix socket and comes back.
//
// The assertion is deliberately shallow — Local's behaviour is tested elsewhere — because what
// can break here is encoding: a field that does not survive marshalling, or a method the client
// and the dispatch table spell differently.
func TestRoundTripEveryMethod(t *testing.T) {
	c, _, _ := served(t)
	ctx := context.Background()

	t.Run("ListProjects", func(t *testing.T) {
		got, err := c.ListProjects(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Slug != "proj" {
			t.Errorf("projects = %+v", got)
		}
	})

	t.Run("ListTickets", func(t *testing.T) {
		got, err := c.ListTickets(ctx, TicketFilter{State: core.StateReview})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "GR-1" {
			t.Errorf("tickets = %+v", got)
		}
	})

	t.Run("CreateTicket", func(t *testing.T) {
		got, err := c.CreateTicket(ctx, CreateTicketReq{
			ProjectID: "p1", Title: "over the wire", Body: "body", Ready: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "over the wire" || got.State != core.StateReady {
			t.Errorf("ticket = %+v", got)
		}
	})

	t.Run("Status", func(t *testing.T) {
		st, err := c.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// The fleet-wide shape must survive: counts, the queue, and the attention join.
		if len(st.Projects) != 1 {
			t.Fatalf("projects = %+v", st.Projects)
		}
		if len(st.Attention) != 1 || st.Attention[0].Attention.Reason != core.ReasonReviewPending {
			t.Errorf("attention = %+v", st.Attention)
		}
		if st.Attention[0].Ticket.ID != "GR-1" {
			t.Errorf("the attention item lost its joined ticket: %+v", st.Attention[0])
		}
	})

	t.Run("GetReview", func(t *testing.T) {
		rb, err := c.GetReview(ctx, "GR-1")
		if err != nil {
			t.Fatal(err)
		}
		if rb.Ticket.ID != "GR-1" || rb.Project.Slug != "proj" {
			t.Errorf("bundle = %+v", rb)
		}
	})

	t.Run("ListRuns", func(t *testing.T) {
		if _, err := c.ListRuns(ctx, "GR-1"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ListAttention", func(t *testing.T) {
		got, err := c.ListAttention(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("attention = %+v", got)
		}
	})

	t.Run("RequestChanges then MoveTicket", func(t *testing.T) {
		if err := c.RequestChanges(ctx, "GR-1", "handle the zero case"); err != nil {
			t.Fatal(err)
		}
		st, err := c.MoveTicket(ctx, "GR-1", core.EventAssign)
		if err != nil {
			t.Fatal(err)
		}
		if st != core.StateAssigned {
			t.Errorf("state = %q, want assigned", st)
		}
	})

	t.Run("ResolveAttention", func(t *testing.T) {
		if err := c.ResolveAttention(ctx, "a1"); err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	})

	t.Run("AddProject rejects a bad path", func(t *testing.T) {
		// The error path is the interesting one here: it proves a failure deep in the service
		// arrives as a failure rather than as a zero value.
		if _, err := c.AddProject(ctx, AddProjectReq{Path: "/definitely/not/here"}); err == nil {
			t.Error("adding a nonexistent path succeeded")
		}
	})

	t.Run("Approve and Reject reach the service", func(t *testing.T) {
		// Local has no lander, so both must arrive and fail for that reason rather than
		// silently succeeding.
		if err := c.Approve(ctx, "GR-1"); err == nil || !strings.Contains(err.Error(), "cannot land") {
			t.Errorf("Approve error = %v, want the no-lander refusal", err)
		}
		// A ticket of its own: an earlier subtest advanced GR-1, and Assigned has no reject
		// edge — which is the state machine working, not a transport problem.
		fresh, err := c.CreateTicket(ctx, CreateTicketReq{ProjectID: "p1", Title: "to reject"})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Reject(ctx, fresh.ID); err != nil {
			t.Errorf("Reject: %v", err)
		}
	})

	t.Run("ExplainTicket", func(t *testing.T) {
		if _, err := c.ExplainTicket(ctx, "GR-1"); err == nil {
			t.Error("explain with no scheduler succeeded")
		}
	})
}

// TestStreamLogsCrossesTheWire is what makes a run watchable from another process: the point of
// GR-011 is not the files, it is that a client with no access to them can follow a live run.
func TestStreamLogsCrossesTheWire(t *testing.T) {
	c, local, _ := served(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := local.logs.Open("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAgent("before the reader arrived"); err != nil {
		t.Fatal(err)
	}

	ch, stop, err := c.StreamLogs(ctx, "run-1")
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	defer stop()

	// History first.
	select {
	case line := <-ch:
		if line.Text != "before the reader arrived" || line.Stream != runlog.StreamAgent {
			t.Errorf("first line = %+v", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no history arrived over the wire")
	}

	// Then live output.
	if err := w.WriteAgent("while it is running"); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-ch:
		if line.Text != "while it is running" {
			t.Errorf("live line = %+v", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a line written after subscribing never arrived")
	}

	// Ending the run closes the stream, which is how a client learns the run is over.
	w.Close()
	select {
	case _, ok := <-ch:
		if ok {
			// A trailing buffered line is fine; the close must still follow.
			select {
			case _, ok2 := <-ch:
				if ok2 {
					t.Error("the stream kept delivering after the run ended")
				}
			case <-time.After(3 * time.Second):
				t.Error("the stream never closed after the run ended")
			}
		}
	case <-time.After(3 * time.Second):
		t.Error("the stream never closed after the run ended")
	}
}

// TestErrorsPreserveTypeAndMessage is AC3. A "not found" that arrives as an untyped string turns
// every caller's check into string comparison.
func TestErrorsPreserveTypeAndMessage(t *testing.T) {
	c, _, _ := served(t)

	_, err := c.GetReview(context.Background(), "nope")
	if err == nil {
		t.Fatal("reviewing a missing ticket succeeded")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("error %v does not match store.ErrNotFound across the wire", err)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %v lost the id it was about", err)
	}
}

// TestUnknownMethodIsRejected guards the dispatch table against silently accepting nonsense.
func TestUnknownMethodIsRejected(t *testing.T) {
	c, _, _ := served(t)
	err := c.call(context.Background(), "NoSuchMethod", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("error = %v, want an unknown-method failure", err)
	}
}

// TestEventsReachEveryClient is AC4.
func TestEventsReachEveryClient(t *testing.T) {
	c, local, path := served(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	second, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	chA, stopA, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stopA()
	chB, stopB, err := second.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stopB()

	local.Publish(Event{Kind: EventTicketChanged, TicketID: "GR-1"})

	for i, ch := range []<-chan Event{chA, chB} {
		select {
		case ev := <-ch:
			if ev.TicketID != "GR-1" || ev.Kind != EventTicketChanged {
				t.Errorf("subscriber %d got %+v", i, ev)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

// TestSlowSubscriberDoesNotBlockThePublisher is AC4's adversarial half. Losing an event costs a
// redundant read; blocking the writer stalls the daemon behind a wedged UI.
func TestSlowSubscriberDoesNotBlockThePublisher(t *testing.T) {
	b := newBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, _, err := func() (<-chan Event, func(), error) {
		ch, stop := b.subscribe(ctx)
		return ch, stop, nil
	}(); err != nil {
		t.Fatal(err)
	}

	// Nobody reads. Publishing far past the buffer must still return.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < eventBufferSize*10; i++ {
			b.publish(Event{Kind: EventTicketChanged})
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("publishing blocked on a subscriber that stopped reading")
	}
}

// TestPublishIsRaceFree runs the broker the way the daemon does.
func TestPublishIsRaceFree(t *testing.T) {
	b := newBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		ch, _ := b.subscribe(ctx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range ch {
			}
		}()
	}
	for i := 0; i < 200; i++ {
		b.publish(Event{Kind: EventRunChanged})
	}
	cancel()
	wg.Wait()
}

// TestSocketIsPrivateAndCleanedUp is AC5. Anything that can open the socket can approve a merge.
func TestSocketIsPrivateAndCleanedUp(t *testing.T) {
	_, _, path := served(t)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
}

func TestCloseRemovesTheSocket(t *testing.T) {
	local, _ := atReview(t)
	dir, _ := os.MkdirTemp("", "gv")
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, SocketName)

	srv := NewServer(local, path, nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the socket file outlived a clean shutdown")
	}
}

// TestStaleSocketIsReplaced is the case that happens after every kill -9. Refusing to start
// because of a file left by a corpse would mean hand-deleting one after every hard stop.
func TestStaleSocketIsReplaced(t *testing.T) {
	local, _ := atReview(t)
	dir, _ := os.MkdirTemp("", "gv")
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, SocketName)

	if err := os.WriteFile(path, []byte("left over"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(local, path, nil)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen over a stale socket file: %v", err)
	}
	defer srv.Close()

	if _, err := net.Dial("unix", path); err != nil {
		t.Errorf("the replaced socket does not accept connections: %v", err)
	}
}

// TestSecondDaemonIsRefused is the other half: a live socket must not be clobbered, or two
// daemons end up writing to one database.
func TestSecondDaemonIsRefused(t *testing.T) {
	_, _, path := served(t)

	local2, _ := atReview(t)
	srv := NewServer(local2, path, nil)
	err := srv.Listen()
	if err == nil {
		srv.Close()
		t.Fatal("a second daemon bound a socket that was already being served")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("error = %v, want it to name the running daemon", err)
	}
}
