package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DevicesHandler serves /api/devices: list, pairing code, rename, sign out.
// Every route needs a session; mutating routes also check Origin.
func (a *Auth) DevicesHandler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, mutating bool, h http.HandlerFunc) {
		mux.Handle(pattern, a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mutating && !SameOrigin(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h(w, r)
		})))
	}

	route("GET /api/devices", false, func(w http.ResponseWriter, r *http.Request) {
		cur := DeviceID(r.Context())
		type view struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Platform string `json:"platform"`
			Created  string `json:"created"`
			LastSeen string `json:"lastSeen"`
			Current  bool   `json:"current"`
		}
		out := []view{}
		for _, d := range sortedByLastSeen(a.devices.List()) {
			out = append(out, view{d.ID, d.Name, d.Platform,
				d.Created.UTC().Format("2006-01-02T15:04:05Z07:00"), d.LastSeen.UTC().Format("2006-01-02T15:04:05Z07:00"), d.ID == cur})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	route("POST /api/devices/pair", true, func(w http.ResponseWriter, _ *http.Request) {
		code, exp, err := a.pair.issue(a.now())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"code": code, "expires": exp.UTC().Format("2006-01-02T15:04:05Z07:00")})
	})

	route("PATCH /api/devices/{id}", true, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLoginBody))
		var req struct {
			Name string `json:"name"`
		}
		name := ""
		if err == nil && json.Unmarshal(b, &req) == nil {
			name = strings.TrimSpace(req.Name)
		}
		if n := utf8.RuneCountInString(name); n < 1 || n > 40 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}
		ok, err := a.devices.rename(r.PathValue("id"), name)
		switch {
		case err != nil:
			http.Error(w, "storage error", http.StatusInternalServerError)
		case !ok:
			http.Error(w, "not found", http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	route("DELETE /api/devices/{id}", true, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !a.Revoke(id) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if id == DeviceID(r.Context()) {
			http.SetCookie(w, a.cookie("", -1))
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
