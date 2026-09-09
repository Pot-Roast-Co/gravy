package daemon

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
)

// StopOutcome says what stopping found.
type StopOutcome int

// The outcomes.
const (
	// StoppedNothing means no daemon was running. It is a success: stopping something that is
	// already stopped is what someone asking for it wanted.
	StoppedNothing StopOutcome = iota
	// StoppedStale means the pidfile outlived its process, and was cleared.
	StoppedStale
	// Stopped means a live daemon was asked to shut down and did.
	Stopped
	// StoppedSlow means it was asked and had not exited before the wait ran out. The signal
	// was delivered, so it is very likely on its way down.
	StoppedSlow
)

// StopResult reports what happened, so the caller can say something true.
type StopResult struct {
	Outcome StopOutcome
	// PID is the process that was found, or zero when there was none.
	PID int
}

// Stop asks the daemon for a home to shut down, and waits for it to go.
//
// Idempotent by design: no pidfile, a stale pidfile, and a live daemon all succeed. Someone
// stopping a daemon twice, or stopping one that already crashed, has got what they asked for
// either way, and an error there would only teach them to ignore errors.
func Stop(home string, wait time.Duration) (StopResult, error) {
	path := PidPath(home)

	pid, err := readPid(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StopResult{Outcome: StoppedNothing}, nil
		}
		// A pidfile that is not a pid is not something to guess about, but it is also not
		// worth failing over: clear it and report nothing running.
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return StopResult{}, fmt.Errorf("stop: %w", err)
		}
		return StopResult{Outcome: StoppedStale}, nil
	}

	if !host.Alive(pid) {
		// The ordinary case after a hard kill. Clearing it here means the next start does not
		// have to, and `gravy stop` leaves the home in the state it claims.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return StopResult{}, fmt.Errorf("stop: remove stale pidfile: %w", err)
		}
		return StopResult{Outcome: StoppedStale, PID: pid}, nil
	}

	if err := host.Terminate(pid); err != nil {
		return StopResult{PID: pid}, fmt.Errorf("stop: %w", err)
	}

	// Wait for it to actually go. Reporting "stopped" while the socket is still bound would
	// make the obvious next step — starting another one — fail with "already running".
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !host.Alive(pid) {
			return StopResult{Outcome: Stopped, PID: pid}, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return StopResult{Outcome: StoppedSlow, PID: pid}, nil
}
