package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/bobbybrady/gravy/internal/config"
)

// Urgency ranks a notification. It maps onto the platform's own notion of urgency where one
// exists, and is otherwise advisory.
type Urgency int

// The urgency levels.
const (
	Low Urgency = iota
	Normal
	Critical
)

// String renders the urgency for logs and for notify-send's -u flag.
func (u Urgency) String() string {
	switch u {
	case Low:
		return "low"
	case Critical:
		return "critical"
	default:
		return "normal"
	}
}

// Runner executes a command. It is the seam that keeps this package clear of os/exec: only
// internal/host may execute anything (ARCHITECTURE.md §1.1), so the daemon supplies a Runner
// backed by host.Host.Exec and this package never learns how a process is started.
type Runner interface {
	Run(ctx context.Context, cmd string, args ...string) error
}

// Notifier tells the human that Gravy needs them.
//
// It is safe for concurrent use: attention items are raised from the scheduler, from finishing
// runs, and from validation, all at once.
type Notifier struct {
	mode   config.NotifyMode
	window time.Duration
	runner Runner
	out    io.Writer
	log    *slog.Logger
	goos   string
	now    func() time.Time

	mu         sync.Mutex
	lastSent   time.Time
	suppressed int
}

// Option configures a Notifier.
type Option func(*Notifier)

// WithRunner supplies the command runner used for OS notifications. Without one, Gravy degrades
// to the terminal bell rather than failing.
func WithRunner(r Runner) Option { return func(n *Notifier) { n.runner = r } }

// WithWriter sets where the terminal bell is written. Defaults to os.Stderr via New's caller.
func WithWriter(w io.Writer) Option { return func(n *Notifier) { n.out = w } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(n *Notifier) { n.log = l } }

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) Option { return func(n *Notifier) { n.now = now } }

// WithGOOS overrides the detected platform, for tests.
func WithGOOS(goos string) Option { return func(n *Notifier) { n.goos = goos } }

// New builds a Notifier from configuration.
func New(cfg config.Notifications, out io.Writer, opts ...Option) *Notifier {
	n := &Notifier{
		mode:   cfg.Mode,
		window: cfg.RateLimitWindow.D(),
		out:    out,
		log:    slog.Default(),
		goos:   runtime.GOOS,
		now:    time.Now,
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// Notify raises a notification, subject to the configured mode and rate limit.
//
// It never returns an error: failing to ring a bell must not fail the run that triggered it, and
// the item is in the Needs You queue regardless of whether the ping arrives. Problems are
// logged.
func (n *Notifier) Notify(ctx context.Context, title, body string, urgency Urgency) {
	if n.mode == config.NotifyOff {
		return
	}

	send, suppressed := n.admit()
	if !send {
		return
	}
	if suppressed > 0 {
		// Say that alerts were coalesced rather than silently under-reporting.
		body = fmt.Sprintf("%s (+%d more while quiet)", body, suppressed)
	}

	if _, err := io.WriteString(n.out, "\a"); err != nil {
		n.log.Debug("notify: writing terminal bell failed", "error", err)
	}

	if n.mode != config.NotifyBellAndOS {
		return
	}
	if n.runner == nil {
		n.log.Debug("notify: no command runner configured; bell only")
		return
	}

	cmd, args, ok := n.osCommand(title, body, urgency)
	if !ok {
		// An unsupported platform is not an error. The bell already rang.
		n.log.Warn("notify: no OS notification mechanism for this platform; bell only", "goos", n.goos)
		return
	}
	if err := n.runner.Run(ctx, cmd, args...); err != nil {
		n.log.Warn("notify: OS notification failed", "command", cmd, "error", err)
	}
}

// admit applies the rate limit, returning whether to send and how many were suppressed since the
// last send.
//
// The limit applies to every urgency alike. Suppressing a ping is not the same as losing the
// item: everything that needs the human is in the Needs You queue whether or not an alert fired.
func (n *Notifier) admit() (send bool, suppressed int) {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := n.now()
	if n.window > 0 && !n.lastSent.IsZero() && now.Sub(n.lastSent) < n.window {
		n.suppressed++
		return false, 0
	}
	suppressed = n.suppressed
	n.suppressed = 0
	n.lastSent = now
	return true, suppressed
}

// osCommand returns the platform's notification command, or ok=false where there is none.
func (n *Notifier) osCommand(title, body string, urgency Urgency) (cmd string, args []string, ok bool) {
	switch n.goos {
	case "darwin":
		script := fmt.Sprintf("display notification %s with title %s",
			appleScriptString(body), appleScriptString(title))
		return "osascript", []string{"-e", script}, true
	case "linux":
		return "notify-send", []string{"-u", urgency.String(), "--", title, body}, true
	default:
		return "", nil, false
	}
}

// appleScriptString quotes a Go string as an AppleScript string literal.
//
// Ticket titles are user text and routinely contain quotes and backslashes; unescaped, they end
// the literal early and turn the rest of the title into broken AppleScript.
func appleScriptString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	// Newlines are legal inside an AppleScript literal but make the -e argument awkward, and a
	// notification is one line anyway.
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return `"` + r.Replace(s) + `"`
}
