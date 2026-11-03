package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const master = "master-token-value"

const iphoneUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Safari/604.1"

type env struct {
	a   *Auth
	mux http.Handler
	now time.Time
	dir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), dir: t.TempDir()}
	a, err := Open(master, filepath.Join(e.dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return e.now }
	e.a = a
	m := http.NewServeMux()
	m.Handle("/api/session", a.SessionHandler())
	m.Handle("/api/devices", a.DevicesHandler())
	m.Handle("/api/devices/", a.DevicesHandler())
	m.Handle("GET /protected", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(DeviceID(r.Context())))
	})))
	e.mux = m
	return e
}

func (e *env) do(method, path, body string, cookie *http.Cookie, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "bridge.test"
	r.Header.Set("User-Agent", iphoneUA)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in response (status %d)", CookieName, w.Code)
	return nil
}

func (e *env) login(t *testing.T) *http.Cookie {
	t.Helper()
	w := e.do("POST", "/api/session", `{"token":"`+master+`"}`, nil)
	if w.Code != 204 {
		t.Fatalf("login = %d", w.Code)
	}
	return sessionCookie(t, w)
}

func (e *env) pair(t *testing.T, c *http.Cookie) string {
	t.Helper()
	w := e.do("POST", "/api/devices/pair", "", c)
	if w.Code != 200 {
		t.Fatalf("pair = %d %s", w.Code, w.Body)
	}
	var out struct{ Code, Expires string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Code) != 6 || strings.Trim(out.Code, pairAlphabet) != "" {
		t.Fatalf("bad code %q", out.Code)
	}
	if _, err := time.Parse(time.RFC3339, out.Expires); err != nil {
		t.Fatalf("expires %q: %v", out.Expires, err)
	}
	return out.Code
}

