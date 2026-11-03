package chat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const ocSchema = `
CREATE TABLE session_v2 (id text PRIMARY KEY, parent_id text, directory text NOT NULL, title text, revert text,
  time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL,
  seq integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE session_part (message_id text NOT NULL, session_id text NOT NULL, ordinal integer NOT NULL,
  type text NOT NULL, call_id text, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL,
  PRIMARY KEY (message_id, ordinal));`

// ocFixture is a writable opencode database in a temp dir (the code under test opens it read-only).
type ocFixture struct {
	t    *testing.T
	path string
	w    *sql.DB
	seq  map[string]int
}

func newOCFixture(t *testing.T) *ocFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	w, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	if _, err := w.Exec(ocSchema); err != nil {
		t.Fatal(err)
	}
	return &ocFixture{t: t, path: path, w: w, seq: map[string]int{}}
}

func (f *ocFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.w.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ocFixture) session(id, dir string, updated int64, parent string) {
	f.t.Helper()
	var p any
	if parent != "" {
		p = parent
	}
	f.exec(`INSERT INTO session_v2 (id, parent_id, directory, title, time_created, time_updated) VALUES (?,?,?,?,?,?)`,
		id, p, dir, "t", updated, updated)
}

// msg adds a message with the next seq of its session and returns that seq.
func (f *ocFixture) msg(session, id, typ string, at int64, data string) int {
	f.t.Helper()
	f.seq[session]++
	f.exec(`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES (?,?,?,?,?,?,?)`,
		id, session, typ, f.seq[session], at, at, data)
	return f.seq[session]
}

func (f *ocFixture) reader(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openOpencode(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

const (
	ocUser      = `{"agents":[],"files":[],"text":"fix the bug","time":{"created":1000}}`
	ocAssistant = `{"agent":"build","content":[` +
		`{"type":"reasoning","text":"hmm","time":{"created":1,"completed":2}},` +
		`{"type":"text","text":"on it"},` +
		`{"type":"tool","id":"call1","name":"bash","executed":true,"time":{"created":1},` +
		`"state":{"status":"completed","input":{"command":"pnpm test"},"content":[{"type":"text","text":"all green"}],"metadata":{"exit":0}}}` +
		`],"time":{"created":1001}}`
)

func TestOpenCodeMapsMessages(t *testing.T) {
	f := newOCFixture(t)
	f.session("ses_1", "/work/p", 5000, "")
	f.msg("ses_1", "m1", "user", 1000, `{"agents":[],"files":[{"mime":"image/png","name":"a.png"}],"text":"fix the bug","time":{"created":1000}}`)
	f.msg("ses_1", "m0", "system", 1001, `{"text":"you are...","time":{"created":1001}}`)
	f.msg("ses_1", "m2", "assistant", 1002, ocAssistant)
	f.msg("ses_1", "m3", "idle", 1003, `{"outcome":"done","time":{"created":1003}}`)
	f.msg("ses_1", "m4", "assistant", 1004, `{"content":[{"type":"tool","id":"call2","name":"edit","state":{"status":"error","input":{"file_path":"/x"},"error":{"type":"aborted","message":"Tool execution interrupted"}}}],"time":{"created":1004}}`)
	f.msg("ses_1", "m5", "assistant", 1005, `{"content":[{"type":"tool","id":"call3","name":"read","state":{"status":"running","input":{"file_path":"/y"}}}]}`)

	fl := newOCFollower(f.reader(t), "ses_1")
	reset, err := fl.Poll()
	if err != nil || !reset {
		t.Fatalf("first poll reset=%v err=%v", reset, err)
	}
	got := snap(fl.Builder())
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4 (user, 3 assistant; system and idle skipped): %+v", len(got), got)
	}
	if got[0].ID != "m1" || got[0].Role != "user" || partTypes(got[0]) != "text,image" || got[0].Parts[0].Text != "fix the bug" {
		t.Errorf("user = %+v", got[0])
	}
	if got[0].Time != "1970-01-01T00:00:01Z" {
		t.Errorf("time = %q", got[0].Time)
	}
	a := got[1]
	if a.ID != "m2" || a.Role != "assistant" || partTypes(a) != "thinking,text,tool" {
		t.Fatalf("assistant = %+v", a)
	}
	if p := a.Parts[2]; p.ID != "call1" || p.Name != "bash" || p.Summary != "pnpm test" || p.Status != "ok" || p.Output != "all green" || p.Input != `{"command":"pnpm test"}` {
		t.Errorf("tool = %+v", p)
	}
	if p := got[2].Parts[0]; p.Status != "error" || !strings.Contains(p.Output, "Tool execution interrupted") {
		t.Errorf("errored tool = %+v", p)
	}
	if p := got[3].Parts[0]; p.Status != "running" || p.Output != "" {
		t.Errorf("running tool = %+v", p)
	}
}

func TestOpenCodeOrdersBySeqNotInsertion(t *testing.T) {
	f := newOCFixture(t)
	f.session("ses_1", "/w", 1, "")
	f.exec(`INSERT INTO session_message VALUES ('b','ses_1','user',2,2,2,?)`, `{"text":"second","time":{"created":2}}`)
	f.exec(`INSERT INTO session_message VALUES ('a','ses_1','user',1,1,1,?)`, `{"text":"first","time":{"created":1}}`)
	fl := newOCFollower(f.reader(t), "ses_1")
	fl.Poll()
	got := snap(fl.Builder())
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("order = %+v", got)
	}
}

