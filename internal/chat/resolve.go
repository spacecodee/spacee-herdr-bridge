package chat

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Terminal is what core knows about a terminal: the foreground process and its directory.
type Terminal struct {
	PID int
	CWD string
}

// Source is the transcript of a running agent.
type Source struct {
	Agent   string
	Session string
	Path    string
	Parser  Parser
	DB      *sql.DB // opencode: the session lives in this database, not in Path/Parser
}

// Why nothing is shown (the reason of an SSE "none" event).
const (
	NoAgent     = "no-agent"
	Unsupported = "unsupported"
	NotFound    = "not-found"
	Waiting     = "waiting" // agent running, no transcript yet (before its first message)
)

// Resolver maps a terminal's process tree to an agent transcript. Proc and Home are
// parameters so tests can use fake trees; Now defaults to time.Now.
type Resolver struct {
	Proc string // /proc
	Home string
	Now  func() time.Time

	OpenCode string // opencode database; default <Home>/.local/share/opencode/opencode.db

	procs map[int]proc // set by Snapshot
}

// Snapshot returns a Resolver that reuses one scan of the process table, for resolving
// many terminals in one request.
func (r Resolver) Snapshot() Resolver {
	r.procs = r.processes()
	return r
}

const clockTicks = 100 // USER_HZ on Linux

var (
	sessionID  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
	nonAlnum   = regexp.MustCompile(`[^A-Za-z0-9]`)
	supported  = map[string]bool{"claude": true, "pi": true, "codex": true, "opencode": true}
	known      = map[string]bool{"claude": true, "pi": true, "codex": true, "opencode": true, "agy": true}
	interpPref = map[string]bool{"node": true, "bun": true, "deno": true, "npx": true}
)

type proc struct {
	pid, ppid int
	start     time.Time
	argv      []string
}

// Resolve finds the transcript of the agent running under t.PID. The reason is one of
// NoAgent, Unsupported, NotFound when the Source is empty. With Waiting the agent is
// running but has not written a transcript yet: the Source carries only Agent (and Session
// when known), with an empty Path.
func (r Resolver) Resolve(t Terminal) (Source, string) {
	procs := r.processes()
	var agents []proc
	kinds := map[int]string{}
	for _, p := range subtree(procs, t.PID) {
		if k := agentKind(p.argv); k != "" {
			agents = append(agents, p)
			kinds[p.pid] = k
		}
	}
	reason := NoAgent
	var waiting *Source
	for _, p := range agents {
		k := kinds[p.pid]
		if !supported[k] {
			if reason == NoAgent {
				reason = Unsupported
			}
			continue
		}
		reason = NotFound
		var src Source
		var ok bool
		switch k {
		case "claude":
			src, ok = r.claude(p)
		case "pi":
			src, ok = r.pi(p, t.CWD)
		case "codex":
			src, ok = r.codex(p, t.CWD)
		case "opencode":
			src, ok = r.opencode(t.CWD)
		}
		if ok {
			return src, ""
		}
		if src.Agent != "" { // running, but no transcript yet
			waiting = &src
		}
	}
	if waiting != nil {
		return *waiting, Waiting
	}
	return Source{}, reason
}

// processes reads {pid, ppid, start, argv} for every process under r.Proc.
func (r Resolver) processes() map[int]proc {
	if r.procs != nil {
		return r.procs
	}
	boot := r.bootTime()
	entries, _ := os.ReadDir(r.Proc)
	out := map[int]proc{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(r.Proc, e.Name(), "stat"))
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ... starttime": comm may contain spaces and parentheses.
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(stat[i+1:]))
		if len(f) < 20 {
			continue
		}
		ppid, err1 := strconv.Atoi(f[1])
		ticks, err2 := strconv.ParseInt(f[19], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join(r.Proc, e.Name(), "cmdline"))
		p := proc{pid: pid, ppid: ppid, start: boot.Add(time.Duration(ticks) * time.Second / clockTicks)}
		for _, a := range bytes.Split(bytes.TrimRight(cmd, "\x00"), []byte{0}) {
			p.argv = append(p.argv, string(a))
		}
		out[pid] = p
	}
	return out
}

func (r Resolver) bootTime() time.Time {
	f, err := os.Open(filepath.Join(r.Proc, "stat"))
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(n, 0)
			}
		}
	}
	return time.Time{}
}

// subtree lists root and its descendants, breadth first.
func subtree(procs map[int]proc, root int) []proc {
	if _, ok := procs[root]; !ok {
		return nil
	}
	kids := map[int][]int{}
	for _, p := range procs {
		kids[p.ppid] = append(kids[p.ppid], p.pid)
	}
	var out []proc
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		out = append(out, procs[pid])
		c := kids[pid]
		sort.Ints(c)
		for _, k := range c {
			if !seen[k] {
				seen[k] = true
				queue = append(queue, k)
			}
		}
	}
	return out
}

