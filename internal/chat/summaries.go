package chat

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

const (
	summaryTail  = 256 << 10 // transcript bytes read from the end
	summaryHead  = 64 << 10  // transcript bytes read from the start when the tail has no title
	maxTitle     = 80
	maxLast      = 140
	summaryProcs = 4 // terminals resolved in parallel
	maxCached    = 256
)

// LastMessage is the preview of a conversation.
type LastMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
	Time string `json:"time,omitempty"`
}

// Summary is one conversation row of the home screen. Messages counts the messages in the
// part of the transcript that was read (its last 256 KiB).
type Summary struct {
	Terminal string       `json:"terminal"`
	Agent    string       `json:"agent"`
	Session  string       `json:"session"`
	Title    string       `json:"title"`
	Last     *LastMessage `json:"last,omitempty"`
	Messages int          `json:"messages"`
	Asking   *Asking      `json:"asking,omitempty"`
}

// Asking says the agent waits for an answer: how many questions and the first one.
// Never options, answers or secrets.
type Asking struct {
	Count    int    `json:"count"`
	Question string `json:"question"`
}

type cached struct {
	size int64
	mod  time.Time
	sum  Summary // Terminal is filled per call
	rev  string  // opencode: the staged revert the summary was built with
}

// Summarizer builds summaries from the tail of transcripts and remembers them per file
// until its size or mtime changes.
type Summarizer struct {
	mu    sync.Mutex
	cache map[string]cached
	reads int
}

func NewSummarizer() *Summarizer { return &Summarizer{cache: map[string]cached{}} }

// Reads is how many transcript reads (tail or head) happened so far; tests use it to see cache hits.
func (s *Summarizer) Reads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *Summarizer) read() { s.mu.Lock(); s.reads++; s.mu.Unlock() }

// SummariesHandler serves GET /api/chat/summaries: one entry per live local terminal that
// runs an agent with a readable transcript. It needs the session and a same-origin request.
func SummariesHandler(a *auth.Auth, c Config, sm *Summarizer) http.Handler {
	if c.List == nil {
		c.List = func() ([]string, error) { return listLive(c.Socket) }
	}
	if c.Lookup == nil {
		c.Lookup = func(address string) (Terminal, error) { return lookupTerminal(c.Socket, address) }
	}
	return a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !auth.SameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		addrs, err := c.List()
		if err != nil {
			http.Error(w, "core unavailable", http.StatusBadGateway)
			return
		}
		resolve := c.Resolve
		if resolve == nil {
			resolve = c.Resolver.Snapshot().Resolve // one /proc scan for all terminals
		}
		out := sm.collect(addrs, c.Lookup, resolve)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"summaries": out})
	}))
}

func (sm *Summarizer) collect(addrs []string, lookup func(string) (Terminal, error), resolve func(Terminal) (Source, string)) []Summary {
	results := make([]*Summary, len(addrs))
	sem := make(chan struct{}, summaryProcs)
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			t, err := lookup(addr)
			if err != nil {
				return
			}
			src, why := resolve(t)
			if why == Waiting {
				results[i] = &Summary{Terminal: addr, Agent: src.Agent, Session: src.Session}
				return
			}
			if why != "" || src.Path == "" {
				return
			}
			if s, ok := sm.summarize(src); ok {
				s.Terminal = addr
				results[i] = &s
			}
		}()
	}
	wg.Wait()
	out := []Summary{}
	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return lastTime(out[i]) > lastTime(out[j]) })
	return out
}

func lastTime(s Summary) string {
	if s.Last == nil {
		return ""
	}
	return s.Last.Time
}

func (sm *Summarizer) summarize(src Source) (Summary, bool) {
	if src.DB != nil {
		return sm.summarizeDB(src)
	}
	fi, err := os.Stat(src.Path)
	if err != nil || !fi.Mode().IsRegular() {
		return Summary{}, false
	}
	sm.mu.Lock()
	e, ok := sm.cache[src.Path]
	sm.mu.Unlock()
	if ok && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		s := e.sum
		s.Agent, s.Session = src.Agent, src.Session
		return s, true
	}

	sm.read()
	b := NewBuilder(1 << 20)
	t := NewTailer(src.Path, src.Parser, b)
	t.window = summaryTail
	if _, err := t.Poll(); err != nil {
		return Summary{}, false
	}
	msgs := b.Snapshot(1 << 20)
	s := Summary{Agent: src.Agent, Session: src.Session, Messages: len(msgs)}
	s.Title = firstTitle(msgs)
	if s.Title == "" && fi.Size() > summaryTail {
		sm.read()
		s.Title = firstTitle(headMessages(src, fi.Size()))
	}
	s.Last = lastMessage(msgs)
	s.Asking = pendingAsk(msgs)
	sm.store(src.Path, cached{size: fi.Size(), mod: fi.ModTime(), sum: s})
	return s, true
}

