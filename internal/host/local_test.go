package host

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func newHost(t *testing.T) *LocalHost {
	t.Helper()
	return NewLocal("local", 4)
}

// readAll drains a reader in a goroutine and returns a channel of the full contents.
func readAll(r interface{ Read([]byte) (int, error) }) <-chan string {
	ch := make(chan string, 1)
	go func() {
		b := make([]byte, 0, 4096)
		buf := make([]byte, 1024)
		for {
			n, err := r.Read(buf)
			b = append(b, buf[:n]...)
			if err != nil {
				break
			}
		}
		ch <- string(b)
	}()
	return ch
}

// TestExecStreamsIncrementally is AC1: output must arrive before the process exits.
//
// This is what lets the TUI show a run live, and it is the difference between a wedged run being
// visible and being indistinguishable from a slow one.
func TestExecStreamsIncrementally(t *testing.T) {
	h := newHost(t)
	// Print immediately, then stay alive. If Exec buffered to completion, nothing would be
	// readable until the sleep finished.
	p, err := h.Exec(context.Background(), ExecSpec{
		Cmd:  "sh",
		Args: []string{"-c", "echo first; sleep 5; echo second"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	defer p.Kill()

	line := make(chan string, 1)
	go func() {
		r := bufio.NewReader(p.Stdout())
		s, _ := r.ReadString('\n')
		line <- strings.TrimSpace(s)
	}()

	select {
	case got := <-line:
		if got != "first" {
			t.Errorf("first line = %q, want %q", got, "first")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no output within 3s; Exec appears to buffer until exit")
	}
}

func TestExecCapturesOutputAndExitCode(t *testing.T) {
	h := newHost(t)
	p, err := h.Exec(context.Background(), ExecSpec{
		Cmd:  "sh",
		Args: []string{"-c", "echo to-stdout; echo to-stderr >&2; exit 3"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	outCh, errCh := readAll(p.Stdout()), readAll(p.Stderr())

	st, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Code != 3 {
		t.Errorf("exit code = %d, want 3", st.Code)
	}
	if st.TimedOut {
		t.Error("TimedOut set on a normal exit")
	}
	if got := strings.TrimSpace(<-outCh); got != "to-stdout" {
		t.Errorf("stdout = %q", got)
	}
	if got := strings.TrimSpace(<-errCh); got != "to-stderr" {
		t.Errorf("stderr = %q", got)
	}
	if st.Duration <= 0 {
		t.Error("duration was not recorded")
	}
}

// TestWaitIsIdempotent: the orchestrator and the kill path may both wait on a process.
func TestWaitIsIdempotent(t *testing.T) {
	h := newHost(t)
	p, err := h.Exec(context.Background(), ExecSpec{Cmd: "sh", Args: []string{"-c", "exit 7"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err1 := p.Wait()
	second, err2 := p.Wait()
	if err1 != nil || err2 != nil {
		t.Fatalf("Wait errors: %v, %v", err1, err2)
	}
	if first.Code != second.Code || first.Code != 7 {
		t.Errorf("Wait returned %d then %d, want 7 both times", first.Code, second.Code)
	}
}

// TestTimeoutProducesTimeoutStatus is AC3: a timeout is a distinct, classifiable outcome, not a
// generic error. It classifies as core.Timeout and is retried once; an unexplained failure is a
// task failure and is not.
func TestTimeoutProducesTimeoutStatus(t *testing.T) {
	h := newHost(t)
	start := time.Now()
	p, err := h.Exec(context.Background(), ExecSpec{
		Cmd:     "sh",
		Args:    []string{"-c", "sleep 30"},
		Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	st, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !st.TimedOut {
		t.Error("TimedOut is false after the timeout elapsed")
	}
	if !st.Signaled {
		t.Error("Signaled is false after a timeout kill")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout took %v to take effect", elapsed)
	}
}

func TestNoTimeoutWhenUnset(t *testing.T) {
	h := newHost(t)
	p, err := h.Exec(context.Background(), ExecSpec{Cmd: "sh", Args: []string{"-c", "sleep 0.2"}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if st.TimedOut || st.Code != 0 {
		t.Errorf("status = %+v, want a clean exit", st)
	}
}

// TestCancelKillsProcessTree is AC2, and the one that actually matters.
//
// A provider CLI spawns compilers, test runners and language servers. Killing only the direct
// child leaves those running: they hold the worktree open, burn CPU, and keep writing to files
// in a directory Gravy is about to remove.
func TestCancelKillsProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are not available on windows")
	}
	h := newHost(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	ctx, cancel := context.WithCancel(context.Background())
	// The child starts a grandchild that outlives it, records the grandchild's pid, and then
	// waits. Killing only the child would leave the grandchild alive.
	p, err := h.Exec(ctx, ExecSpec{
		Cmd:  "sh",
		Args: []string{"-c", "sleep 60 & echo $! > " + pidFile + "; echo ready; sleep 60"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	// Wait for the grandchild to exist.
	var grandchild int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				grandchild = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatal("grandchild never started")
	}
	if !processAlive(grandchild) {
		t.Fatal("grandchild was not alive before cancellation")
	}

	cancel()

	// Check the grandchild directly, and BEFORE waiting on the parent.
	//
	// Waiting first would mask the failure this test exists to catch: with no process group,
	// nothing is killed, Wait blocks for the child's full lifetime, and by the time it returns
	// the grandchild has exited on its own — so the assertion passes for the wrong reason. The
	// only honest measurement is elapsed time from cancel to the grandchild being gone.
	deadline = time.Now().Add(2 * time.Second)
	killed := false
	for time.Now().Before(deadline) {
		if !processAlive(grandchild) {
			killed = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !killed {
		// Do not leave strays behind if the assertion fails.
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		_ = p.Kill()
		t.Fatalf("grandchild %d survived cancellation by more than 2s", grandchild)
	}

	// The direct child must be gone too, and promptly.
	waited := make(chan error, 1)
	go func() {
		_, err := p.Wait()
		waited <- err
	}()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("Wait after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the direct child was still running 2s after cancellation")
	}
}

// processAlive reports whether a pid exists. Signal 0 performs the permission and existence
// checks without delivering anything.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestKillIsSafeBeforeAndAfterExit(t *testing.T) {
	h := newHost(t)
	p, err := h.Exec(context.Background(), ExecSpec{Cmd: "sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	// Killing an already-finished process must not error; the TUI's kill switch can race a
	// run that has just ended.
	if err := p.Kill(); err != nil {
		t.Errorf("Kill after exit: %v", err)
	}
}

func TestExecRespectsDirAndEnv(t *testing.T) {
	h := newHost(t)
	dir := t.TempDir()
	p, err := h.Exec(context.Background(), ExecSpec{
		Cmd:  "sh",
		Args: []string{"-c", "pwd; echo $GRAVY_TEST_VAR"},
		Dir:  dir,
		Env:  map[string]string{"GRAVY_TEST_VAR": "set-by-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := readAll(p.Stdout())
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	got := <-out
	// macOS resolves /var to /private/var, so compare resolved paths.
	wantDir, _ := filepath.EvalSymlinks(dir)
	gotDir, _ := filepath.EvalSymlinks(strings.TrimSpace(strings.Split(got, "\n")[0]))
	if gotDir != wantDir {
		t.Errorf("working directory = %q, want %q", gotDir, wantDir)
	}
	if !strings.Contains(got, "set-by-test") {
		t.Errorf("environment variable not passed through: %q", got)
	}
}

func TestExecStdin(t *testing.T) {
	h := newHost(t)
	p, err := h.Exec(context.Background(), ExecSpec{
		Cmd:   "cat",
		Stdin: strings.NewReader("hello from stdin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	out := readAll(p.Stdout())
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := <-out; got != "hello from stdin" {
		t.Errorf("stdout = %q", got)
	}
}

func TestExecRejectsEmptyCommand(t *testing.T) {
	h := newHost(t)
	if _, err := h.Exec(context.Background(), ExecSpec{}); err == nil {
		t.Error("Exec accepted an empty command")
	}
}

func TestExecMissingBinary(t *testing.T) {
	h := newHost(t)
	if _, err := h.Exec(context.Background(), ExecSpec{Cmd: "gravy-no-such-binary-9000"}); err == nil {
		t.Error("Exec accepted a nonexistent binary")
	}
}

// TestCapabilities is AC4.
func TestCapabilities(t *testing.T) {
	h := newHost(t)
	caps, err := h.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.OS != runtime.GOOS {
		t.Errorf("OS = %q, want %q", caps.OS, runtime.GOOS)
	}
	if caps.Arch != runtime.GOARCH {
		t.Errorf("Arch = %q, want %q", caps.Arch, runtime.GOARCH)
	}
	// git and go are both required to build and test this repository, so they are present by
	// construction wherever this test runs.
	for _, tool := range []string{"git", "go"} {
		version, ok := caps.Tools[tool]
		if !ok {
			t.Errorf("%s was not detected", tool)
			continue
		}
		if version == "" {
			t.Errorf("%s detected but no version parsed", tool)
		}
	}
	if _, ok := caps.Tools["gravy-no-such-tool-9000"]; ok {
		t.Error("a nonexistent tool was reported as present")
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if caps.RAMBytes == 0 {
			t.Error("RAMBytes = 0 on a platform where it is detectable")
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"git version 2.39.5 (Apple Git-154)", "2.39.5"},
		{"go version go1.23.2 darwin/arm64", "1.23.2"},
		{"v22.11.0", "22.11.0"},
		{"Python 3.12.1", "3.12.1"},
		{"Docker version 24.0.6, build ed223bc", "24.0.6"},
		{"Xcode 15.2\nBuild version 15C500b", "15.2"},
		{"no numbers here", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := parseVersion(tt.in); got != tt.want {
			t.Errorf("parseVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSlotsAreRaceFree is AC5.
func TestSlotsAreRaceFree(t *testing.T) {
	const total = 8
	h := NewLocal("local", total)

	var wg sync.WaitGroup
	var claimed int64
	var mu sync.Mutex
	peak := 0
	held := 0

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !h.TryClaim() {
				return
			}
			mu.Lock()
			held++
			if held > peak {
				peak = held
			}
			claimed++
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			held--
			mu.Unlock()
			h.Release()
		}()
	}
	wg.Wait()

	if peak > total {
		t.Errorf("%d slots were held at once, cap is %d", peak, total)
	}
	if claimed == 0 {
		t.Error("no slots were ever claimed")
	}
	used, gotTotal := h.Slots()
	if used != 0 {
		t.Errorf("used = %d after every claim was released, want 0", used)
	}
	if gotTotal != total {
		t.Errorf("total = %d, want %d", gotTotal, total)
	}
}

func TestSlotsExhaust(t *testing.T) {
	h := NewLocal("local", 2)
	// Claimed one at a time and asserted separately: written as a single || expression, a
	// failure on the first claim short-circuits the second, so "two slots are claimable" would
	// silently go untested.
	if !h.TryClaim() {
		t.Fatal("could not claim the first slot")
	}
	if !h.TryClaim() {
		t.Fatal("could not claim the second slot")
	}
	if h.TryClaim() {
		t.Error("claimed a third slot from a host with two")
	}
	h.Release()
	if !h.TryClaim() {
		t.Error("could not reclaim a released slot")
	}
}

func TestNewLocalFloorsSlots(t *testing.T) {
	if _, total := NewLocal("h", 0).Slots(); total != 1 {
		t.Errorf("total = %d for a zero-slot host, want 1", total)
	}
}

func TestFS(t *testing.T) {
	fs := NewLocal("local", 1).FS()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")

	if fs.Exists(path) {
		t.Error("Exists reported a nonexistent file")
	}
	if err := fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := fs.WriteFile(path, []byte("contents"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !fs.Exists(path) {
		t.Error("Exists reported a written file as missing")
	}
	b, err := fs.ReadFile(path)
	if err != nil || string(b) != "contents" {
		t.Errorf("ReadFile = %q, %v", b, err)
	}
	if _, err := fs.Stat(path); err != nil {
		t.Errorf("Stat: %v", err)
	}
	if err := fs.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if fs.Exists(path) {
		t.Error("file survived RemoveAll")
	}
}
