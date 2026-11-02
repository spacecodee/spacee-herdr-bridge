package push

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"time"
)

const (
	// readTimeout detects a dead connection: core sends a heartbeat every 15 s.
	readTimeout = 45 * time.Second
	maxLine     = 16 << 20
)

// Watcher follows core's events.subscribe stream and reports transitions.
type Watcher struct {
	Socket     string
	Tracker    *Tracker
	Notify     func(Notice)
	MinBackoff time.Duration // default 1 s
	MaxBackoff time.Duration // default 30 s
}

// Run connects, watches and reconnects with backoff until stop is closed.
func (w *Watcher) Run(stop <-chan struct{}) {
	minB, maxB := w.MinBackoff, w.MaxBackoff
	if minB == 0 {
		minB = time.Second
	}
	if maxB == 0 {
		maxB = 30 * time.Second
	}
	backoff := minB
	for {
		got, err := w.session(stop)
		if got {
			backoff = minB
		}
		if err != nil {
			log.Printf("push watcher: core connection ended: %v", err)
		}
		select {
		case <-stop:
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxB {
			backoff = maxB
		}
	}
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result *struct {
		Seq       uint64            `json:"seq"`
		Terminals []json.RawMessage `json:"terminals"`
	} `json:"result"`
	Error  json.RawMessage `json:"error"`
	Params struct {
		Seq       uint64            `json:"seq"`
		Terminal  string            `json:"terminal"`
		Type      string            `json:"type"`
		Data      json.RawMessage   `json:"data"`
		Terminals []json.RawMessage `json:"terminals"`
	} `json:"params"`
}

// session runs one connection. got reports whether the snapshot arrived.
func (w *Watcher) session(stop <-chan struct{}) (got bool, err error) {
	c, err := net.DialTimeout("unix", w.Socket, 5*time.Second)
	if err != nil {
		return false, err
	}
	defer c.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			c.Close()
		case <-done:
		}
	}()

	if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"events.subscribe","params":{}}` + "\n")); err != nil {
		return false, err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	var snapSeq uint64
	for {
		c.SetReadDeadline(time.Now().Add(readTimeout))
		if !sc.Scan() {
			return got, sc.Err()
		}
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch {
		case m.Result != nil:
			w.snapshot(m.Result.Terminals)
			snapSeq, got = m.Result.Seq, true
		case m.Method == "missed":
			if m.Params.Terminals != nil { // fresh snapshot for a subscriber that fell behind
				w.snapshot(m.Params.Terminals)
				snapSeq = m.Params.Seq
			}
		case m.Method == "event":
			if !got || m.Params.Seq <= snapSeq {
				continue // before the snapshot, or already reflected in it
			}
			for _, n := range w.Tracker.Event(m.Params.Type, m.Params.Terminal, m.Params.Data) {
				w.Notify(n)
			}
		}
	}
}

func (w *Watcher) snapshot(raw []json.RawMessage) {
	terms := make([]Terminal, 0, len(raw))
	for _, r := range raw {
		if t, _, err := ParseTerminal(r); err == nil && t.Address != "" {
			terms = append(terms, t)
		}
	}
	w.Tracker.Snapshot(terms)
}
