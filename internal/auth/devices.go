package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxDevices is how many signed-in devices are kept. Signing in on one more
// evicts the device that was seen least recently (and signs it out), so the
// owner is never locked out of adding a phone.
const MaxDevices = 20

// lastSeenPersistEvery bounds how often a device's lastSeen reaches the disk.
const lastSeenPersistEvery = 5 * time.Minute

// Device is one signed-in browser. Only the hash of its secret is stored.
type Device struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Platform   string    `json:"platform"`
	Created    time.Time `json:"created"`
	LastSeen   time.Time `json:"lastSeen"`
	SecretHash string    `json:"secretHash"`
}

// deviceStore keeps devices in a JSON file (0600, atomic write-rename). An
// empty path keeps them in memory only.
type deviceStore struct {
	mu        sync.Mutex
	path      string
	devs      []Device
	persisted map[string]time.Time // when each device's lastSeen last reached the disk
}

func openDeviceStore(path string) (*deviceStore, error) {
	s := &deviceStore{path: path, persisted: map[string]time.Time{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.devs); err != nil {
		quarantine := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if err := os.Rename(path, quarantine); err != nil {
			return nil, err
		}
		s.devs = nil
		log.Printf("auth: devices file was unreadable; moved to %s, starting with no devices", filepath.Base(quarantine))
	}
	for _, d := range s.devs {
		s.persisted[d.ID] = d.LastSeen
	}
	return s, nil
}

// List returns a copy of the devices.
func (s *deviceStore) List() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Device(nil), s.devs...)
}

// add stores d and returns the ids evicted to stay within MaxDevices.
func (s *deviceStore) add(d Device) (evicted []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append(append([]Device(nil), s.devs...), d)
	for len(next) > MaxDevices {
		oldest := 0
		for i, x := range next[:len(next)-1] { // never evict the device just added
			if x.LastSeen.Before(next[oldest].LastSeen) {
				oldest = i
			}
		}
		evicted = append(evicted, next[oldest].ID)
		next = append(next[:oldest], next[oldest+1:]...)
	}
	if err := s.commit(next); err != nil {
		return nil, err
	}
	for _, id := range evicted {
		delete(s.persisted, id)
	}
	s.persisted[d.ID] = d.LastSeen
	return evicted, nil
}

// verify checks the secret of device id in constant time and, when it matches,
// records now as lastSeen (persisted at most every lastSeenPersistEvery).
func (s *deviceStore) verify(id, secret string, now time.Time) (Device, bool) {
	sum := sha256.Sum256([]byte(secret))
	got := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.devs {
		if s.devs[i].ID != id {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.devs[i].SecretHash)) != 1 {
			return Device{}, false
		}
		s.devs[i].LastSeen = now
		if now.Sub(s.persisted[id]) >= lastSeenPersistEvery {
			if err := s.commit(append([]Device(nil), s.devs...)); err != nil {
				log.Printf("auth: could not save devices: %v", err)
			} else {
				s.persisted[id] = now
			}
		}
		return s.devs[i], true
	}
	return Device{}, false
}

func (s *deviceStore) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devs {
		if d.ID == id {
			return true
		}
	}
	return false
}

func (s *deviceStore) rename(id, name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]Device(nil), s.devs...)
	for i := range next {
		if next[i].ID == id {
			next[i].Name = name
			return true, s.commit(next)
		}
	}
	return false, nil
}

func (s *deviceStore) remove(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]Device, 0, len(s.devs))
	for _, d := range s.devs {
		if d.ID != id {
			next = append(next, d)
		}
	}
	if len(next) == len(s.devs) {
		return false, nil
	}
	if err := s.commit(next); err != nil {
		return false, err
	}
	delete(s.persisted, id)
	return true, nil
}

// commit persists next and, only on success, makes it current.
func (s *deviceStore) commit(next []Device) error {
	if s.path != "" {
		b, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(s.path, b); err != nil {
			return err
		}
	}
	s.devs = next
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var r [6]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	name := path + "." + hex.EncodeToString(r[:]) + ".tmp"
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// sortedByLastSeen returns devices, most recently seen first.
func sortedByLastSeen(ds []Device) []Device {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].LastSeen.After(ds[j].LastSeen) })
	return ds
}

// platformFromUA classifies a User-Agent as ios, ipad, android, mac or other.
func platformFromUA(ua string) string {
	switch {
	case strings.Contains(ua, "iPad"):
		return "ipad"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPod"):
		return "ios"
	case strings.Contains(ua, "Android"):
		return "android"
	case strings.Contains(ua, "Macintosh"):
		return "mac"
	}
	return "other"
}

func defaultName(platform string) string {
	switch platform {
	case "ios":
		return "iPhone"
	case "ipad":
		return "iPad"
	case "android":
		return "Android"
	case "mac":
		return "Mac"
	}
	return "Browser"
}