func TestMasterLoginCreatesDevice(t *testing.T) {
	e := newEnv(t)
	w := e.do("POST", "/api/session", `{"token":"`+master+`"}`, nil)
	if w.Code != 204 {
		t.Fatalf("code = %d", w.Code)
	}
	c := sessionCookie(t, w)
	if c.Value == master || strings.Contains(c.Value, master) {
		t.Fatal("cookie holds the master token")
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 31536000 {
		t.Fatalf("cookie flags: %+v", c)
	}
	if w := e.do("GET", "/api/session", "", c); w.Code != 204 {
		t.Fatalf("session check = %d", w.Code)
	}
	w = e.do("GET", "/protected", "", c)
	id, _, _ := strings.Cut(c.Value, ".")
	if w.Code != 200 || w.Body.String() != id {
		t.Fatalf("protected = %d %q, want device id %q", w.Code, w.Body, id)
	}
}

func TestLoginRejections(t *testing.T) {
	e := newEnv(t)
	bad := e.do("POST", "/api/session", `{"token":"nope"}`, nil)
	badCode := e.do("POST", "/api/session", `{"code":"ZZZZZZ"}`, nil)
	if bad.Code != 401 || badCode.Code != 401 || bad.Body.String() != badCode.Body.String() {
		t.Fatalf("%d %q vs %d %q", bad.Code, bad.Body, badCode.Code, badCode.Body)
	}
	if w := e.do("POST", "/api/session", `{}`, nil); w.Code != 401 {
		t.Fatalf("empty = %d", w.Code)
	}
	if w := e.do("POST", "/api/session", `{"token":"`+master+`"}`, nil, "Origin", "https://evil.test"); w.Code != 403 {
		t.Fatalf("foreign origin = %d", w.Code)
	}
	big := `{"token":"` + strings.Repeat("a", 5000) + `"}`
	if w := e.do("POST", "/api/session", big, nil); w.Code != 401 {
		t.Fatalf("oversized = %d", w.Code)
	}
}

func TestPairingCodeLoginIsSingleUse(t *testing.T) {
	e := newEnv(t)
	c1 := e.login(t)
	code := e.pair(t, c1)
	spaced := strings.ToLower(code[:3] + " " + code[3:])
	w := e.do("POST", "/api/session", `{"code":"`+spaced+`"}`, nil)
	if w.Code != 204 {
		t.Fatalf("code login = %d", w.Code)
	}
	c2 := sessionCookie(t, w)
	if c2.Value == c1.Value {
		t.Fatal("second device got the first device's cookie")
	}
	if w := e.do("POST", "/api/session", `{"code":"`+code+`"}`, nil); w.Code != 401 {
		t.Fatalf("reused code = %d, want 401", w.Code)
	}
}

func TestPairingRequiresSessionAndOrigin(t *testing.T) {
	e := newEnv(t)
	if w := e.do("POST", "/api/devices/pair", "", nil); w.Code != 401 {
		t.Fatalf("no cookie = %d", w.Code)
	}
	c := e.login(t)
	if w := e.do("POST", "/api/devices/pair", "", c, "Origin", "https://evil.test"); w.Code != 403 {
		t.Fatalf("foreign origin = %d", w.Code)
	}
}

func TestExpiredCode(t *testing.T) {
	e := newEnv(t)
	code := e.pair(t, e.login(t))
	e.now = e.now.Add(5*time.Minute + time.Second)
	if w := e.do("POST", "/api/session", `{"code":"`+code+`"}`, nil); w.Code != 401 {
		t.Fatalf("expired = %d", w.Code)
	}
}

func TestLiveCodeCapDropsOldest(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	var codes []string
	for i := 0; i < 6; i++ {
		codes = append(codes, e.pair(t, c))
		e.now = e.now.Add(time.Second)
	}
	if w := e.do("POST", "/api/session", `{"code":"`+codes[0]+`"}`, nil); w.Code != 401 {
		t.Fatalf("oldest code still valid: %d", w.Code)
	}
	if w := e.do("POST", "/api/session", `{"code":"`+codes[5]+`"}`, nil); w.Code != 204 {
		t.Fatalf("newest code = %d", w.Code)
	}
}

func TestBruteForceLocksCodeLogins(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	code := e.pair(t, c)
	for i := 0; i < 10; i++ {
		if w := e.do("POST", "/api/session", `{"code":"AAAAAA"}`, nil); w.Code != 401 {
			t.Fatalf("attempt %d = %d", i, w.Code)
		}
	}
	if w := e.do("POST", "/api/session", `{"code":"`+code+`"}`, nil); w.Code != 429 {
		t.Fatalf("locked = %d, want 429", w.Code)
	}
	if w := e.do("POST", "/api/session", `{"token":"`+master+`"}`, nil); w.Code != 204 {
		t.Fatalf("master login while locked = %d", w.Code)
	}
	e.now = e.now.Add(10*time.Minute + time.Second)
	// live codes were invalidated by the lockout
	if w := e.do("POST", "/api/session", `{"code":"`+code+`"}`, nil); w.Code != 401 {
		t.Fatalf("old code after lockout = %d, want 401", w.Code)
	}
	fresh := e.pair(t, c)
	if w := e.do("POST", "/api/session", `{"code":"`+fresh+`"}`, nil); w.Code != 204 {
		t.Fatalf("fresh code after lock expiry = %d", w.Code)
	}
}

func TestLegacyCookieUpgrades(t *testing.T) {
	e := newEnv(t)
	legacy := &http.Cookie{Name: CookieName, Value: master}
	w := e.do("GET", "/protected", "", legacy)
	if w.Code != 200 {
		t.Fatalf("legacy = %d", w.Code)
	}
	c := sessionCookie(t, w)
	if c.Value == master || w.Body.Len() == 0 {
		t.Fatalf("not upgraded: cookie %q body %q", c.Value, w.Body)
	}
	if w := e.do("GET", "/protected", "", c); w.Code != 200 {
		t.Fatalf("upgraded cookie = %d", w.Code)
	}
	// a burst of requests still carrying the old cookie shares one device
	w2 := e.do("GET", "/api/session", "", legacy)
	if w2.Code != 204 {
		t.Fatalf("legacy session = %d", w2.Code)
	}
	if n := len(e.a.devices.List()); n != 1 {
		t.Fatalf("devices after legacy burst = %d, want 1", n)
	}
}

func TestRevokedDevice(t *testing.T) {
	e := newEnv(t)
	c1 := e.login(t)
	c2 := e.login(t)
	id2, _, _ := strings.Cut(c2.Value, ".")
	var revoked []string
	e.a.OnRevoke(func(id string) { revoked = append(revoked, id) })
	if w := e.do("DELETE", "/api/devices/"+id2, "", c1); w.Code != 204 {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := e.do("GET", "/protected", "", c2); w.Code != 401 {
		t.Fatalf("revoked device = %d", w.Code)
	}
	if w := e.do("GET", "/api/session", "", c2); w.Code != 401 {
		t.Fatalf("revoked session = %d", w.Code)
	}
	if len(revoked) != 1 || revoked[0] != id2 {
		t.Fatalf("OnRevoke calls: %v", revoked)
	}
	if w := e.do("GET", "/protected", "", c1); w.Code != 200 {
		t.Fatalf("other device = %d", w.Code)
	}
	if w := e.do("DELETE", "/api/devices/"+id2, "", c1); w.Code != 404 {
		t.Fatalf("delete again = %d", w.Code)
	}
}

func TestRevocationCancelsOpenRequests(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	id, _, _ := strings.Cut(c.Value, ".")
	started := make(chan struct{})
	closed := make(chan struct{})
	h := e.a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(closed)
	}))
	r := httptest.NewRequest("GET", "/stream", nil)
	r.AddCookie(c)
	go h.ServeHTTP(httptest.NewRecorder(), r)
	<-started
	e.a.Revoke(id)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stream context not cancelled within 1s")
	}
}

