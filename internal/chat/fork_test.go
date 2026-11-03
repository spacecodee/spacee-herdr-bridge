package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Claude: Up pulls queued messages back (popAll, one entry per message) ---

func queueOp(op, text string) string {
	return fmt.Sprintf(`{"type":"queue-operation","operation":%q,"timestamp":"2026-11-03T10:00:00Z","content":%q}`, op, text)
}

func TestClaudePopAllRemovesTheQueuedMessages(t *testing.T) {
	b := feed(claudeParser{}, queueOp("enqueue", "A"), queueOp("enqueue", "B"))
	same(t, texts(b), "user:A", "user:B")
	claudeParser{}.Line([]byte(queueOp("popAll", "A")), b)
	same(t, texts(b), "user:B")
	claudeParser{}.Line([]byte(queueOp("popAll", "B")), b)
	same(t, texts(b))
}

func TestClaudePopAllWhileTailingReportsTheRemoved(t *testing.T) {
	b := feed(claudeParser{}, queueOp("enqueue", "A"), queueOp("enqueue", "B"))
	b.Drain()
	b.Gone()
	claudeParser{}.Line([]byte(queueOp("popAll", "A")), b)
	claudeParser{}.Line([]byte(queueOp("popAll", "B")), b)
	if g := b.Gone(); len(g) != 2 {
		t.Fatalf("gone = %q, want both queued messages", g)
	}
}

func TestClaudeQueueWithoutPopAllRendersAsBefore(t *testing.T) {
	b := feed(claudeParser{}, queueOp("enqueue", "A"), queueOp("enqueue", "B"), queueOp("dequeue", "A"))
	same(t, texts(b), "user:B")
	if m := snap(b); !m[0].Queued {
		t.Errorf("B must stay queued: %+v", m[0])
	}
}

// --- Codex: a rewind forks into a new rollout whose session_meta has history_base ---

const forkID = "01a1a393-ccef-76c2-b2d8-002731047408"

func cxMeta(cwd, base string) string {
	if base != "" {
		base = `,"history_base":` + base
	}
	return fmt.Sprintf(`{"timestamp":"2026-11-03T10:00:00Z","type":"session_meta","payload":{"id":%q,"cwd":%q%s}}`, forkID, cwd, base)
}

// cxBase is history_base for the first n lines (meta included) of a rollout.
func cxBase(lines []string, n int) string {
	off := 0
	for _, l := range lines[:n] {
		off += len(l) + 1
	}
	return fmt.Sprintf(`{"thread_id":%q,"end_ordinal_exclusive":%d,"end_byte_offset":%d}`, forkID, n, off)
}

// rollout writes lines to dir/name, one per line.
func rollout(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	write(t, p, strings.Join(lines, "\n")+"\n")
	return p
}

// forkResolve resolves terminal pid 300 (a codex started on 2026-11-03 14:00) in /work/p.
func forkResolve(t *testing.T, f fixture, newest string) Source {
	t.Helper()
	f.proc_(t, 300, 1, 3600*30, "codex")
	// the others are older than the process; newest is the one being written
	old, _ := filepath.Glob(filepath.Join(f.home, ".codex/sessions/*/*/*/rollout-*.jsonl"))
	for _, p := range old {
		touch(t, p, bootTime.Add(29*time.Hour))
	}
	touch(t, newest, bootTime.Add(30*time.Hour+time.Minute))
	src, why := f.resolver().Resolve(Terminal{PID: 300, CWD: "/work/p"})
	if why != "" || src.Path != newest {
		t.Fatalf("src=%+v why=%q", src, why)
	}
	return src
}

func chatOf(t *testing.T, src Source) (*Tailer, []string) {
	t.Helper()
	tl := NewTailer(src.Path, src.Parser, NewBuilder(400))
	tl.Prefix = src.Prefix
	if _, err := tl.Poll(); err != nil {
		t.Fatal(err)
	}
	return tl, texts(tl.B)
}

