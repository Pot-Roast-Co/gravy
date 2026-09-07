package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pot-roast-co/gravy/internal/host"
)

// PidFileName is the daemon's pidfile inside the Gravy home directory.
const PidFileName = "gravyd.pid"

// PidPath is where the pidfile lives for a given home.
func PidPath(home string) string { return filepath.Join(home, PidFileName) }

// acquirePidFile claims the pidfile for this process.
//
// A pidfile outliving its process is the ordinary case after a kill -9, so a stale one is cleared
// rather than treated as a running daemon; refusing to start otherwise would mean deleting a file
// by hand after every hard stop. A pidfile naming a live process is the opposite case and must
// stop us, because two daemons writing one SQLite database is the failure this guards.
func acquirePidFile(path string) error {
	// Deliberately no exemption for our own pid: a second Daemon inside one process is still a
	// second daemon, and letting it overwrite the file would then let its own cleanup delete a
	// live daemon's pidfile on the way out.
	if pid, err := readPid(path); err == nil {
		if host.Alive(pid) {
			return fmt.Errorf("a gravy daemon is already running (pid %d)", pid)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale pidfile %s: %w", path, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("write pidfile %s: %w", path, err)
	}
	return nil
}

// releasePidFile removes the pidfile if it is still ours, so a restart that raced us does not
// have its file deleted out from under it.
func releasePidFile(path string) error {
	pid, err := readPid(path)
	if err != nil {
		return nil
	}
	if pid != os.Getpid() {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pidfile %s: %w", path, err)
	}
	return nil
}

// readPid reads a pidfile.
func readPid(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("pidfile %s is not a pid: %w", path, err)
	}
	return pid, nil
}

// Running reports the pid of a live daemon for this home, or zero.
//
// Clients use it to decide whether to start one. It answers from the pidfile rather than by
// dialling, so "starting up, socket not bound yet" is not mistaken for "not running".
func Running(home string) int {
	pid, err := readPid(PidPath(home))
	if err != nil || !host.Alive(pid) {
		return 0
	}
	return pid
}
