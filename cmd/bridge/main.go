// Command bridge connects the web client to herdr-core (hc).
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
	"github.com/spacecodee/spacee-herdr-bridge/internal/coreproxy"
	"github.com/spacecodee/spacee-herdr-bridge/internal/webui"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on (keep it on loopback or the tailnet)")
	core := flag.String("core", coreproxy.DefaultSocketPath(), "herdr-core Unix socket path")
	flag.Parse()

	tokenPath := auth.DefaultTokenPath()
	token, err := auth.LoadOrCreateToken(tokenPath)
	if err != nil {
		log.Fatalf("token: %v", err)
	}
	log.Printf("token file: %s", tokenPath)
	a := auth.New(token)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("/api/session", a.SessionHandler())
	mux.Handle("GET /ws", a.Require(coreproxy.Handler(*core)))
	mux.Handle("/", webui.Handler())

	log.Printf("bridge listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
