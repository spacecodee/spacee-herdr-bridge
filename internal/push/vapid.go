package push

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// VAPID is the bridge's application server key pair (base64url).
type VAPID struct {
	Public  string `json:"public"`
	Private string `json:"private"`
}

// LoadOrCreateVAPID reads the key pair at path, or generates one and creates the
// file atomically with mode 0600 (a half-written file is never visible).
func LoadOrCreateVAPID(path string) (VAPID, error) {
	var v VAPID
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &v); err != nil || v.Public == "" || v.Private == "" {
			return VAPID{}, fmt.Errorf("vapid file %s is invalid", path)
		}
		return v, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return VAPID{}, err
	}

	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return VAPID{}, err
	}
	v = VAPID{Public: pub, Private: priv}
	data, _ := json.Marshal(v)
	tmp, err := tempFile(path)
	if err != nil {
		return VAPID{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return VAPID{}, err
	}
	if err := tmp.Close(); err != nil {
		return VAPID{}, err
	}
	// Link fails if path exists: whoever got there first wins, and we use its keys.
	if err := os.Link(tmp.Name(), path); errors.Is(err, os.ErrExist) {
		return LoadOrCreateVAPID(path)
	} else if err != nil {
		return VAPID{}, err
	}
	return v, nil
}
