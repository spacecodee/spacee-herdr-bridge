package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func userLine(id, text string) string {
	return `{"type":"user","uuid":"` + id + `","message":{"role":"user","content":"` + text + `"}}` + "\n"
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func newTail(t *testing.T) (*Tailer, string) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	return NewTailer(path, claudeParser{}, NewBuilder(400)), path
}

func TestTailInitialReadThenAppend(t *testing.T) {
	tl, path := newTail(t)
	appendTo(t, path, userLine("u1", "one")+userLine("u2", "two"))
	reset, err := tl.Poll()
	if err != nil || !reset {
		t.Fatalf("first poll reset=%v err=%v", reset, err)
	}
	if n := len(tl.B.Snapshot(10)); n != 2 {
		t.Fatalf("messages = %d, want 2", n)
	}
	tl.B.Drain()

	if reset, _ := tl.Poll(); reset {
		t.Error("idle poll reported reset")
	}
	appendTo(t, path, userLine("u3", "three"))
	reset, _ = tl.Poll()
	added, _ := tl.B.Drain()
	if reset || len(added) != 1 || added[0].ID != "u3" {
		t.Errorf("reset=%v added=%+v", reset, added)
	}
}

func TestTailPartialLastLine(t *testing.T) {
	tl, path := newTail(t)
	full := userLine("u1", "hello")
	appendTo(t, path, full[:20])
	tl.Poll()
	if n := len(tl.B.Snapshot(10)); n != 0 {
		t.Fatalf("partial line parsed: %d messages", n)
	}
	appendTo(t, path, full[20:])
	tl.Poll()
	if got := tl.B.Snapshot(10); len(got) != 1 || got[0].Parts[0].Text != "hello" {
		t.Errorf("after completing the line: %+v", got)
	}
}

func TestTailTruncationResets(t *testing.T) {
	tl, path := newTail(t)
	appendTo(t, path, userLine("a1", "old one")+userLine("a2", "old two")+userLine("a3", "old three"))
	tl.Poll()
	if err := os.WriteFile(path, []byte(userLine("b1", "new")), 0o600); err != nil {
		t.Fatal(err)
	}
	reset, err := tl.Poll()
	got := tl.B.Snapshot(10)
	if err != nil || !reset || len(got) != 1 || got[0].ID != "b1" {
		t.Errorf("reset=%v err=%v got=%+v", reset, err, got)
	}
}

func TestTailRotationResets(t *testing.T) {
	tl, path := newTail(t)
	appendTo(t, path, userLine("a1", "x"))
	tl.Poll()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, userLine("b1", "replacement that is longer than the first file")+userLine("b2", "more"))
	reset, _ := tl.Poll()
	if got := tl.B.Snapshot(10); !reset || len(got) != 2 || got[0].ID != "b1" {
		t.Errorf("reset=%v got=%+v", reset, got)
	}
}

func TestTailSkipsOversizedLine(t *testing.T) {
	tl, path := newTail(t)
	huge := `{"type":"user","uuid":"big","message":{"content":"` + strings.Repeat("a", maxLine+10) + `"}}` + "\n"
	appendTo(t, path, userLine("u1", "before")+huge+userLine("u2", "after"))
	tl.Poll()
	got := tl.B.Snapshot(10)
	if len(got) != 2 || got[0].ID != "u1" || got[1].ID != "u2" {
		t.Errorf("got %d messages: %+v", len(got), got)
	}
}

func TestTailBigFileStartsNearTheEnd(t *testing.T) {
	tl, path := newTail(t)
	var sb strings.Builder
	for i := 0; sb.Len() < initialTail+(1<<20); i++ {
		sb.WriteString(userLine(fmt.Sprint("id", i), strings.Repeat("p", 200)))
	}
	appendTo(t, path, sb.String())
	if _, err := tl.Poll(); err != nil {
		t.Fatal(err)
	}
	if n := len(tl.B.Snapshot(10000)); n == 0 || n > 400 {
		t.Errorf("kept %d messages", n)
	}
}

func TestTailMissingFile(t *testing.T) {
	tl, _ := newTail(t)
	if _, err := tl.Poll(); err == nil {
		t.Error("want an error for a missing file")
	}
}
