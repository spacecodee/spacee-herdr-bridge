package push

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func TestPayloadAngularFormat(t *testing.T) {
	b := Payload(Notice{Address: "mac/t7", Title: "Proj / Agent", Body: "Needs you"})
	var got struct {
		Notification struct {
			Title, Body, Tag, Icon string
			Data                   struct {
				OnActionClick map[string]map[string]string `json:"onActionClick"`
			} `json:"data"`
		} `json:"notification"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	n := got.Notification
	d := n.Data.OnActionClick["default"]
	if n.Title != "Proj / Agent" || n.Body != "Needs you" || n.Tag != "mac/t7" || n.Icon != "/icons/icon-192.png" ||
		d["operation"] != "navigateLastFocusedOrOpen" || d["url"] != "/t/mac/t7" {
		t.Fatalf("payload: %s", b)
	}
}

func TestPayloadURLOverride(t *testing.T) {
	b := Payload(Notice{Address: "test", Title: "x", Body: "y", URL: "/"})
	if !strings.Contains(string(b), `"url":"/"`) {
		t.Fatalf("payload: %s", b)
	}
}

// subscriber returns a valid browser-style subscription pointing at url.
func subscriber(t *testing.T, url string) webpush.Subscription {
	t.Helper()
	// a real P-256 public key and a 16-byte auth secret, both base64url
	v, err := LoadOrCreateVAPID(filepath.Join(t.TempDir(), "peer.json"))
	if err != nil {
		t.Fatal(err)
	}
	return webpush.Subscription{Endpoint: url, Keys: webpush.Keys{P256dh: v.Public, Auth: "AAAAAAAAAAAAAAAAAAAAAA"}}
}

func TestSenderEncryptsAndPrunesGone(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusCreated
	var hdr http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hdr = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()

	st, _ := OpenStore(filepath.Join(t.TempDir(), "push.json"))
	st.Upsert(subscriber(t, srv.URL+"/ok"))
	vapid, _ := LoadOrCreateVAPID(filepath.Join(t.TempDir(), "vapid.json"))
	s := &Sender{Store: st, VAPID: vapid, Subject: "https://example.test"}

	s.Notify(Notice{Address: "t1", Title: "SecretTitle", Body: "Needs you", Urgent: true})
	mu.Lock()
	if hdr.Get("Content-Encoding") != "aes128gcm" || hdr.Get("TTL") != "3600" || hdr.Get("Urgency") != "high" ||
		!strings.HasPrefix(hdr.Get("Authorization"), "vapid ") {
		t.Fatalf("headers: %v", hdr)
	}
	if len(body) == 0 || strings.Contains(string(body), "SecretTitle") {
		t.Fatal("payload must be encrypted")
	}
	mu.Unlock()
	if len(st.List()) != 1 {
		t.Fatal("201 must keep the subscription")
	}

	// non-urgent notifications carry no high urgency
	s.Notify(Notice{Address: "t1", Title: "x", Body: "Finished its turn"})
	mu.Lock()
	if hdr.Get("Urgency") == "high" {
		t.Fatal("urgency must be high only for urgent notices")
	}
	status = http.StatusGone
	mu.Unlock()
	s.Notify(Notice{Address: "t1", Title: "x", Body: "y"})
	if len(st.List()) != 0 {
		t.Fatal("410 must remove the subscription")
	}
}

func TestSenderKeepsOnOtherErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	st, _ := OpenStore(filepath.Join(t.TempDir(), "push.json"))
	st.Upsert(subscriber(t, srv.URL))
	vapid, _ := LoadOrCreateVAPID(filepath.Join(t.TempDir(), "vapid.json"))
	(&Sender{Store: st, VAPID: vapid, Subject: "https://example.test"}).Notify(Notice{Address: "t1", Title: "x", Body: "y"})
	if len(st.List()) != 1 {
		t.Fatal("500 must keep the subscription")
	}
}