func TestOpenCodePollAppendsAndUpdates(t *testing.T) {
	f := newOCFixture(t)
	f.session("ses_1", "/w", 1, "")
	f.msg("ses_1", "m1", "user", 1000, ocUser)
	f.msg("ses_1", "m2", "assistant", 1001, `{"content":[{"type":"tool","id":"c1","name":"bash","state":{"status":"running","input":{"command":"ls"}}}]}`)
	fl := newOCFollower(f.reader(t), "ses_1")
	fl.Poll()
	fl.Builder().Drain()

	if reset, err := fl.Poll(); err != nil || reset {
		t.Fatalf("idle poll reset=%v err=%v", reset, err)
	}
	if a, u := fl.Builder().Drain(); len(a)+len(u) != 0 {
		t.Fatalf("idle poll produced %d/%d", len(a), len(u))
	}

	// the running tool completes: same message id, newer time_updated -> update, not a duplicate
	f.exec(`UPDATE session_message SET time_updated = 2000, data = ? WHERE id = 'm2'`,
		`{"content":[{"type":"tool","id":"c1","name":"bash","state":{"status":"completed","input":{"command":"ls"},"content":[{"type":"text","text":"a b"}]}}]}`)
	// and a new message arrives
	f.msg("ses_other", "x", "user", 1, `{"text":"other session","time":{"created":1}}`)
	f.msg("ses_1", "m3", "assistant", 2100, `{"content":[{"type":"text","text":"done"}]}`)
	if reset, err := fl.Poll(); err != nil || reset {
		t.Fatalf("poll reset=%v err=%v", reset, err)
	}
	added, updated := fl.Builder().Drain()
	if len(added) != 1 || added[0].ID != "m3" {
		t.Fatalf("added = %+v", added)
	}
	if len(updated) != 1 || updated[0].ID != "m2" || len(updated[0].Parts) != 1 || updated[0].Parts[0].Status != "ok" || updated[0].Parts[0].Output != "a b" {
		t.Fatalf("updated = %+v", updated)
	}
	if n := len(snap(fl.Builder())); n != 3 {
		t.Fatalf("messages = %d, want 3", n)
	}
}

