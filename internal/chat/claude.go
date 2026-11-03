package chat

import "encoding/json"

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
		Message     struct {
			ID      string          `json:"id"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil || r.IsMeta || r.IsSidechain {
		return
	}
	switch r.Type {
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
