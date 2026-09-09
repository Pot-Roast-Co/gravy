package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
)

func writePidFile(t *testing.T, home string, pid int) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PidPath(home), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sleeper starts a real process to stop, and returns its pid.
//
// Through host.Exec rather than os/exec: only internal/host may execute anything, and a test
// that reaches around the rule it is testing under is the first place the rule rots.
func sleeper(t *testing.T) int {
	t.Helper()
	h := host.NewLocal("stop-test", 1)
	p, err := h.Exec(context.Background(), host.ExecSpec{
		Cmd: "sleep", Args: []string{"60"}, Timeout: time.Minute,
	})
	if err != nil {
		t.Skipf("cannot start a test process: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Kill()
		_, _ = p.Wait()
	})
	go func() { _, _ = p.Wait() }()
	return p.PID()
}

// TestStopIsIdempotent is the property the command is built around: stopping something already
// stopped is what the person asking for it wanted, and an error there teaches them to ignore
// errors.
func TestStopIsIdempotent(t *testing.T) {
	home := t.TempDir()

	for i := 0; i < 3; i++ {
		res, err := Stop(home, time.Second)
		if err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
		if res.Outcome != StoppedNothing {
			t.Errorf("stop %d: outcome = %v, want StoppedNothing", i, res.Outcome)
		}
	}
}

// TestStopClearsAStalePidfile is the ordinary case after a kill -9, and leaving the file behind
// would make `gravy stop` leave the home in a state it does not claim.
func TestStopClearsAStalePidfile(t *testing.T) {
	home := t.TempDir()
	// A pid that cannot be running: the kernel would have to have wrapped all the way round.
	writePidFile(t, home, 0x7FFFFFFE)

	res, err := Stop(home, time.Second)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res.Outcome != StoppedStale {
		t.Errorf("outcome = %v, want StoppedStale", res.Outcome)
	}
	if _, err := os.Stat(PidPath(home)); !os.IsNotExist(err) {
		t.Error("the stale pidfile survived")
	}
}

func TestStopClearsACorruptPidfile(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(PidPath(home), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Stop(home, time.Second)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res.Outcome != StoppedStale {
		t.Errorf("outcome = %v, want StoppedStale", res.Outcome)
	}
	if _, err := os.Stat(PidPath(home)); !os.IsNotExist(err) {
		t.Error("the corrupt pidfile survived")
	}
}

// TestStopTerminatesALiveProcess also covers the wait: reporting "stopped" while the socket is
// still bound would make the obvious next step — starting another daemon — fail with "already
// running".
func TestStopTerminatesALiveProcess(t *testing.T) {
	home := t.TempDir()
	pid := sleeper(t)
	writePidFile(t, home, pid)

	res, err := Stop(home, 5*time.Second)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res.Outcome != Stopped {
		t.Fatalf("outcome = %v, want Stopped", res.Outcome)
	}
	if res.PID != pid {
		t.Errorf("PID = %d, want %d", res.PID, pid)
	}
	if Running(home) != 0 {
		t.Error("Stop returned before the process was gone")
	}
}

// TestStopThenStartIsClean: the reason to wait rather than fire and forget.
func TestStopThenStartIsClean(t *testing.T) {
	home := t.TempDir()
	pid := sleeper(t)
	writePidFile(t, home, pid)

	if _, err := Stop(home, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// acquirePidFile is what a starting daemon calls, and it refuses when one is live.
	if err := acquirePidFile(filepath.Join(home, PidFileName)); err != nil {
		t.Errorf("a daemon could not start after Stop returned: %v", err)
	}
}
