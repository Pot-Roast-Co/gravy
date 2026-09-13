//go:build unix

package host

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

func TestPreviewServicesCancelBothAndTheirChildren(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"backend", "frontend"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "backend", "test.env"), []byte("PREVIEW_TEST_VALUE='loaded value'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	r := &TerminalRun{Dir: root, stdout: &output, stderr: &output, Services: []core.PreviewService{
		{Name: "Backend", Dir: "backend", EnvFile: "test.env", Command: `echo "$PREVIEW_TEST_VALUE"; sleep 60 & echo $! > child.pid; wait`},
		{Name: "Frontend", Dir: "frontend", Command: `echo frontend-ready; sleep 60 & echo $! > child.pid; wait`},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.runServices(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	var pids []int
	for _, dir := range []string{"backend", "frontend"} {
		for {
			data, err := os.ReadFile(filepath.Join(root, dir, "child.pid"))
			if err == nil && len(bytes.TrimSpace(data)) > 0 {
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil {
					t.Fatal(err)
				}
				pids = append(pids, pid)
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("services did not start together")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInterrupted) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preview cancellation hung")
	}
	for _, pid := range pids {
		// A killed child can briefly remain a zombie until the system reaps it.
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err == nil && !strings.Contains(string(data), ") Z ") {
			t.Fatalf("child %d survived", pid)
		}
	}
	for _, want := range []string{"[Backend] loaded value", "[Frontend] frontend-ready"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q in %s", want, output.String())
		}
	}
}

func TestPreviewPreflightsEveryService(t *testing.T) {
	root := t.TempDir()
	var output bytes.Buffer
	r := &TerminalRun{Dir: root, stdout: &output, stderr: &output, Services: []core.PreviewService{
		{Name: "First", Command: "touch started"},
		{Name: "MissingEnv", Command: "sleep 60", EnvFile: "missing.env"},
	}}
	if err := r.runServices(context.Background()); err == nil || !strings.Contains(err.Error(), "MissingEnv") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "started")); !os.IsNotExist(err) {
		t.Fatal("started before validating all services")
	}
}

func TestPreviewServiceFailureStopsPeer(t *testing.T) {
	var output bytes.Buffer
	r := &TerminalRun{Dir: t.TempDir(), stdout: &output, stderr: &output, Services: []core.PreviewService{
		{Name: "Backend", Command: "exit 7"}, {Name: "Frontend", Command: "sleep 60"},
	}}
	start := time.Now()
	if err := r.runServices(context.Background()); err == nil || !strings.Contains(err.Error(), "Backend") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("peer survived service failure")
	}
}
