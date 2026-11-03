package chat

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go driver: the image stays static, no CGO
)

const (
	ocSummaryRows = 200 // messages read for a summary (from the end, and from the start for the title)
	ocDefaultDB   = ".local/share/opencode/opencode.db"
)

var ocDBs = struct {
	sync.Mutex
	m map[string]*sql.DB
}{m: map[string]*sql.DB{}}

// openOpencode returns the shared read-only handle of an opencode database. It never
// writes or checkpoints: mode=ro plus query_only. It is not immutable, so rows committed
// to the WAL by a running opencode are visible.
func openOpencode(path string) (*sql.DB, error) {
	ocDBs.Lock()
	defer ocDBs.Unlock()
	if db := ocDBs.m[path]; db != nil {
		return db, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	ocDBs.m[path] = db
	return db, nil
}

// opencode finds the session of an opencode process: the newest top-level session whose
// directory is the terminal's cwd (same heuristic, and caveat, as pi and codex).
func (r Resolver) opencode(cwd string) (Source, bool) {
	if cwd == "" {
		return Source{}, false
	}
	path := r.OpenCode
	if path == "" {
		path = filepath.Join(r.Home, ocDefaultDB)
	}
	db, err := openOpencode(path)
	if err != nil {
		return Source{}, false
	}
	var id string
	err = db.QueryRow(`SELECT id FROM session_v2 WHERE directory = ? AND parent_id IS NULL ORDER BY time_updated DESC LIMIT 1`, cwd).Scan(&id)
	if err != nil || !sessionID.MatchString(id) {
		return Source{Agent: "opencode"}, false
	}
	return Source{Agent: "opencode", Session: id, Path: path + "#" + id, DB: db}, true
}

// ocFollower reads one session from the database: the last messages on the first poll,
// then the messages that are newer or were updated (a running tool completing).
type ocFollower struct {
	db      *sql.DB
	sid     string
	b       *Builder
	limit   int
	started bool
	seq     int64
	upd     int64
}

func newOCFollower(db *sql.DB, sid string) *ocFollower {
	return &ocFollower{db: db, sid: sid, b: NewBuilder(keep), limit: keep}
}

func (f *ocFollower) Builder() *Builder { return f.b }

type ocRow struct {
	id, typ         string
	seq, created, u int64
	data            []byte
}

const ocCols = `SELECT id, type, seq, time_created, time_updated, data FROM session_message WHERE session_id = ?`

func (f *ocFollower) query(q string, args ...any) ([]ocRow, error) {
	rows, err := f.db.Query(q, append([]any{f.sid}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ocRow
	for rows.Next() {
		var r ocRow
		if err := rows.Scan(&r.id, &r.typ, &r.seq, &r.created, &r.u, &r.data); err != nil {
			return nil, err
		}
		if len(r.data) <= maxLine {
			out = append(out, r)
		}
		f.seq, f.upd = max(f.seq, r.seq), max(f.upd, r.u)
	}
	return out, rows.Err()
}

// Poll implements follower: reset is true on the first read.
func (f *ocFollower) Poll() (reset bool, err error) {
	var rows []ocRow
	if !f.started {
		rows, err = f.query(ocCols+` ORDER BY seq DESC LIMIT ?`, f.limit)
		if err != nil {
			return false, err
		}
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
		f.b.Reset()
		f.started, reset = true, true
	} else if rows, err = f.query(ocCols+` AND (seq > ? OR time_updated > ?) ORDER BY seq`, f.seq, f.upd); err != nil {
		return false, err
	}
	for _, r := range rows {
		if m, ok := ocMessage(r); ok {
			f.b.Replace(m)
		}
	}
	return reset, nil
}

// head returns the first n messages of the session.
func (f *ocFollower) head(n int) ([]Message, error) {
	rows, err := f.query(ocCols+` ORDER BY seq LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	b := NewBuilder(n)
	for _, r := range rows {
		if m, ok := ocMessage(r); ok {
			b.Add(m)
		}
	}
	return b.Snapshot(n), nil
}

type ocBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	State struct {
		Status  string          `json:"status"`
		Input   json.RawMessage `json:"input"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"state"`
}

// ocMessage maps one session_message row. Only user and assistant messages are chat;
// system, synthetic, idle and model-switched rows are bookkeeping.
func ocMessage(r ocRow) (Message, bool) {
	m := Message{ID: r.id, Role: r.typ, Time: time.UnixMilli(r.created).UTC().Format(time.RFC3339)}
	switch r.typ {
	case "user":
		var d struct {
			Text  string            `json:"text"`
			Files []json.RawMessage `json:"files"`
		}
		if json.Unmarshal(r.data, &d) != nil {
			return m, false
		}
		if !blank(d.Text) {
			m.Parts = append(m.Parts, textPart("text", d.Text))
		}
		for range d.Files {
			m.Parts = append(m.Parts, Part{Type: "image", Note: "attachment"})
		}
	case "assistant":
		var d struct {
			Content []ocBlock `json:"content"`
		}
		if json.Unmarshal(r.data, &d) != nil {
			return m, false
		}
		for _, bl := range d.Content {
			switch bl.Type {
			case "text":
				if !blank(bl.Text) {
					m.Parts = append(m.Parts, textPart("text", bl.Text))
				}
			case "reasoning":
				if !blank(bl.Text) {
					m.Parts = append(m.Parts, textPart("thinking", bl.Text))
				}
			case "tool":
				m.Parts = append(m.Parts, ocTool(bl))
			}
		}
	default:
		return m, false
	}
	return m, len(m.Parts) > 0
}

func ocTool(bl ocBlock) Part {
	p := toolPart(bl.ID, bl.Name, string(bl.State.Input))
	var out string
	switch bl.State.Status {
	case "completed":
		p.Status = "ok"
		for _, c := range bl.State.Content {
			switch c.Type {
			case "text":
				out += c.Text
			case "file":
				out += "[file]"
			}
		}
	case "error":
		p.Status = "error"
		out = bl.State.Error.Message
	default:
		return p
	}
	var cut bool
	p.Output, cut = clip(out, maxIO)
	p.Truncated = p.Truncated || cut
	return p
}
