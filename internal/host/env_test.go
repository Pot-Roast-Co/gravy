package host

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestExecUnsetsInheritedEnvironment(t *testing.T) {
	t.Setenv("GRAVY_TEST_UNSET", "inherited")
	spec := ExecSpec{Cmd: "sh", Args: []string{"-c", `test -z "${GRAVY_TEST_UNSET+x}"`}, Env: map[string]string{"GRAVY_TEST_UNSET": "overlay"}, UnsetEnv: []string{"GRAVY_TEST_UNSET"}}
	p, err := NewLocal("test", 1).Exec(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, p.Stdout())
	io.Copy(io.Discard, p.Stderr())
	status, err := p.Wait()
	if err != nil || status.Code != 0 {
		t.Fatalf("%+v %v", status, err)
	}
	remote := remoteCommand(spec)
	if !strings.Contains(remote, "unset") || strings.Index(remote, "unset") < strings.Index(remote, "export") {
		t.Fatal(remote)
	}
}
