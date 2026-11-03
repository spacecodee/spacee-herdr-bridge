// Package chat reads coding-agent transcripts on the host, read-only, and normalizes
// them into chat messages. Input to the agents still goes through the terminal.
package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxText = 64 << 10 // per text part
	maxIO   = 8 << 10  // per tool input/output
	maxSumm = 80       // tool summary, in bytes

	maxQuestions = 4   // questions per ask
	maxOptions   = 6   // options per question
	maxQText     = 512 // each question, header, label, description and answer
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
	// Questions is an agent asking the user to choose (Claude Code's AskUserQuestion); the
	// tool is running while it waits. Answers maps question text to the chosen answer.
	Questions []Question        `json:"questions,omitempty"`
	Answers   map[string]string `json:"answers,omitempty"`
}

// Question is one multiple-choice question an agent asks.
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
	Other       bool     `json:"other,omitempty"`  // a free-text answer is allowed
	Secret      bool     `json:"secret,omitempty"` // the answer is never sent to the client
	Options     []Option `json:"options"`          // empty for a free-text question
	id          string   // the agent's own question id (Codex), to match answers
}

// Option is one choice of a Question.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Message is a normalized chat message. Its ID is stable across updates.
type Message struct {
	ID    string `json:"id"`
	Role  string `json:"role"` // user, assistant, system
	Time  string `json:"time,omitempty"`
	Parts []Part `json:"parts"`
	// Queued is a user message typed while the agent was busy and not delivered yet.
	Queued bool `json:"queued,omitempty"`
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

// questionTools are the agents' structured multiple-choice tools: Claude Code
// AskUserQuestion, pi ask_user_question / ask_user_choice (extensions), Codex
// request_user_input (Plan mode).
var questionTools = map[string]bool{
	"AskUserQuestion": true, "ask_user_question": true, "ask_user_choice": true,
	"request_user_input": true, "question": true,
}

// newToolPart builds a running tool part; a question tool with usable input also gets
// Questions. Input that is not a usable question list yields the generic tool part.
func newToolPart(id, name, input string) Part {
	p := toolPart(id, name, input)
	if !questionTools[name] {
		return p
	}
	type opt struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	}
	type ask struct {
		ID          string `json:"id"`
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Multiple    bool   `json:"multiple"` // opencode
		IsOther     bool   `json:"isOther"`  // Codex
		IsSecret    bool   `json:"isSecret"` // Codex
		Options     []opt  `json:"options"`
	}
	var in struct {
		ask
		Questions []ask `json:"questions"`
	}
	if json.Unmarshal([]byte(input), &in) != nil {
		return p
	}
	qs := in.Questions
	if len(qs) == 0 { // ask_user_choice carries a single question at the top level
		qs = []ask{in.ask}
	}
	for _, q := range qs {
		if len(p.Questions) == maxQuestions {
			p.Truncated = true
			break
		}
		if strings.TrimSpace(q.Question) == "" {
			continue
		}
		out := Question{id: q.ID, MultiSelect: q.MultiSelect || q.Multiple, Secret: q.IsSecret, Options: []Option{}}
		switch name {
		case "ask_user_choice": // options only
		case "request_user_input":
			out.Other = q.IsOther || len(q.Options) == 0
		default: // Claude, pi ask_user_question and opencode always take a typed answer
			out.Other = true
		}
		out.Question = p.clipQ(q.Question)
		out.Header = p.clipQ(q.Header)
		for _, o := range q.Options {
			if len(out.Options) == maxOptions {
				p.Truncated = true
				break
			}
			out.Options = append(out.Options, Option{Label: p.clipQ(o.Label), Description: p.clipQ(o.Description)})
		}
		p.Questions = append(p.Questions, out)
	}
	return p
}

// clipQ clips s to maxQText and marks the part truncated when it cut something.
func (p *Part) clipQ(s string) string {
	s, cut := clip(s, maxQText)
	p.Truncated = p.Truncated || cut
	return s
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
	gone  []string        // removed since the last Gone, after the client saw them
	queue []queued        // the agent's input queue, oldest first
	qseq  int

	// Branches (see branch.go).
	cur    string              // record being parsed
	nodes  map[string]string   // tree records seen -> parent
	live   map[string]bool     // records on the live branch
	leaf   string              // newest leaf record: the end of the live branch
	owners map[string][]string // message id -> records that built it
	turns  [][]string          // message ids per user turn
}

// queued is one entry of the agent's input queue. Hidden entries (not typed by the user)
// keep their place so a dequeue still pops the right one, but have no message.
type queued struct {
	id, text string
	shown    bool
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
	b.gone = nil
	b.queue = nil
	b.qseq = 0
	b.cur, b.leaf, b.turns = "", "", nil
	b.nodes = map[string]string{}
	b.live = map[string]bool{}
	b.owners = map[string][]string{}
}

