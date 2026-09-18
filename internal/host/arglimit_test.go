package host

import (
	"context"
	"errors"
	"strings"
	"testing"
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
func TestExecAcceptsALargePayloadOnStdin(t *testing.T) {
	h := NewLocal("local", 1)
	proc, err := h.Exec(context.Background(), ExecSpec{
		Cmd:   "cat",
		Stdin: strings.NewReader(strings.Repeat("y", 4*MaxArgLen)),
	})
	if err != nil {
		t.Fatalf("a 512KiB stdin was refused: %v", err)
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
