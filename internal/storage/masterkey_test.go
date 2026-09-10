package storage

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Spec: docs/configuration.md master-key resolution order — ENV
// (hex/base64, 32 bytes) over keyfile; keyfile generated 0600 on first
// start; malformed values error, never silently default.

func TestLoadMasterKey_EnvHex(t *testing.T) {
	key, err := LoadMasterKey(strings.Repeat("ab", 32), true, "")
	if err != nil {
		t.Fatalf("ENV hex: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("key length: got %d", len(key))
	}
}

func TestLoadMasterKey_EnvBase64(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	encoded := base64StdEncode(t, raw)
	key, err := LoadMasterKey(encoded, true, "")
	if err != nil {
		t.Fatalf("ENV base64: %v", err)
	}
	if string(key) != string(raw) {
		t.Error("base64 round trip mismatch")
	}
}

func TestLoadMasterKey_EnvMalformed(t *testing.T) {
	if _, err := LoadMasterKey("not-hex-not-base64!!!", true, ""); err == nil {
		t.Error("malformed ENV key must error")
	}
	if _, err := LoadMasterKey("abcd", true, ""); err == nil {
		t.Error("short ENV key must error (32 bytes required)")
	}
}

func TestLoadMasterKey_KeyfileGenerated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	key, err := LoadMasterKey("", false, path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("generated key length: %d", len(key))
	}

	// File exists, 0600, and re-loading returns the SAME key.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("keyfile not written: %v", err)
	}
	// File exists, 0600, and re-loading returns the SAME key. The
	// permission assertion is POSIX-only: NTFS does not map Unix modes.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("keyfile permissions: got %o want 600", perm)
		}
	}
	key2, err := LoadMasterKey("", false, path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if string(key) != string(key2) {
		t.Error("reloaded key differs — key must be stable across restarts")
	}
}

func TestLoadMasterKey_KeyfileHexContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	want := strings.Repeat("cd", 32)
	if err := os.WriteFile(path, []byte(want+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadMasterKey("", false, path)
	if err != nil {
		t.Fatalf("keyfile: %v", err)
	}
	if hex.EncodeToString(key) != want {
		t.Errorf("keyfile content mismatch: got %x", key)
	}
}

func TestLoadMasterKey_NoSource(t *testing.T) {
	if _, err := LoadMasterKey("", false, ""); err == nil {
		t.Error("missing master key source must error")
	}
}

// base64StdEncode is a test helper.
func base64StdEncode(t *testing.T, b []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(b)
}
