package chat

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

type sseEvent struct{ name, data string }

// stream opens /api/chat and delivers parsed events (heartbeat comments as name "hb").
type stream struct {
	resp   *http.Response
	events chan sseEvent
}

func openStream(t *testing.T, url, origin string) *stream {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "tok"})
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{resp: resp, events: make(chan sseEvent, 64)}
	if resp.StatusCode != 200 {
		return s
	}
	t.Cleanup(func() { resp.Body.Close() })
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, ": "):
				s.events <- sseEvent{"hb", ""}
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				s.events <- sseEvent{name, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	return s
}

func (s *stream) next(t *testing.T) sseEvent {
	t.Helper()
	select {
	case e, ok := <-s.events:
		if !ok {
			t.Fatal("stream closed")
		}
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return sseEvent{}
}

func (s *stream) expect(t *testing.T, name string) map[string]any {
	t.Helper()
	e := s.next(t)
	if e.name != name {
		t.Fatalf("event %q (%s), want %q", e.name, e.data, name)
	}
	var m map[string]any
	if e.data != "" {
		if err := json.Unmarshal([]byte(e.data), &m); err != nil {
			t.Fatalf("bad json %q: %v", e.data, err)
		}
	}
	return m
}

type fakes struct {
	mu   sync.Mutex
	src  Source
	why  string
	poll chan time.Time
	beat chan time.Time
}

func (f *fakes) set(src Source, why string) { f.mu.Lock(); f.src, f.why = src, why; f.mu.Unlock() }
func (f *fakes) resolve(Terminal) (Source, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.src, f.why
}
func (f *fakes) tick(kind string) (<-chan time.Time, func()) {
	if kind == "heartbeat" {
		return f.beat, func() {}
	}
	return f.poll, func() {}
}
func (f *fakes) fire(t *testing.T, ch chan time.Time) {
	t.Helper()
	select {
	case ch <- time.Now():
	case <-time.After(3 * time.Second):
		t.Fatal("server did not take the tick")
	}
}

func newChatServer(t *testing.T, max int) (*httptest.Server, *fakes) {
	f := &fakes{poll: make(chan time.Time), beat: make(chan time.Time)}
	h := Handler(auth.New("tok"), Config{
		Lookup:       func(string) (Terminal, error) { return Terminal{PID: 1, CWD: "/w"}, nil },
		Resolve:      f.resolve,
		Tick:         f.tick,
		ResolveEvery: 1,
		MaxStreams:   max,
	})
	mux := http.NewServeMux()
	mux.Handle("/api/chat", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

func claudeSource(path string) Source {
	return Source{Agent: "claude", Session: "s1", Path: path, Parser: claudeParser{}}
}

func tmpTranscript(t *testing.T, content string) string {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	appendTo(t, p, content)
	return p
}

func TestStreamResetAppendUpdate(t *testing.T) {
	srv, f := newChatServer(t, 8)
	path := tmpTranscript(t, userLine("u1", "hi"))
	f.set(claudeSource(path), "")

	s := openStream(t, srv.URL+"/api/chat?terminal=t16", "")
	if ct := s.resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	reset := s.expect(t, "reset")
	msgs := reset["messages"].([]any)
	if reset["agent"] != "claude" || reset["session"] != "s1" || len(msgs) != 1 {
		t.Fatalf("reset = %v", reset)
	}

	appendTo(t, path, `{"type":"assistant","uuid":"a","message":{"id":"m1","content":[{"type":"tool_use","id":"tc","name":"Bash","input":{"command":"ls"}}]}}`+"\n")
	f.fire(t, f.poll)
	app := s.expect(t, "append")
	if got := app["messages"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] != "m1" {
		t.Fatalf("append = %v", app)
	}

	appendTo(t, path, `{"type":"user","uuid":"r","message":{"content":[{"type":"tool_result","tool_use_id":"tc","content":"out"}]}}`+"\n")
	f.fire(t, f.poll)
	upd := s.expect(t, "update")
	part := upd["messages"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if part["status"] != "ok" || part["output"] != "out" {
		t.Errorf("update part = %v", part)
	}
}

func TestStreamQueuedMessageAppendedThenRemoved(t *testing.T) {
	srv, f := newChatServer(t, 8)
	path := tmpTranscript(t, userLine("u1", "hi"))
	f.set(claudeSource(path), "")

	s := openStream(t, srv.URL+"/api/chat?terminal=t16", "")
	s.expect(t, "reset")

	appendTo(t, path, `{"type":"queue-operation","operation":"enqueue","timestamp":"t","content":"later"}`+"\n")
	f.fire(t, f.poll)
	app := s.expect(t, "append")
	got := app["messages"].([]any)[0].(map[string]any)
	if got["queued"] != true || got["id"] != "queued-1" {
		t.Fatalf("append = %v", app)
	}

	appendTo(t, path, `{"type":"queue-operation","operation":"dequeue","timestamp":"t2"}`+"\n")
	f.fire(t, f.poll)
	rm := s.expect(t, "remove")
	if ids := rm["ids"].([]any); len(ids) != 1 || ids[0] != "queued-1" {
		t.Fatalf("remove = %v", rm)
	}
}

func TestStreamSessionSwitchSendsReset(t *testing.T) {
	srv, f := newChatServer(t, 8)
	a := tmpTranscript(t, userLine("a1", "old"))
	b := tmpTranscript(t, userLine("b1", "new")+userLine("b2", "newer"))
	f.set(claudeSource(a), "")
	s := openStream(t, srv.URL+"/api/chat?terminal=t16", "")
	s.expect(t, "reset")

	f.set(Source{Agent: "claude", Session: "s2", Path: b, Parser: claudeParser{}}, "")
	f.fire(t, f.poll)
	r := s.expect(t, "reset")
	if r["session"] != "s2" || len(r["messages"].([]any)) != 2 {
		t.Errorf("reset after switch = %v", r)
	}
}

func TestStreamTruncationSendsReset(t *testing.T) {
	srv, f := newChatServer(t, 8)
	path := tmpTranscript(t, userLine("a1", "one")+userLine("a2", "two")+userLine("a3", "three"))
	f.set(claudeSource(path), "")
	s := openStream(t, srv.URL+"/api/chat?terminal=t16", "")
	s.expect(t, "reset")

	if err := writeFile(path, userLine("z1", "fresh")); err != nil {
		t.Fatal(err)
	}
	f.fire(t, f.poll)
	r := s.expect(t, "reset")
	if len(r["messages"].([]any)) != 1 {
		t.Errorf("reset = %v", r)
	}
}

func TestStreamNoneThenReset(t *testing.T) {
	srv, f := newChatServer(t, 8)
	f.set(Source{}, NoAgent)
	s := openStream(t, srv.URL+"/api/chat?terminal=t9", "")
	if n := s.expect(t, "none"); n["reason"] != "no-agent" {
		t.Fatalf("none = %v", n)
	}
	f.fire(t, f.poll) // same reason: no repeat
	f.set(claudeSource(tmpTranscript(t, userLine("u1", "hi"))), "")
	f.fire(t, f.poll)
	if r := s.expect(t, "reset"); len(r["messages"].([]any)) != 1 {
		t.Errorf("reset = %v", r)
	}
}

func TestStreamHeartbeat(t *testing.T) {
	srv, f := newChatServer(t, 8)
	f.set(Source{}, NoAgent)
	s := openStream(t, srv.URL+"/api/chat?terminal=t9", "")
	s.expect(t, "none")
	f.fire(t, f.beat)
	s.expect(t, "hb")
}

func TestStreamRemoteTerminalUnsupported(t *testing.T) {
	srv, f := newChatServer(t, 8)
	f.set(claudeSource(tmpTranscript(t, userLine("u1", "x"))), "") // must not be consulted
	s := openStream(t, srv.URL+"/api/chat?terminal=rohan/t7", "")
	if n := s.expect(t, "none"); n["reason"] != "unsupported" {
		t.Errorf("none = %v", n)
	}
}

func TestChatAuthOriginAndValidation(t *testing.T) {
	srv, _ := newChatServer(t, 8)
	get := func(path string, cookie bool, origin string, method string) int {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		if cookie {
			req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "tok"})
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("/api/chat?terminal=t1", false, "", "GET"); c != 401 {
		t.Errorf("no cookie = %d, want 401", c)
	}
	if c := get("/api/chat?terminal=t1", true, "https://evil.example", "GET"); c != 403 {
		t.Errorf("cross-origin = %d, want 403", c)
	}
	for _, bad := range []string{"", "../etc", "t1;x", "a//b", "t 1"} {
		if c := get("/api/chat?terminal="+strings.ReplaceAll(bad, " ", "%20"), true, "", "GET"); c != 400 {
			t.Errorf("terminal %q = %d, want 400", bad, c)
		}
	}
	if c := get("/api/chat?terminal=t1", true, "", "POST"); c != 405 {
		t.Errorf("POST = %d, want 405", c)
	}
}

func TestChatConcurrencyLimit(t *testing.T) {
	srv, f := newChatServer(t, 2)
	f.set(Source{}, NoAgent)
	a := openStream(t, srv.URL+"/api/chat?terminal=t1", "")
	b := openStream(t, srv.URL+"/api/chat?terminal=t2", "")
	a.expect(t, "none")
	b.expect(t, "none")
	req, _ := http.NewRequest("GET", srv.URL+"/api/chat?terminal=t3", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "tok"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Errorf("third stream = %d, want 429", resp.StatusCode)
	}
	// Closing one frees its slot.
	a.resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		code := resp.StatusCode
		if code == 200 {
			resp.Body.Close()
			return
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatalf("slot never freed (last status %d)", code)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamWaitingSendsEmptyResetOnceThenRealReset(t *testing.T) {
	srv, f := newChatServer(t, 8)
	f.set(Source{Agent: "claude", Session: "s1"}, Waiting)
	s := openStream(t, srv.URL+"/api/chat?terminal=t9", "")
	r := s.expect(t, "reset")
	if r["agent"] != "claude" || r["session"] != "s1" || len(r["messages"].([]any)) != 0 {
		t.Fatalf("waiting reset = %v", r)
	}
	f.fire(t, f.poll) // still waiting: no repeat
	f.fire(t, f.poll)
	f.set(claudeSource(tmpTranscript(t, userLine("u1", "hi"))), "")
	f.fire(t, f.poll)
	if r := s.expect(t, "reset"); len(r["messages"].([]any)) != 1 {
		t.Errorf("reset = %v", r)
	}
}

func TestStreamNoneThenWaitingStillSendsReset(t *testing.T) {
	srv, f := newChatServer(t, 8)
	f.set(Source{}, NotFound)
	s := openStream(t, srv.URL+"/api/chat?terminal=t9", "")
	s.expect(t, "none")
	f.set(Source{Agent: "pi"}, Waiting)
	f.fire(t, f.poll)
	if r := s.expect(t, "reset"); r["agent"] != "pi" {
		t.Errorf("reset = %v", r)
	}
}

func TestRevokedDeviceClosesChatStream(t *testing.T) {
	f := &fakes{poll: make(chan time.Time), beat: make(chan time.Time)}
	f.set(Source{}, NoAgent)
	a := auth.New("tok")
	h := a.Require(Handler(a, Config{
		Lookup:       func(string) (Terminal, error) { return Terminal{PID: 1, CWD: "/w"}, nil },
		Resolve:      f.resolve,
		Tick:         f.tick,
		ResolveEvery: 1,
		MaxStreams:   4,
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	login := httptest.NewRecorder()
	a.SessionHandler().ServeHTTP(login, httptest.NewRequest("POST", "/api/session", strings.NewReader(`{"token":"tok"}`)))
	cookie := login.Result().Cookies()[0]
	id, _, _ := strings.Cut(cookie.Value, ".")

	req, _ := http.NewRequest("GET", srv.URL+"?terminal=t1", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("open stream: %v %v", resp, err)
	}
	defer resp.Body.Close()
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, resp.Body); close(done) }()
	a.Revoke(id)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("chat stream still open 1s after revoke")
	}
}
