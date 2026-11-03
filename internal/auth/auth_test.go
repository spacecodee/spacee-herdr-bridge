package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "token")
	tok, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 43 {
		t.Fatalf("token too short: %d", len(tok))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", di.Mode().Perm())
	}
	again, err := LoadOrCreateToken(path)
	if err != nil || again != tok {
		t.Fatalf("reload = %q, %v; want %q", again, err, tok)
	}
}

func TestLoadTokenTrimsAndRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t")
	os.WriteFile(p, []byte("  abc\n"), 0o600)
	if tok, err := LoadOrCreateToken(p); err != nil || tok != "abc" {
		t.Fatalf("got %q, %v", tok, err)
	}
	os.WriteFile(p, []byte("\n"), 0o600)
	if _, err := LoadOrCreateToken(p); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestDefaultTokenPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got := DefaultTokenPath(); got != "/x/cfg/spacee-herdr/token" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/h")
	if got := DefaultTokenPath(); got != "/h/.config/spacee-herdr/token" {
		t.Fatalf("got %q", got)
	}
}

func post(h http.Handler, body, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://bridge.test/api/session", strings.NewReader(body))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLogin(t *testing.T) {
	a := New("secret")
	h := a.SessionHandler()

	if w := post(h, `{"token":"nope"}`, "http://bridge.test"); w.Code != 401 {
		t.Fatalf("wrong token: %d", w.Code)
	}
	if w := post(h, `{}`, "http://bridge.test"); w.Code != 401 {
		t.Fatalf("missing token: %d", w.Code)
	}
	if w := post(h, `garbage`, "http://bridge.test"); w.Code != 401 {
		t.Fatalf("bad json: %d", w.Code)
	}
	if w := post(h, `{"token":"secret"}`, "http://evil.test"); w.Code != 403 {
		t.Fatalf("bad origin: %d", w.Code)
	}
	if w := post(h, `{"token":"`+strings.Repeat("a", 5000)+`"}`, "http://bridge.test"); w.Code != 401 {
		t.Fatalf("oversize body: %d", w.Code)
	}

	w := post(h, `{"token":"secret"}`, "http://bridge.test")
	if w.Code != 204 {
		t.Fatalf("right token: %d", w.Code)
	}
	c := w.Result().Cookies()
	if len(c) != 1 {
		t.Fatalf("cookies = %v", c)
	}
	ck := c[0]
	if ck.Name != "spacee_session" || ck.Value == "secret" || ck.Value == "" || ck.Path != "/" ||
		ck.MaxAge != 31536000 || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", ck)
	}
}

func TestSessionCheckAndRequire(t *testing.T) {
	a := New("secret")
	h := a.SessionHandler()

	get := func(h http.Handler, cookie string) int {
		r := httptest.NewRequest("GET", "http://bridge.test/api/session", nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "spacee_session", Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := get(h, ""); c != 401 {
		t.Fatalf("no cookie: %d", c)
	}
	if c := get(h, "bad"); c != 401 {
		t.Fatalf("bad cookie: %d", c)
	}
	if c := get(h, "secret"); c != 204 {
		t.Fatalf("good cookie: %d", c)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	req := a.Require(next)
	if c := get(req, ""); c != 401 {
		t.Fatalf("Require no cookie: %d", c)
	}
	if c := get(req, "secret"); c != 200 {
		t.Fatalf("Require good cookie: %d", c)
	}
}
