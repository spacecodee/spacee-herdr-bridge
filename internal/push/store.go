// Package push sends Web Push notifications when a herdr-core terminal needs attention.
package push

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// MaxSubscriptions is how many browsers are kept; the oldest is dropped beyond it.
const MaxSubscriptions = 10

// Store keeps push subscriptions in a JSON file (0600, atomic write-rename).
type Store struct {
	mu   sync.Mutex
	path string
	subs []entry // oldest first
}

// entry is a stored subscription and the device that created it (empty for
// subscriptions saved before devices existed).
type entry struct {
	webpush.Subscription
	DeviceID string `json:"deviceId,omitempty"`
}

// OpenStore loads path, or starts empty when it does not exist. A corrupt file
// must not take the bridge down: it is renamed to path.corrupt-<unix-seconds>
// (kept for inspection) and the store starts empty.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.subs); err != nil {
		quarantine := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if err := os.Rename(path, quarantine); err != nil {
			return nil, err
		}
		s.subs = nil
		log.Printf("push: subscriptions file was unreadable; moved to %s, starting empty", filepath.Base(quarantine))
	}
	return s, nil
}

// List returns a copy of the subscriptions.
func (s *Store) List() []webpush.Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]webpush.Subscription, len(s.subs))
	for i, e := range s.subs {
		out[i] = e.Subscription
	}
	return out
}

// Upsert adds sub, replacing any with the same endpoint, and drops the oldest beyond the cap.
func (s *Store) Upsert(sub webpush.Subscription) error { return s.UpsertDevice(sub, "") }

// UpsertDevice is Upsert recording the device that owns the subscription.
func (s *Store) UpsertDevice(sub webpush.Subscription, deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := without(s.subs, sub.Endpoint)
	next = append(next, entry{sub, deviceID})
	if len(next) > MaxSubscriptions {
		next = next[len(next)-MaxSubscriptions:]
	}
	return s.commit(next)
}

// Delete removes the subscription with endpoint; an unknown endpoint is not an error.
func (s *Store) Delete(endpoint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commit(without(s.subs, endpoint))
}

// DeleteByDevice removes every subscription saved by deviceID.
func (s *Store) DeleteByDevice(deviceID string) error {
	if deviceID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]entry, 0, len(s.subs))
	for _, e := range s.subs {
		if e.DeviceID != deviceID {
			next = append(next, e)
		}
	}
	if len(next) == len(s.subs) {
		return nil
	}
	return s.commit(next)
}

func without(subs []entry, endpoint string) []entry {
	out := make([]entry, 0, len(subs)+1)
	for _, x := range subs {
		if x.Endpoint != endpoint {
			out = append(out, x)
		}
	}
	return out
}

// commit persists next and, only on success, makes it current.
func (s *Store) commit(next []entry) error {
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, b); err != nil {
		return err
	}
	s.subs = next
	return nil
}

// writeFileAtomic writes data to a 0600 temp file in the same directory and renames it over path.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := tempFile(path)
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// tempFile creates an exclusive 0600 file next to path.
func tempFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	var r [6]byte
	if _, err := rand.Read(r[:]); err != nil {
		return nil, err
	}
	return os.OpenFile(path+"."+hex.EncodeToString(r[:])+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}