// agentKind names the agent a command line runs: the executable, or the first arguments
// when it is launched through an interpreter (node .../caveman claude).
func agentKind(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	base := filepath.Base(argv[0])
	if known[base] {
		return base
	}
	if interpPref[base] {
		for _, a := range argv[1:min(len(argv), 4)] {
			if b := filepath.Base(a); known[b] {
				return b
			}
		}
	}
	return ""
}

// inside reports whether path, with symlinks resolved, is under root (also resolved).
func inside(root, path string) (string, bool) {
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	rp, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(rr, rp)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rp, true
}

func (r Resolver) claude(p proc) (Source, bool) {
	root := filepath.Join(r.Home, ".claude")
	raw, err := readSmall(filepath.Join(root, "sessions", fmt.Sprintf("%d.json", p.pid)))
	if err != nil {
		return Source{}, false
	}
	var s struct {
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
	}
	if json.Unmarshal(raw, &s) != nil || !sessionID.MatchString(s.SessionID) || s.CWD == "" {
		return Source{}, false
	}
	dirs := []string{
		strings.NewReplacer("/", "-", ".", "-").Replace(s.CWD),
		nonAlnum.ReplaceAllString(s.CWD, "-"),
	}
	missing := true
	for _, d := range dirs {
		cand := filepath.Join(root, "projects", d, s.SessionID+".jsonl")
		if path, ok := inside(root, cand); ok {
			return Source{Agent: "claude", Session: s.SessionID, Path: path, Parser: claudeParser{}}, true
		}
		if _, err := os.Lstat(cand); err == nil {
			missing = false // exists but is refused (symlink out of ~/.claude): not "waiting"
		}
	}
	if missing {
		return Source{Agent: "claude", Session: s.SessionID}, false // written on the first message
	}
	return Source{}, false
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n, _ := f.Read(buf)
	return buf[:n], nil
}

// newestAfter returns the most recently modified file among paths (resolved inside root)
// whose mtime is not before since.
func newestAfter(root string, paths []string, since time.Time) (string, bool) {
	var best string
	var bestMod time.Time
	for _, p := range paths {
		rp, ok := inside(root, p)
		if !ok {
			continue
		}
		fi, err := os.Stat(rp)
		if err != nil || !fi.Mode().IsRegular() || fi.ModTime().Before(since) {
			continue
		}
		if best == "" || fi.ModTime().After(bestMod) {
			best, bestMod = rp, fi.ModTime()
		}
	}
	return best, best != ""
}

func (r Resolver) pi(p proc, cwd string) (Source, bool) {
	if cwd == "" {
		return Source{}, false
	}
	root := filepath.Join(r.Home, ".pi")
	dir := "--" + strings.ReplaceAll(strings.Trim(cwd, "/"), "/", "-") + "--"
	files, _ := filepath.Glob(filepath.Join(root, "agent", "sessions", dir, "*.jsonl"))
	path, ok := newestAfter(root, files, p.start)
	if !ok {
		return Source{Agent: "pi"}, false
	}
	return Source{Agent: "pi", Session: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path, Parser: piParser{}}, true
}

func (r Resolver) codex(p proc, cwd string) (Source, bool) {
	if cwd == "" {
		return Source{}, false
	}
	root := filepath.Join(r.Home, ".codex")
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	var files []string
	// Rollouts live in one directory per day: only the days since the process started.
	for d := p.start.UTC().Truncate(24 * time.Hour); !d.After(now().UTC()); d = d.AddDate(0, 0, 1) {
		m, _ := filepath.Glob(filepath.Join(root, "sessions", d.Format("2006/01/02"), "rollout-*.jsonl"))
		files = append(files, m...)
	}
	// Newest first; the first one recorded for this cwd wins.
	sort.Slice(files, func(i, j int) bool { return modTime(files[i]).After(modTime(files[j])) })
	for _, f := range files {
		if path, ok := newestAfter(root, []string{f}, p.start); ok && codexCWD(path) == cwd {
			return Source{Agent: "codex", Session: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path, Parser: codexParser{}}, true
		}
	}
	return Source{Agent: "codex"}, false
}

func modTime(p string) time.Time {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// codexCWD reads the cwd from the session_meta on the first line (which carries the long
// base instructions, hence the large buffer).
func codexCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 1<<20).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ""
	}
	var m struct {
		Type    string `json:"type"`
		Payload struct {
			CWD string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "session_meta" {
		return ""
	}
	return m.Payload.CWD
}
