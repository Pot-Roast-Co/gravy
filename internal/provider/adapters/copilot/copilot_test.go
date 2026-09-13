package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

type fakeHost struct {
	host.Host
	specs   []host.ExecSpec
	outputs []*fakeProcess
}

func (h *fakeHost) FS() host.FS { return host.NewLocal("test", 1).FS() }

func (h *fakeHost) Exec(_ context.Context, spec host.ExecSpec) (host.Process, error) {
	h.specs = append(h.specs, spec)
	if len(h.outputs) == 0 {
		return nil, fmt.Errorf("missing CLI")
	}
	p := h.outputs[0]
	h.outputs = h.outputs[1:]
	return p, nil
}

type fakeProcess struct {
	stdout, stderr string
	code           int
	timeout        bool
	killed         bool
}

func (p *fakeProcess) Stdout() io.Reader { return strings.NewReader(p.stdout) }
func (p *fakeProcess) Stderr() io.Reader { return strings.NewReader(p.stderr) }
func (p *fakeProcess) Wait() (host.ExitStatus, error) {
	return host.ExitStatus{Code: p.code, TimedOut: p.timeout}, nil
}
func (p *fakeProcess) Kill() error { p.killed = true; return nil }
func (p *fakeProcess) PID() int    { return 1 }

func TestDetectUsesAuthRPCWithoutModelRequest(t *testing.T) {
	for _, authed := range []bool{false, true} {
		t.Run(fmt.Sprint(authed), func(t *testing.T) {
			response := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"isAuthenticated":%t}}`, authed)
			rpc := &fakeProcess{stdout: fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(response), response)}
			h := &fakeHost{outputs: []*fakeProcess{{stdout: "GitHub Copilot CLI 1.0.83"}, rpc}}
			av, err := New().Detect(context.Background(), h)
			if err != nil || !av.Installed || av.Authenticated != authed {
				t.Fatalf("%+v %v", av, err)
			}
			if !rpc.killed {
				t.Fatal("auth server left running")
			}
			for _, spec := range h.specs {
				for _, arg := range spec.Args {
					if arg == "-p" || arg == "--prompt" {
						t.Fatal("authentication spent a model request")
					}
				}
			}
		})
	}
	av, _ := New().Detect(context.Background(), &fakeHost{})
	if av.Installed || !strings.Contains(av.Detail, "copilot login") {
		t.Fatalf("%+v", av)
	}
}

func TestRunAndResumePreservePermissions(t *testing.T) {
	output := `{"type":"assistant.message","data":{"content":"done"}}
{"type":"assistant.turn_end","data":{}}
{"type":"result","sessionId":"session-1","exitCode":0}`
	h := &fakeHost{outputs: []*fakeProcess{{stdout: output}, {stdout: output}}}
	p := New(WithCommand("custom-copilot"))
	task := provider.AgentTask{WorktreePath: "/repo with spaces", Prompt: "Fix `a`\nand b", Model: "some-model", Timeout: time.Minute, AskPath: filepath.Join(t.TempDir(), "runs", "ask.json"), Allowlist: core.Allowlist{Commands: []core.Pattern{{Match: "go test"}}}}
	handle, err := p.Run(context.Background(), h, task)
	if err != nil {
		t.Fatal(err)
	}
	out, err := handle.Wait()
	if err != nil || out.Class != provider.Success || !out.Session.Valid() {
		t.Fatalf("%+v %v", out, err)
	}
	handle, err = p.Resume(context.Background(), h, out.Session, "continue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handle.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, spec := range h.specs {
		if spec.Cmd != "custom-copilot" || spec.Dir != task.WorktreePath || spec.Timeout != task.Timeout {
			t.Fatalf("%+v", spec)
		}
		if !reflect.DeepEqual(spec.UnsetEnv, []string{"COPILOT_ALLOW_ALL"}) {
			t.Fatalf("missing env removal: %+v", spec)
		}
		if _, err := os.Stat(filepath.Dir(task.AskPath)); err != nil {
			t.Fatal(err)
		}
		args := strings.Join(spec.Args, "\n")
		for _, want := range []string{"--allow-tool=shell(go test)", "--allow-tool=shell(go test *)", "--allow-tool=write", "--model\nsome-model", "--add-dir\n" + filepath.Dir(task.AskPath)} {
			if !strings.Contains(args, want) {
				t.Fatalf("missing %q: %v", want, spec.Args)
			}
		}
		for _, arg := range spec.Args {
			if arg == "--allow-all" || arg == "--allow-all-tools" || arg == "--allow-all-paths" {
				t.Fatalf("permissions widened: %v", spec.Args)
			}
		}
	}
	if !strings.Contains(strings.Join(h.specs[1].Args, " "), "--resume=session-1") {
		t.Fatal(h.specs[1].Args)
	}
	args, _ := runArgs(provider.AgentTask{Model: DefaultModel})
	if strings.Contains(strings.Join(args, " "), "--model") {
		t.Fatal("default passed as a model name")
	}
	for _, bad := range []string{"^go.*", "go),shell(*)"} {
		if _, err := runArgs(provider.AgentTask{Allowlist: core.Allowlist{Commands: []core.Pattern{{Match: bad}}}}); err == nil {
			t.Fatalf("unsafe pattern accepted %q", bad)
		}
	}
	if _, err := p.Resume(context.Background(), h, provider.SessionRef{ProviderID: "other", ID: "session"}, "x"); err == nil {
		t.Fatal("wrong provider accepted")
	}
}

func TestStreamOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, stream, stderr string
		code                 int
		timeout              bool
		want                 provider.FailureClass
	}{
		{name: "recovered model error", stream: `{"type":"session.error","data":{"errorType":"model_call","message":"retrying"}}
{"type":"result","exitCode":0}`, want: provider.Success},
		{name: "successful permission text", stream: `{"type":"tool.execution_complete","data":{"success":true,"result":{"content":"Permission denied (publickey)"}}}
{"type":"result","exitCode":0}`, want: provider.Success},
		{name: "shell permission output", stream: `{"type":"tool.execution_complete","data":{"success":false,"result":{"content":"Permission denied (publickey)"}}}
{"type":"result","exitCode":0}`, want: provider.Success},
		{name: "success", stream: `{"type":"result","sessionId":"abc","exitCode":0}`, want: provider.Success},
		{name: "empty clean exit", want: provider.TaskFailure},
		{name: "result failure with exit zero", stream: `{"type":"result","exitCode":1}`, want: provider.TaskFailure},
		{name: "quota", stream: `{"type":"session.error","data":{"errorType":"quota","message":"quota exhausted"}}`, want: provider.QuotaExhausted},
		{name: "rate limit", stream: `{"type":"session.error","data":{"statusCode":429,"message":"slow down"}}`, want: provider.RateLimited},
		{name: "auth", stream: `{"type":"session.error","data":{"statusCode":401,"message":"sign in"}}`, want: provider.AuthExpired},
		{name: "unknown", code: 1, stderr: "something unexpected", want: provider.TaskFailure},
		{name: "timeout", timeout: true, want: provider.Timeout},
		{name: "permission", stream: `{"type":"tool.execution_start","data":{"toolCallId":"1","toolName":"bash","arguments":{"command":"git push"}}}
{"type":"tool.execution_complete","data":{"toolCallId":"1","success":false,"error":{"code":"permission_denied","message":"Permission denied"}}}
{"type":"result","exitCode":0}`, want: provider.TaskFailure},
		{name: "quota in assistant text", stream: `{"type":"assistant.message","data":{"content":"Fixed the quota exceeded error"}}
{"type":"result","exitCode":0}`, want: provider.Success},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := streamState{calls: map[string]provider.PermissionDenial{}}
			for _, line := range strings.Split(tc.stream, "\n") {
				s.parse(line)
			}
			out := s.outcome(New(), host.ExitStatus{Code: tc.code, TimedOut: tc.timeout}, tc.stderr)
			if out.Class != tc.want {
				t.Fatalf("%+v", out)
			}
			if tc.name == "permission" && (len(out.Denials) != 1 || out.Denials[0].Input["command"] != "git push") {
				t.Fatalf("lost denial: %+v", out)
			}
		})
	}
}

func TestUsageAndMessages(t *testing.T) {
	s := streamState{calls: map[string]provider.PermissionDenial{}}
	e := s.parse(`{"type":"assistant.message","data":{"content":"done"}}`)
	if e == nil || e.Text != "done" || e.Raw == "" {
		t.Fatalf("%+v", e)
	}
	for i := 0; i < 2; i++ {
		s.parse(`{"type":"assistant.usage","data":{"inputTokens":12,"outputTokens":3,"cost":0.25}}`)
	}
	out := s.outcome(New(), host.ExitStatus{}, "")
	if out.TokensIn != 24 || out.TokensOut != 6 || out.CostUSD != nil {
		t.Fatalf("%+v", out)
	}
}

func TestRPCFraming(t *testing.T) {
	body := `{"result":{"isAuthenticated":true}}`
	raw, err := readRPC(bufio.NewReader(strings.NewReader(fmt.Sprintf("Content-Length: %d\r\nContent-Type: application/json\r\n\r\n%s", len(body), body))))
	if err != nil || string(raw) != body {
		t.Fatalf("%s %v", raw, err)
	}
	for _, bad := range []string{"\r\n", "Content-Length: -1\r\n\r\n", "Content-Length: 999999999\r\n\r\n"} {
		if _, err := readRPC(bufio.NewReader(strings.NewReader(bad))); err == nil {
			t.Fatal("bad framing accepted")
		}
	}
}

func TestSessionReferenceExcludesPrompt(t *testing.T) {
	h := &fakeHost{outputs: []*fakeProcess{{stdout: `{"type":"result","sessionId":"abc","exitCode":0}`}}}
	handle, err := New().Run(context.Background(), h, provider.AgentTask{WorktreePath: "/repo", Prompt: "private prompt", LogPath: "", RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := handle.Wait()
	var ref sessionRef
	if err := json.Unmarshal([]byte(out.Session.ID), &ref); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ref.Task, provider.AgentTask{WorktreePath: "/repo"}) {
		t.Fatalf("%+v", ref)
	}
}

func TestCapturedOfflineEdit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "fixtures", "copilot", "offline-edit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := streamState{calls: map[string]provider.PermissionDenial{}}
	kinds := map[provider.EventKind]int{}
	for _, line := range strings.Split(string(raw), "\n") {
		if e := s.parse(line); e != nil {
			kinds[e.Kind]++
		}
	}
	out := s.outcome(New(), host.ExitStatus{}, "")
	if out.Class != provider.Success || !out.Session.Valid() {
		t.Fatalf("%+v", out)
	}
	for _, kind := range []provider.EventKind{provider.EventToolUse, provider.EventToolResult, provider.EventMessage, provider.EventFinished} {
		if kinds[kind] == 0 {
			t.Fatalf("missing %s: %v", kind, kinds)
		}
	}
}