func TestCodexForkKeepsEarlierTurnsAndDropsTheRemovedPrompt(t *testing.T) {
	f := newFixture(t)
	day := filepath.Join(f.home, ".codex/sessions/2026/11/03")
	parent := []string{cxMeta("/work/p", ""), cxEnv, cx("user", "u1", "one"), cx("assistant", "a1", "ans one"),
		cx("user", "u2", "two"), cx("assistant", "a2", "ans two"), cx("user", "u3", "three"), cx("assistant", "a3", "ans three")}
	rollout(t, day, "rollout-2026-11-03T10-00-00-"+forkID+".jsonl", parent)
	fork := rollout(t, day, "rollout-2026-11-03T10-05-00-"+forkID+"_t1.jsonl",
		[]string{cxMeta("/work/p", cxBase(parent, 6)), cx("user", "u4", "four"), cx("assistant", "a4", "ans four")})

	_, got := chatOf(t, forkResolve(t, f, fork))
	same(t, got, "user:one", "assistant:ans one", "user:two", "assistant:ans two", "user:four", "assistant:ans four")
}

func TestCodexForkFindsTheParentOnAnEarlierDay(t *testing.T) {
	f := newFixture(t)
	root := filepath.Join(f.home, ".codex/sessions/2026/11")
	parent := []string{cxMeta("/work/p", ""), cx("user", "u1", "one"), cx("assistant", "a1", "ans one"), cx("user", "u2", "two")}
	rollout(t, filepath.Join(root, "02"), "rollout-2026-11-02T22-00-00-"+forkID+".jsonl", parent)
	fork := rollout(t, filepath.Join(root, "03"), "rollout-2026-11-03T15-00-00-"+forkID+"_t1.jsonl",
		[]string{cxMeta("/work/p", cxBase(parent, 3)), cx("user", "u5", "five")})

	_, got := chatOf(t, forkResolve(t, f, fork))
	same(t, got, "user:one", "assistant:ans one", "user:five")
}

func TestCodexChainedForks(t *testing.T) {
	f := newFixture(t)
	day := filepath.Join(f.home, ".codex/sessions/2026/11/03")
	g := []string{cxMeta("/work/p", ""), cx("user", "u1", "one"), cx("assistant", "a1", "ans one"), cx("user", "x", "gone first")}
	rollout(t, day, "rollout-2026-11-03T10-00-00-"+forkID+".jsonl", g)
	f1 := []string{cxMeta("/work/p", cxBase(g, 3)), cx("user", "u3", "three"), cx("assistant", "a3", "ans three"), cx("user", "y", "gone second")}
	rollout(t, day, "rollout-2026-11-03T10-05-00-"+forkID+"_t1.jsonl", f1)
	f2 := rollout(t, day, "rollout-2026-11-03T10-10-00-"+forkID+"_t2.jsonl",
		[]string{cxMeta("/work/p", cxBase(f1, 3)), cx("user", "u5", "five")})

	_, got := chatOf(t, forkResolve(t, f, f2))
	same(t, got, "user:one", "assistant:ans one", "user:three", "assistant:ans three", "user:five")
}

func TestCodexForkWithoutParentShowsOnlyTheNewFile(t *testing.T) {
	f := newFixture(t)
	day := filepath.Join(f.home, ".codex/sessions/2026/11/03")
	fork := rollout(t, day, "rollout-2026-11-03T10-05-00-"+forkID+"_t1.jsonl",
		[]string{cxMeta("/work/p", fmt.Sprintf(`{"thread_id":%q,"end_ordinal_exclusive":4,"end_byte_offset":999}`, forkID)), cx("user", "u4", "four")})

	src := forkResolve(t, f, fork)
	if len(src.Prefix) != 0 {
		t.Fatalf("prefix = %+v", src.Prefix)
	}
	_, got := chatOf(t, src)
	same(t, got, "user:four")
}

