package runlog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "runs"))
}

// collect drains a channel until it closes or the deadline passes.
func collect(t *testing.T, ch <-chan Line, want int, within time.Duration) []Line {
	t.Helper()
	var got []Line
	deadline := time.After(within)
	for len(got) < want {
		select {
		case l, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, l)
		case <-deadline:
			return got
		}
	}
	return got
}

// TestReadableWhileInFlight is AC1. A run you cannot watch is a run you can only wait for.
func TestReadableWhileInFlight(t *testing.T) {
	s := newStore(t)
	w, err := s.Open("r1")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.WriteAgent("compiling\n"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, stop, err := s.Tail(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// What was written before the reader arrived.
	if got := collect(t, ch, 1, 2*time.Second); len(got) != 1 || got[0].Text != "compiling" {
		t.Fatalf("history = %+v, want the line written before tailing", got)
	}

	// And what arrives after.
	if err := w.WriteAgent("running tests\n"); err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch, 1, 2*time.Second)
	if len(got) != 1 || got[0].Text != "running tests" {
		t.Errorf("live line = %+v", got)
	}
}

// TestTwoSubscribersBothGetEverything is AC2.
func TestTwoSubscribersBothGetEverything(t *testing.T) {
	s := newStore(t)
	w, err := s.Open("r1")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chA, stopA, err := s.Tail(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer stopA()
	chB, stopB, err := s.Tail(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer stopB()

	const n = 50
	for i := 0; i < n; i++ {
		if err := w.WriteAgent(fmt.Sprintf("line %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	for name, ch := range map[string]<-chan Line{"A": chA, "B": chB} {
		got := collect(t, ch, n, 3*time.Second)
		if len(got) != n {
			t.Fatalf("subscriber %s got %d lines, want %d", name, len(got), n)
		}
		for i, l := range got {
			if want := fmt.Sprintf("line %d", i); l.Text != want {
				t.Fatalf("subscriber %s line %d = %q, want %q", name, i, l.Text, want)
			}
		}
	}
}

// TestSlowSubscriberIsDroppedNotBlocking is AC3, and the reason the buffer is bounded: a writer
// blocked on a UI that stopped reading stalls the run itself.
func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	s := newStore(t)
	w, err := s.Open("r1")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A subscriber that never reads.
	_, stopSlow, err := s.Tail(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer stopSlow()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberBuffer*8; i++ {
			if err := w.WriteAgent(fmt.Sprintf("line %d", i)); err != nil {
				t.Errorf("write blocked or failed: %v", err)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer blocked on a subscriber that stopped reading")
	}

	// Every line still reached the file, which is the record that matters.
	history, err := s.History("r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != subscriberBuffer*8 {
		t.Errorf("file holds %d lines, want %d — dropping a reader must not drop the log",
			len(history), subscriberBuffer*8)
	}
}

// TestHistorySurvivesARestart is AC4: the files are the record, the fanout is a convenience.
func TestHistorySurvivesARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")

	first := New(dir)
	w, err := first.Open("r1")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAgent("before the crash"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEvent(map[string]any{"kind": "tool_use", "at": time.Now()}); err != nil {
		t.Fatal(err)
	}
	// A daemon that is killed never closes its writer; the appends are already on disk.

	second := New(dir)
	ctx := context.Background()
	ch, stop, err := second.Tail(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	got := collect(t, ch, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("after restart got %d lines, want 2: %+v", len(got), got)
	}
	if got[0].Text != "before the crash" || got[0].Stream != StreamAgent {
		t.Errorf("agent line = %+v", got[0])
	}
	if got[1].Stream != StreamEvent {
		t.Errorf("event line = %+v", got[1])
	}
	// A finished run's tail closes rather than hanging on a writer that will never return.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("tail of a finished run produced an unexpected extra line")
		}
	case <-time.After(time.Second):
		t.Error("tail of a finished run never closed")
	}
	_ = w.Close()
}

// TestPruneRemovesOldRunsOnly is AC5.
func TestPruneRemovesOldRunsOnly(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = ctx

	for _, id := range []string{"old", "recent", "spared", "live"} {
		w, err := s.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WriteAgent("output for " + id); err != nil {
			t.Fatal(err)
		}
		if id != "live" {
			w.Close()
		} else {
			defer w.Close()
		}
	}

	old := time.Now().Add(-48 * time.Hour)
	for _, id := range []string{"old", "spared"} {
		if err := os.Chtimes(s.Dir(id), old, old); err != nil {
			t.Fatal(err)
		}
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	removed, err := s.Prune(cutoff, func(runID string) bool { return runID == "spared" })
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("pruned %d runs, want 1", removed)
	}

	if _, err := os.Stat(s.Dir("old")); !os.IsNotExist(err) {
		t.Error("an old run's directory survived pruning")
	}
	for _, id := range []string{"recent", "spared", "live"} {
		if _, err := os.Stat(s.Dir(id)); err != nil {
			t.Errorf("%s was pruned but should not have been: %v", id, err)
		}
	}
}

// TestPruneNeverLeavesTheRoot guards the obvious catastrophe.
func TestPruneNeverLeavesTheRoot(t *testing.T) {
	base := t.TempDir()
	sibling := filepath.Join(base, "summaries.db")
	if err := os.WriteFile(sibling, []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(filepath.Join(base, "runs"))
	w, _ := s.Open("r1")
	w.Close()
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(s.Dir("r1"), old, old)

	if _, err := s.Prune(time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("pruning reached outside its root: %v", err)
	}
}

// TestRemoveDeletesOneRunNow covers deleting a run whose row has gone: waiting for retention to
// notice leaves an unreachable directory on disk for the length of the window.
func TestRemoveDeletesOneRunNow(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"gone", "kept"} {
		w, err := s.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WriteAgent("output for " + id); err != nil {
			t.Fatal(err)
		}
		w.Close()
	}

	// Fresh, not old: Remove is not a second retention window.
	if err := s.Remove("gone"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(s.Dir("gone")); !os.IsNotExist(err) {
		t.Error("the removed run's directory survived")
	}
	if _, err := os.Stat(s.Dir("kept")); err != nil {
		t.Errorf("Remove took a run it was not asked for: %v", err)
	}

	// Already gone is what the caller asked for.
	if err := s.Remove("gone"); err != nil {
		t.Errorf("removing a run twice: %v", err)
	}
}

// TestRemoveRefusesALiveRunAndStaysInItsRoot is the pair of catastrophes: deleting the files a
// run is still writing to, and handing RemoveAll something from a database row that is not a
// plain run id.
func TestRemoveRefusesALiveRunAndStaysInItsRoot(t *testing.T) {
	base := t.TempDir()
	sibling := filepath.Join(base, "gravy.db")
	if err := os.WriteFile(sibling, []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(base, "runs"))

	live, err := s.Open("live")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := s.Remove("live"); err == nil {
		t.Error("Remove deleted a run that was still writing")
	}
	if _, err := os.Stat(s.Dir("live")); err != nil {
		t.Errorf("the live run's directory went anyway: %v", err)
	}

	for _, id := range []string{"", ".", "..", "../..", "a/b"} {
		if err := s.Remove(id); err == nil {
			t.Errorf("Remove(%q) was accepted as a run id", id)
		}
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("Remove reached outside its root: %v", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Errorf("Remove deleted its root's parent: %v", err)
	}
}

// TestConcurrentWritersAndReaders runs the shape the daemon actually produces.
func TestConcurrentWritersAndReaders(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		runID := fmt.Sprintf("r%d", r)
		w, err := s.Open(runID)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			ch, _, err := s.Tail(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range ch {
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer w.Close()
			for i := 0; i < 100; i++ {
				if err := w.WriteAgent(fmt.Sprintf("line %d", i)); err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestTailBeforeOpenWaitsForTheRun is the race a client cannot avoid: it asks to follow a run at
// the same moment it asks for the run to start, and which arrives first is not up to it.
//
// Losing that race used to close the follower immediately, so the run looked silent for its
// whole duration — the exact pause the follower was opened to explain.
func TestTailBeforeOpenWaitsForTheRun(t *testing.T) {
	s := New(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, stop, err := s.Tail(ctx, "not-started-yet")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	defer stop()

	// Nothing should have been delivered, and the channel must still be open.
	select {
	case l, ok := <-ch:
		t.Fatalf("tail produced %v (open=%v) before the run started", l, ok)
	case <-time.After(50 * time.Millisecond):
	}

	w, err := s.Open("not-started-yet")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.WriteAgent("reading CLAUDE.md"); err != nil {
		t.Fatalf("WriteAgent: %v", err)
	}

	select {
	case l, ok := <-ch:
		if !ok {
			t.Fatal("the tail closed instead of delivering the line")
		}
		if l.Text != "reading CLAUDE.md" {
			t.Errorf("line = %q, want the written line", l.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the line written after Open never reached the waiting tail")
	}
	_ = w.Close()
}

// TestTailBeforeOpenStopsCleanly: a follower of a run that never starts must not wedge anyone.
func TestTailBeforeOpenStopsCleanly(t *testing.T) {
	s := New(t.TempDir())
	ch, stop, err := s.Tail(context.Background(), "never-starts")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	stop()
	stop() // idempotent

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("a stopped tail delivered a line")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the tail did not close after stop")
	}

	// The run can still start afterwards without blocking on the abandoned waiter.
	w, err := s.Open("never-starts")
	if err != nil {
		t.Fatalf("Open after abandoning the tail: %v", err)
	}
	_ = w.Close()
}

// TestTailOfAFinishedRunStillCloses pins the behaviour the waiting path must not have changed.
func TestTailOfAFinishedRunStillCloses(t *testing.T) {
	s := New(t.TempDir())
	w, err := s.Open("done")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = w.WriteAgent("a line")
	_ = w.Close()

	ch, stop, err := s.Tail(context.Background(), "done")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	defer stop()

	var got []Line
	for l := range ch {
		got = append(got, l)
	}
	if len(got) != 1 || got[0].Text != "a line" {
		t.Errorf("history = %v, want the one written line", got)
	}
}