func TestListMarksCurrentAndOrdersByLastSeen(t *testing.T) {
	e := newEnv(t)
	c1 := e.login(t)
	e.now = e.now.Add(time.Minute)
	c2 := e.login(t)
	e.now = e.now.Add(time.Minute)
	e.do("GET", "/protected", "", c1) // c1 seen last
	w := e.do("GET", "/api/devices", "", c1)
	var list []struct {
		ID, Name, Platform string
		Created, LastSeen  string
		Current            bool
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list) != 2 {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	id1, _, _ := strings.Cut(c1.Value, ".")
	id2, _, _ := strings.Cut(c2.Value, ".")
	if list[0].ID != id1 || !list[0].Current || list[1].ID != id2 || list[1].Current {
		t.Fatalf("order/current wrong: %+v", list)
	}
	if list[0].Platform != "ios" || list[0].Name == "" || list[0].Created == "" || list[0].LastSeen == "" {
		t.Fatalf("fields: %+v", list[0])
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatal("list leaks secret fields")
	}
}

func TestRenameValidation(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	id, _, _ := strings.Cut(c.Value, ".")
	p := "/api/devices/" + id
	if w := e.do("PATCH", p, `{"name":"  Pixel 9  "}`, c); w.Code != 204 {
		t.Fatalf("rename = %d", w.Code)
	}
	if got := e.a.devices.List()[0].Name; got != "Pixel 9" {
		t.Fatalf("name = %q", got)
	}
	for _, bad := range []string{`{"name":""}`, `{"name":"   "}`, `{"name":"` + strings.Repeat("x", 41) + `"}`, `{"name":"a\u0007b"}`, `{`} {
		if w := e.do("PATCH", p, bad, c); w.Code != 400 {
			t.Errorf("%s = %d, want 400", bad, w.Code)
		}
	}
	if w := e.do("PATCH", p, `{"name":"ok"}`, c, "Origin", "https://evil.test"); w.Code != 403 {
		t.Errorf("foreign origin = %d", w.Code)
	}
	if w := e.do("PATCH", "/api/devices/unknown", `{"name":"ok"}`, c); w.Code != 404 {
		t.Errorf("unknown = %d", w.Code)
	}
	if w := e.do("PATCH", p, `{"name":"ok"}`, nil); w.Code != 401 {
		t.Errorf("no cookie = %d", w.Code)
	}
}

func TestDeleteSessionSignsOutCurrentDevice(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	w := e.do("DELETE", "/api/session", "", c)
	if w.Code != 204 {
		t.Fatalf("sign out = %d", w.Code)
	}
	if cleared := sessionCookie(t, w); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Fatalf("cookie not cleared: %+v", cleared)
	}
	if w := e.do("GET", "/protected", "", c); w.Code != 401 {
		t.Fatalf("after sign out = %d", w.Code)
	}
	if w := e.do("DELETE", "/api/session", "", c, "Origin", "https://evil.test"); w.Code != 403 {
		t.Fatalf("foreign origin = %d", w.Code)
	}
}

