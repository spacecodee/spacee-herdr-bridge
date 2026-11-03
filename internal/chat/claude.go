package chat

import (
	"encoding/json"
	"strings"
)

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
		Message struct {
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
		if json.Unmarshal(r.Message.Content, &s) == nil {
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
					parts = append(parts, toolPart(bl.ID, bl.Name, string(bl.Input)))
				case "tool_result":
					b.Result(bl.ToolUseID, contentText(bl.Content), bl.IsError)
				case "image":
					parts = append(parts, imagePart())
				}
			}
		}
		b.Add(Message{ID: id, Role: r.Type, Time: r.Timestamp, Parts: parts})
	}
}
