package host

import "fmt"

// MaxArgLen is the kernel's ceiling on a single argv entry.
//
// 32 pages — MAX_ARG_STRLEN in the kernel's binfmts.h. It is not ARG_MAX, which governs argv and
// the environment together, is far larger, and moves with RLIMIT_STACK. This one is a compiled-in
// constant: no sysctl raises it, no ulimit raises it, and exceeding it fails the exec itself
// rather than the program.
//
// The kernel counts the terminating NUL, so an argument of exactly this many bytes is already
// one too many and the longest that execs is MaxArgLen-1. Measured, not assumed: a 131072-byte
// argument is refused.
const MaxArgLen = 32 * 4096

// ArgTooLongError is an argument the kernel will refuse to exec.
//
// It exists because the kernel's own report of this is "argument list too long", which names
// neither the argument nor its size nor the way out, and arrives wrapped in a fork/exec error
// that reads like the binary is missing. Tracing one back to a single -p took three commands.
type ArgTooLongError struct {
	Cmd   string
	Index int
	Len   int
}

func (e *ArgTooLongError) Error() string {
	return fmt.Sprintf(
		"exec %s: argument %d is %d bytes, at or over the %d-byte kernel limit on a single "+
			"argument; send it on stdin or write it to a file and pass the path",
		e.Cmd, e.Index, e.Len, MaxArgLen)
}

// checkArgLengths refuses an exec the kernel would refuse, before it is attempted.
//
// Every provider reaches the operating system through Exec, including over ssh, where the remote
// command is assembled into one argument to ssh and is therefore checked here too. That makes
// this the one place a new adapter cannot forget.
func checkArgLengths(spec ExecSpec) error {
	for i, a := range spec.Args {
		if len(a) >= MaxArgLen {
			return &ArgTooLongError{Cmd: spec.Cmd, Index: i, Len: len(a)}
		}
	}
	return nil
}