func TestOpenCodeKeepsOnlyRecentMessages(t *testing.T) {
	f := newOCFixture(t)
	f.session("ses_1", "/w", 1, "")
	for i := 1; i <= keep+20; i++ {
		f.msg("ses_1", fmt.Sprintf("m%d", i), "user", int64(i), fmt.Sprintf(`{"text":"n%d","time":{"created":%d}}`, i, i))
	}
	fl := newOCFollower(f.reader(t), "ses_1")
	fl.Poll()
	got := snap(fl.Builder())
	if len(got) != keep || got[len(got)-1].ID != fmt.Sprintf("m%d", keep+20) || got[0].ID != "m21" {
		t.Fatalf("kept %d: first %s last %s", len(got), got[0].ID, got[len(got)-1].ID)
	}
}

func TestOpenCodeIsReadOnly(t *testing.T) {
	f := newOCFixture(t)
	f.session("ses_1", "/w", 1, "")
	db := f.reader(t)
	if _, err := db.Exec(`INSERT INTO session_v2 (id, directory, time_created, time_updated) VALUES ('x','/',1,1)`); err == nil {
		t.Fatal("write through the reader succeeded")
	}
	if _, err := db.Exec(`DELETE FROM session_v2`); err == nil {
		t.Fatal("delete through the reader succeeded")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM session_v2`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
}

func TestOpenCodeReaderSeesFreshWALRows(t *testing.T) {
	f := newOCFixture(t)
	f.session("ses_1", "/w", 1, "")
	fl := newOCFollower(f.reader(t), "ses_1")
	fl.Poll()
	f.msg("ses_1", "m1", "user", 5, ocUser) // committed to the WAL while the reader is open
	fl.Poll()
	if n := len(snap(fl.Builder())); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
}

func TestOpenCodeMissingDB(t *testing.T) {
	if _, err := openOpencode(filepath.Join(t.TempDir(), "nope.db")); err == nil {
		t.Fatal("expected an error for a missing database")
	}
}

func TestResolveOpenCodeBySessionDirectory(t *testing.T) {
	f := newOCFixture(t)
	f.session("ses_old", "/work/p", 1000, "")
	f.session("ses_new", "/work/p", 3000, "")
	f.session("ses_sub", "/work/p", 4000, "ses_new") // sub-agent session of the same directory
	f.session("ses_other", "/work/q", 5000, "")
	fx := newFixture(t)
	fx.proc_(t, 600, 1, 5, "/home/u/.opencode/bin/opencode")
	r := fx.resolver()
	r.OpenCode = f.path

	src, why := r.Resolve(Terminal{PID: 600, CWD: "/work/p"})
	if why != "" || src.Agent != "opencode" || src.Session != "ses_new" || src.DB == nil {
		t.Fatalf("src=%+v why=%q", src, why)
	}
	other, _ := r.Resolve(Terminal{PID: 600, CWD: "/work/q"})
	if other.Path == src.Path {
		t.Errorf("different sessions share a Path: %q", src.Path)
	}
	if _, why := r.Resolve(Terminal{PID: 600, CWD: "/work/none"}); why != Waiting {
		t.Errorf("unknown directory: %q, want waiting", why)
	}
	if _, why := r.Resolve(Terminal{PID: 600}); why != NotFound {
		t.Errorf("no cwd: %q", why)
	}
	r.OpenCode = filepath.Join(t.TempDir(), "missing.db")
	if _, why := r.Resolve(Terminal{PID: 600, CWD: "/work/p"}); why != NotFound {
		t.Errorf("missing db: %q", why)
	}
}

func TestResolveOpenCodeDefaultPathUnderHome(t *testing.T) {
	fx := newFixture(t)
	fx.proc_(t, 600, 1, 5, "opencode")
	if _, why := fx.resolver().Resolve(Terminal{PID: 600, CWD: "/work/p"}); why != NotFound {
		t.Fatalf("no db under home: %q (want %q)", why, NotFound)
	}
}

func TestSummarizeOpenCode(t *testing.T) {
	f := newOCFixture(t)
	f.session("ses_1", "/w", 1, "")
	f.msg("ses_1", "m1", "user", 1000, `{"text":"/init","time":{"created":1000}}`)
	f.msg("ses_1", "m2", "user", 1001, `{"text":"fix the **login** bug","time":{"created":1001}}`)
	f.msg("ses_1", "m3", "assistant", 1002, `{"content":[{"type":"text","text":"Done, all green."}]}`)
	src := Source{Agent: "opencode", Session: "ses_1", Path: f.path + "#ses_1", DB: f.reader(t)}

	sm := NewSummarizer()
	s, ok := sm.summarize(src)
	if !ok || s.Title != "fix the **login** bug" || s.Messages != 3 || s.Last == nil || s.Last.Role != "assistant" || s.Last.Text != "Done, all green." {
		t.Fatalf("summary = %+v ok=%v", s, ok)
	}
	if s.Agent != "opencode" || s.Session != "ses_1" {
		t.Errorf("summary ids = %+v", s)
	}
	reads := sm.Reads()
	if _, ok := sm.summarize(src); !ok || sm.Reads() != reads {
		t.Fatalf("unchanged session re-read: reads %d -> %d", reads, sm.Reads())
	}
	f.msg("ses_1", "m4", "assistant", 2000, `{"content":[{"type":"text","text":"One more thing."}]}`)
	s, _ = sm.summarize(src)
	if s.Last.Text != "One more thing." || s.Messages != 4 || sm.Reads() == reads {
		t.Fatalf("after change: %+v reads=%d", s, sm.Reads())
	}
}

func TestSummarizeOpenCodeTitleFromHeadWhenTailHasNone(t *testing.T) {
	f := newOCFixture(t)
	f.session("ses_1", "/w", 1, "")
	f.msg("ses_1", "m0", "user", 1, `{"text":"the very first ask","time":{"created":1}}`)
	for i := 0; i < ocSummaryRows+5; i++ {
		f.msg("ses_1", fmt.Sprintf("a%d", i), "assistant", int64(10+i), `{"content":[{"type":"text","text":"working"}]}`)
	}
	src := Source{Agent: "opencode", Session: "ses_1", Path: f.path + "#ses_1", DB: f.reader(t)}
	s, ok := NewSummarizer().summarize(src)
	if !ok || s.Title != "the very first ask" {
		t.Fatalf("summary = %+v ok=%v", s, ok)
	}
}

// --- rewind (revert) ---

func (f *ocFixture) setRevert(session, messageID string) {
	f.t.Helper()
	var v any
	if messageID != "" {
		v = `{"messageID":"` + messageID + `","snapshot":"abc"}`
	}
	f.exec(`UPDATE session_v2 SET revert = ? WHERE id = ?`, v, session)
}

// dropFrom deletes the messages with seq >= seq, as a committed rewind does.
func (f *ocFixture) dropFrom(session string, seq int) {
	f.t.Helper()
	f.exec(`DELETE FROM session_message WHERE session_id = ? AND seq >= ?`, session, seq)
	f.seq[session] = seq - 1
}

func ocTexts(f *ocFollower) []string { return texts(f.Builder()) }

func ocConversation(f *ocFixture) {
	f.session("ses_1", "/w", 1, "")
	f.msg("ses_1", "m1", "user", 1000, `{"text":"one","time":{"created":1000}}`)
	f.msg("ses_1", "m2", "assistant", 1001, `{"content":[{"type":"text","text":"ans one"}]}`)
	f.msg("ses_1", "m3", "user", 1002, `{"text":"two","time":{"created":1002}}`)
	f.msg("ses_1", "m4", "assistant", 1003, `{"content":[{"type":"text","text":"ans two"}]}`)
}

func TestOpenCodeStagedRevertHidesTheUndoneMessages(t *testing.T) {
	f := newOCFixture(t)
	ocConversation(f)
	f.setRevert("ses_1", "m3")
	fl := newOCFollower(f.reader(t), "ses_1")
	if reset, err := fl.Poll(); err != nil || !reset {
		t.Fatalf("first poll reset=%v err=%v", reset, err)
	}
	same(t, ocTexts(fl), "user:one", "assistant:ans one")
}

func TestOpenCodeRevertStagedAndClearedWhileFollowing(t *testing.T) {
	f := newOCFixture(t)
	ocConversation(f)
	fl := newOCFollower(f.reader(t), "ses_1")
	fl.Poll()
	same(t, ocTexts(fl), "user:one", "assistant:ans one", "user:two", "assistant:ans two")
	fl.Builder().Drain()

	f.setRevert("ses_1", "m3") // /undo
	if reset, err := fl.Poll(); err != nil || !reset {
		t.Fatalf("staged revert: reset=%v err=%v", reset, err)
	}
	same(t, ocTexts(fl), "user:one", "assistant:ans one")

	if reset, err := fl.Poll(); err != nil || reset {
		t.Fatalf("nothing changed: reset=%v err=%v", reset, err)
	}

	f.setRevert("ses_1", "") // /redo
	if reset, err := fl.Poll(); err != nil || !reset {
		t.Fatalf("cleared revert: reset=%v err=%v", reset, err)
	}
	same(t, ocTexts(fl), "user:one", "assistant:ans one", "user:two", "assistant:ans two")
}

func TestOpenCodeCommittedRewindResets(t *testing.T) {
	f := newOCFixture(t)
	ocConversation(f)
	f.setRevert("ses_1", "m3")
	fl := newOCFollower(f.reader(t), "ses_1")
	fl.Poll()

	// the next prompt commits: rows >= seq 3 are deleted, revert cleared, a new prompt takes seq 3
	f.dropFrom("ses_1", 3)
	f.setRevert("ses_1", "")
	f.msg("ses_1", "m5", "user", 2000, `{"text":"new two","time":{"created":2000}}`)
	if reset, err := fl.Poll(); err != nil || !reset {
		t.Fatalf("committed rewind: reset=%v err=%v", reset, err)
	}
	same(t, ocTexts(fl), "user:one", "assistant:ans one", "user:new two")
}

func TestOpenCodeDeletedRowsWithoutStagingReset(t *testing.T) {
	f := newOCFixture(t)
	ocConversation(f)
	fl := newOCFollower(f.reader(t), "ses_1")
	fl.Poll()
	f.dropFrom("ses_1", 3)
	f.msg("ses_1", "m5", "user", 2000, `{"text":"replaced","time":{"created":2000}}`)
	f.msg("ses_1", "m6", "assistant", 2001, `{"content":[{"type":"text","text":"ans"}]}`)
	f.msg("ses_1", "m7", "user", 2002, `{"text":"more","time":{"created":2002}}`)
	if reset, err := fl.Poll(); err != nil || !reset {
		t.Fatalf("reset=%v err=%v", reset, err)
	}
	same(t, ocTexts(fl), "user:one", "assistant:ans one", "user:replaced", "assistant:ans", "user:more")
}

func TestOpenCodeWithoutRevertRendersAsBefore(t *testing.T) {
	f := newOCFixture(t)
	ocConversation(f)
	fl := newOCFollower(f.reader(t), "ses_1")
	fl.Poll()
	fl.Builder().Drain()
	f.msg("ses_1", "m5", "user", 3000, `{"text":"three","time":{"created":3000}}`)
	if reset, err := fl.Poll(); err != nil || reset {
		t.Fatalf("append: reset=%v err=%v", reset, err)
	}
	added, _ := fl.Builder().Drain()
	if len(added) != 1 || added[0].ID != "m5" {
		t.Fatalf("added = %+v", added)
	}
}

func TestSummarizeOpenCodeUsesTheLiveBranch(t *testing.T) {
	f := newOCFixture(t)
	ocConversation(f)
	src := Source{Agent: "opencode", Session: "ses_1", Path: f.path + "#ses_1", DB: f.reader(t)}
	sm := NewSummarizer()
	s, _ := sm.summarize(src)
	if s.Messages != 4 || s.Last.Text != "ans two" {
		t.Fatalf("summary = %+v", s)
	}
	f.setRevert("ses_1", "m3") // same rows and update times: only the staged revert changed
	s, _ = sm.summarize(src)
	if s.Messages != 2 || s.Last.Text != "ans one" {
		t.Fatalf("after /undo: %+v", s)
	}
}
