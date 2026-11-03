package chat

import "encoding/json"

// piParser reads pi transcripts (~/.pi/agent/sessions/--<cwd>--/*.jsonl).
type piParser struct{}

func (piParser) Line(line []byte, b *Builder) {
	var r struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Role         string          `json:"role"`
			Content      json.RawMessage `json:"content"`
			ToolCallID   string          `json:"toolCallId"`
			IsError      bool            `json:"isError"`
			ErrorMessage string          `json:"errorMessage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "message" {
		return
	}
	m := r.Message
	switch m.Role {
	case "toolResult":
		b.Result(m.ToolCallID, contentText(m.Content), m.IsError)
	case "user", "assistant":
		var parts []Part
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			if s != "" {
				parts = append(parts, textPart("text", s))
			}
		} else {
			var blocks []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				Thinking  string          `json:"thinking"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(m.Content, &blocks) != nil {
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
				case "toolCall":
					parts = append(parts, toolPart(bl.ID, bl.Name, string(bl.Arguments)))
				case "image":
					parts = append(parts, imagePart())
				}
			}
		}
		if len(parts) == 0 && m.Role == "assistant" && m.ErrorMessage != "" {
			b.Add(Message{ID: r.ID, Role: "system", Time: r.Timestamp, Parts: []Part{textPart("text", m.ErrorMessage)}})
			return
		}
		b.Add(Message{ID: r.ID, Role: m.Role, Time: r.Timestamp, Parts: parts})
	}
}