func (sm *Summarizer) store(key string, c cached) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if len(sm.cache) >= maxCached {
		sm.cache = map[string]cached{} // transcripts of finished sessions pile up; start over
	}
	sm.cache[key] = c
}

// summarizeDB is summarize for an opencode session. The cache stamp is the number of
// rows and the newest update time of the session.
func (sm *Summarizer) summarizeDB(src Source) (Summary, bool) {
	var n, upd int64
	if err := src.DB.QueryRow(`SELECT count(*), coalesce(max(time_updated), 0) FROM session_message WHERE session_id = ?`, src.Session).Scan(&n, &upd); err != nil {
		return Summary{}, false
	}
	mod := time.UnixMilli(upd)
	rev := ocRevert(src.DB, src.Session)
	sm.mu.Lock()
	e, ok := sm.cache[src.Path]
	sm.mu.Unlock()
	if ok && e.size == n && e.mod.Equal(mod) && e.rev == rev {
		return e.sum, true
	}

	sm.read()
	f := newOCFollower(src.DB, src.Session)
	f.limit = ocSummaryRows
	if _, err := f.Poll(); err != nil {
		return Summary{}, false
	}
	msgs := f.Builder().Snapshot(ocSummaryRows)
	s := Summary{Agent: src.Agent, Session: src.Session, Messages: len(msgs), Title: firstTitle(msgs), Last: lastMessage(msgs), Asking: pendingAsk(msgs)}
	if s.Title == "" && n > ocSummaryRows {
		sm.read()
		if head, err := newOCFollower(src.DB, src.Session).head(ocSummaryRows); err == nil {
			s.Title = firstTitle(head)
		}
	}
	sm.store(src.Path, cached{size: n, mod: mod, sum: s, rev: rev})
	return s, true
}

// headMessages parses the first summaryHead bytes of a transcript (complete lines only).
func headMessages(src Source, size int64) []Message {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, min(size, summaryHead))
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil
	}
	buf = buf[:n]
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[:i]
	} else {
		return nil
	}
	b := NewBuilder(1 << 20)
	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		if len(line) > 0 && len(line) <= maxLine {
			src.Parser.Line(line, b)
		}
	}
	return b.Snapshot(1 << 20)
}

// firstTitle is the first user text that is not a slash command.
func firstTitle(msgs []Message) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type != "text" {
				continue
			}
			t := squash(p.Text)
			if t == "" || strings.HasPrefix(t, "/") || strings.HasPrefix(t, "<command-") || strings.HasPrefix(t, "<local-command") {
				continue
			}
			t, _ = clip(t, maxTitle)
			return t
		}
	}
	return ""
}

// pendingAsk reports the newest question tool part when it is still waiting for an answer.
func pendingAsk(msgs []Message) *Asking {
	for i := len(msgs) - 1; i >= 0; i-- {
		for j := len(msgs[i].Parts) - 1; j >= 0; j-- {
			p := msgs[i].Parts[j]
			if len(p.Questions) == 0 {
				continue
			}
			if p.Status != "running" {
				return nil
			}
			return &Asking{Count: len(p.Questions), Question: p.Questions[0].Question}
		}
	}
	return nil
}

// lastMessage previews the newest message that has text or a tool call.
func lastMessage(msgs []Message) *LastMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if text := previewText(m); text != "" {
			text, _ = clip(text, maxLast)
			return &LastMessage{Role: m.Role, Text: text, Time: m.Time}
		}
	}
	return nil
}

func previewText(m Message) string {
	var texts []string
	for _, p := range m.Parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	if t := plain(strings.Join(texts, "\n")); t != "" {
		return t
	}
	for i := len(m.Parts) - 1; i >= 0; i-- {
		p := m.Parts[i]
		if p.Type != "tool" {
			continue
		}
		verb := p.Name
		switch strings.ToLower(p.Name) {
		case "bash", "shell", "exec_command":
			verb = "Ran"
		}
		if p.Summary == "" {
			return verb
		}
		return verb + ": " + p.Summary
	}
	return ""
}

var (
	fenceRe  = regexp.MustCompile("(?m)^\\s*(```|~~~).*$")
	imageRe  = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	linkRe   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	blockRe  = regexp.MustCompile(`(?m)^\s*(#{1,6}\s+|>\s?|[-*+]\s+|\d+[.)]\s+)`)
	inlineRe = regexp.MustCompile("(\\*\\*|__|~~|`)")
)

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// plain turns markdown into one line of plain text.
func plain(md string) string {
	s := fenceRe.ReplaceAllString(md, "")
	s = imageRe.ReplaceAllString(s, "$1")
	s = linkRe.ReplaceAllString(s, "$1")
	s = blockRe.ReplaceAllString(s, "")
	s = inlineRe.ReplaceAllString(s, "")
	return squash(s)
}
