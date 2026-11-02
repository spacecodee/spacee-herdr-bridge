package push

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func sub(n int) webpush.Subscription {
	return webpush.Subscription{
		Endpoint: fmt.Sprintf("https://push.example/%d", n),
		Keys:     webpush.Keys{P256dh: "p", Auth: "a"},
	}
}

func TestStoreCreateUpsertCapDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "push.json")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.List()) != 0 {
		t.Fatal("new store must be empty")
	}
	if err := st.Upsert(sub(1)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file perms = %v, err = %v; want 0600", fi.Mode().Perm(), err)
	}

	// upsert by endpoint replaces keys, does not duplicate
	changed := sub(1)
	changed.Keys.Auth = "other"
	if err := st.Upsert(changed); err != nil {
		t.Fatal(err)
	}
	if l := st.List(); len(l) != 1 || l[0].Keys.Auth != "other" {
		t.Fatalf("upsert: %+v", l)
	}

	// cap at 10: the oldest is dropped
	for i := 2; i <= 12; i++ {
		if err := st.Upsert(sub(i)); err != nil {
			t.Fatal(err)
		}
	}
	l := st.List()
	if len(l) != MaxSubscriptions || l[0].Endpoint != sub(3).Endpoint || l[9].Endpoint != sub(12).Endpoint {
		t.Fatalf("cap: len=%d first=%s last=%s", len(l), l[0].Endpoint, l[len(l)-1].Endpoint)
	}

	// persisted
	st2, err := OpenStore(path)
	if err != nil || len(st2.List()) != MaxSubscriptions {
		t.Fatalf("reopen: %v len=%d", err, len(st2.List()))
	}

	if err := st.Delete(sub(12).Endpoint); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("https://unknown"); err != nil {
		t.Fatal(err)
	}
	if len(st.List()) != MaxSubscriptions-1 {
		t.Fatalf("delete: len=%d", len(st.List()))
	}
}

func TestStoreCorruptFileIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "push.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("corrupt file must not be an error: %v", err)
	}
	if len(st.List()) != 0 {
		t.Fatal("must start empty")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("corrupt file must be moved away")
	}
	m, _ := filepath.Glob(filepath.Join(dir, "push.json.corrupt-*"))
	if len(m) != 1 {
		t.Fatalf("quarantine files: %v", m)
	}
	if b, _ := os.ReadFile(m[0]); string(b) != "{not json" {
		t.Fatal("quarantined content changed")
	}
	if err := st.Upsert(sub(1)); err != nil || len(st.List()) != 1 {
		t.Fatalf("store must work after quarantine: %v", err)
	}
}

func TestLoadOrCreateVAPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d", "vapid.json")
	a, err := LoadOrCreateVAPID(path)
	if err != nil || a.Public == "" || a.Private == "" {
		t.Fatalf("create: %+v %v", a, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("perms = %v", fi.Mode().Perm())
	}
	b, err := LoadOrCreateVAPID(path)
	if err != nil || b != a {
		t.Fatalf("second load must return the same keys: %+v %v", b, err)
	}
	os.WriteFile(path, []byte("garbage"), 0o600)
	if _, err := LoadOrCreateVAPID(path); err == nil {
		t.Fatal("corrupt vapid file must be an error")
	}
}
