package chat

import (
	"encoding/json"
	"strings"
)

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
			Details      json.RawMessage `json:"details"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "message" {
		return
	}
	m := r.Message
	switch m.Role {
	case "toolResult":
		asking := b.Asking(m.ToolCallID)
		b.Result(m.ToolCallID, contentText(m.Content), m.IsError)
		if asking {
			b.Answers(m.ToolCallID, piAnswers(m.Details))
		}
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
					parts = append(parts, newToolPart(bl.ID, bl.Name, string(bl.Arguments)))
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

// piAnswers reads the details of an ask_user_question result (answers: free text or the
// selected labels, per question) or an ask_user_choice result (the selected option).
func piAnswers(raw json.RawMessage) map[string]string {
	var d struct {
		Question  string `json:"question"`
		Selection *struct {
			Label string `json:"label"`
		} `json:"selection"`
		Answers []struct {
			Question string   `json:"question"`
			Answer   *string  `json:"answer"`
			Selected []string `json:"selected"`
		} `json:"answers"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return nil
	}
	out := map[string]string{}
	if d.Selection != nil && d.Question != "" {
		out[d.Question] = d.Selection.Label
	}
	for _, a := range d.Answers {
		switch {
		case a.Answer != nil && *a.Answer != "":
			out[a.Question] = *a.Answer
		case len(a.Selected) > 0:
			out[a.Question] = strings.Join(a.Selected, ", ")
		}
	}
	return out
}
