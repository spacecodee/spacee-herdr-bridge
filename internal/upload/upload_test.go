package upload

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func post(h http.Handler, body []byte, contentType string, cookie bool, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/upload", bytes.NewReader(body))
	r.Host = "bridge.test"
	r.Header.Set("Content-Type", contentType)
	if cookie {
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: "tok"})
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func newHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "uploads")
	return Handler(auth.New("tok"), dir), dir
}

func TestUploadStoresImage(t *testing.T) {
	nameRe := regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{16}\.(png|jpg)$`)
	for _, c := range []struct {
		name string
		body []byte
		ext  string
	}{{"png", pngBytes(t), ".png"}, {"jpeg", jpegBytes(t), ".jpg"}} {
		t.Run(c.name, func(t *testing.T) {
			h, dir := newHandler(t)
			// The header lies: the type comes from the bytes.
			w := post(h, c.body, "application/octet-stream", true, "")
			if w.Code != 201 {
				t.Fatalf("code %d: %s", w.Code, w.Body.String())
			}
			var res struct{ Path string }
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
				t.Fatal(err)
			}
			if filepath.Dir(res.Path) != dir || !strings.HasSuffix(res.Path, c.ext) || !nameRe.MatchString(filepath.Base(res.Path)) {
				t.Fatalf("path %q", res.Path)
			}
			st, err := os.Stat(res.Path)
			if err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("stat %v %v", st, err)
			}
			got, _ := os.ReadFile(res.Path)
			if !bytes.Equal(got, c.body) {
				t.Fatal("content differs")
			}
			if ds, _ := os.Stat(dir); ds.Mode().Perm() != 0o700 {
				t.Fatalf("dir mode %v", ds.Mode())
			}
			if es, _ := os.ReadDir(dir); len(es) != 1 {
				t.Fatalf("leftover files: %d", len(es))
			}
		})
	}
}

func TestUploadRejectsNonImage(t *testing.T) {
	h, dir := newHandler(t)
	for _, body := range []string{"hello", "<html><body>x</body></html>", "<svg xmlns='http://www.w3.org/2000/svg'/>"} {
		if w := post(h, []byte(body), "image/png", true, ""); w.Code != 415 {
			t.Errorf("%q = %d, want 415", body, w.Code)
		}
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("files written: %d", len(es))
	}
}

func TestUploadTooLarge(t *testing.T) {
	h, dir := newHandler(t)
	big := append(pngBytes(t), make([]byte, MaxBytes)...)
	if w := post(h, big, "image/png", true, ""); w.Code != 413 {
		t.Fatalf("code %d, want 413", w.Code)
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("files written: %d", len(es))
	}
}

func TestUploadAuthAndOrigin(t *testing.T) {
	h, _ := newHandler(t)
	if w := post(h, pngBytes(t), "image/png", false, ""); w.Code != 401 {
		t.Errorf("no cookie = %d, want 401", w.Code)
	}
	if w := post(h, pngBytes(t), "image/png", true, "https://evil.example"); w.Code != 403 {
		t.Errorf("foreign origin = %d, want 403", w.Code)
	}
	if w := post(h, pngBytes(t), "image/png", true, "http://bridge.test"); w.Code != 201 {
		t.Errorf("same origin = %d, want 201", w.Code)
	}
}

func TestCleanup(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	mk := func(name string, mod time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mod, mod)
	}
	oldName := "20260101-000000-0123456789abcdef.png"
	newName := "20261101-000000-0123456789abcdef.jpg"
	mk(oldName, old)
	mk(newName, now.Add(-time.Hour))
	mk("notes.txt", old)                            // wrong pattern
	mk("20260101-000000-0123456789abcdef.exe", old) // wrong ext
	outside := filepath.Join(t.TempDir(), "target")
	os.WriteFile(outside, []byte("x"), 0o600)
	os.Chtimes(outside, old, old)
	link := filepath.Join(dir, "20260101-000000-fedcba9876543210.png")
	os.Symlink(outside, link)
	os.Mkdir(filepath.Join(dir, "20260101-000000-aaaaaaaaaaaaaaaa.png"), 0o700) // directory

	n := Cleanup(dir, 7*24*time.Hour, now)
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, err := os.Lstat(filepath.Join(dir, oldName)); !os.IsNotExist(err) {
		t.Error("old file kept")
	}
	for _, keep := range []string{newName, "notes.txt", "20260101-000000-0123456789abcdef.exe", filepath.Base(link), "20260101-000000-aaaaaaaaaaaaaaaa.png"} {
		if _, err := os.Lstat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s removed", keep)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("symlink target removed")
	}
}

func TestCleanupMissingDir(t *testing.T) {
	if n := Cleanup(filepath.Join(t.TempDir(), "none"), time.Hour, time.Now()); n != 0 {
		t.Fatal(n)
	}
}

func TestDefaultDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/x/cache")
	if got := DefaultDir(); got != "/x/cache/spacee-herdr/uploads" {
		t.Fatal(got)
	}
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/h")
	if got := DefaultDir(); got != "/h/.cache/spacee-herdr/uploads" {
		t.Fatal(got)
	}
}
