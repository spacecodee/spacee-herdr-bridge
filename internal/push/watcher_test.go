package push

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeCore accepts connections on a temp Unix socket and runs script for each.
func fakeCore(t *testing.T, script func(n int, c net.Conn, r *bufio.Reader)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "core.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for n := 0; ; n++ {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(n int) { defer c.Close(); script(n, c, bufio.NewReader(c)) }(n)
		}
	}()
	return path
}

func snapshot(seq int, terms ...string) string {
	m, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
		"seq": seq, "terminals": json.RawMessage("[" + join(terms) + "]"),
	}})
	return string(m) + "\n"
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

func statusEvent(seq int, term, state string) string {
	return `{"jsonrpc":"2.0","method":"event","params":{"seq":` + itoa(seq) + `,"terminal":"` + term +
		`","type":"status","data":{"status":{"state":"` + state + `"}}}}` + "\n"
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestWatcherSubscribesNotifiesAndReconnects(t *testing.T) {
	gotSub := make(chan string, 4)
	sock := fakeCore(t, func(n int, c net.Conn, r *bufio.Reader) {
		line, _ := r.ReadString('\n')
		gotSub <- line
		switch n {
		case 0:
			c.Write([]byte(snapshot(10, `{"address":"t1","state":"live","status":{"state":"working"},"labels":{"where":"A"}}`)))
			c.Write([]byte(`{"jsonrpc":"2.0","method":"heartbeat","params":{"seq":10}}` + "\n"))
			c.Write([]byte(statusEvent(9, "t1", "blocked"))) // seq not above snapshot: stale, ignored
			c.Write([]byte(statusEvent(11, "t1", "blocked")))
			time.Sleep(50 * time.Millisecond) // then the core goes away
		default:
			// after reconnect t1 is already done: snapshot only, no notice
			c.Write([]byte(snapshot(20,
				`{"address":"t1","state":"live","status":{"state":"done"},"labels":{"where":"A"}}`,
				`{"address":"t2","state":"live","status":{"state":"working"}}`)))
			c.Write([]byte(statusEvent(21, "t2", "blocked")))
			time.Sleep(time.Second)
		}
	})

	notes := make(chan Notice, 8)
	tr := NewTracker(time.Now)
	w := &Watcher{Socket: sock, Tracker: tr, Notify: func(n Notice) { notes <- n }, MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}
	stop := make(chan struct{})
	defer close(stop)
	go w.Run(stop)

	first := <-notes
	if first.Address != "t1" || first.Body != "Needs you" {
		t.Fatalf("first: %+v", first)
	}
	second := <-notes
	if second.Address != "t2" || second.Body != "Needs you" {
		t.Fatalf("second (after reconnect, snapshot must be silent): %+v", second)
	}
	var req struct{ Method string }
	if json.Unmarshal([]byte(<-gotSub), &req); req.Method != "events.subscribe" {
		t.Fatalf("subscribe request: %+v", req)
	}
	select {
	case n := <-notes:
		t.Fatalf("unexpected extra notice: %+v", n)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatcherCoreAbsentThenAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late.sock")
	notes := make(chan Notice, 2)
	w := &Watcher{Socket: path, Tracker: NewTracker(time.Now), Notify: func(n Notice) { notes <- n }, MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}
	stop := make(chan struct{})
	defer close(stop)
	go w.Run(stop)
	time.Sleep(50 * time.Millisecond) // core not there yet

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, _ := l.Accept()
		r := bufio.NewReader(c)
		r.ReadString('\n')
		c.Write([]byte(snapshot(1, `{"address":"t1","state":"live","status":{"state":"working"}}`)))
		c.Write([]byte(statusEvent(2, "t1", "done")))
	}()
	select {
	case n := <-notes:
		if n.Body != "Finished its turn" {
			t.Fatalf("%+v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher never connected")
	}
}
