package layout

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

const maxBody = 8 << 10

var idRe = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// Handler serves /api/layout/*: JSON bodies up to 8 KiB, session required, same origin.
func Handler(a *auth.Auth, s *Service) http.Handler {
	h := &handler{s: s}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/layout/space", h.createSpace)
	mux.HandleFunc("POST /api/layout/tab", h.createTab)
	mux.HandleFunc("POST /api/layout/split", h.split)
	mux.HandleFunc("PATCH /api/layout/space/{id}", h.renameSpace)
	mux.HandleFunc("PATCH /api/layout/tab/{id}", h.renameTab)
	mux.HandleFunc("POST /api/layout/move", h.move)
	mux.HandleFunc("DELETE /api/layout/space/{id}", h.del(s.DeleteSpace))
	mux.HandleFunc("DELETE /api/layout/tab/{id}", h.del(s.DeleteTab))
	mux.HandleFunc("DELETE /api/layout/pane/{id}", h.del(s.DeletePane))
	return a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auth.SameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

type handler struct{ s *Service }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func badID(w http.ResponseWriter, id string) bool {
	if !idRe.MatchString(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return true
	}
	return false
}

// fail maps a service error to a status. Messages never carry core internals.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		http.Error(w, "invalid input", http.StatusBadRequest)
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrConflict):
		http.Error(w, "conflict", http.StatusConflict)
	default:
		log.Printf("layout: core unavailable or refused")
		http.Error(w, "core error", http.StatusBadGateway)
	}
}

func reply(w http.ResponseWriter, v Created, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func done(w http.ResponseWriter, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) createSpace(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name    string   `json:"name"`
		Folder  string   `json:"folder"`
		Machine string   `json:"machine"`
		Argv    []string `json:"argv"`
	}
	if !decode(w, r, &b) {
		return
	}
	c, err := h.s.CreateSpace(b.Name, b.Folder, b.Machine, b.Argv)
	reply(w, c, err)
}

func (h *handler) createTab(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Space  string   `json:"space"`
		Name   string   `json:"name"`
		Folder string   `json:"folder"`
		Argv   []string `json:"argv"`
	}
	if !decode(w, r, &b) || badID(w, b.Space) {
		return
	}
	c, err := h.s.CreateTab(b.Space, b.Name, b.Folder, b.Argv)
	reply(w, c, err)
}

func (h *handler) split(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Tab       string   `json:"tab"`
		Pane      string   `json:"pane"`
		Direction string   `json:"direction"`
		Folder    string   `json:"folder"`
		Argv      []string `json:"argv"`
	}
	if !decode(w, r, &b) || badID(w, b.Tab) || (b.Pane != "" && badID(w, b.Pane)) {
		return
	}
	c, err := h.s.Split(b.Tab, b.Pane, b.Direction, b.Folder, b.Argv)
	reply(w, c, err)
}

func (h *handler) rename(w http.ResponseWriter, r *http.Request, fn func(id, name string) error) {
	var b struct {
		Name string `json:"name"`
	}
	id := r.PathValue("id")
	if badID(w, id) || !decode(w, r, &b) {
		return
	}
	done(w, fn(id, b.Name))
}

func (h *handler) renameSpace(w http.ResponseWriter, r *http.Request) {
	h.rename(w, r, h.s.RenameSpace)
}
func (h *handler) renameTab(w http.ResponseWriter, r *http.Request) { h.rename(w, r, h.s.RenameTab) }

func (h *handler) move(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Index int    `json:"index"`
	}
	if !decode(w, r, &b) || badID(w, b.ID) {
		return
	}
	done(w, h.s.Move(b.Kind, b.ID, b.Index))
}

// del serves a delete that only runs with ?confirmed=1: it ends running programs.
func (h *handler) del(fn func(id string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if badID(w, id) {
			return
		}
		if r.URL.Query().Get("confirmed") != "1" {
			http.Error(w, "confirmation required", http.StatusBadRequest)
			return
		}
		done(w, fn(id))
	}
}
