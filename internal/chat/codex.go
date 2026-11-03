package chat

import (
	"encoding/json"
	"regexp"
	"strings"
)

// codexParser reads Codex rollouts (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl).
type codexParser struct{}

// codexInjected are the user-role messages Codex writes for itself, not the human.
var codexInjected = []string{"<environment_context", "<user_instructions", "<turn_aborted", "# AGENTS.md instructions"}

var exitCode = regexp.MustCompile(`^Exit code: (\d+)`)

func (codexParser) Line(line []byte, b *Builder) {
	var r struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Type      string                  `json:"type"`
			ID        string                  `json:"id"`
			Role      string                  `json:"role"`
			Content   json.RawMessage         `json:"content"`
			Summary   []struct{ Text string } `json:"summary"`
			CallID    string                  `json:"call_id"`
			Name      string                  `json:"name"`
			Arguments string                  `json:"arguments"`
			Input     string                  `json:"input"`
			Output    json.RawMessage         `json:"output"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "response_item" {
		return
	}
	p := r.Payload
	id := p.ID
	if id == "" {
		id = "call:" + p.CallID
	}
	msg := func(role string, parts ...Part) {
		b.Add(Message{ID: id, Role: role, Time: r.Timestamp, Parts: parts})
	}
	switch p.Type {
	case "message":
		if p.Role != "user" && p.Role != "assistant" {
			return
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(p.Content, &blocks) != nil {
			return
		}
		var parts []Part
		for _, bl := range blocks {
			switch bl.Type {
			case "input_text", "output_text":
				if !blank(bl.Text) && !injected(p.Role, bl.Text) {
					parts = append(parts, textPart("text", bl.Text))
				}
			case "input_image":
				parts = append(parts, imagePart())
			}
		}
		msg(p.Role, parts...)
	case "reasoning":
		var texts []string
		for _, s := range p.Summary {
			if !blank(s.Text) {
				texts = append(texts, s.Text)
			}
		}
		if len(texts) > 0 {
			msg("assistant", textPart("thinking", strings.Join(texts, "\n\n")))
		}
	case "function_call":
		msg("assistant", toolPart(p.CallID, p.Name, p.Arguments))
	case "custom_tool_call":
		msg("assistant", toolPart(p.CallID, p.Name, p.Input))
	case "function_call_output", "custom_tool_call_output":
		out, failed := codexOutput(p.Output)
		b.Result(p.CallID, out, failed)
	}
}

// codexOutput reads a tool output: plain text, an array of blocks, or the JSON envelope
// {"output":..., "metadata":{"exit_code":N}}. A non-zero exit code is an error.
func codexOutput(raw json.RawMessage) (string, bool) {
	out := contentText(raw)
	failed := false
	var env struct {
		Output   *string `json:"output"`
		Metadata struct {
			ExitCode int `json:"exit_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(out), &env) == nil && env.Output != nil {
		out = *env.Output
		failed = env.Metadata.ExitCode != 0
	}
	if m := exitCode.FindStringSubmatch(out); m != nil && m[1] != "0" {
		failed = true
	}
	return out, failed
}

func injected(role, text string) bool {
	if role != "user" {
		return false
	}
	t := strings.TrimSpace(text)
	for _, p := range codexInjected {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }
