package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

type sumResp struct {
	Summaries []struct {
		Terminal string `json:"terminal"`
		Agent    string `json:"agent"`
		Session  string `json:"session"`
		Title    string `json:"title"`
		Last     *struct {
			Role string `json:"role"`
			Text string `json:"text"`
			Time string `json:"time"`
		} `json:"last"`
		Messages int `json:"messages"`
		Asking   *struct {
			Count    int    `json:"count"`
			Question string `json:"question"`
		} `json:"asking"`
	} `json:"summaries"`
}

func cuser(id, ts, text string) string {
	return fmt.Sprintf(`{"type":"user","uuid":%q,"timestamp":%q,"message":{"role":"user","content":%s}}`, id, ts, marshal0(text))
}

func cassistant(id, ts string, blocks string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"timestamp":%q,"message":{"id":%q,"role":"assistant","content":%s}}`, id, ts, id, blocks)
}

func marshal0(s string) string { b, _ := json.Marshal(s); return string(b) }

func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// sumServer serves /api/chat/summaries over fake terminals: address -> source (nil = shell).
func sumServer(t *testing.T, terms map[string]*Source) (*httptest.Server, *Summarizer) {
	t.Helper()
	var addrs []string
	for a := range terms {
		addrs = append(addrs, a)
	}
	sm := NewSummarizer()
	h := SummariesHandler(auth.New("tok"), Config{
		List:   func() ([]string, error) { return addrs, nil },
		Lookup: func(a string) (Terminal, error) { return Terminal{PID: len(a), CWD: a}, nil },
		Resolve: func(tm Terminal) (Source, string) {
			if s := terms[tm.CWD]; s != nil {
				if s.Path == "" {
					return *s, Waiting
				}
				return *s, ""
			}
			return Source{}, NoAgent
		},
	}, sm)
	mux := http.NewServeMux()
	mux.Handle("/api/chat/summaries", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, sm
}

func getSummaries(t *testing.T, srv *httptest.Server) sumResp {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/chat/summaries", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "tok"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out sumResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeT(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSummaryTitleSkipsSlashCommandsAndLastTextIsPlain(t *testing.T) {
	p := writeT(t, lines(
		cuser("u0", "2026-11-02T10:00:00Z", "<command-name>/clear</command-name>"),
		cuser("u1", "2026-11-02T10:00:01Z", "/clear"),
		cuser("u2", "2026-11-02T10:00:02Z", "  fix   the\nlogin bug  "),
		cassistant("a1", "2026-11-02T10:00:03Z", `[{"type":"text","text":"## Done\n\nI **fixed** it in [auth.go](http://x/y).\n\n`+"```go\\nfunc main() {}\\n```"+`\n- one\n- two"}]`),
	))
	srv, _ := sumServer(t, map[string]*Source{"t1": {Agent: "claude", Session: "s1", Path: p, Parser: claudeParser{}}})
	got := getSummaries(t, srv)
	if len(got.Summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(got.Summaries))
	}
	s := got.Summaries[0]
	if s.Terminal != "t1" || s.Agent != "claude" || s.Session != "s1" {
		t.Fatalf("identity = %+v", s)
	}
	if s.Title != "fix the login bug" {
		t.Fatalf("title = %q", s.Title)
	}
	if s.Messages != 4 {
		t.Fatalf("messages = %d, want 4", s.Messages)
	}
	if s.Last == nil || s.Last.Role != "assistant" || s.Last.Time != "2026-11-02T10:00:03Z" {
		t.Fatalf("last = %+v", s.Last)
	}
	want := "Done I fixed it in auth.go. func main() {} one two"
	if s.Last.Text != want {
		t.Fatalf("last text = %q, want %q", s.Last.Text, want)
	}
}

func TestSummaryClipsTitleAndLast(t *testing.T) {
	long := strings.Repeat("é", 200)
	p := writeT(t, lines(
		cuser("u1", "2026-11-02T10:00:00Z", long),
		cassistant("a1", "2026-11-02T10:00:01Z", `[{"type":"text","text":`+marshal0(long)+`}]`),
	))
	srv, _ := sumServer(t, map[string]*Source{"t1": {Agent: "claude", Session: "s", Path: p, Parser: claudeParser{}}})
	s := getSummaries(t, srv).Summaries[0]
	if len(s.Title) > 80 || len(s.Last.Text) > 140 || !strings.HasPrefix(long, s.Title) || !strings.HasPrefix(long, s.Last.Text) {
		t.Fatalf("title %d bytes, last %d bytes", len(s.Title), len(s.Last.Text))
	}
}

func TestSummaryToolOnlyLastMessage(t *testing.T) {
	p := writeT(t, lines(
		cuser("u1", "2026-11-02T10:00:00Z", "run the tests"),
		cassistant("a1", "2026-11-02T10:00:01Z", `[{"type":"thinking","thinking":"hm"},{"type":"tool_use","id":"tc1","name":"Bash","input":{"command":"pnpm test"}}]`),
	))
	srv, _ := sumServer(t, map[string]*Source{"t1": {Agent: "claude", Session: "s", Path: p, Parser: claudeParser{}}})
	s := getSummaries(t, srv).Summaries[0]
	if s.Last == nil || s.Last.Text != "Ran: pnpm test" {
		t.Fatalf("last = %+v", s.Last)
	}
}

func TestSummaryShellsAndUnresolvedTerminalsAreOmitted(t *testing.T) {
	p := writeT(t, lines(cuser("u1", "2026-11-02T10:00:00Z", "hi")))
	srv, _ := sumServer(t, map[string]*Source{
		"t1": {Agent: "claude", Session: "s", Path: p, Parser: claudeParser{}},
		"t2": nil,
		"t3": {Agent: "claude", Session: "gone", Path: filepath.Join(t.TempDir(), "missing.jsonl"), Parser: claudeParser{}},
	})
	got := getSummaries(t, srv)
	if len(got.Summaries) != 1 || got.Summaries[0].Terminal != "t1" {
		t.Fatalf("summaries = %+v", got.Summaries)
	}
}

func TestSummaryEmptyIsArrayNotNull(t *testing.T) {
	srv, _ := sumServer(t, map[string]*Source{"t2": nil})
	resp, err := http.Get(srv.URL + "/api/chat/summaries")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/chat/summaries", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "tok"})
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var raw map[string]json.RawMessage
	json.NewDecoder(r2.Body).Decode(&raw)
	if string(raw["summaries"]) != "[]" {
		t.Fatalf("summaries = %s", raw["summaries"])
	}
}

func TestSummaryCacheHitSkipsReadUntilFileChanges(t *testing.T) {
	p := writeT(t, lines(cuser("u1", "2026-11-02T10:00:00Z", "hi"), cassistant("a1", "2026-11-02T10:00:01Z", `[{"type":"text","text":"one"}]`)))
	srv, sm := sumServer(t, map[string]*Source{"t1": {Agent: "claude", Session: "s", Path: p, Parser: claudeParser{}}})
	getSummaries(t, srv)
	first := sm.Reads()
	if first == 0 {
		t.Fatal("first call read nothing")
	}
	getSummaries(t, srv)
	if sm.Reads() != first {
		t.Fatalf("reads %d -> %d: unchanged file was re-read", first, sm.Reads())
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(cassistant("a2", "2026-11-02T10:00:09Z", `[{"type":"text","text":"two"}]`) + "\n")
	f.Close()
	os.Chtimes(p, time.Now().Add(time.Second), time.Now().Add(time.Second))
	s := getSummaries(t, srv).Summaries[0]
	if sm.Reads() == first || s.Last.Text != "two" || s.Messages != 3 {
		t.Fatalf("reads %d, last %+v, messages %d", sm.Reads(), s.Last, s.Messages)
	}
}

func TestSummaryReadsOnlyTailButFindsTitleInHead(t *testing.T) {
	var b strings.Builder
	b.WriteString(cuser("u0", "2026-11-02T10:00:00Z", "the original ask") + "\n")
	pad := strings.Repeat("x", 4000)
	for i := 0; i < 120; i++ { // ~480 KiB of assistant noise, no user message in the tail
		b.WriteString(cassistant(fmt.Sprintf("a%d", i), "2026-11-02T10:00:01Z", `[{"type":"text","text":"`+pad+`"}]`) + "\n")
	}
	p := writeT(t, b.String())
	srv, _ := sumServer(t, map[string]*Source{"t1": {Agent: "claude", Session: "s", Path: p, Parser: claudeParser{}}})
	s := getSummaries(t, srv).Summaries[0]
	if s.Title != "the original ask" {
		t.Fatalf("title = %q", s.Title)
	}
	if s.Messages >= 120 || s.Messages < 40 {
		t.Fatalf("messages = %d: expected only the 256 KiB tail to be counted", s.Messages)
	}
}

func TestSummariesRequireSessionAndSameOrigin(t *testing.T) {
	srv, _ := sumServer(t, map[string]*Source{})
	resp, _ := http.Get(srv.URL + "/api/chat/summaries")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no cookie: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/chat/summaries", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "tok"})
	req.Header.Set("Origin", "https://evil.example")
	r2, _ := http.DefaultClient.Do(req)
	r2.Body.Close()
	if r2.StatusCode != 403 {
		t.Fatalf("foreign origin: %d", r2.StatusCode)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/chat/summaries", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "tok"})
	r3, _ := http.DefaultClient.Do(req)
	r3.Body.Close()
	if r3.StatusCode != 405 {
		t.Fatalf("POST: %d", r3.StatusCode)
	}
}

func TestListLiveLocalFiltersRemoteAndFinished(t *testing.T) {
	got := liveLocal([]byte(`{"terminals":[{"address":"t1","state":"live"},{"address":"t2","state":"finished"},{"address":"mac/t3","state":"live"},{"address":"t4","state":"live"}]}`))
	if strings.Join(got, ",") != "t1,t4" {
		t.Fatalf("got %v", got)
	}
}

func TestSummaryWaitingTerminalListedAfterConversations(t *testing.T) {
	p := writeT(t, lines(cuser("u1", "2026-11-02T10:00:00Z", "hi")))
	srv, _ := sumServer(t, map[string]*Source{
		"t1": {Agent: "claude", Session: "s", Path: p, Parser: claudeParser{}},
		"t2": {Agent: "pi", Session: "w"},
	})
	got := getSummaries(t, srv)
	if len(got.Summaries) != 2 || got.Summaries[0].Terminal != "t1" {
		t.Fatalf("summaries = %+v", got.Summaries)
	}
	w := got.Summaries[1]
	if w.Terminal != "t2" || w.Agent != "pi" || w.Session != "w" || w.Messages != 0 || w.Last != nil || w.Title != "" {
		t.Errorf("waiting = %+v", w)
	}
}

const (
	sumAskUse = `{"type":"assistant","uuid":"a1","timestamp":"2026-11-03T10:00:01Z","message":{"id":"m1","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Which db?","header":"DB","options":[{"label":"pg"}]},{"question":"Which mode?","header":"M","options":[{"label":"fast"}]}]}}]}}`
	sumAskRes = `{"type":"user","uuid":"u2","timestamp":"2026-11-03T10:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"q1","content":"User has answered your questions"}]},"toolUseResult":{"answers":{"Which db?":"pg","Which mode?":"fast"}}}`
)

