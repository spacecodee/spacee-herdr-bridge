package push

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spacecodee/spacee-herdr-bridge/internal/auth"
)

type fakeNotifier struct{ got []Notice }

func (f *fakeNotifier) Notify(n Notice) { f.got = append(f.got, n) }

func newHandler(t *testing.T) (http.Handler, *Store, *fakeNotifier) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	fn := &fakeNotifier{}
	return Handler(auth.New("tok"), st, "PUBKEY", fn), st, fn
}

func do(h http.Handler, method, path, body string, cookie bool, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "bridge.test"
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

const goodSub = `{"endpoint":"https://push.example/abc","keys":{"p256dh":"P","auth":"A"}}`

func TestHandlerRequiresAuth(t *testing.T) {
	h, _, _ := newHandler(t)
	for _, c := range [][2]string{
		{"GET", "/api/push/key"}, {"POST", "/api/push/subscription"},
		{"DELETE", "/api/push/subscription"}, {"POST", "/api/push/test"},
	} {
		if w := do(h, c[0], c[1], goodSub, false, ""); w.Code != 401 {
			t.Errorf("%s %s without cookie = %d, want 401", c[0], c[1], w.Code)
		}
	}
}

func TestHandlerKey(t *testing.T) {
	h, _, _ := newHandler(t)
	w := do(h, "GET", "/api/push/key", "", true, "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"publicKey":"PUBKEY"}` {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
}

func TestHandlerOrigin(t *testing.T) {
	h, st, fn := newHandler(t)
	for _, c := range [][2]string{
		{"POST", "/api/push/subscription"}, {"DELETE", "/api/push/subscription"}, {"POST", "/api/push/test"},
	} {
		if w := do(h, c[0], c[1], goodSub, true, "https://evil.example"); w.Code != 403 {
			t.Errorf("%s %s foreign origin = %d, want 403", c[0], c[1], w.Code)
		}
	}
	if len(st.List()) != 0 || len(fn.got) != 0 {
		t.Fatal("rejected requests must have no effect")
	}
	if w := do(h, "POST", "/api/push/subscription", goodSub, true, "https://bridge.test"); w.Code != 204 {
		t.Fatalf("same origin = %d", w.Code)
	}
}

func TestHandlerSubscriptionValidation(t *testing.T) {
	h, st, _ := newHandler(t)
	bad := map[string]string{
		"not json":      `nope`,
		"http endpoint": `{"endpoint":"http://push.example/x","keys":{"p256dh":"P","auth":"A"}}`,
		"no endpoint":   `{"keys":{"p256dh":"P","auth":"A"}}`,
		"no p256dh":     `{"endpoint":"https://push.example/x","keys":{"auth":"A"}}`,
		"no auth":       `{"endpoint":"https://push.example/x","keys":{"p256dh":"P"}}`,
		"too big":       `{"endpoint":"https://push.example/` + strings.Repeat("a", 5000) + `","keys":{"p256dh":"P","auth":"A"}}`,
	}
	for name, body := range bad {
		if w := do(h, "POST", "/api/push/subscription", body, true, ""); w.Code != 400 {
			t.Errorf("%s = %d, want 400", name, w.Code)
		}
	}
	if len(st.List()) != 0 {
		t.Fatal("invalid bodies must not be stored")
	}
	if w := do(h, "POST", "/api/push/subscription", goodSub, true, ""); w.Code != 204 {
		t.Fatalf("good = %d", w.Code)
	}
	if l := st.List(); len(l) != 1 || l[0].Endpoint != "https://push.example/abc" || l[0].Keys.Auth != "A" {
		t.Fatalf("stored: %+v", l)
	}
}

func TestHandlerDelete(t *testing.T) {
	h, st, _ := newHandler(t)
	do(h, "POST", "/api/push/subscription", goodSub, true, "")
	if w := do(h, "DELETE", "/api/push/subscription", `{}`, true, ""); w.Code != 400 {
		t.Fatalf("missing endpoint = %d, want 400", w.Code)
	}
	if w := do(h, "DELETE", "/api/push/subscription", `{"endpoint":"https://push.example/abc"}`, true, ""); w.Code != 204 {
		t.Fatalf("delete = %d", w.Code)
	}
	if len(st.List()) != 0 {
		t.Fatal("not deleted")
	}
}

func TestHandlerTest(t *testing.T) {
	h, _, fn := newHandler(t)
	if w := do(h, "POST", "/api/push/test", "", true, ""); w.Code != 204 {
		t.Fatalf("test = %d", w.Code)
	}
	if len(fn.got) != 1 || fn.got[0].Body == "" || fn.got[0].URL != "/" {
		t.Fatalf("notified: %+v", fn.got)
	}
}
