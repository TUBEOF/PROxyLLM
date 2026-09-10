package config

import (
	"testing"
)

// Spec: docs/configuration.md — layering defaults -> ENV (file and DB
// layers pending); ENV overrides defaults; unknown values must error, not
// silently default.

// withEnv sets env vars for the test and restores them after.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("default addr: got %q want :8080", cfg.Server.Addr)
	}
	if cfg.Storage.Path != "data/proxyllm.db" {
		t.Errorf("default storage path: got %q", cfg.Storage.Path)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Errorf("default log: got %s/%s", cfg.Log.Level, cfg.Log.Format)
	}
	if cfg.Auth.Disabled {
		t.Error("auth must never default to disabled")
	}
}

func TestLoad_EnvOverridesDefaults(t *testing.T) {
	withEnv(t, map[string]string{
		"PROXYLLM_SERVER_ADDR":  ":9090",
		"PROXYLLM_STORAGE_PATH": "/tmp/x.db",
		"PROXYLLM_LOG_LEVEL":    "debug",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("addr: got %q want :9090", cfg.Server.Addr)
	}
	if cfg.Storage.Path != "/tmp/x.db" {
		t.Errorf("storage path: got %q", cfg.Storage.Path)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log level: got %q want debug", cfg.Log.Level)
	}
}

func TestLoad_EnvInvalidBool(t *testing.T) {
	withEnv(t, map[string]string{
		"PROXYLLM_AUTH_DISABLED": "definitely-not",
	})
	if _, err := Load(); err == nil {
		t.Fatal("invalid boolean must error, not silently default")
	}
}

func TestLoad_EnvAuthDisabledOptOut(t *testing.T) {
	withEnv(t, map[string]string{
		"PROXYLLM_AUTH_DISABLED": "1",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.Disabled {
		t.Error("explicit opt-out must be honored")
	}
}

func TestMasterKeyFromEnv(t *testing.T) {
	t.Setenv("PROXYLLM_MASTER_KEY", "6f86a7b2...")
	raw, ok := MasterKeyFromEnv()
	if !ok || raw != "6f86a7b2..." {
		t.Errorf("master key from env: got %q ok=%v", raw, ok)
	}
}
