package storage

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LoadMasterKey resolves the AES-256-GCM envelope master key:
//
//  1. PROXYLLM_MASTER_KEY ENV (hex or base64, 32 bytes after decode)
//  2. keyfile at path (created with 0600 on first start if absent)
//
// The key never lives beside the data in exported config; storage reads
// it directly from ENV or the dedicated keyfile.
func LoadMasterKey(envValue string, envSet bool, path string) ([]byte, error) {
	if envSet {
		return decodeMasterKey(envValue)
	}

	// Keyfile path required for persistence.
	if path == "" {
		return nil, errors.New("storage: no master key source (set PROXYLLM_MASTER_KEY or storage.master_key_path)")
	}

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return nil, fmt.Errorf("storage: master key path %s is a directory", path)
		}
		// path is operator configuration (ENV or defaults), never
		// request-derived — no inclusion taint in practice.
		raw, err := os.ReadFile(path) //nolint:gosec // operator-configured path
		if err != nil {
			return nil, fmt.Errorf("storage: read master key: %w", err)
		}
		key, err := decodeMasterKey(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, err
		}
		return key, nil

	case errors.Is(err, os.ErrNotExist):
		// First start: generate the key and persist with 0600.
		key := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("storage: generate master key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("storage: create key dir: %w", err)
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("storage: write master key: %w", err)
		}
		return key, nil

	default:
		return nil, fmt.Errorf("storage: stat master key: %w", err)
	}
}

// decodeMasterKey accepts a 32-byte master key encoded as hex (exactly
// 64 characters) or base64/base64url (exactly 44 characters, padded).
// The length dispatch removes the hex/base64 ambiguity: a 64-character
// string is unambiguously hex, a 44-character one unambiguously base64,
// so an operator's intended encoding can never be silently mis-decoded
// into a different key. A trailing newline is tolerated.
func decodeMasterKey(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	switch len(s) {
	case 64: // 32 bytes as hex
		key, err := hex.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("storage: master key: 64-char value is not valid hex: %w", err)
		}
		return checkKeyLen(key, "hex")
	case 44: // 32 bytes as padded base64 or base64url
		key, err := base64.StdEncoding.DecodeString(s)
		if err == nil {
			return checkKeyLen(key, "base64")
		}
		key, err = base64.URLEncoding.DecodeString(s)
		if err == nil {
			return checkKeyLen(key, "base64url")
		}
		return nil, fmt.Errorf("storage: master key: 44-char value is not valid base64")
	default:
		return nil, fmt.Errorf("storage: master key must be 64 hex chars or 44 base64 chars (got %d)", len(s))
	}
}

func checkKeyLen(key []byte, via string) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("storage: master key must be 32 bytes after decode (got %d via %s)", len(key), via)
	}
	return key, nil
}
