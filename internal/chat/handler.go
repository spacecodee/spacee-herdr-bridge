package chat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

const (
	snapshotSize   = 200
	pollEvery      = 500 * time.Millisecond
	heartbeatEvery = 15 * time.Second
	resolveEvery   = 4 // polls between process-tree/session re-resolutions (2 s)
	maxStreams     = 8
	lookupTimeout  = 3 * time.Second
)

var addressRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// Config wires the chat endpoint. Zero values get production defaults; the function
// fields exist so tests can run without a core, a /proc or real timers.
type Config struct {
	Socket       string                   // herdr-core Unix socket (terminal.get, terminal.list)
	List         func() ([]string, error) // live local terminal addresses (summaries)
	Resolver     Resolver
	Lookup       func(address string) (Terminal, error)
	Resolve      func(Terminal) (Source, string)
	Tick         func(kind string) (<-chan time.Time, func()) // kind: "poll" or "heartbeat"
	ResolveEvery int
	MaxStreams   int
}

// Handler serves GET /api/chat?terminal=<address> as Server-Sent Events. It needs the
// session and a same-origin request.
func Handler(a *auth.Auth, c Config) http.Handler {
	if c.Lookup == nil {
		c.Lookup = func(address string) (Terminal, error) { return lookupTerminal(c.Socket, address) }
	}
	if c.Resolve == nil {
		c.Resolve = c.Resolver.Resolve
	}
	if c.Tick == nil {
		c.Tick = realTicks
	}
	if c.ResolveEvery == 0 {
		c.ResolveEvery = resolveEvery
	}
	if c.MaxStreams == 0 {
		c.MaxStreams = maxStreams
	}
	var active atomic.Int32
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
		address := r.URL.Query().Get("terminal")
		if !addressRe.MatchString(address) {
			http.Error(w, "bad terminal", http.StatusBadRequest)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		if int(active.Add(1)) > c.MaxStreams {
			active.Add(-1)
			http.Error(w, "too many chat streams", http.StatusTooManyRequests)
			return
		}
		defer active.Add(-1)

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		fl.Flush()

		s := &session{w: w, fl: fl, c: c, address: address}
		s.run(r)
	}))
}

func realTicks(kind string) (<-chan time.Time, func()) {
	d := pollEvery
	if kind == "heartbeat" {
		d = heartbeatEvery
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// follower keeps a Builder in sync with a transcript: a JSONL file (Tailer) or an
// opencode session (ocFollower). reset is true when the Builder was rebuilt.
type follower interface {
	Poll() (reset bool, err error)
	Builder() *Builder
}

func (src Source) follow() follower {
	if src.DB != nil {
		return newOCFollower(src.DB, src.Session)
	}
	return NewTailer(src.Path, src.Parser, NewBuilder(keep))
}

// session is one open SSE connection following one terminal.
type session struct {
	w       http.ResponseWriter
	fl      http.Flusher
	c       Config
	address string

	src      *Source
	tail     follower
	lastNone string
	err      error // first write error: the client is gone
}

func (s *session) run(r *http.Request) {
	poll, stopPoll := s.c.Tick("poll")
	defer stopPoll()
	beat, stopBeat := s.c.Tick("heartbeat")
	defer stopBeat()

	s.refresh()
	s.poll()
	n := 0
	for s.err == nil {
		select {
		case <-r.Context().Done():
			return
		case <-beat:
			s.raw(": hb\n\n")
		case <-poll:
			if n++; s.tail == nil || n%s.c.ResolveEvery == 0 {
				s.refresh()
			}
			s.poll()
		}
	}
}

// refresh re-resolves the transcript and switches to it when it changed.
func (s *session) refresh() {
	if strings.Contains(s.address, "/") { // another machine: not supported yet
		s.none(Unsupported)
		return
	}
	term, err := s.c.Lookup(s.address)
	if err != nil {
		s.drop(NotFound)
		return
	}
	src, why := s.c.Resolve(term)
	if why != "" {
		s.drop(why)
		return
	}
	s.lastNone = ""
	if s.src == nil || s.src.Path != src.Path {
		s.src = &src
		s.tail = src.follow()
	}
}

// drop forgets the current transcript and tells the client why there is none.
func (s *session) drop(why string) {
	s.src, s.tail = nil, nil
	s.none(why)
}

func (s *session) none(why string) {
	if s.lastNone == why {
		return
	}
	s.lastNone = why
	s.event("none", map[string]string{"reason": why})
}

func (s *session) poll() {
	if s.tail == nil {
		return
	}
	reset, err := s.tail.Poll()
	if err != nil {
		s.drop(NotFound)
		return
	}
	added, updated := s.tail.Builder().Drain()
	if reset {
		s.event("reset", map[string]any{"agent": s.src.Agent, "session": s.src.Session, "messages": s.tail.Builder().Snapshot(snapshotSize)})
		return
	}
	if len(added) > 0 {
		s.event("append", map[string]any{"messages": added})
	}
	if len(updated) > 0 {
		s.event("update", map[string]any{"messages": updated})
	}
}

func (s *session) event(name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.raw(fmt.Sprintf("event: %s\ndata: %s\n\n", name, data))
}

func (s *session) raw(text string) {
	if s.err != nil {
		return
	}
	if _, s.err = s.w.Write([]byte(text)); s.err == nil {
		s.fl.Flush()
	}
}

// call sends one JSON-RPC request to core and returns its result.
func call(socket, method string, params any) (json.RawMessage, error) {
	c, err := net.DialTimeout("unix", socket, lookupTimeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(lookupTimeout))
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if _, err := c.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReaderSize(c, 1<<20).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if len(resp.Result) == 0 || string(resp.Result) == "null" {
		return nil, fmt.Errorf("%s: no result", method)
	}
	return resp.Result, nil
}

// lookupTerminal asks core (terminal.get) for the foreground process and directory.
func lookupTerminal(socket, address string) (Terminal, error) {
	raw, err := call(socket, "terminal.get", map[string]string{"terminal": address})
	if err != nil {
		return Terminal{}, err
	}
	var r struct {
		PID        int    `json:"pid"`
		CWD        string `json:"cwd"`
		Foreground *struct {
			PID int    `json:"pid"`
			CWD string `json:"cwd"`
		} `json:"foreground"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Terminal{}, err
	}
	t := Terminal{PID: r.PID, CWD: r.CWD}
	if fg := r.Foreground; fg != nil && fg.PID > 0 {
		t.PID = fg.PID
		if fg.CWD != "" {
			t.CWD = fg.CWD
		}
	}
	return t, nil
}

// listLive asks core (terminal.list) for the live terminals that run on this machine.
func listLive(socket string) ([]string, error) {
	raw, err := call(socket, "terminal.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	return liveLocal(raw), nil
}

// liveLocal picks the addresses of live terminals of this machine (remote ones carry a
// "machine/" prefix) out of a terminal.list result.
func liveLocal(raw []byte) []string {
	var r struct {
		Terminals []struct {
			Address string `json:"address"`
			State   string `json:"state"`
		} `json:"terminals"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	var out []string
	for _, t := range r.Terminals {
		if t.State == "live" && addressRe.MatchString(t.Address) && !strings.Contains(t.Address, "/") {
			out = append(out, t.Address)
		}
	}
	return out
}
