package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var bootTime = time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)

type fixture struct {
	proc, home string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{proc: t.TempDir(), home: t.TempDir()}
	write(t, filepath.Join(f.proc, "stat"), fmt.Sprintf("cpu  1 2 3\nbtime %d\n", bootTime.Unix()))
	return f
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// proc adds a fake /proc/<pid> started startSec seconds after boot.
func (f fixture) proc_(t *testing.T, pid, ppid, startSec int, argv ...string) {
	t.Helper()
	d := filepath.Join(f.proc, fmt.Sprint(pid))
	write(t, filepath.Join(d, "stat"), fmt.Sprintf("%d (my (cmd)) S %d%s %d 0 0", pid, ppid, strings.Repeat(" 0", 17), startSec*100))
	write(t, filepath.Join(d, "cmdline"), strings.Join(argv, "\x00")+"\x00")
}

func (f fixture) resolver() Resolver {
	return Resolver{Proc: f.proc, Home: f.home, Now: func() time.Time { return bootTime.Add(48 * time.Hour) }}
}

func touch(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestResolveClaudeByPid(t *testing.T) {
	f := newFixture(t)
	f.proc_(t, 100, 1, 10, "node", "/x/caveman/index.js", "claude", "--flag")
	f.proc_(t, 101, 100, 11, "/home/u/.local/bin/claude", "--flag")
	f.proc_(t, 102, 101, 12, "/usr/bin/zsh", "-c", "ls")
	write(t, filepath.Join(f.home, ".claude/sessions/101.json"), `{"pid":101,"sessionId":"sess-1","cwd":"/work/my.proj"}`)
	tr := filepath.Join(f.home, ".claude/projects/-work-my-proj/sess-1.jsonl")
	write(t, tr, "")

	src, why := f.resolver().Resolve(Terminal{PID: 100, CWD: "/elsewhere"})
	if why != "" || src.Agent != "claude" || src.Session != "sess-1" || src.Path != tr {
		t.Fatalf("src=%+v why=%q", src, why)
	}
	if _, ok := src.Parser.(claudeParser); !ok {
		t.Errorf("parser = %T", src.Parser)
	}
}

func TestResolveClaudeSessionSwitch(t *testing.T) {
	f := newFixture(t)
	f.proc_(t, 101, 1, 11, "claude")
	write(t, filepath.Join(f.home, ".claude/sessions/101.json"), `{"sessionId":"new","cwd":"/w"}`)
	write(t, filepath.Join(f.home, ".claude/projects/-w/old.jsonl"), "")
	write(t, filepath.Join(f.home, ".claude/projects/-w/new.jsonl"), "")
	src, _ := f.resolver().Resolve(Terminal{PID: 101})
	if src.Session != "new" {
		t.Errorf("session = %q, want the one in sessions/<pid>.json", src.Session)
	}
}

func TestResolveClaudeRefusesEscapes(t *testing.T) {
	f := newFixture(t)
	f.proc_(t, 101, 1, 11, "claude")
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	write(t, outside, "x")

	write(t, filepath.Join(f.home, ".claude/sessions/101.json"), `{"sessionId":"../../../etc/x","cwd":"/w"}`)
	if _, why := f.resolver().Resolve(Terminal{PID: 101}); why != "not-found" {
		t.Errorf("traversing session id: why=%q, want not-found", why)
	}

	write(t, filepath.Join(f.home, ".claude/sessions/101.json"), `{"sessionId":"s","cwd":"/w"}`)
	dir := filepath.Join(f.home, ".claude/projects/-w")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "s.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, why := f.resolver().Resolve(Terminal{PID: 101}); why != "not-found" {
		t.Errorf("symlink out of ~/.claude: why=%q, want not-found", why)
	}
}

func TestResolvePiNewestAfterStart(t *testing.T) {
	f := newFixture(t)
	f.proc_(t, 200, 1, 50, "node", "/x/index.js", "pi")
	f.proc_(t, 201, 200, 51, "pi")
	dir := filepath.Join(f.home, ".pi/agent/sessions/--work-proj--")
	old, mid, newest := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl"), filepath.Join(dir, "c.jsonl")
	for _, p := range []string{old, mid, newest} {
		write(t, p, "")
	}
	start := bootTime.Add(51 * time.Second)
	touch(t, old, start.Add(-time.Hour))
	touch(t, mid, start.Add(time.Minute))
	touch(t, newest, start.Add(time.Hour))
	write(t, filepath.Join(f.home, ".pi/agent/sessions/--other--/z.jsonl"), "")

	src, why := f.resolver().Resolve(Terminal{PID: 200, CWD: "/work/proj"})
	if why != "" || src.Agent != "pi" || src.Path != newest {
		t.Fatalf("src=%+v why=%q", src, why)
	}
	if _, ok := src.Parser.(piParser); !ok {
		t.Errorf("parser = %T", src.Parser)
	}

	touch(t, mid, start.Add(-time.Hour))
	touch(t, newest, start.Add(-time.Hour))
	if _, why := f.resolver().Resolve(Terminal{PID: 200, CWD: "/work/proj"}); why != "not-found" {
		t.Errorf("only files older than the process: why=%q, want not-found", why)
	}
}

func TestResolveCodexMatchesCwdAndStart(t *testing.T) {
	f := newFixture(t)
	f.proc_(t, 300, 1, 3600*30, "codex") // started 2026-11-03 14:00
	day := filepath.Join(f.home, ".codex/sessions/2026/11/03")
	mine, other, stale := filepath.Join(day, "rollout-a.jsonl"), filepath.Join(day, "rollout-b.jsonl"), filepath.Join(day, "rollout-c.jsonl")
	meta := func(cwd string) string {
		return `{"type":"session_meta","payload":{"cwd":"` + cwd + `","base_instructions":{"text":"` + strings.Repeat("i", 100<<10) + `"}}}` + "\n"
	}
	write(t, mine, meta("/work/proj")+`{"type":"event_msg"}`+"\n")
	write(t, other, meta("/work/else"))
	write(t, stale, meta("/work/proj"))
	start := bootTime.Add(30 * time.Hour)
	touch(t, mine, start.Add(time.Minute))
	touch(t, other, start.Add(2*time.Minute))
	touch(t, stale, start.Add(-time.Hour))

	src, why := f.resolver().Resolve(Terminal{PID: 300, CWD: "/work/proj"})
	if why != "" || src.Agent != "codex" || src.Path != mine {
		t.Fatalf("src=%+v why=%q", src, why)
	}
	if _, ok := src.Parser.(codexParser); !ok {
		t.Errorf("parser = %T", src.Parser)
	}
}

func TestResolveReasons(t *testing.T) {
	f := newFixture(t)
	f.proc_(t, 400, 1, 1, "/usr/bin/zsh")
	f.proc_(t, 401, 400, 2, "vim", "notes.txt")
	if _, why := f.resolver().Resolve(Terminal{PID: 400}); why != "no-agent" {
		t.Errorf("shell only: %q", why)
	}
	f.proc_(t, 402, 400, 3, "/home/u/.opencode/bin/opencode")
	if _, why := f.resolver().Resolve(Terminal{PID: 400}); why != "not-found" {
		t.Errorf("opencode without a database: %q", why)
	}
	f.proc_(t, 500, 1, 1, "agy", "--x")
	if _, why := f.resolver().Resolve(Terminal{PID: 500}); why != "unsupported" {
		t.Errorf("agy: %q", why)
	}
	f.proc_(t, 600, 1, 1, "claude")
	if _, why := f.resolver().Resolve(Terminal{PID: 600}); why != "not-found" {
		t.Errorf("claude without session file: %q", why)
	}
	if _, why := f.resolver().Resolve(Terminal{PID: 0}); why != "no-agent" {
		t.Errorf("no pid: %q", why)
	}
	if _, why := f.resolver().Resolve(Terminal{PID: 999}); why != "no-agent" {
		t.Errorf("unknown pid: %q", why)
	}
}
