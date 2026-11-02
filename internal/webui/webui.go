// Package webui serves the embedded Angular build.
package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
)

//go:embed all:dist
var dist embed.FS

var hashed = regexp.MustCompile(`-[A-Za-z0-9]{8,}\.(js|css)$`)

// revalidated files must never be served stale: the service worker and the
// manifest decide when the app updates.
var revalidated = map[string]bool{
	"index.html":           true,
	"ngsw.json":            true,
	"ngsw-worker.js":       true,
	"safety-worker.js":     true,
	"worker-basic.min.js":  true,
	"manifest.webmanifest": true,
}

func init() {
	// Not in the stdlib's built-in table on every platform.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Handler serves the embedded PWA.
func Handler() http.Handler {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return handler(root)
}

// handler serves root with SPA routing: a missing path without a file
// extension is an app route and gets index.html; a missing file is a 404.
func handler(root fs.FS) http.Handler {
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if info, err := fs.Stat(root, name); err != nil || info.IsDir() {
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			name = "index.html"
			r.URL.Path = "/"
		}
		w.Header().Set("Cache-Control", cacheControl(name))
		files.ServeHTTP(w, r)
	})
}

func cacheControl(name string) string {
	switch {
	case revalidated[name]:
		return "no-cache"
	case hashed.MatchString(name):
		return "public, max-age=31536000, immutable"
	default:
		return "public, max-age=86400"
	}
}
