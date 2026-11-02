package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		"index.html":           f("<html>INDEX</html>"),
		"manifest.webmanifest": f(`{"name":"x"}`),
		"ngsw.json":            f(`{}`),
		"ngsw-worker.js":       f("//w"),
		"safety-worker.js":     f("//s"),
		"worker-basic.min.js":  f("//b"),
		"main-JSTJFTCT.js":     f("//m"),
		"chunk-89Y3lOnY.js":    f("//c"),
		"styles-RNL3ZZVZ.css":  f("body{}"),
		"favicon.ico":          f("ico"),
		"icons/icon-192.png":   f("png"),
		"fonts/font.woff2":     f("font"),
	}
}

func TestHandler(t *testing.T) {
	const (
		noCache   = "no-cache"
		immutable = "public, max-age=31536000, immutable"
		daily     = "public, max-age=86400"
	)
	tests := []struct {
		path        string
		status      int
		contentType string // prefix; "" = not checked
		cache       string // "" = not checked
		index       bool
	}{
		{"/", 200, "text/html", noCache, true},
		{"/t/t7", 200, "text/html", noCache, true},
		{"/t/rohan/t7", 200, "text/html", noCache, true},
		{"/settings", 200, "text/html", noCache, true},
		{"/login", 200, "text/html", noCache, true},
		{"/manifest.webmanifest", 200, "application/manifest+json", noCache, false},
		{"/ngsw.json", 200, "application/json", noCache, false},
		{"/ngsw-worker.js", 200, "", noCache, false},
		{"/safety-worker.js", 200, "", noCache, false},
		{"/worker-basic.min.js", 200, "", noCache, false},
		{"/main-JSTJFTCT.js", 200, "", immutable, false},
		{"/chunk-89Y3lOnY.js", 200, "", immutable, false},
		{"/styles-RNL3ZZVZ.css", 200, "text/css", immutable, false},
		{"/favicon.ico", 200, "", daily, false},
		{"/icons/icon-192.png", 200, "image/png", daily, false},
		{"/fonts/font.woff2", 200, "font/woff2", daily, false},
		{"/nope.js", 404, "", "", false},
		{"/main-ABCDEFGH.js", 404, "", "", false},
		{"/icons/x.png", 404, "", "", false},
		{"/t/missing.json", 404, "", "", false},
	}
	h := handler(testFS())
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			res := rec.Result()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.status)
			}
			if tc.contentType != "" && !strings.HasPrefix(res.Header.Get("Content-Type"), tc.contentType) {
				t.Errorf("Content-Type = %q, want prefix %q", res.Header.Get("Content-Type"), tc.contentType)
			}
			if tc.cache != "" && res.Header.Get("Cache-Control") != tc.cache {
				t.Errorf("Cache-Control = %q, want %q", res.Header.Get("Cache-Control"), tc.cache)
			}
			if tc.index && !strings.Contains(string(body), "INDEX") {
				t.Errorf("body is not index.html: %q", body)
			}
			if !tc.index && strings.Contains(string(body), "INDEX") {
				t.Errorf("unexpected index.html body")
			}
		})
	}
}
