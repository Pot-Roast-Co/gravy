package notify

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bobbybrady/gravy/internal/config"
)

// fakeRunner records commands instead of executing them.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, cmd string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{cmd}, args...))
	return f.err
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTest(t *testing.T, mode config.NotifyMode, window time.Duration, goos string, r Runner) (*Notifier, *bytes.Buffer, *fakeClock) {
	t.Helper()
	buf := &bytes.Buffer{}
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	cfg := config.Notifications{Mode: mode, RateLimitWindow: config.Duration(window)}
	n := New(cfg, buf, WithRunner(r), WithLogger(quietLogger()), WithGOOS(goos), WithClock(clock.Now))
	return n, buf, clock
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestDisabledProducesNothing is AC1: no output and no subprocess.
func TestDisabledProducesNothing(t *testing.T) {
	r := &fakeRunner{}
	n, buf, _ := newTest(t, config.NotifyOff, 0, "darwin", r)

	n.Notify(context.Background(), "Review ready", "GR-014 needs you", Normal)

	if buf.Len() != 0 {
		t.Errorf("wrote %q with notifications off", buf.String())
	}
	if r.count() != 0 {
		t.Errorf("ran %v with notifications off", r.calls)
	}
}

func TestBellOnlyRingsButRunsNothing(t *testing.T) {
	r := &fakeRunner{}
	n, buf, _ := newTest(t, config.NotifyBell, 0, "darwin", r)

	n.Notify(context.Background(), "Review ready", "GR-014", Normal)

	if buf.String() != "\a" {
		t.Errorf("wrote %q, want a single bell", buf.String())
	}
	if r.count() != 0 {
		t.Errorf("bell-only mode ran a subprocess: %v", r.calls)
	}
}

func TestDarwinUsesOsascript(t *testing.T) {
	r := &fakeRunner{}
	n, buf, _ := newTest(t, config.NotifyBellAndOS, 0, "darwin", r)

	n.Notify(context.Background(), "Review ready", "GR-014 needs you", Normal)

	if buf.String() != "\a" {
		t.Errorf("no bell alongside the OS notification: %q", buf.String())
	}
	if r.count() != 1 {
		t.Fatalf("ran %d commands, want 1: %v", r.count(), r.calls)
	}
	got := r.calls[0]
	if got[0] != "osascript" || got[1] != "-e" {
		t.Fatalf("command = %v, want osascript -e ...", got)
	}
	if !strings.Contains(got[2], `display notification "GR-014 needs you" with title "Review ready"`) {
		t.Errorf("script = %q", got[2])
	}
}

func TestLinuxUsesNotifySend(t *testing.T) {
	r := &fakeRunner{}
	n, _, _ := newTest(t, config.NotifyBellAndOS, 0, "linux", r)

	n.Notify(context.Background(), "Auth expired", "claude-code needs re-auth", Critical)

	if r.count() != 1 {
		t.Fatalf("ran %d commands, want 1", r.count())
	}
	got := r.calls[0]
	want := []string{"notify-send", "-u", "critical", "--", "Auth expired", "claude-code needs re-auth"}
	if len(got) != len(want) {
		t.Fatalf("command = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command = %v, want %v", got, want)
		}
	}
}

// TestUnsupportedPlatformDegradesToBell is AC2.
func TestUnsupportedPlatformDegradesToBell(t *testing.T) {
	r := &fakeRunner{}
	n, buf, _ := newTest(t, config.NotifyBellAndOS, 0, "plan9", r)

	n.Notify(context.Background(), "Review ready", "GR-014", Normal)

	if buf.String() != "\a" {
		t.Errorf("wrote %q, want a bell", buf.String())
	}
	if r.count() != 0 {
		t.Errorf("ran a command on an unsupported platform: %v", r.calls)
	}
}

func TestMissingRunnerDegradesToBell(t *testing.T) {
	buf := &bytes.Buffer{}
	cfg := config.Notifications{Mode: config.NotifyBellAndOS}
	// No WithRunner: the daemon has not wired host.Exec in yet.
	n := New(cfg, buf, WithLogger(quietLogger()), WithGOOS("darwin"))

	n.Notify(context.Background(), "Review ready", "GR-014", Normal)

	if buf.String() != "\a" {
		t.Errorf("wrote %q, want a bell", buf.String())
	}
}

