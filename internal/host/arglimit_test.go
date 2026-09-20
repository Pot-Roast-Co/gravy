package host

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestExecRefusesAnArgumentTheKernelWould turns an opaque failure into a named one.
//
// The kernel reports this as "argument list too long" inside a fork/exec error, which names
// neither the argument, nor its size, nor the way out — it reads like the binary is missing.
func TestExecRefusesAnArgumentTheKernelWould(t *testing.T) {
	h := NewLocal("local", 1)
	huge := strings.Repeat("x", MaxArgLen)

	_, err := h.Exec(context.Background(), ExecSpec{Cmd: "echo", Args: []string{"-n", huge}})
	if err == nil {
		t.Fatal("an over-long argument was accepted")
	}

	var tooLong *ArgTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("error %v is not an ArgTooLongError, so a caller cannot tell this apart", err)
	}
	if tooLong.Index != 1 {
		t.Errorf("Index = %d, want 1 — the caller needs to know which argument", tooLong.Index)
	}
	// The message has to carry the three things the kernel's does not.
	msg := err.Error()
	for _, want := range []string{"echo", "argument 1", "stdin"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message omits %q: %s", want, msg)
		}
	}
}

// The longest argument that actually execs is MaxArgLen-1, because the kernel counts the
// terminating NUL. Verified against the real kernel rather than reasoned about: an argument of
// exactly MaxArgLen bytes is refused with E2BIG.
func TestExecAllowsTheLongestArgumentThatExecs(t *testing.T) {
	h := NewLocal("local", 1)
	proc, err := h.Exec(context.Background(), ExecSpec{
		Cmd:  "true",
		Args: []string{strings.Repeat("x", MaxArgLen-1)},
	})
	if err != nil {
		t.Fatalf("an argument at exactly the limit was refused: %v", err)
	}
	_, _ = proc.Wait()
}

// A large payload on stdin is fine, which is the entire point: the limit is on arguments.
//
// stdout is drained while the process runs, as every real caller does. Not draining it is a
// deadlock of the test's own making: cat cannot finish writing 512KiB into a pipe nobody is
// emptying, so it never exits and there is nothing to wait for.
func TestExecAcceptsALargePayloadOnStdin(t *testing.T) {
	h := NewLocal("local", 1)
	const size = 4 * MaxArgLen

	proc, err := h.Exec(context.Background(), ExecSpec{
		Cmd:   "cat",
		Stdin: strings.NewReader(strings.Repeat("y", size)),
	})
	if err != nil {
		t.Fatalf("a 512KiB stdin was refused: %v", err)
	}

	read := make(chan int, 1)
	go func() {
		b, _ := io.ReadAll(proc.Stdout())
		read <- len(b)
	}()

	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	select {
	case n := <-read:
		if n != size {
			t.Errorf("cat echoed %d bytes, want %d — the whole payload did not get through", n, size)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stdout never finished")
	}
}

// And a child that does read stdin still receives all of it.
func TestStdinReachesAChildThatReadsIt(t *testing.T) {
	h := NewLocal("local", 1)
	const payload = "the whole prompt arrives"

	proc, err := h.Exec(context.Background(), ExecSpec{Cmd: "cat", Stdin: strings.NewReader(payload)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(proc.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if string(out) != payload {
		t.Errorf("child received %q, want %q", out, payload)
	}
}

// TestReadingOutputEndsWhenAGrandchildHoldsThePipe is the regression, and it wedged a running
// daemon for the better part of an hour.
//
// A pipe reaches EOF when every writer is gone, and the process gravy starts is not necessarily
// the last one: anything it spawns inherits the descriptor. Here the child exits immediately and
// leaves a sleeper holding stdout. A reader waiting for EOF blocks forever — and an agent run
// decides a run is over by waiting for exactly that reader, with no timeout above it, so the
// worker slot and the serial project behind it are held indefinitely.
func TestReadingOutputEndsWhenAGrandchildHoldsThePipe(t *testing.T) {
	h := NewLocal("local", 1)

	// sh exits at once; the backgrounded sleep inherits stdout and keeps the write end open.
	proc, err := h.Exec(context.Background(), ExecSpec{
		Cmd:  "sh",
		Args: []string{"-c", "echo hello; sleep 120 & exit 0"},
	})
	if err != nil {
		t.Fatal(err)
	}

	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(proc.Stdout())
		read <- string(b)
	}()

	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	select {
	case got := <-read:
		// What the process actually wrote is still delivered; the grace period is what makes
		// that true rather than a race.
		if !strings.Contains(got, "hello") {
			t.Errorf("output written before exit was lost: %q", got)
		}
	case <-time.After(outputGrace + 10*time.Second):
		t.Fatal("reading stdout never ended; a grandchild held the pipe and the worker is wedged")
	}
	_ = proc.Kill()
}

// TestReadingEndsEvenWhenNobodyCallsWaitFirst is the regression, and it is the case the previous
// fix could not reach.
//
// Callers read before they wait: the claude adapter scans stdout to EOF and only then calls
// Wait. So when a grandchild held the pipe open past the agent's exit, the reader blocked, Wait
// was never reached, and the grace period that closes the pipes — which lived inside Wait — never
// started. A finished agent's answer sat in a log file while the screen said "thinking…".
//
// The reader here never calls Wait at all, which is the point.
func TestReadingEndsEvenWhenNobodyCallsWaitFirst(t *testing.T) {
	h := NewLocal("local", 1)

	proc, err := h.Exec(context.Background(), ExecSpec{
		Cmd:  "sh",
		Args: []string{"-c", "echo answer; sleep 120 & exit 0"},
	})
	if err != nil {
		t.Fatal(err)
	}

	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(proc.Stdout())
		read <- string(b)
	}()

	select {
	case got := <-read:
		if !strings.Contains(got, "answer") {
			t.Errorf("output written before exit was lost: %q", got)
		}
	case <-time.After(outputGrace + 15*time.Second):
		t.Fatal("reading never ended, and nobody had called Wait to start the grace period")
	}

	// And the status is still correct afterwards, since reaping no longer depends on Wait.
	st, err := proc.Wait()
	if err != nil {
		t.Fatalf("Wait after the fact: %v", err)
	}
	if st.Code != 0 {
		t.Errorf("exit code = %d, want 0", st.Code)
	}
	_ = proc.Kill()
}

// Wait is safe to call from several places at once, now that it only reads what reap wrote.
func TestWaitIsSafeConcurrently(t *testing.T) {
	h := NewLocal("local", 1)
	proc, err := h.Exec(context.Background(), ExecSpec{Cmd: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil {
		t.Fatal(err)
	}

	codes := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func() {
			st, _ := proc.Wait()
			codes <- st.Code
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case c := <-codes:
			if c != 3 {
				t.Errorf("exit code = %d, want 3 — a caller saw a status that was not written yet", c)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a concurrent Wait never returned")
		}
	}
}
