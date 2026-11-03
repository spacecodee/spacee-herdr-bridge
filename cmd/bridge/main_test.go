package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release scripts stamp the version with -ldflags "-X main.version=..."; -version must print it
// and exit before touching the token or any socket.
func TestVersionFlagPrintsStampedVersion(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bridge")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=9.9.9-test", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "-version").Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "9.9.9-test" {
		t.Fatalf("version = %q, want 9.9.9-test", got)
	}
}

func TestVersionDefaultsToDev(t *testing.T) {
	if version != "dev" {
		t.Fatalf("version = %q, want dev", version)
	}
}
