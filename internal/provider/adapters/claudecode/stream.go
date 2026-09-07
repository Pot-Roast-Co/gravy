package claudecode

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/provider"
)

// streamEvent is one line of `--output-format stream-json` output.
//
// Only the fields Gravy uses are declared; the CLI emits a good deal more, and unknown fields
// are ignored rather than being an error, so a new field in the CLI does not break a run.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	SessionID string `json:"session_id"`
	Model     string `json:"model"`

	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`

	// Result fields, present on the terminal "result" event.
	//
	// IsError is authoritative and Subtype is not: a hard 404 and a 401 were both observed
	// arriving as subtype "success" with is_error true (docs/SPIKE-claude-code.md, F1).
	IsError        bool     `json:"is_error"`
	Result         string   `json:"result"`
	APIErrorStatus *int     `json:"api_error_status"`
	TerminalReason string   `json:"terminal_reason"`
	NumTurns       int      `json:"num_turns"`
	TotalCostUSD   *float64 `json:"total_cost_usd"`
	Usage          struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`

	// PermissionDenials lists tool calls that were refused. A run can report exit 0 and
	// is_error false with every one of its tool calls denied, so this is the only way to tell
	// "did the work" from "was not allowed to".
	PermissionDenials []struct {
		ToolName  string         `json:"tool_name"`
		ToolUseID string         `json:"tool_use_id"`
		ToolInput map[string]any `json:"tool_input"`
	} `json:"permission_denials"`

	EstimatedTokens int `json:"estimated_tokens"`

	RateLimitInfo json.RawMessage `json:"rate_limit_info"`
}

// contentBlock is one item in an assistant or user message's content array.
type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// parseLine turns one NDJSON line into zero or more Gravy events.
//
// A line that cannot be parsed yields no events and no error: the run continues. Failing a run
// because a log line was unrecognised would turn a cosmetic CLI change into a broken pipeline.
func parseLine(line string) []provider.Event {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return nil
	}

	var se streamEvent
	if err := json.Unmarshal([]byte(line), &se); err != nil {
		return nil
	}

	now := time.Now()
	base := func(kind provider.EventKind) provider.Event {
		return provider.Event{Kind: kind, At: now, Raw: line}
	}

	switch se.Type {
	case "system":
		switch se.Subtype {
		case "init":
			e := base(provider.EventStarted)
			e.Text = "session " + se.SessionID
			e.Fields = map[string]any{"session_id": se.SessionID, "model": se.Model}
			return []provider.Event{e}
		case "thinking_tokens":
			// A liveness signal during long thinking stretches. It is what distinguishes a
			// working run from a stalled one, which process state alone cannot.
			e := base(provider.EventThinking)
			e.Fields = map[string]any{"estimated_tokens": se.EstimatedTokens}
			return []provider.Event{e}
		}
		return nil

	case "rate_limit_event":
		// Quota telemetry arrives on healthy runs, carrying per-window utilization and an
		// exact reset time. That is strictly better than inferring quota state from failures
		// after the fact, so it is captured now even though the router lands in M1.
		e := base(provider.EventRateLimit)
		if len(se.RateLimitInfo) > 0 {
			var info map[string]any
			if err := json.Unmarshal(se.RateLimitInfo, &info); err == nil {
				e.Fields = info
			}
		}
		return []provider.Event{e}

	case "assistant", "user":
		return contentEvents(se, base)

	case "result":
		e := base(provider.EventFinished)
		e.Text = se.Result
		e.Fields = map[string]any{
			"is_error":        se.IsError,
			"subtype":         se.Subtype,
			"terminal_reason": se.TerminalReason,
			"num_turns":       se.NumTurns,
		}
		if se.APIErrorStatus != nil {
			e.Fields["api_error_status"] = *se.APIErrorStatus
		}
		if se.IsError {
			e.Kind = provider.EventError
		}
		return []provider.Event{e}
	}
	return nil
}

// contentEvents extracts tool use, tool results and text from a message's content array.
func contentEvents(se streamEvent, base func(provider.EventKind) provider.Event) []provider.Event {
	if len(se.Message.Content) == 0 {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(se.Message.Content, &blocks); err != nil {
		return nil
	}

	var out []provider.Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			e := base(provider.EventMessage)
			e.Text = b.Text
			out = append(out, e)
		case "tool_use":
			e := base(provider.EventToolUse)
			e.Tool = b.Name
			e.Text = b.Name
			if len(b.Input) > 0 {
				var in map[string]any
				if err := json.Unmarshal(b.Input, &in); err == nil {
					e.Fields = in
					e.Text = b.Name + " " + summarizeToolInput(in)
				}
			}
			out = append(out, e)
		case "tool_result":
			e := base(provider.EventToolResult)
			out = append(out, e)
		}
	}
	return out
}

// summarizeToolInput renders the most useful field of a tool call for a one-line display.
func summarizeToolInput(in map[string]any) string {
	for _, key := range []string{"command", "file_path", "path", "pattern", "description"} {
		if v, ok := in[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return truncate(s, 120)
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
