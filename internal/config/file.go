package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Config-file layer (docs/configuration.md layer 2): JSON file at
// PROXYLLM_CONFIG_FILE (or the documented default config.json). It sits
// between defaults and ENV: file overrides defaults, ENV overrides file.
//
// Strict parsing — unknown keys and trailing data reject with an error
// naming the offending field: a typo'd key in the operator's file must
// fail loudly at startup, never silently route with the default (same
// rule as the provider config parser and the admin PUT bodies).

// DefaultConfigFile is the config-file path used when
// PROXYLLM_CONFIG_FILE is unset.
const DefaultConfigFile = "config.json"

// ConfigFileFromEnv returns the configured file path if set via ENV.
func ConfigFileFromEnv() (string, bool) {
	v, ok := os.LookupEnv("PROXYLLM_CONFIG_FILE")
	return v, ok
}

// configFile is the effective path for Load.
func configFile() string {
	if p, ok := ConfigFileFromEnv(); ok && p != "" {
		return p
	}
	return DefaultConfigFile
}

// fileWire is the JSON-tagged mirror of Config for the file layer. Only
// non-secret settings: the master key and the Firecrawl API key never
// enter the exported Config struct, so they never enter the file layer
// either (docs/configuration.md — secrets resolve via ENV/keyfile only).
type fileWire struct {
	Server struct {
		Addr           string   `json:"addr"`
		TrustedProxies []string `json:"trusted_proxies"`
	} `json:"server"`
	Storage struct {
		Path          string `json:"path"`
		MasterKeyPath string `json:"master_key_path"`
	} `json:"storage"`
	Log struct {
		Level  string `json:"level"`
		Format string `json:"format"`
	} `json:"log"`
	// Auth is decoded only so the refusal below can be explicit: disabling
	// authentication is an environment-only decision (PROXYLLM_AUTH_DISABLED),
	// and a file that tries to do it fails with a message that says so
	// instead of a generic unknown-field error.
	Auth      json.RawMessage `json:"auth"`
	Firecrawl struct {
		BaseURL string `json:"base_url"`
	} `json:"firecrawl"`
}

// applyFile overlays the config file onto cfg (defaults present). A file
// that does not exist is the normal state — no error, no overlay. All
// other read/parse failures error loudly.
func applyFile(cfg *Config) (*Config, error) {
	path := configFile()
	raw, err := os.ReadFile(path) //nolint:gosec // operator-configured path (PROXYLLM_CONFIG_FILE or config.json default), never request-derived — no inclusion taint
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var w fileWire
	if err := dec.Decode(&w); err != nil {
		return cfg, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return cfg, fmt.Errorf("config: parsing %s: unexpected trailing data", path)
	}

	if w.Server.Addr != "" {
		cfg.Server.Addr = w.Server.Addr
	}
	if len(w.Server.TrustedProxies) > 0 {
		cfg.Server.TrustedProxies = w.Server.TrustedProxies
	}
	if w.Storage.Path != "" {
		cfg.Storage.Path = w.Storage.Path
	}
	if w.Storage.MasterKeyPath != "" {
		cfg.Storage.MasterKeyPath = w.Storage.MasterKeyPath
	}
	if w.Log.Level != "" {
		cfg.Log.Level = w.Log.Level
	}
	if w.Log.Format != "" {
		cfg.Log.Format = w.Log.Format
	}
	if len(w.Auth) > 0 && string(w.Auth) != "null" {
		return cfg, fmt.Errorf("config: %s: auth cannot be configured in the file — "+
			"disabling authentication is environment-only (PROXYLLM_AUTH_DISABLED=1)", path)
	}
	if w.Firecrawl.BaseURL != "" {
		cfg.Firecrawl.BaseURL = w.Firecrawl.BaseURL
	}
	return cfg, nil
}
