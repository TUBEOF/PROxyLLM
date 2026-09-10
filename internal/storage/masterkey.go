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

// decodeMasterKey accepts hex or base64 (standard or URL), 32 bytes after
// decoding. A trailing newline is tolerated.
func decodeMasterKey(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	key, hexErr := hex.DecodeString(s)
	if hexErr == nil {
		return checkKeyLen(key, "hex")
	}
	key, b64Err := base64.StdEncoding.DecodeString(s)
	if b64Err == nil {
		return checkKeyLen(key, "base64")
	}
	key, urlErr := base64.URLEncoding.DecodeString(s)
	if urlErr == nil {
		return checkKeyLen(key, "base64url")
	}
	return nil, fmt.Errorf("storage: master key is neither valid hex nor base64")
}

func checkKeyLen(key []byte, via string) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("storage: master key must be 32 bytes after decode (got %d via %s)", len(key), via)
	}
	return key, nil
}
