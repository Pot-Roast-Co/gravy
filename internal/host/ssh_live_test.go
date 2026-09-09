package host

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

// TestSSHLive probes a real machine, and is skipped unless one is named.
//
// Run it with GRAVY_SSH_TARGET=air. It is not part of the ordinary suite: a test that needs
// another computer switched on is not a test the gate can depend on.
func TestSSHLive(t *testing.T) {
	target := os.Getenv("GRAVY_SSH_TARGET")
	if target == "" {
		t.Skip("set GRAVY_SSH_TARGET to probe a real host")
	}

	h := NewSSH("live", target, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	caps, err := h.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	t.Logf("os=%s arch=%s ram=%d", caps.OS, caps.Arch, caps.RAMBytes)
	t.Logf("tools=%v", caps.Tools)
	t.Logf("providers=%v", caps.Providers)
	if caps.OS == "" || caps.Arch == "" {
		t.Error("the probe did not identify the machine")
	}

	// Quoting a naive join would mangle, and a working directory that must be honoured.
	p, err := h.Exec(ctx, ExecSpec{
		Cmd: "sh", Args: []string{"-c", `echo "quoted 'arg' works"; pwd`},
		Dir: "/tmp", Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	out, _ := io.ReadAll(p.Stdout())
	st, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	t.Logf("exec code=%d out=%q", st.Code, string(out))

	// The remote filesystem, which contextbuild reads project documents through.
	const probe = "/tmp/gravy-ssh-probe.txt"
	if err := h.FS().WriteFile(probe, []byte("hello from gravy\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	body, err := h.FS().ReadFile(probe)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(body) != "hello from gravy\n" {
		t.Errorf("read back %q", body)
	}
	if !h.FS().Exists(probe) {
		t.Error("Exists says a file it just wrote is not there")
	}
	if err := h.FS().RemoveAll(probe); err != nil {
		t.Errorf("RemoveAll: %v", err)
	}
	if h.FS().Exists(probe) {
		t.Error("the probe file survived removal")
	}
}
