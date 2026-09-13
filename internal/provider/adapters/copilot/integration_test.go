package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// This opt-in test runs the actual CLI against a fake local model, with isolated
// Copilot state and offline mode. It never needs a subscription or paid model call.
func TestRealCLIWithFakeModel(t *testing.T) {
	command := os.Getenv("GRAVY_COPILOT_CLI")
	if command == "" {
		t.Skip("set GRAVY_COPILOT_CLI to test the real CLI against a free local fake model")
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gravy-test","object":"model"}]}`)
			return
		}
		call := requests.Add(1)
		var req struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		if !req.Stream {
			http.Error(w, "expected streaming", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"gravy-test","choices":[{"index":0,"delta":{"role":"assistant","content":"gravy smoke ok"},"finish_reason":null}]}`,
			`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"gravy-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		}
		if call == 1 {
			chunks = []string{
				`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"gravy-test","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"create-1","type":"function","function":{"name":"create","arguments":"{\"path\":\"hello.txt\",\"file_text\":\"hello\\n\"}"}}]},"finish_reason":null}]}`,
				`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"gravy-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			}
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("COPILOT_HOME", t.TempDir())
	t.Setenv("COPILOT_OFFLINE", "true")
	t.Setenv("COPILOT_PROVIDER_BASE_URL", server.URL+"/v1")
	t.Setenv("COPILOT_PROVIDER_TYPE", "openai")
	t.Setenv("COPILOT_MODEL", "gravy-test")
	// Explicitly remove inherited provider credentials for the local fake endpoint.
	t.Setenv("COPILOT_PROVIDER_API_KEY", "")
	t.Setenv("COPILOT_PROVIDER_BEARER_TOKEN", "")
	p, h := New(WithCommand(command)), host.NewLocal("local", 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	av, err := p.Detect(ctx, h)
	if err != nil || !av.Installed || strings.Contains(av.Detail, "could not") {
		t.Fatalf("detect: %+v %v", av, err)
	}
	if requests.Load() != 0 {
		t.Fatal("auth check called model")
	}
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	handle, err := p.Run(ctx, h, provider.AgentTask{WorktreePath: t.TempDir(), Prompt: "Reply with ok.", Model: DefaultModel, Timeout: 20 * time.Second, LogPath: logPath})
	if err != nil {
		t.Fatal(err)
	}
	out, err := handle.Wait()
	if err != nil || out.Class != provider.Success {
		t.Fatalf("run: %+v %v", out, err)
	}
	var ref sessionRef
	if err := json.Unmarshal([]byte(out.Session.ID), &ref); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(ref.Task.WorktreePath, "hello.txt"))
	if err != nil || string(contents) != "hello\n" {
		t.Fatalf("actual CLI file edit: %q %v", contents, err)
	}
	var text string
	for event := range handle.Events() {
		if event.Kind == provider.EventMessage {
			text += event.Text
		}
	}
	if !strings.Contains(text, "gravy smoke ok") {
		t.Fatalf("lost model response: %s", text)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(raw), `"type":"result"`) {
		t.Fatalf("lost raw log: %s %v", raw, err)
	}
	handle, err = p.Resume(ctx, h, out.Session, "Again, please.")
	if err != nil {
		t.Fatal(err)
	}
	out, err = handle.Wait()
	if err != nil || out.Class != provider.Success {
		t.Fatalf("resume: %+v %v", out, err)
	}
	if requests.Load() != 3 {
		t.Fatalf("model requests: %d", requests.Load())
	}
	// Captured output can be saved deliberately for parser regression fixtures.
	if target := os.Getenv("GRAVY_COPILOT_CAPTURE"); target != "" {
		if err := os.WriteFile(target, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// A real account is necessary to verify GitHub entitlement and model access.
func TestLiveCopilotAccount(t *testing.T) {
	if os.Getenv("GRAVY_COPILOT_LIVE") != "1" {
		t.Skip("GRAVY_COPILOT_LIVE=1 uses your authenticated Copilot account and consumes requests")
	}
	command := os.Getenv("GRAVY_COPILOT_CLI")
	if command == "" {
		command = DefaultCommand
	}
	p, h := New(WithCommand(command)), host.NewLocal("local", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	handle, err := p.Run(ctx, h, provider.AgentTask{WorktreePath: t.TempDir(), Prompt: "Create a file named hello.txt containing exactly hello followed by a newline. Do nothing else.", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	out, err := handle.Wait()
	if err != nil || out.Class != provider.Success {
		t.Fatalf("%+v %v", out, err)
	}
	// Decode the worktree from the resumable reference to verify the actual file edit.
	var ref sessionRef
	if err := json.Unmarshal([]byte(out.Session.ID), &ref); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(ref.Task.WorktreePath, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if string(b) != "hello\n" {
		t.Fatalf("file: %q", b)
	}
}

func TestRealCLIDeniesShellWithInheritedAllowAll(t *testing.T) {
	command := os.Getenv("GRAVY_COPILOT_CLI")
	if command == "" {
		t.Skip("set GRAVY_COPILOT_CLI to test the real CLI against a free local fake model")
	}
	t.Setenv("COPILOT_ALLOW_ALL", "false")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gravy-test","object":"model"}]}`)
			return
		}
		call := requests.Add(1)
		var req struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		if !req.Stream {
			http.Error(w, "expected streaming", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"gravy-test","choices":[{"index":0,"delta":{"role":"assistant","content":"gravy smoke ok"},"finish_reason":null}]}`,
			`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"gravy-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		}
		if call == 1 {
			chunks = []string{
				`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"gravy-test","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"create-1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"touch denied.txt\"}"}}]},"finish_reason":null}]}`,
				`{"id":"mock-1","object":"chat.completion.chunk","created":1,"model":"gravy-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			}
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("COPILOT_HOME", t.TempDir())
	t.Setenv("COPILOT_OFFLINE", "true")
	t.Setenv("COPILOT_PROVIDER_BASE_URL", server.URL+"/v1")
	t.Setenv("COPILOT_PROVIDER_TYPE", "openai")
	t.Setenv("COPILOT_MODEL", "gravy-test")
	// Explicitly remove inherited provider credentials for the local fake endpoint.
	t.Setenv("COPILOT_PROVIDER_API_KEY", "")
	t.Setenv("COPILOT_PROVIDER_BEARER_TOKEN", "")
	p, h := New(WithCommand(command)), host.NewLocal("local", 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	av, err := p.Detect(ctx, h)
	if err != nil || !av.Installed || strings.Contains(av.Detail, "could not") {
		t.Fatalf("detect: %+v %v", av, err)
	}
	if requests.Load() != 0 {
		t.Fatal("auth check called model")
	}
	worktree := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	handle, err := p.Run(ctx, h, provider.AgentTask{WorktreePath: worktree, Prompt: "Reply with ok.", Model: DefaultModel, Timeout: 20 * time.Second, LogPath: logPath})
	if err != nil {
		t.Fatal(err)
	}
	out, err := handle.Wait()
	if err != nil || out.Class != provider.TaskFailure || len(out.Denials) != 1 {
		t.Fatalf("run: %+v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "denied.txt")); !os.IsNotExist(err) {
		t.Fatalf("disallowed command ran: %v", err)
	}
}