// Enqueue adds an entry to the agent's input queue; text is its raw content, used to match
// a later Dequeue. With parts it is shown as a queued message (its ID is assigned here);
// without parts it only holds its place in the queue.
func (b *Builder) Enqueue(text string, m Message) {
	b.qseq++
	q := queued{id: fmt.Sprintf("queued-%d", b.qseq), text: text, shown: len(m.Parts) > 0}
	b.queue = append(b.queue, q)
	if q.shown {
		m.ID, m.Queued = q.id, true
		b.Add(m)
	}
}

// Dequeue takes the entry whose text is text out of the queue, or the oldest one when text
// is empty or matches none, and removes its queued message.
func (b *Builder) Dequeue(text string) {
	if len(b.queue) == 0 {
		return
	}
	at := 0
	if text != "" {
		for i, q := range b.queue {
			if q.text == text {
				at = i
				break
			}
		}
	}
	q := b.queue[at]
	b.queue = append(b.queue[:at], b.queue[at+1:]...)
	if q.shown {
		b.remove(q.id)
	}
}

// Gone returns the IDs of messages removed since the previous call that the client had
// already been sent. A message added and removed between two drains is never reported.
func (b *Builder) Gone() []string {
	g := b.gone
	b.gone = nil
	return g
}

func (b *Builder) remove(id string) {
	cur := b.byID[id]
	if cur == nil {
		return
	}
	for i, m := range b.msgs {
		if m == cur {
			b.msgs = append(b.msgs[:i], b.msgs[i+1:]...)
			break
		}
	}
	for _, p := range cur.Parts {
		delete(b.tools, p.ID)
	}
	if !b.fresh[id] {
		b.gone = append(b.gone, id)
	}
	delete(b.byID, id)
	delete(b.fresh, id)
	delete(b.dirty, id)
	delete(b.owners, id)
}

// Add appends a message, or appends the parts of a message with the same ID (an assistant
// turn written one block per line).
func (b *Builder) Add(m Message) {
	if len(m.Parts) == 0 || m.ID == "" || !b.visible(b.cur) {
		return
	}
	cur := b.byID[m.ID]
	if cur == nil {
		cur = &Message{ID: m.ID, Role: m.Role, Time: m.Time, Queued: m.Queued}
		b.msgs = append(b.msgs, cur)
		b.byID[m.ID] = cur
		b.fresh[m.ID] = true
		if n := len(b.turns); n > 0 {
			b.turns[n-1] = append(b.turns[n-1], m.ID)
		}
	}
	b.own(m.ID)
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
		delete(b.owners, old.ID)
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
	for _, q := range p.Questions {
		if q.Secret { // the raw output holds the secret answer
			p.Output = ""
		}
	}
	p.Status = "ok"
	if isErr {
		p.Status = "error"
	}
	b.dirty[ref.msg.ID] = true
}

// Asking reports whether callID is a pending question call.
func (b *Builder) Asking(callID string) bool {
	ref, ok := b.tools[callID]
	return ok && len(ref.msg.Parts[ref.idx].Questions) > 0
}

// Answers records the answers of a question call, keyed by question text (or by the
// agent's question id). Unknown call ids and calls without questions are ignored.
func (b *Builder) Answers(callID string, answers map[string]string) {
	ref, ok := b.tools[callID]
	if !ok {
		return
	}
	if ref.msg.Parts[ref.idx].setAnswers(answers) {
		b.dirty[ref.msg.ID] = true
	}
}

// setAnswers stores the answers of a question part (at most maxQuestions, clipped). The
// answer to a secret question is masked. It reports whether anything was stored.
func (p *Part) setAnswers(answers map[string]string) bool {
	if len(p.Questions) == 0 || len(answers) == 0 {
		return false
	}
	keys := make([]string, 0, len(answers))
	for k := range answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	p.Answers = map[string]string{}
	for _, k := range keys {
		if len(p.Answers) == maxQuestions {
			p.Truncated = true
			break
		}
		name, answer := k, answers[k]
		for _, q := range p.Questions {
			if q.id != "" && q.id == k {
				name = q.Question
			}
			if (q.id == k || q.Question == k) && q.Secret {
				answer = "••••"
			}
		}
		p.Answers[p.clipQ(name)] = p.clipQ(answer)
	}
	return true
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
	for i, p := range c.Parts {
		if p.Answers != nil { // later results mutate the map in place; give the copy its own
			a := make(map[string]string, len(p.Answers))
			for k, v := range p.Answers {
				a[k] = v
			}
			c.Parts[i].Answers = a
		}
	}
	return c
}
