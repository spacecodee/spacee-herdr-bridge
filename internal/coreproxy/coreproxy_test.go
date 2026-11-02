package coreproxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/usuario/spacee-herdr-app/spacee-herdr-bridge/internal/auth"
)

func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "core.sock")
}

// fakeCore listens on a Unix socket and hands accepted conns to the channel.
func fakeCore(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()
	p := sockPath(t)
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			ch <- c
		}
	}()
	return p, ch
}

func echo(ch <-chan net.Conn) {
	go func() {
		for c := range ch {
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
}

func dial(t *testing.T, srv *httptest.Server, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	opts := &websocket.DialOptions{}
	if origin != "" {
		opts.HTTPHeader = http.Header{"Origin": {origin}}
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", opts)
}

func TestBytesRoundTrip(t *testing.T) {
	p, ch := fakeCore(t)
	echo(ch)
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()

	c, _, err := dial(t, srv, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	small := []byte(`{"jsonrpc":"2.0","method":"hello"}` + "\n")
	big := bytes.Repeat([]byte{0, 1, 2, 255, 'x'}, 20000) // 100000 bytes > 32 KiB
	for _, want := range [][]byte{small, big} {
		if err := c.Write(ctx, websocket.MessageBinary, want); err != nil {
			t.Fatal(err)
		}
		var got []byte
		for len(got) < len(want) {
			typ, b, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if typ != websocket.MessageBinary {
				t.Fatalf("type = %v", typ)
			}
			got = append(got, b...)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("bytes differ (len got %d want %d)", len(got), len(want))
		}
	}
}

func TestCoreToBrowserLargeFrame(t *testing.T) {
	p, ch := fakeCore(t)
	payload := bytes.Repeat([]byte("abcdefgh"), 10000) // 80000 bytes
	go func() {
		c := <-ch
		c.Write(payload)
		time.Sleep(200 * time.Millisecond)
		c.Close()
	}()
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	c, _, err := dial(t, srv, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []byte
	for len(got) < len(payload) {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("after %d bytes: %v", len(got), err)
		}
		got = append(got, b...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload differs")
	}
}

func TestCrossOriginRejected(t *testing.T) {
	p, ch := fakeCore(t)
	echo(ch)
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	_, resp, err := dial(t, srv, "http://evil.example")
	if err == nil {
		t.Fatal("expected cross-origin dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("resp = %v", resp)
	}
}

func TestCoreUnavailable(t *testing.T) {
	srv := httptest.NewServer(Handler(sockPath(t))) // nothing listening
	defer srv.Close()
	c, _, err := dial(t, srv, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("close status = %v (%v)", websocket.CloseStatus(err), err)
	}
}

func TestWSCloseClosesCore(t *testing.T) {
	p, ch := fakeCore(t)
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	c, _, err := dial(t, srv, "")
	if err != nil {
		t.Fatal(err)
	}
	core := <-ch
	c.Close(websocket.StatusNormalClosure, "")
	core.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := core.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("core read err = %v, want EOF", err)
	}
}

func TestCoreCloseClosesWS(t *testing.T) {
	p, ch := fakeCore(t)
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	c, _, err := dial(t, srv, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	(<-ch).Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("expected WS to close, err = %v", err)
	}
}

func TestDefaultSocketPath(t *testing.T) {
	clear := func() {
		for _, k := range []string{"HC_SESSION", "HC_SOCKET", "XDG_RUNTIME_DIR", "XDG_STATE_HOME"} {
			t.Setenv(k, "")
		}
	}
	clear()
	t.Setenv("HC_SOCKET", "/s/x.sock")
	if got := DefaultSocketPath(); got != "/s/x.sock" {
		t.Fatalf("HC_SOCKET: %q", got)
	}
	clear()
	t.Setenv("XDG_RUNTIME_DIR", "/run/u")
	t.Setenv("HC_SESSION", "work")
	if got := DefaultSocketPath(); got != "/run/u/herdr-core/work.sock" {
		t.Fatalf("runtime dir: %q", got)
	}
	clear()
	t.Setenv("XDG_STATE_HOME", "/st")
	got := DefaultSocketPath()
	if _, err := os.Stat("/run/user"); err != nil {
		if got != "/st/herdr-core/default/default.sock" {
			t.Fatalf("state home: %q", got)
		}
	}
	clear()
	t.Setenv("HOME", "/h")
	if _, err := os.Stat("/run/user"); err != nil {
		if got := DefaultSocketPath(); got != "/h/.local/state/herdr-core/default/default.sock" {
			t.Fatalf("home fallback: %q", got)
		}
	}
}

func TestMissingCookieUnauthorized(t *testing.T) {
	p, ch := fakeCore(t)
	echo(ch)
	srv := httptest.NewServer(auth.New("tok").Require(Handler(p)))
	defer srv.Close()
	_, resp, err := dial(t, srv, "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err=%v resp=%v", err, resp)
	}
}