func TestSummaryAskingClaude(t *testing.T) {
	p := writeT(t, lines(cuser("u1", "2026-11-03T10:00:00Z", "hi"), sumAskUse))
	srv, _ := sumServer(t, map[string]*Source{"t1": {Agent: "claude", Session: "s", Path: p, Parser: claudeParser{}}})
	s := getSummaries(t, srv).Summaries[0]
	if s.Asking == nil || s.Asking.Count != 2 || s.Asking.Question != "Which db?" {
		t.Fatalf("pending: asking = %+v", s.Asking)
	}

	// The answer arrives: the cache must not keep the stale asking.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(sumAskRes + "\n")
	f.Close()
	os.Chtimes(p, time.Now().Add(time.Second), time.Now().Add(time.Second))
	if s := getSummaries(t, srv).Summaries[0]; s.Asking != nil {
		t.Fatalf("answered: asking = %+v", s.Asking)
	}

	// A newer plain message after the answer: still absent.
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(cassistant("a3", "2026-11-03T10:00:09Z", `[{"type":"text","text":"done"}]`) + "\n")
	f.Close()
	os.Chtimes(p, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second))
	if s := getSummaries(t, srv).Summaries[0]; s.Asking != nil {
		t.Fatalf("newer message: asking = %+v", s.Asking)
	}
}