func TestDeleteCurrentDeviceViaDevicesAPI(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	id, _, _ := strings.Cut(c.Value, ".")
	if w := e.do("DELETE", "/api/devices/"+id, "", c); w.Code != 204 {
		t.Fatalf("delete current = %d", w.Code)
	}
	if w := e.do("GET", "/protected", "", c); w.Code != 401 {
		t.Fatalf("after delete = %d", w.Code)
	}
}

func TestForgedAndMalformedCookies(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	id, secret, _ := strings.Cut(c.Value, ".")
	for _, v := range []string{"", ".", id, id + ".", id + ".wrong", "unknown." + secret, secret, "a.b.c"} {
		if w := e.do("GET", "/protected", "", &http.Cookie{Name: CookieName, Value: v}); w.Code != 401 {
			t.Errorf("cookie %q = %d, want 401", v, w.Code)
		}
	}
}

func TestPlatformFromUserAgent(t *testing.T) {
	for ua, want := range map[string]string{
		iphoneUA: "ios",
		"Mozilla/5.0 (iPad; CPU OS 18_0 like Mac OS X)":                   "ipad",
		"Mozilla/5.0 (Linux; Android 15; Pixel 9) Chrome/130 Mobile":      "android",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Safari/605.1.15": "mac",
		"Mozilla/5.0 (X11; Linux x86_64) Firefox/130.0":                   "other",
		"": "other",
	} {
		if got := platformFromUA(ua); got != want {
			t.Errorf("%q = %q, want %q", ua, got, want)
		}
	}
}

func TestStoreFileModeAtomicAndReload(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	path := filepath.Join(e.dir, "devices.json")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	if ents, _ := filepath.Glob(path + ".*.tmp"); len(ents) != 0 {
		t.Fatalf("temp files left: %v", ents)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(strings.SplitN(c.Value, ".", 2)[1])) {
		t.Fatal("devices.json stores the raw secret")
	}
	a2, err := Open(master, path)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/x", nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	a2.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("reloaded store rejects cookie: %d", w.Code)
	}
}

func TestStoreCorruptFileQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	a, err := Open(master, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.devices.List()) != 0 {
		t.Fatal("expected no devices")
	}
	m, _ := filepath.Glob(path + ".corrupt-*")
	if len(m) != 1 {
		t.Fatalf("quarantine files: %v", m)
	}
}

func TestStoreLimitEvictsLeastRecentlySeen(t *testing.T) {
	e := newEnv(t)
	var cookies []*http.Cookie
	var revoked []string
	e.a.OnRevoke(func(id string) { revoked = append(revoked, id) })
	for i := 0; i < MaxDevices; i++ {
		cookies = append(cookies, e.login(t))
		e.now = e.now.Add(time.Minute)
	}
	e.do("GET", "/protected", "", cookies[0]) // device 0 is now the freshest; device 1 the stalest
	newest := e.login(t)
	if n := len(e.a.devices.List()); n != MaxDevices {
		t.Fatalf("devices = %d, want %d", n, MaxDevices)
	}
	id1, _, _ := strings.Cut(cookies[1].Value, ".")
	if w := e.do("GET", "/protected", "", cookies[1]); w.Code != 401 {
		t.Fatalf("evicted device = %d", w.Code)
	}
	if w := e.do("GET", "/protected", "", cookies[0]); w.Code != 200 {
		t.Fatalf("refreshed device = %d", w.Code)
	}
	if w := e.do("GET", "/protected", "", newest); w.Code != 200 {
		t.Fatalf("new device = %d", w.Code)
	}
	if len(revoked) != 1 || revoked[0] != id1 {
		t.Fatalf("evicted device not reported to OnRevoke: %v", revoked)
	}
}

func TestLastSeenPersistedAtMostEveryFiveMinutes(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	path := filepath.Join(e.dir, "devices.json")
	read := func() time.Time {
		var ds []struct {
			LastSeen time.Time `json:"lastSeen"`
		}
		raw, _ := os.ReadFile(path)
		if err := json.Unmarshal(raw, &ds); err != nil || len(ds) != 1 {
			t.Fatalf("file: %v %s", err, raw)
		}
		return ds[0].LastSeen
	}
	start := read()
	e.now = e.now.Add(time.Minute)
	e.do("GET", "/protected", "", c)
	if !read().Equal(start) {
		t.Fatal("lastSeen persisted after 1 minute")
	}
	if got := e.a.devices.List()[0].LastSeen; !got.Equal(e.now) {
		t.Fatalf("in-memory lastSeen = %v, want %v", got, e.now)
	}
	e.now = e.now.Add(5 * time.Minute)
	e.do("GET", "/protected", "", c)
	if !read().Equal(e.now) {
		t.Fatal("lastSeen not persisted after 5 minutes")
	}
}

