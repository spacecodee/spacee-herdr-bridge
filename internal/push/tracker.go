package push

import (
	"encoding/json"
	"fmt"
	"time"
)

// rateWindow is the minimum gap between notifications for one terminal.
const rateWindow = 10 * time.Second

// Terminal is the part of a core terminal the tracker needs.
type Terminal struct {
	Address  string            `json:"address"`
	Name     *string           `json:"name"`
	Labels   map[string]string `json:"labels"`
	State    string            `json:"state"` // live or finished
	Status   string            `json:"-"`     // agent state: idle, working, done, blocked, error
	ExitCode *int              `json:"exit_code"`
}

// rawTerminal adds the nested status object to Terminal when decoding core JSON.
type rawTerminal struct {
	Terminal
	StatusObj *struct {
		State string `json:"state"`
	} `json:"status"`
}

// ParseTerminal decodes a core terminal (snapshot entry or event data).
func ParseTerminal(b []byte) (Terminal, bool, error) {
	var r rawTerminal
	if err := json.Unmarshal(b, &r); err != nil {
		return Terminal{}, false, err
	}
	t := r.Terminal
	hasStatus := false
	var probe map[string]json.RawMessage
	if json.Unmarshal(b, &probe) == nil {
		_, hasStatus = probe["status"]
	}
	if r.StatusObj != nil {
		t.Status = r.StatusObj.State
	}
	return t, hasStatus, nil
}

// Tracker turns snapshots and core events into notices. Only transitions notify:
// a snapshot only records state. It is not safe for concurrent use.
type Tracker struct {
	now   func() time.Time
	terms map[string]*Terminal
	last  map[string]time.Time
}

func NewTracker(now func() time.Time) *Tracker {
	return &Tracker{now: now, terms: map[string]*Terminal{}, last: map[string]time.Time{}}
}

// Snapshot replaces the known terminals without notifying.
func (t *Tracker) Snapshot(terms []Terminal) {
	t.terms = make(map[string]*Terminal, len(terms))
	for i := range terms {
		x := terms[i]
		t.terms[x.Address] = &x
	}
}

// Event applies one core event and returns the notices it causes (at most one).
func (t *Tracker) Event(typ, address string, data json.RawMessage) []Notice {
	cur := t.terms[address]
	if cur == nil {
		cur = &Terminal{Address: address}
		t.terms[address] = cur
	}
	prevStatus := cur.Status

	if len(data) > 0 {
		if p, hasStatus, err := ParseTerminal(data); err == nil {
			if p.Labels != nil {
				cur.Labels = p.Labels
			}
			if p.Name != nil {
				cur.Name = p.Name
			}
			if p.ExitCode != nil {
				cur.ExitCode = p.ExitCode
			}
			if hasStatus {
				cur.Status = p.Status
			}
		}
	}

	var body string
	urgent := false
	switch typ {
	case "status":
		if cur.Status == prevStatus {
			return nil
		}
		switch cur.Status {
		case "blocked":
			body, urgent = "Needs you", true
		case "done":
			body = "Finished its turn"
		}
	case "finished":
		cur.State = "finished"
		cur.Status = ""
		if cur.ExitCode != nil {
			body = fmt.Sprintf("Exited (code %d)", *cur.ExitCode)
		} else {
			body = "Exited"
		}
	case "lost":
		body = "Lost connection"
	}
	if body == "" || !t.allow(address) {
		return nil
	}
	return []Notice{{Address: address, Title: title(cur), Body: body, Urgent: urgent}}
}

// allow enforces at most one notice per terminal per rateWindow.
func (t *Tracker) allow(address string) bool {
	now := t.now()
	if last, ok := t.last[address]; ok && now.Sub(last) < rateWindow {
		return false
	}
	t.last[address] = now
	return true
}

func title(t *Terminal) string {
	if w := t.Labels["where"]; w != "" {
		return w
	}
	if t.Name != nil && *t.Name != "" {
		return *t.Name
	}
	return t.Address
}
