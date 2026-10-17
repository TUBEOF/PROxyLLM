package config

// Spec-derived tests (docs/configuration.md): the config file is layer 2
// — it overrides defaults and is overridden by ENV. Strict parsing: an
// unknown key or trailing data errors, it never silently defaults. A
// missing file is the normal state.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg writes a config file and points PROXYLLM_CONFIG_FILE at it.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXYLLM_CONFIG_FILE", p)
	return p
}

func TestLoad_MissingFileIsNormal(t *testing.T) {
	t.Setenv("PROXYLLM_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.json"))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("missing config file must not error: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("addr = %q, want defaults", cfg.Server.Addr)
	}
}

func TestLoad_FileOverridesDefaults(t *testing.T) {
	writeCfg(t, `{"server":{"addr":":9999"},"storage":{"path":"/tmp/file.db"},"log":{"level":"warn","format":"text"},"firecrawl":{"base_url":"http://127.0.0.1:9"}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9999" {
		t.Errorf("addr = %q, want :9999 from file", cfg.Server.Addr)
	}
	if cfg.Storage.Path != "/tmp/file.db" {
		t.Errorf("storage.path = %q, want from file", cfg.Storage.Path)
	}
	if cfg.Log.Level != "warn" || cfg.Log.Format != "text" {
		t.Errorf("log = %s/%s, want warn/text from file", cfg.Log.Level, cfg.Log.Format)
	}
	if cfg.Firecrawl.BaseURL != "http://127.0.0.1:9" {
		t.Errorf("firecrawl.base_url = %q, want from file", cfg.Firecrawl.BaseURL)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	writeCfg(t, `{"server":{"addr":":9090"}}`)
	t.Setenv("PROXYLLM_SERVER_ADDR", ":8888")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":8888" {
		t.Errorf("addr = %q, want :8888 (ENV over file)", cfg.Server.Addr)
	}
}

func TestLoad_FileInvalidJSONErrors(t *testing.T) {
	writeCfg(t, `{"server":`)
	if _, err := Load(); err == nil {
		t.Fatal("invalid JSON must error, not silently default")
	}
}

func TestLoad_FileUnknownKeyErrors(t *testing.T) {
	writeCfg(t, `{"server":{"addr":":9090"},"srever":{"addr":":1"}}`)
	if _, err := Load(); err == nil {
		t.Error("unknown key must error, not silently default")
	}
}

func TestLoad_FileTrailingDataErrors(t *testing.T) {
	writeCfg(t, `{"server":{"addr":":9090"}} {"sneaky":true}`)
	if _, err := Load(); err == nil {
		t.Error("trailing data must error, not silently parse the first value")
	}
}

// Disabling authentication is an environment-only decision: a config file
// that tries must fail loudly, and the error names the variable that can.
func TestLoad_FileAuthGroupIsRefused(t *testing.T) {
	p := writeCfg(t, `{"auth":{"disabled":true}}`)
	_, err := Load()
	if err == nil {
		t.Fatal("a config file disabling auth must be refused")
	}
	if !strings.Contains(err.Error(), "PROXYLLM_AUTH_DISABLED") || !strings.Contains(err.Error(), p) {
		t.Errorf("error = %v, want it to name the file and the environment variable", err)
	}
}

// An explicit null is the absence of a statement, not a refusal: the file
// says nothing about auth, so it loads.
func TestLoad_FileAuthNullIsANoOp(t *testing.T) {
	writeCfg(t, `{"auth":null}`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("an explicit null auth group must load: %v", err)
	}
	if cfg.Auth.Disabled {
		t.Error("a null auth group must not disable authentication")
	}
}
