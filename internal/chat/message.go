// Package chat reads coding-agent transcripts on the host, read-only, and normalizes
// them into chat messages. Input to the agents still goes through the terminal.
package chat

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	maxText = 64 << 10 // per text part
	maxIO   = 8 << 10  // per tool input/output
	maxSumm = 80       // tool summary, in bytes
)

// Part is one piece of a message: text, thinking, a tool call (with its result) or an image.
type Part struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Input     string `json:"input,omitempty"`
	Output    string `json:"output,omitempty"`
	Status    string `json:"status,omitempty"` // tool: running, ok, error
	Path      string `json:"path,omitempty"`   // image
	Note      string `json:"note,omitempty"`   // image
	Truncated bool   `json:"truncated,omitempty"`
}

// Message is a normalized chat message. Its ID is stable across updates.
type Message struct {
	ID    string `json:"id"`
	Role  string `json:"role"` // user, assistant, system
	Time  string `json:"time,omitempty"`
	Parts []Part `json:"parts"`
}

// Parser turns one transcript line into builder calls. Unknown or malformed lines are ignored.
type Parser interface {
	Line(line []byte, b *Builder)
}

// clip cuts s to at most max bytes on a rune boundary.
func clip(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func textPart(kind, s string) Part {
	s, cut := clip(s, maxText)
	return Part{Type: kind, Text: s, Truncated: cut}
}

func imagePart() Part { return Part{Type: "image", Note: "inline image"} }

// toolPart builds a running tool part. input is the call arguments: a JSON object, or any
// other text (e.g. a patch) that is kept as is.
func toolPart(id, name string, input string) Part {
	input = strings.TrimSpace(input)
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &args) == nil {
		var buf bytes.Buffer
		if json.Compact(&buf, []byte(input)) == nil {
			input = buf.String()
		}
	} else {
		args = nil
	}
	p := Part{Type: "tool", ID: id, Name: name, Status: "running", Summary: summarize(name, args, input)}
	var cut bool
	p.Input, cut = clip(input, maxIO)
	p.Truncated = cut
	return p
}

// summaryKeys picks, per tool name, which argument is the one-line summary.
var summaryKeys = map[string][]string{
	"bash": {"command"}, "shell": {"command"}, "exec_command": {"cmd", "command"},
	"read": {"file_path", "path"}, "write": {"file_path", "path"}, "edit": {"file_path", "path"},
	"notebookedit": {"notebook_path", "file_path"}, "multiedit": {"file_path"},
	"grep": {"pattern"}, "glob": {"pattern"}, "find": {"pattern", "path"}, "ls": {"path"},
	"webfetch": {"url"}, "websearch": {"query"},
	"task": {"description"}, "agent": {"description"},
}

func summarize(name string, args map[string]json.RawMessage, rawInput string) string {
	for _, k := range summaryKeys[strings.ToLower(name)] {
		if s := argString(args[k]); s != "" {
			return oneLine(s)
		}
	}
	return oneLine(rawInput)
}

// argString renders a JSON string, or an array of strings joined by spaces (argv).
func argString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, " ")
	}
	return ""
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	// Fields collapses runs of spaces; the cut below keeps whole runes.
	s, _ = clip(s, maxSumm)
	return s
}

// contentText joins the text of a tool result that is a string or an array of blocks.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "text" || b.Type == "input_text" || b.Type == "output_text" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "\n")
}

type toolRef struct {
	msg *Message
	idx int
}

// Builder accumulates the last messages of a transcript and tracks what changed since the
// last Drain. It is not safe for concurrent use.
type Builder struct {
	max   int
	msgs  []*Message
	byID  map[string]*Message
	tools map[string]toolRef
	fresh map[string]bool // added since the last Drain
	dirty map[string]bool // changed since the last Drain
}

func NewBuilder(max int) *Builder {
	b := &Builder{max: max}
	b.Reset()
	return b
}

// Reset forgets everything.
func (b *Builder) Reset() {
	b.msgs = nil
	b.byID = map[string]*Message{}
	b.tools = map[string]toolRef{}
	b.fresh = map[string]bool{}
	b.dirty = map[string]bool{}
}

// Add appends a message, or appends the parts of a message with the same ID (an assistant
// turn written one block per line).
func (b *Builder) Add(m Message) {
	if len(m.Parts) == 0 || m.ID == "" {
		return
	}
	cur := b.byID[m.ID]
	if cur == nil {
		cur = &Message{ID: m.ID, Role: m.Role, Time: m.Time}
		b.msgs = append(b.msgs, cur)
		b.byID[m.ID] = cur
		b.fresh[m.ID] = true
	}
	for _, p := range m.Parts {
		cur.Parts = append(cur.Parts, p)
		if p.Type == "tool" && p.ID != "" {
			b.tools[p.ID] = toolRef{cur, len(cur.Parts) - 1}
		}
	}
	b.dirty[m.ID] = true
	b.trim()
}

// Replace sets the parts of a message that is rewritten in place (a running tool
// completing), or adds it when it is new. A client sees it as an update.
func (b *Builder) Replace(m Message) {
	if len(m.Parts) == 0 || m.ID == "" {
		return
	}
	if cur := b.byID[m.ID]; cur != nil {
		for _, p := range cur.Parts {
			delete(b.tools, p.ID)
		}
		cur.Role, cur.Time, cur.Parts = m.Role, m.Time, nil
	}
	b.Add(m)
}

func (b *Builder) trim() {
	for len(b.msgs) > b.max {
		old := b.msgs[0]
		b.msgs = b.msgs[1:]
		delete(b.byID, old.ID)
		delete(b.fresh, old.ID)
		delete(b.dirty, old.ID)
		for _, p := range old.Parts {
			delete(b.tools, p.ID)
		}
	}
}

// Result attaches a tool result to its call. Unknown call ids are ignored.
func (b *Builder) Result(callID, output string, isErr bool) {
	ref, ok := b.tools[callID]
	if !ok {
		return
	}
	p := &ref.msg.Parts[ref.idx]
	var cut bool
	p.Output, cut = clip(output, maxIO)
	p.Truncated = p.Truncated || cut
	p.Status = "ok"
	if isErr {
		p.Status = "error"
	}
	b.dirty[ref.msg.ID] = true
}

// Snapshot returns copies of the last n messages.
func (b *Builder) Snapshot(n int) []Message {
	from := 0
	if len(b.msgs) > n {
		from = len(b.msgs) - n
	}
	out := make([]Message, 0, len(b.msgs)-from)
	for _, m := range b.msgs[from:] {
		out = append(out, copyMsg(m))
	}
	return out
}

// Drain returns the messages added and the ones changed (already known to the client)
// since the previous call, in transcript order.
func (b *Builder) Drain() (added, updated []Message) {
	if len(b.dirty) == 0 {
		return nil, nil
	}
	for _, m := range b.msgs {
		switch {
		case b.fresh[m.ID]:
			added = append(added, copyMsg(m))
		case b.dirty[m.ID]:
			updated = append(updated, copyMsg(m))
		}
	}
	b.fresh = map[string]bool{}
	b.dirty = map[string]bool{}
	return added, updated
}

func copyMsg(m *Message) Message {
	c := *m
	c.Parts = append([]Part(nil), m.Parts...)
	return c
}
