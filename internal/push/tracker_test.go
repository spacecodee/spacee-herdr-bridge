package push

import (
	"encoding/json"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func term(addr, where, status string) Terminal {
	return Terminal{Address: addr, State: "live", Status: status, Labels: map[string]string{"where": where}}
}

func ev(t *testing.T, tr *Tracker, typ, addr, data string) []Notice {
	t.Helper()
	return tr.Event(typ, addr, json.RawMessage(data))
}

func TestTrackerTransitionsOnly(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	tr := NewTracker(c.now)
	tr.Snapshot([]Terminal{term("t1", "Proj / Agent", "blocked"), term("t2", "", "working")})

	// snapshot never notifies; same state again is not a transition
	if n := ev(t, tr, "status", "t1", `{"status":{"state":"blocked"}}`); len(n) != 0 {
		t.Fatalf("no transition, got %+v", n)
	}
	// working: no notification
	if n := ev(t, tr, "status", "t2", `{"status":{"state":"working"}}`); len(n) != 0 {
		t.Fatalf("got %+v", n)
	}
	// blocked: needs you, urgent, title = where
	c.t = c.t.Add(time.Minute)
	n := ev(t, tr, "status", "t1", `{"status":{"state":"idle"}}`)
	if len(n) != 0 {
		t.Fatalf("idle: %+v", n)
	}
	n = ev(t, tr, "status", "t1", `{"status":{"state":"blocked"}}`)
	if len(n) != 1 || n[0].Body != "Needs you" || !n[0].Urgent || n[0].Title != "Proj / Agent" || n[0].Address != "t1" {
		t.Fatalf("blocked: %+v", n)
	}
	// done on a terminal without where/name: title falls back to the address
	n = ev(t, tr, "status", "t2", `{"status":{"state":"done"}}`)
	if len(n) != 1 || n[0].Body != "Finished its turn" || n[0].Urgent || n[0].Title != "t2" {
		t.Fatalf("done: %+v", n)
	}
	// the redundant `turn` event never notifies
	if n := ev(t, tr, "turn", "t2", `{"state":"done","turn":3}`); len(n) != 0 {
		t.Fatalf("turn: %+v", n)
	}
}

func TestTrackerFinishedAndLost(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	tr := NewTracker(c.now)
	tr.Snapshot([]Terminal{term("t1", "A", ""), term("t2", "B", "")})
	n := ev(t, tr, "finished", "t1", `{"exit_code":3,"state":"finished"}`)
	if len(n) != 1 || n[0].Body != "Exited (code 3)" {
		t.Fatalf("finished: %+v", n)
	}
	n = ev(t, tr, "lost", "t2", `{}`)
	if len(n) != 1 || n[0].Body != "Lost connection" {
		t.Fatalf("lost: %+v", n)
	}
	// unknown terminal still notifies, titled by address
	n = ev(t, tr, "finished", "t9", `{"exit_code":0}`)
	if len(n) != 1 || n[0].Title != "t9" || n[0].Body != "Exited (code 0)" {
		t.Fatalf("unknown: %+v", n)
	}
}

func TestTrackerRateLimitPerTerminal(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	tr := NewTracker(c.now)
	tr.Snapshot([]Terminal{term("t1", "A", "working"), term("t2", "B", "working")})
	if n := ev(t, tr, "status", "t1", `{"status":{"state":"done"}}`); len(n) != 1 {
		t.Fatal("first must notify")
	}
	c.t = c.t.Add(5 * time.Second)
	ev(t, tr, "status", "t1", `{"status":{"state":"working"}}`)
	if n := ev(t, tr, "status", "t1", `{"status":{"state":"blocked"}}`); len(n) != 0 {
		t.Fatalf("within 10s must be dropped: %+v", n)
	}
	// other terminals are independent
	if n := ev(t, tr, "status", "t2", `{"status":{"state":"done"}}`); len(n) != 1 {
		t.Fatal("t2 must notify")
	}
	c.t = c.t.Add(10 * time.Second)
	ev(t, tr, "status", "t1", `{"status":{"state":"working"}}`)
	if n := ev(t, tr, "status", "t1", `{"status":{"state":"blocked"}}`); len(n) != 1 {
		t.Fatal("after 10s must notify")
	}
}

func TestTrackerReconnectSnapshotIsSilent(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	tr := NewTracker(c.now)
	tr.Snapshot([]Terminal{term("t1", "A", "working")})
	// core was away; on return the terminal is blocked: that is a snapshot, not a transition
	tr.Snapshot([]Terminal{term("t1", "A", "blocked")})
	if n := ev(t, tr, "status", "t1", `{"status":{"state":"blocked"}}`); len(n) != 0 {
		t.Fatalf("got %+v", n)
	}
}

func TestTrackerLabelsUpdateTitle(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	tr := NewTracker(c.now)
	tr.Snapshot([]Terminal{term("t1", "Old", "working")})
	ev(t, tr, "labels", "t1", `{"labels":{"where":"New"}}`)
	n := ev(t, tr, "status", "t1", `{"status":{"state":"done"}}`)
	if len(n) != 1 || n[0].Title != "New" {
		t.Fatalf("%+v", n)
	}
}
