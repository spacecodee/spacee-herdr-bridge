package layout

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

type rig struct {
	srv  *httptest.Server
	core *fakeCore
	a    *auth.Auth
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := newFakeCore(t, fixtureJSON)
	a := auth.New("secret-token")
	svc := &Service{Core: &UnixCore{Socket: f.Socket}, Logf: func(string, ...any) {}}
	srv := httptest.NewServer(Handler(a, svc))
	t.Cleanup(srv.Close)
	return &rig{srv, f, a}
}

// do sends a request with a valid session cookie unless noAuth.
func (r *rig) do(t *testing.T, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if hdr == nil || hdr["noauth"] == "" {
		login := httptest.NewRecorder()
		lr := httptest.NewRequest("POST", "/api/session", strings.NewReader(`{"token":"secret-token"}`))
		r.a.SessionHandler().ServeHTTP(login, lr)
		for _, c := range login.Result().Cookies() {
			req.AddCookie(c)
		}
	}
	for k, v := range hdr {
		if k != "noauth" {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if e != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func TestHandlerRequiresSessionAndSameOrigin(t *testing.T) {
	r := newRig(t)
	if code, _ := r.do(t, "POST", "/api/layout/space", `{}`, map[string]string{"noauth": "1"}); code != 401 {
		t.Fatalf("no session: %d", code)
	}
	if code, _ := r.do(t, "POST", "/api/layout/space", `{"name":"A","folder":"/x"}`, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("foreign origin: %d", code)
	}
	if len(r.core.Runs) != 0 {
		t.Fatal("rejected request touched core")
	}
}

func TestHandlerCreateSpace(t *testing.T) {
	r := newRig(t)
	code, body := r.do(t, "POST", "/api/layout/space", `{"name":"Proj","folder":"/home/me"}`, nil)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var got Created
	json.Unmarshal([]byte(body), &got)
	if got.Space == "" || got.Tab == "" || got.Terminal == "" {
		t.Fatalf("body %s", body)
	}
}

func TestHandlerStatusMapping(t *testing.T) {
	r := newRig(t)
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/layout/space", `{"name":"A","folder":"/x","argv":["rm"]}`, 400},
		{"POST", "/api/layout/space", `{"name":"A","folder":"/x","bogus":1}`, 400},
		{"POST", "/api/layout/space", `not json`, 400},
		{"POST", "/api/layout/space", `{"name":"` + strings.Repeat("a", 9000) + `","folder":"/x"}`, 400},
		{"POST", "/api/layout/tab", `{"space":"nope","name":"T"}`, 404},
		{"POST", "/api/layout/split", `{"tab":"tbB","direction":"left"}`, 400},
		{"POST", "/api/layout/split", `{"tab":"nope","direction":"right"}`, 404},
		{"PATCH", "/api/layout/space/spc1", `{"name":""}`, 400},
		{"PATCH", "/api/layout/tab/nope", `{"name":"x"}`, 404},
		{"POST", "/api/layout/move", `{"kind":"tab","id":"tbB","index":9}`, 400},
		{"DELETE", "/api/layout/tab/tbC?confirmed=1", ``, 409},
		{"DELETE", "/api/layout/pane/nope?confirmed=1", ``, 404},
		{"GET", "/api/layout/space", ``, 405},
	}
	for _, c := range cases {
		if code, body := r.do(t, c.method, c.path, c.body, nil); code != c.want {
			t.Errorf("%s %s: got %d (%s), want %d", c.method, c.path, code, strings.TrimSpace(body), c.want)
		}
	}
}

func TestHandlerRoutesPatchMoveSplitTab(t *testing.T) {
	r := newRig(t)
	if code, b := r.do(t, "PATCH", "/api/layout/space/spc1", `{"name":"Renamed"}`, nil); code != 204 {
		t.Fatalf("rename space %d %s", code, b)
	}
	if code, b := r.do(t, "PATCH", "/api/layout/tab/tbA", `{"name":"TabX"}`, nil); code != 204 {
		t.Fatalf("rename tab %d %s", code, b)
	}
	if code, b := r.do(t, "POST", "/api/layout/move", `{"kind":"space","id":"spc2","index":0}`, nil); code != 204 {
		t.Fatalf("move %d %s", code, b)
	}
	if code, b := r.do(t, "POST", "/api/layout/tab", `{"space":"spc1","name":"New","argv":["codex"]}`, nil); code != 200 {
		t.Fatalf("tab %d %s", code, b)
	}
	code, b := r.do(t, "POST", "/api/layout/split", `{"tab":"tbB","pane":"pB1","direction":"down"}`, nil)
	if code != 200 {
		t.Fatalf("split %d %s", code, b)
	}
	d := r.core.Doc()
	if d["spaces"].([]any)[0].(map[string]any)["id"] != "spc2" {
		t.Fatal("move not applied")
	}
}

func TestHandlerDeleteNeedsConfirmed(t *testing.T) {
	r := newRig(t)
	for _, p := range []string{"/api/layout/space/spc1", "/api/layout/space/spc1?confirmed=0", "/api/layout/tab/tbB", "/api/layout/pane/pB1"} {
		if code, _ := r.do(t, "DELETE", p, "", nil); code != 400 {
			t.Errorf("%s: %d, want 400", p, code)
		}
	}
	if len(r.core.Killed) != 0 || r.core.Sets != 0 {
		t.Fatal("unconfirmed delete touched core")
	}
	if code, b := r.do(t, "DELETE", "/api/layout/space/spc1?confirmed=1", "", nil); code != 204 {
		t.Fatalf("%d %s", code, b)
	}
	if len(r.core.Killed) != 3 {
		t.Fatalf("killed %v", r.core.Killed)
	}
}