func TestLogsNeverContainSecrets(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	e := newEnv(t)
	c := e.login(t)
	code := e.pair(t, c)
	e.do("PATCH", "/api/devices/"+strings.SplitN(c.Value, ".", 2)[0], `{"name":"Secret Phone Name"}`, c)
	e.do("POST", "/api/session", `{"code":"`+code+`"}`, nil)
	e.do("POST", "/api/session", `{"code":"BADBAD"}`, nil)
	e.do("POST", "/api/session", `{"token":"wrong-token-xyz"}`, nil)
	e.do("GET", "/protected", "", &http.Cookie{Name: CookieName, Value: master})
	secret := strings.SplitN(c.Value, ".", 2)[1]
	for _, s := range []string{master, code, secret, "Secret Phone Name", "wrong-token-xyz", "BADBAD"} {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("log contains %q:\n%s", s, buf.String())
		}
	}
}

func TestDeviceIDOutsideRequest(t *testing.T) {
	if DeviceID(context.Background()) != "" {
		t.Fatal("expected empty id")
	}
}

func TestLegacyCookieNeverMintsMoreThanOneDevicePerDay(t *testing.T) {
	e := newEnv(t)
	legacy := &http.Cookie{Name: CookieName, Value: master}
	var first string
	for i := 0; i < 100; i++ { // one request every 36s for an hour, cookie never stored
		w := e.do("GET", "/protected", "", legacy)
		if w.Code != 200 {
			t.Fatalf("request %d = %d", i, w.Code)
		}
		if i == 0 {
			first = sessionCookie(t, w).Value
		} else if got := sessionCookie(t, w).Value; got != first {
			t.Fatalf("request %d minted a new cookie", i)
		}
		e.now = e.now.Add(36 * time.Second)
	}
	if n := len(e.a.devices.List()); n != 1 {
		t.Fatalf("devices = %d, want 1", n)
	}
	e.now = e.now.Add(24 * time.Hour)
	if w := e.do("GET", "/protected", "", legacy); w.Code != 200 {
		t.Fatalf("after 24h = %d", w.Code)
	}
	if n := len(e.a.devices.List()); n != 2 {
		t.Fatalf("devices after the grant expired = %d, want 2", n)
	}
}

func TestLegacyUpgradeRevokedInsideEvictionGap(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < MaxDevices; i++ {
		e.login(t)
		e.now = e.now.Add(time.Minute)
	}
	// The legacy upgrade evicts one device; the OnRevoke hook runs between
	// creating the new device and serving the request. Sign the new one out there.
	e.a.OnRevoke(func(string) {
		for _, d := range e.a.devices.List() {
			if d.Created.Equal(e.now) { // the device being created
				e.a.Revoke(d.ID)
			}
		}
	})
	w := e.do("GET", "/protected", "", &http.Cookie{Name: CookieName, Value: master})
	if w.Code != 401 {
		t.Fatalf("upgrade revoked mid-flight = %d, want 401", w.Code)
	}
	e.a.mu.Lock()
	n := len(e.a.ctxs)
	e.a.mu.Unlock()
	if n != 0 {
		t.Fatalf("leaked %d ctxs entries", n)
	}
}

func TestPairingCodesComparedInConstantTime(t *testing.T) {
	// behaviour check: wrong-length and near-miss inputs are rejected, exact match accepted
	var p pairing
	now := time.Now()
	code, _, _ := p.issue(now)
	for _, in := range []string{"", code[:5], code + "A", strings.Repeat("A", 6)} {
		if in != code && p.redeem(now, in) != redeemInvalid {
			t.Fatalf("%q accepted", in)
		}
	}
	if p.redeem(now, code) != redeemOK {
		t.Fatal("exact code rejected")
	}
}
