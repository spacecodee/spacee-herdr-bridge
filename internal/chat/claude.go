package chat

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	// A slash command is written as a user message made only of these tags.
	commandMarkup = regexp.MustCompile(`^\s*<command-name>([^<]*)</command-name>\s*(?:<command-message>[^<]*</command-message>\s*)?(?:<command-args>([^<]*)</command-args>\s*)?$`)
	// Its output is a user message made only of this tag.
	commandStdout = regexp.MustCompile(`^\s*<local-command-stdout>([\s\S]*)</local-command-stdout>\s*$`)
)

// slashCommand rewrites a user message that is only slash-command markup: the command as the
// user typed it, or the command's output as a system message. drop is true for empty output.
func slashCommand(s string) (role, text string, drop, ok bool) {
	if m := commandMarkup.FindStringSubmatch(s); m != nil {
		return "user", strings.TrimSpace(strings.TrimSpace(m[1]) + " " + strings.TrimSpace(m[2])), false, true
	}
	if m := commandStdout.FindStringSubmatch(s); m != nil {
		text = strings.TrimSpace(m[1])
		return "system", text, text == "", true
	}
	return "", "", false, false
}

// claudeParser reads Claude Code transcripts (~/.claude/projects/<dir>/<session>.jsonl).
type claudeParser struct{}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

func (claudeParser) Line(line []byte, b *Builder) {
	var r struct {
		Type        string          `json:"type"`
		UUID        string          `json:"uuid"`
		Timestamp   string          `json:"timestamp"`
		IsMeta      bool            `json:"isMeta"`
		IsSidechain bool            `json:"isSidechain"`
		Level       string          `json:"level"`
		Content     json.RawMessage `json:"content"`
		Operation   string          `json:"operation"`
		Attachment  struct {
			Type        string `json:"type"`
			Prompt      string `json:"prompt"`
			CommandMode string `json:"commandMode"`
		} `json:"attachment"`
		ToolUseResult json.RawMessage `json:"toolUseResult"`
		Message       struct {
			ID      string          `json:"id"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil || r.IsMeta || r.IsSidechain {
		return
	}
	switch r.Type {
	case "queue-operation":
		// Messages typed while Claude works wait here until it takes them.
		text := contentText(r.Content)
		switch r.Operation {
		case "enqueue":
			m := Message{Role: "user", Time: r.Timestamp}
			if !blank(text) && !strings.HasPrefix(text, "<task-notification>") {
				m.Parts = []Part{textPart("text", text)}
			}
			b.Enqueue(text, m)
		case "dequeue", "remove":
			b.Dequeue(text)
		}
	case "attachment":
		// A queued message delivered in the middle of a turn (one taken at a turn's start is
		// written as a plain user message instead).
		if a := r.Attachment; a.Type == "queued_command" && a.CommandMode == "prompt" && !blank(a.Prompt) {
			b.Add(Message{ID: r.UUID, Role: "user", Time: r.Timestamp, Parts: []Part{textPart("text", a.Prompt)}})
		}
	case "system":
		if r.Level != "error" {
			return
		}
		if s := contentText(r.Content); s != "" {
			b.Add(Message{ID: r.UUID, Role: "system", Time: r.Timestamp, Parts: []Part{textPart("text", s)}})
		}
	case "user", "assistant":
		id := r.UUID
		if r.Type == "assistant" && r.Message.ID != "" {
			id = r.Message.ID
		}
		var parts []Part
		var s string
		role := r.Type
		if json.Unmarshal(r.Message.Content, &s) == nil {
			if r.Type == "user" {
				if cr, ct, drop, ok := slashCommand(s); ok {
					if drop {
						return
					}
					role, s = cr, ct
				}
			}
			if s != "" {
				parts = append(parts, textPart("text", s))
			}
		} else {
			var blocks []claudeBlock
			if json.Unmarshal(r.Message.Content, &blocks) != nil {
				return
			}
			for _, bl := range blocks {
				switch bl.Type {
				case "text":
					if !blank(bl.Text) {
						parts = append(parts, textPart("text", bl.Text))
					}
				case "thinking":
					if !blank(bl.Thinking) {
						parts = append(parts, textPart("thinking", bl.Thinking))
					}
				case "tool_use":
					parts = append(parts, newToolPart(bl.ID, bl.Name, string(bl.Input)))
				case "tool_result":
					b.Result(bl.ToolUseID, contentText(bl.Content), bl.IsError)
					var tur struct {
						Answers map[string]string `json:"answers"`
					}
					if json.Unmarshal(r.ToolUseResult, &tur) == nil {
						b.Answers(bl.ToolUseID, tur.Answers)
					}
				case "image":
					parts = append(parts, imagePart())
				}
			}
		}
		b.Add(Message{ID: id, Role: role, Time: r.Timestamp, Parts: parts})
	}
}
