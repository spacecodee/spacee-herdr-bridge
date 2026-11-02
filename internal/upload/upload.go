// Package upload stores images sent from the phone on the host, so agents running in
// terminals can read them by file path.
package upload

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

// MaxBytes is the largest accepted image.
const MaxBytes = 15 << 20

// Retention is how long an uploaded file is kept.
const Retention = 7 * 24 * time.Hour

var extensions = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
	"image/gif":  "gif",
}

var namePattern = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{16}\.(png|jpg|webp|gif)$`)

// DefaultDir returns $XDG_CACHE_HOME/spacee-herdr/uploads, or ~/.cache/spacee-herdr/uploads.
func DefaultDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "spacee-herdr", "uploads")
}

// Handler serves POST /api/upload: the body is the raw image, the response is its path on
// the host. It needs the session and a same-origin request.
func Handler(a *auth.Auth, dir string) http.Handler {
	return a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !auth.SameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ext, ok := extensions[http.DetectContentType(body)]
		if !ok {
			http.Error(w, "unsupported image type", http.StatusUnsupportedMediaType)
			return
		}
		path, err := save(dir, ext, body, time.Now())
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"path": path})
	}))
}

// save writes data to a temp file in dir (0600) and renames it to its final name.
func save(dir, ext string, data []byte, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	final := filepath.Join(dir, now.Format("20060102-150405")+"-"+hex.EncodeToString(rnd[:])+"."+ext)
	tmp, err := os.CreateTemp(dir, ".upload-*.tmp") // CreateTemp uses mode 0600
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}

// Cleanup deletes regular files in dir named like uploads and older than maxAge. It never
// follows symlinks and returns how many files it removed.
func Cleanup(dir string, maxAge time.Duration, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !namePattern.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	return removed
}

// RunCleanup cleans dir now and then every hour, until stop is closed. It logs only a count.
func RunCleanup(dir string, stop <-chan struct{}, logf func(format string, args ...any)) {
	run := func() {
		if n := Cleanup(dir, Retention, time.Now()); n > 0 {
			logf("upload cleanup: removed %d old files", n)
		}
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			run()
		case <-stop:
			return
		}
	}
}
