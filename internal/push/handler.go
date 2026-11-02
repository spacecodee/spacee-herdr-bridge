package push

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

const maxBody = 4 << 10

// Notifier delivers a notification to every subscription.
type Notifier interface{ Notify(Notice) }

// Handler serves /api/push/*. Every route needs the session; mutating routes also check Origin.
func Handler(a *auth.Auth, st *Store, publicKey string, n Notifier) http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, a.Require(h))
	}
	mutating := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !auth.SameOrigin(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}

	route("GET /api/push/key", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"publicKey": publicKey})
	})
	route("POST /api/push/subscription", mutating(func(w http.ResponseWriter, r *http.Request) {
		var sub webpush.Subscription
		if !decode(w, r, &sub) {
			return
		}
		if u, err := url.Parse(sub.Endpoint); err != nil || u.Scheme != "https" || u.Host == "" ||
			sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
			http.Error(w, "invalid subscription", http.StatusBadRequest)
			return
		}
		if err := st.Upsert(sub); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	route("DELETE /api/push/subscription", mutating(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Endpoint string `json:"endpoint"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.Endpoint == "" {
			http.Error(w, "endpoint required", http.StatusBadRequest)
			return
		}
		if err := st.Delete(req.Endpoint); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	route("POST /api/push/test", mutating(func(w http.ResponseWriter, _ *http.Request) {
		n.Notify(Notice{Address: "test", Title: "spacee-herdr", Body: "Test notification", URL: "/"})
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

// decode reads a JSON body of at most maxBody bytes into v, answering 400 on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil || json.Unmarshal(b, v) != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return false
	}
	return true
}
