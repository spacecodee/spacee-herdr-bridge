package chat

import (
	"encoding/json"
	"os"
	"testing"
)

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }
