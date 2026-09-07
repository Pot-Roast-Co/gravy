package codex

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/provider"
)

// streamEvent is one line of `codex exec --json` output.
//
// The shapes here were captured from codex-cli 0.152.1 against a real run; see the fixtures in
// docs/fixtures/codex/. Fields the CLI does not send are simply absent, which is why everything
// is a pointer or a zero-able value rather than something the parser insists on.
type streamEvent struct {
	Type string `json:"type"`

	// thread.started
	ThreadID string `json:"thread_id"`

	// item.started / item.completed
	Item *struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Text string `json:"text"`
		// command_execution
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
		Status           string `json:"status"`
		// file_change
		Changes []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"changes"`
	} `json:"item"`

	// turn.completed
	Usage *struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
	} `json:"usage"`

	// error / turn.failed
	Message string `json:"message"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// The event types codex emits.
const (
	typeThreadStarted = "thread.started"
	typeTurnStarted   = "turn.started"
	typeTurnCompleted = "turn.completed"
	typeTurnFailed    = "turn.failed"
	typeItemStarted   = "item.started"
	typeItemCompleted = "item.completed"
	typeError         = "error"
)

// The item types inside item.* events.
const (
	itemAgentMessage     = "agent_message"
	itemReasoning        = "reasoning"
	itemCommandExecution = "command_execution"
	itemFileChange       = "file_change"
)

// parseLine turns one JSONL line into zero or more events.
//
// Unparseable lines produce nothing rather than an error: the raw stream is logged verbatim, so a
// parsing gap is diagnosable after the fact and never costs the run.
func parseLine(line string) []provider.Event {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return nil
	}
	var se streamEvent
	if err := json.Unmarshal([]byte(line), &se); err != nil {
		return nil
	}

	at := time.Now()
	ev := provider.Event{At: at, Raw: line}

	switch se.Type {
	case typeThreadStarted:
		ev.Kind = provider.EventStarted
		ev.Text = "thread " + se.ThreadID
		ev.Fields = map[string]any{"thread_id": se.ThreadID}
		return []provider.Event{ev}

	case typeTurnStarted:
		return nil // a turn boundary with nothing in it to show

	case typeTurnCompleted:
		ev.Kind = provider.EventUsage
		if se.Usage != nil {
			ev.Fields = map[string]any{
				"input_tokens":  se.Usage.InputTokens,
				"cached_tokens": se.Usage.CachedInputTokens,
				"output_tokens": se.Usage.OutputTokens,
			}
		}
		return []provider.Event{ev}

	case typeError, typeTurnFailed:
		ev.Kind = provider.EventError
		ev.Text = se.errorText()
		return []provider.Event{ev}

	case typeItemStarted, typeItemCompleted:
		return itemEvents(se, ev)
	}
	return nil
}

func itemEvents(se streamEvent, ev provider.Event) []provider.Event {
	if se.Item == nil {
		return nil
	}
	completed := se.Type == typeItemCompleted

	switch se.Item.Type {
	case itemAgentMessage:
		// Only the completed message carries text worth showing.
		if !completed || se.Item.Text == "" {
			return nil
		}
		ev.Kind = provider.EventMessage
		ev.Text = se.Item.Text

	case itemReasoning:
		if !completed || se.Item.Text == "" {
			return nil
		}
		ev.Kind = provider.EventThinking
		ev.Text = se.Item.Text

	case itemCommandExecution:
		ev.Tool = "Bash"
		if completed {
			ev.Kind = provider.EventToolResult
			ev.Text = se.Item.Command
			ev.Fields = map[string]any{"output": se.Item.AggregatedOutput}
			if se.Item.ExitCode != nil {
				ev.Fields["exit_code"] = *se.Item.ExitCode
			}
		} else {
			ev.Kind = provider.EventToolUse
			ev.Text = se.Item.Command
		}

	case itemFileChange:
		ev.Tool = "Edit"
		paths := make([]string, 0, len(se.Item.Changes))
		for _, c := range se.Item.Changes {
			paths = append(paths, c.Kind+" "+c.Path)
		}
		ev.Text = strings.Join(paths, ", ")
		if completed {
			ev.Kind = provider.EventToolResult
		} else {
			ev.Kind = provider.EventToolUse
		}

	default:
		return nil
	}
	return []provider.Event{ev}
}

// errorText pulls the human-readable part out of an error event.
//
// codex nests the upstream error as a JSON string inside its own message, so the raw value is a
// wall of escapes. The inner message is what a human needs; the whole thing is kept for the
// classifier, which matches on status codes the inner text does not carry.
func (se streamEvent) errorText() string {
	raw := se.Message
	if raw == "" && se.Error != nil {
		raw = se.Error.Message
	}
	var inner struct {
		Status int `json:"status"`
		Error  struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &inner) == nil && inner.Error.Message != "" {
		return inner.Error.Message
	}
	return raw
}

// threadIDFrom returns the thread id if the line announces one.
//
// Unlike claude-code, codex assigns the session id itself, so it has to be scraped rather than
// supplied. That is a real difference in what the two CLIs allow, not a shortcut.
func threadIDFrom(line string) string {
	var se streamEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &se); err != nil {
		return ""
	}
	if se.Type == typeThreadStarted {
		return se.ThreadID
	}
	return ""
}
