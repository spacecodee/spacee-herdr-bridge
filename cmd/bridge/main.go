// Command bridge connects the web client to herdr-core (hc).
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/usuario/spacee-herdr-app/spacee-herdr-bridge/internal/webui"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on (keep it on loopback or the tailnet)")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("/", webui.Handler())

	log.Printf("bridge listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