func TestSummaryNoQuestionHasNoAsking(t *testing.T) {
	p := writeT(t, lines(cuser("u1", "2026-11-03T10:00:00Z", "hi"), cassistant("a1", "2026-11-03T10:00:01Z", `[{"type":"text","text":"x"}]`)))
	srv, _ := sumServer(t, map[string]*Source{"t1": {Agent: "claude", Session: "s", Path: p, Parser: claudeParser{}}})
	if s := getSummaries(t, srv).Summaries[0]; s.Asking != nil {
		t.Fatalf("asking = %+v", s.Asking)
	}
}

func TestSummaryAskingPi(t *testing.T) {
	p := writeT(t, lines(piAsk))
	srv, _ := sumServer(t, map[string]*Source{"t1": {Agent: "pi", Session: "s", Path: p, Parser: piParser{}}})
	s := getSummaries(t, srv).Summaries[0]
	if s.Asking == nil || s.Asking.Count != 2 || s.Asking.Question != "Which db?" {
		t.Fatalf("pi asking = %+v", s.Asking)
	}
}

func TestSummaryAskingCodex(t *testing.T) {
	p := writeT(t, lines(codexAsk))
	srv, _ := sumServer(t, map[string]*Source{"t1": {Agent: "codex", Session: "s", Path: p, Parser: codexParser{}}})
	s := getSummaries(t, srv).Summaries[0]
	if s.Asking == nil || s.Asking.Count != 1 || s.Asking.Question != "Which scope?" {
		t.Fatalf("codex asking = %+v", s.Asking)
	}
}