func TestCodexForkWithAWrongOffsetIsNotTrusted(t *testing.T) {
	f := newFixture(t)
	day := filepath.Join(f.home, ".codex/sessions/2026/11/03")
	parent := []string{cxMeta("/work/p", ""), cx("user", "u1", "one"), cx("assistant", "a1", "ans one")}
	rollout(t, day, "rollout-2026-11-03T10-00-00-"+forkID+".jsonl", parent)
	// ordinal 2 does not end at byte 7 of the parent
	fork := rollout(t, day, "rollout-2026-11-03T10-05-00-"+forkID+"_t1.jsonl",
		[]string{cxMeta("/work/p", fmt.Sprintf(`{"thread_id":%q,"end_ordinal_exclusive":2,"end_byte_offset":7}`, forkID)), cx("user", "u4", "four")})

	_, got := chatOf(t, forkResolve(t, f, fork))
	same(t, got, "user:four")
}

func TestCodexForkOutsideTheSessionsTreeIsIgnored(t *testing.T) {
	f := newFixture(t)
	day := filepath.Join(f.home, ".codex/sessions/2026/11/03")
	parent := []string{cxMeta("/work/p", ""), cx("user", "u1", "one")}
	outside := rollout(t, t.TempDir(), "rollout-2026-11-03T10-00-00-"+forkID+".jsonl", parent)
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(day, "rollout-2026-11-03T10-00-00-"+forkID+".jsonl")); err != nil {
		t.Fatal(err)
	}
	fork := rollout(t, day, "rollout-2026-11-03T10-05-00-"+forkID+"_t1.jsonl",
		[]string{cxMeta("/work/p", cxBase(parent, 2)), cx("user", "u4", "four")})

	_, got := chatOf(t, forkResolve(t, f, fork))
	same(t, got, "user:four")
}

func TestCodexForkTailingAppendsAfterThePrefix(t *testing.T) {
	f := newFixture(t)
	day := filepath.Join(f.home, ".codex/sessions/2026/11/03")
	parent := []string{cxMeta("/work/p", ""), cx("user", "u1", "one"), cx("assistant", "a1", "ans one"), cx("user", "u2", "two")}
	rollout(t, day, "rollout-2026-11-03T10-00-00-"+forkID+".jsonl", parent)
	fork := rollout(t, day, "rollout-2026-11-03T10-05-00-"+forkID+"_t1.jsonl",
		[]string{cxMeta("/work/p", cxBase(parent, 3)), cx("user", "u4", "four")})

	tl, got := chatOf(t, forkResolve(t, f, fork))
	same(t, got, "user:one", "assistant:ans one", "user:four")
	appendTo(t, fork, cx("assistant", "a4", "ans four")+"\n")
	reset, err := tl.Poll()
	if err != nil || reset {
		t.Fatalf("reset=%v err=%v", reset, err)
	}
	same(t, texts(tl.B), "user:one", "assistant:ans one", "user:four", "assistant:ans four")
}

func TestCodexForkSummaryUsesTheChain(t *testing.T) {
	f := newFixture(t)
	day := filepath.Join(f.home, ".codex/sessions/2026/11/03")
	parent := []string{cxMeta("/work/p", ""), cx("user", "u1", "the first question"), cx("assistant", "a1", "ans one"), cx("user", "u2", "two")}
	rollout(t, day, "rollout-2026-11-03T10-00-00-"+forkID+".jsonl", parent)
	fork := rollout(t, day, "rollout-2026-11-03T10-05-00-"+forkID+"_t1.jsonl",
		[]string{cxMeta("/work/p", cxBase(parent, 3)), cx("user", "u4", "four")})

	src := forkResolve(t, f, fork)
	s, ok := NewSummarizer().summarize(src)
	if !ok || s.Title != "the first question" || s.Messages != 3 {
		t.Fatalf("summary = %+v ok=%v", s, ok)
	}
}

func TestCodexWithoutHistoryBaseHasNoPrefix(t *testing.T) {
	f := newFixture(t)
	day := filepath.Join(f.home, ".codex/sessions/2026/11/03")
	one := rollout(t, day, "rollout-2026-11-03T10-00-00-"+forkID+".jsonl",
		[]string{cxMeta("/work/p", ""), cx("user", "u1", "one"), cx("assistant", "a1", "ans one")})
	src := forkResolve(t, f, one)
	if len(src.Prefix) != 0 {
		t.Fatalf("prefix = %+v", src.Prefix)
	}
	_, got := chatOf(t, src)
	same(t, got, "user:one", "assistant:ans one")
}
