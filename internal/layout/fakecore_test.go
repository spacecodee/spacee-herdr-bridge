package layout

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// fakeCore is a herdr-core on a temp Unix socket: it serves doc.get/doc.set with
// optimistic versions, terminal.run/kill and machine.list.
type fakeCore struct {
	mu        sync.Mutex
	t         *testing.T
	Socket    string
	doc       json.RawMessage
	version   uint64
	nextTerm  int
	Runs      []map[string]any
	Killed    []string
	Conflicts int  // doc.set calls that answer "conflict" before one succeeds
	FailSet   bool // doc.set always answers "internal"
	FailRun   bool
	Sets      int
	Machines  []string
}

func newFakeCore(t *testing.T, doc string) *fakeCore {
	t.Helper()
	f := &fakeCore{t: t, doc: json.RawMessage(doc), version: 7, nextTerm: 20, Machines: []string{"rohan"}}
	f.Socket = filepath.Join(t.TempDir(), "core.sock")
	l, err := net.Listen("unix", f.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeCore) Doc() Doc {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := decodeDoc(f.doc)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *fakeCore) serve(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(sc.Bytes(), &req)
		result, rpcErr := f.handle(req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != "" {
			resp["error"] = map[string]any{"code": -32000, "message": rpcErr, "data": map[string]any{"kind": rpcErr}}
		} else {
			resp["result"] = result
		}
		b, _ := json.Marshal(resp)
		c.Write(append(b, '\n'))
	}
}

func (f *fakeCore) handle(method string, raw json.RawMessage) (any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "doc.get":
		return map[string]any{"value": f.doc, "version": f.version}, ""
	case "doc.set":
		var p struct {
			Name    string          `json:"name"`
			Value   json.RawMessage `json:"value"`
			Version uint64          `json:"version"`
		}
		json.Unmarshal(raw, &p)
		f.Sets++
		if f.FailSet {
			return nil, "internal"
		}
		if f.Conflicts > 0 {
			f.Conflicts--
			f.version++ // somebody else wrote in between
			return nil, "conflict"
		}
		if p.Name != "herdr-app" || p.Version != f.version {
			return nil, "conflict"
		}
		f.doc = p.Value
		f.version++
		return map[string]any{"version": f.version}, ""
	case "terminal.run":
		if f.FailRun {
			return nil, "spawn_failed"
		}
		var p map[string]any
		json.Unmarshal(raw, &p)
		f.Runs = append(f.Runs, p)
		f.nextTerm++
		return map[string]any{"address": "t" + strconv.Itoa(f.nextTerm)}, ""
	case "terminal.kill":
		var p struct {
			Terminal string `json:"terminal"`
		}
		json.Unmarshal(raw, &p)
		f.Killed = append(f.Killed, p.Terminal)
		return map[string]any{"address": p.Terminal}, ""
	case "machine.list":
		ms := []map[string]any{}
		for _, m := range f.Machines {
			ms = append(ms, map[string]any{"name": m})
		}
		return map[string]any{"machines": ms}, ""
	}
	return nil, "invalid_params"
}
