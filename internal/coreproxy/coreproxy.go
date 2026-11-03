// Package coreproxy pipes bytes between a WebSocket and herdr-core's Unix socket.
package coreproxy

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

const dialTimeout = 5 * time.Second

// DefaultSocketPath follows herdr-core's socket location rules.
func DefaultSocketPath() string {
	session := os.Getenv("HC_SESSION")
	if session == "" {
		session = "default"
	}
	if s := os.Getenv("HC_SOCKET"); s != "" {
		return s
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "herdr-core", session+".sock")
	}
	runUser := "/run/user/" + strconv.Itoa(os.Getuid())
	if fi, err := os.Stat(runUser); err == nil && fi.IsDir() {
		return filepath.Join(runUser, "herdr-core", session+".sock")
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "herdr-core", session, session+".sock")
}

// Handler upgrades to a WebSocket and relays bytes verbatim to the core socket.
// Wrap it in auth.Require; Origin is checked by websocket.Accept.
func Handler(socketPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return // Accept already replied (e.g. 403 on foreign Origin)
		}
		c.SetReadLimit(1 << 20)

		d := net.Dialer{Timeout: dialTimeout}
		sock, err := d.DialContext(r.Context(), "unix", socketPath)
		if err != nil {
			log.Printf("core unavailable")
			c.Close(websocket.StatusInternalError, "core unavailable")
			return
		}
		log.Printf("client connected")
		defer log.Printf("client disconnected")

		// Derived from the request so that signing the device out (auth.Require
		// cancels the request context) tears the relay down.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		ws := websocket.NetConn(ctx, c, websocket.MessageBinary)
		go func() {
			<-ctx.Done()
			sock.Close()
			ws.Close()
		}()

		done := make(chan struct{}, 2)
		go func() { io.Copy(sock, ws); done <- struct{}{} }()
		go func() { io.Copy(ws, sock); done <- struct{}{} }()
		<-done
		// One direction ended: tear down both sides, which unblocks the other copy.
		sock.Close()
		ws.Close()
		<-done
	})
}