// TestRunnerErrorIsNotFatal: a failing notifier must not take anything else down with it.
func TestRunnerErrorIsNotFatal(t *testing.T) {
	r := &fakeRunner{err: context.DeadlineExceeded}
	n, buf, _ := newTest(t, config.NotifyBellAndOS, 0, "darwin", r)

	n.Notify(context.Background(), "Review ready", "GR-014", Normal)

	if buf.String() != "\a" {
		t.Error("the bell did not ring when the OS notification failed")
	}
}

// TestRateLimitCoalesces is AC3: N notifications inside the window produce one alert.
func TestRateLimitCoalesces(t *testing.T) {
	r := &fakeRunner{}
	n, buf, clock := newTest(t, config.NotifyBellAndOS, 30*time.Second, "darwin", r)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		n.Notify(ctx, "Review ready", "a ticket", Normal)
		clock.advance(time.Second)
	}

	if got := strings.Count(buf.String(), "\a"); got != 1 {
		t.Errorf("rang %d times inside the window, want 1", got)
	}
	if r.count() != 1 {
		t.Errorf("ran %d commands inside the window, want 1", r.count())
	}

	// Past the window, the next one gets through and reports what it swallowed.
	clock.advance(time.Minute)
	n.Notify(ctx, "Review ready", "another ticket", Normal)

	if r.count() != 2 {
		t.Fatalf("ran %d commands, want 2 after the window elapsed", r.count())
	}
	if !strings.Contains(r.calls[1][2], "+9 more") {
		t.Errorf("second notification does not report the coalesced ones: %q", r.calls[1][2])
	}

	// After reporting, the counter resets rather than accumulating forever.
	clock.advance(time.Minute)
	n.Notify(ctx, "Review ready", "a third", Normal)
	if strings.Contains(r.calls[2][2], "more") {
		t.Errorf("suppressed count was not reset: %q", r.calls[2][2])
	}
}

func TestZeroWindowDisablesRateLimiting(t *testing.T) {
	r := &fakeRunner{}
	n, _, _ := newTest(t, config.NotifyBellAndOS, 0, "darwin", r)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		n.Notify(ctx, "t", "b", Normal)
	}
	if r.count() != 5 {
		t.Errorf("ran %d commands with rate limiting off, want 5", r.count())
	}
}

// TestAppleScriptEscaping is the adversarial case: ticket titles are user text, and an
// unescaped quote ends the AppleScript literal early and corrupts the command.
func TestAppleScriptEscaping(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantSub   string
		wantNoSub string
	}{
		{"double quote", `fix the "broken" test`, `\"broken\"`, ""},
		{"backslash", `path C:\temp`, `C:\\temp`, ""},
		{"newline", "line one\nline two", "line one line two", "\n"},
		{"carriage return", "line one\rline two", "line one line two", "\r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{}
			n, _, _ := newTest(t, config.NotifyBellAndOS, 0, "darwin", r)
			n.Notify(context.Background(), tt.in, "body", Normal)

			if r.count() != 1 {
				t.Fatalf("ran %d commands", r.count())
			}
			script := r.calls[0][2]
			if !strings.Contains(script, tt.wantSub) {
				t.Errorf("script %q does not contain %q", script, tt.wantSub)
			}
			if tt.wantNoSub != "" && strings.Contains(script, tt.wantNoSub) {
				t.Errorf("script %q still contains %q", script, tt.wantNoSub)
			}
			// The literal must be balanced, or osascript gets a syntax error.
			if unescapedQuotes(script) != 4 {
				t.Errorf("script has unbalanced string literals: %q", script)
			}
		})
	}
}

// unescapedQuotes counts double quotes that actually delimit literals.
func unescapedQuotes(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip whatever is escaped
		case '"':
			n++
		}
	}
	return n
}

func TestUrgencyString(t *testing.T) {
	for u, want := range map[Urgency]string{Low: "low", Normal: "normal", Critical: "critical", Urgency(99): "normal"} {
		if got := u.String(); got != want {
			t.Errorf("Urgency(%d).String() = %q, want %q", int(u), got, want)
		}
	}
}

// TestConcurrentNotifyIsSafe: attention items are raised from several goroutines at once.
func TestConcurrentNotifyIsSafe(t *testing.T) {
	r := &fakeRunner{}
	n, _, _ := newTest(t, config.NotifyBellAndOS, time.Minute, "darwin", r)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.Notify(ctx, "Review ready", "a ticket", Normal)
		}()
	}
	wg.Wait()

	// Exactly one gets through the window, whichever wins the race.
	if r.count() != 1 {
		t.Errorf("ran %d commands, want 1", r.count())
	}
}
