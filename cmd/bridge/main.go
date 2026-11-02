// Command bridge connects the web client to herdr-core (hc).
package main

import (
	"flag"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
	"github.com/spacecodee/spacee-herdr-bridge/internal/coreproxy"
	"github.com/spacecodee/spacee-herdr-bridge/internal/push"
	"github.com/spacecodee/spacee-herdr-bridge/internal/upload"
	"github.com/spacecodee/spacee-herdr-bridge/internal/webui"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on (keep it on loopback or the tailnet)")
	core := flag.String("core", coreproxy.DefaultSocketPath(), "herdr-core Unix socket path")
	subject := flag.String("push-subject", "https://github.com/spacecodee/spacee-herdr-bridge", "VAPID subject for Web Push (https: or mailto: URL)")
	uploadDir := flag.String("upload-dir", upload.DefaultDir(), "directory for uploaded images (must be a path valid on the host: agents read the images from there)")
	flag.Parse()

	tokenPath := auth.DefaultTokenPath()
	token, err := auth.LoadOrCreateToken(tokenPath)
	if err != nil {
		log.Fatalf("token: %v", err)
	}
	log.Printf("token file: %s", tokenPath)
	a := auth.New(token)

	configDir := filepath.Dir(tokenPath)
	vapid, err := push.LoadOrCreateVAPID(filepath.Join(configDir, "vapid.json"))
	if err != nil {
		log.Fatalf("vapid: %v", err)
	}
	subs, err := push.OpenStore(filepath.Join(configDir, "push.json"))
	if err != nil {
		log.Fatalf("push subscriptions: %v", err)
	}
	sender := &push.Sender{Store: subs, VAPID: vapid, Subject: *subject}
	watcher := &push.Watcher{
		Socket:  *core,
		Tracker: push.NewTracker(time.Now),
		Notify:  func(n push.Notice) { go sender.Notify(n) },
	}
	go watcher.Run(nil)

	go upload.RunCleanup(*uploadDir, nil, log.Printf)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("/api/session", a.SessionHandler())
	mux.Handle("GET /ws", a.Require(coreproxy.Handler(*core)))
	mux.Handle("/api/push/", push.Handler(a, subs, vapid.Public, sender))
	mux.Handle("/api/upload", upload.Handler(a, *uploadDir))
	mux.Handle("/", webui.Handler())

	log.Printf("bridge listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
