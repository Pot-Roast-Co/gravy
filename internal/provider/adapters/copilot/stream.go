package copilot

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

type streamState struct {
	session                    string
	result                     *int
	failures                   []string
	turns, tokensIn, tokensOut int
	calls                      map[string]provider.PermissionDenial
	denials                    []provider.PermissionDenial
}

func (s *streamState) parse(line string) *provider.Event {
	var wire struct {
		Type      string         `json:"type"`
		Timestamp time.Time      `json:"timestamp"`
		SessionID string         `json:"sessionId"`
		ExitCode  *int           `json:"exitCode"`
		Data      map[string]any `json:"data"`
	}
	if json.Unmarshal([]byte(line), &wire) != nil {
		if strings.TrimSpace(line) == "" {
			return nil
		}
		return &provider.Event{Kind: provider.EventMessage, Text: line, Raw: line}
	}
	data := wire.Data
	event := &provider.Event{At: wire.Timestamp, Raw: line, Fields: data}
	switch wire.Type {
	case "session.start", "session.resume":
		if id := stringField(data, "sessionId"); id != "" {
			s.session = id
		}
		event.Kind = provider.EventStarted
	case "assistant.message":
		event.Kind, event.Text = provider.EventMessage, stringField(data, "content")
	case "assistant.reasoning":
		event.Kind, event.Text = provider.EventThinking, stringField(data, "content")
	case "assistant.turn_end":
		s.turns++
		return nil
	case "assistant.usage":
		s.tokensIn += intField(data, "inputTokens")
		s.tokensOut += intField(data, "outputTokens")
		event.Kind = provider.EventUsage
	case "tool.execution_start":
		event.Kind, event.Tool = provider.EventToolUse, stringField(data, "toolName")
		event.Text = event.Tool
		arguments, _ := data["arguments"].(map[string]any)
		s.calls[stringField(data, "toolCallId")] = provider.PermissionDenial{Tool: event.Tool, Input: arguments}
	case "tool.execution_complete":
		event.Kind = provider.EventToolResult
		call := s.calls[stringField(data, "toolCallId")]
		event.Tool = call.Tool
		result, _ := data["result"].(map[string]any)
		event.Text = stringField(result, "content")
		failure, _ := data["error"].(map[string]any)
		message := stringField(failure, "message")
		if message != "" {
			event.Text = message
		}
		// Tool errors may be corrected by the agent. Permission refusals need a human,
		// even if the CLI exits successfully after explaining why it could not act.
		denied := strings.ToLower(stringField(failure, "code") + " " + message)
		success, reported := data["success"].(bool)
		if reported && !success && (strings.Contains(denied, "permission_denied") || strings.Contains(denied, "permission denied") || strings.Contains(denied, "permission was denied") || strings.Contains(denied, "user denied")) {
			if call.Tool == "" {
				call.Tool = "copilot tool"
			}
			s.denials = append(s.denials, call)
		}
	case "session.error":
		raw, _ := json.Marshal(data)
		s.failures = append(s.failures, string(raw))
		event.Kind, event.Text = provider.EventError, stringField(data, "message")
	case "result":
		s.result = wire.ExitCode
		if wire.SessionID != "" {
			s.session = wire.SessionID
		}
		event.Kind = provider.EventFinished
	default:
		return nil // raw output is still preserved in the run log
	}
	return event
}

func stringField(m map[string]any, name string) string { s, _ := m[name].(string); return s }
func intField(m map[string]any, name string) int       { n, _ := m[name].(float64); return int(n) }

func (s *streamState) outcome(p *Provider, status host.ExitStatus, stderr string) provider.Outcome {
	out := provider.Outcome{ExitCode: status.Code, TimedOut: status.TimedOut, Session: provider.SessionRef{ProviderID: ID, ID: s.session}, Turns: s.turns, TokensIn: s.tokensIn, TokensOut: s.tokensOut, Denials: s.denials}
	if status.TimedOut {
		out.Class, out.Note = provider.Timeout, "Copilot exceeded its wall-clock limit"
		return out
	}
	code := status.Code
	if code == 0 && s.result != nil {
		code = *s.result
	}
	if len(s.denials) > 0 {
		out.Class, out.Note = provider.TaskFailure, "Copilot was denied permission: "+s.denials[0].Summary()
		return out
	}
	if code == 0 && s.result == nil && len(s.failures) > 0 {
		code = 1
	}
	if code == 0 && s.result == nil {
		out.Class, out.Note = provider.TaskFailure, "Copilot exited without a final JSON result; check the CLI version and raw log"
		return out
	}
	// Only error events are classified: a successful assistant explaining a rate-limit
	// bug is not evidence that the user's account is out of quota.
	c := p.Classify(code, strings.Join(s.failures, "\n"), stderr)
	out.Class, out.Note = c.Class.Effective(), c.Note()
	if out.Class == provider.TaskFailure && len(s.failures) > 0 {
		out.Note += ": " + strings.Join(s.failures, "; ")
	}
	if out.Class == provider.TaskFailure && code != 0 && len(s.failures) == 0 {
		out.Note += fmt.Sprintf(" (CLI result exit %d)", code)
	}
	return out
}

// Classify uses explicit service errors; unfamiliar failures remain task failures.
func (p *Provider) Classify(exit int, stdout, stderr string) provider.Classification {
	if exit == 0 {
		return provider.Classification{Class: provider.Success, Rule: "clean exit"}
	}
	for _, line := range strings.Split(stdout, "\n") {
		var data struct {
			ErrorType string `json:"errorType"`
			Status    int    `json:"statusCode"`
			Message   string `json:"message"`
		}
		if json.Unmarshal([]byte(line), &data) != nil {
			continue
		}
		class := provider.TaskFailure
		switch {
		case data.Status == 401 || data.ErrorType == "authentication":
			class = provider.AuthExpired
		case data.ErrorType == "quota":
			class = provider.QuotaExhausted
		case data.Status == 429 || data.ErrorType == "rate_limit":
			class = provider.RateLimited
		case data.Status >= 500 && data.Status <= 599:
			class = provider.ProviderUnavailable
		}
		if class != provider.TaskFailure {
			return provider.Classification{Class: class, Rule: "Copilot service error", Evidence: data.Message}
		}
	}
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "no authentication information found") || strings.Contains(lower, "authentication failed") {
		return provider.Classification{Class: provider.AuthExpired, Rule: "Copilot authentication error", Evidence: "run `copilot login`"}
	}
	if exit == 127 {
		return provider.Classification{Class: provider.ProviderUnavailable, Rule: "CLI missing"}
	}
	return provider.NewMatcher().Classify(exit, stdout, stderr)
}
