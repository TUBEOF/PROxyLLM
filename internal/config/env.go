package config

import (
	"fmt"
	"os"
	"strings"
)

// applyEnv overlays the ENV layer onto cfg. ENV keys use the PROXYLLM_
// prefix; the highest layer (DB overrides) applies after this in Load.
func applyEnv(cfg *Config) (*Config, error) {
	if v, ok := os.LookupEnv("PROXYLLM_SERVER_ADDR"); ok {
		cfg.Server.Addr = v
	}
	if v, ok := os.LookupEnv("PROXYLLM_SERVER_TRUSTED_PROXIES"); ok {
		var entries []string
		for e := range strings.SplitSeq(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
		cfg.Server.TrustedProxies = entries
	}
	if v, ok := os.LookupEnv("PROXYLLM_STORAGE_PATH"); ok {
		cfg.Storage.Path = v
	}
	if v, ok := os.LookupEnv("PROXYLLM_MASTER_KEY_PATH"); ok {
		cfg.Storage.MasterKeyPath = v
	}
	if v, ok := os.LookupEnv("PROXYLLM_LOG_LEVEL"); ok {
		cfg.Log.Level = v
	}
	if v, ok := os.LookupEnv("PROXYLLM_LOG_FORMAT"); ok {
		cfg.Log.Format = v
	}
	if v, ok := os.LookupEnv("PROXYLLM_AUTH_DISABLED"); ok {
		b, err := parseBool("PROXYLLM_AUTH_DISABLED", v)
		if err != nil {
			return nil, err
		}
		cfg.Auth.Disabled = b
	}
	if v, ok := os.LookupEnv("PROXYLLM_FIRECRAWL_BASE_URL"); ok {
		cfg.Firecrawl.BaseURL = v
	}

	// Master key is held separately from the file path: the raw key never
	// lives in the exported Config struct; storage reads it directly from
	// ENV to avoid it leaking through config dumps.
	return cfg, nil
}

func parseBool(key, raw string) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off", "":
		return false, nil
	}
	return false, fmt.Errorf("config: %s: invalid boolean %q", key, raw)
}

// MasterKeyFromEnv returns the raw master key if set via ENV. Kept here so
// the ENV surface is documented in one place.
func MasterKeyFromEnv() (string, bool) {
	v, ok := os.LookupEnv("PROXYLLM_MASTER_KEY")
	return v, ok
}

// FirecrawlAPIKeyFromEnv returns the Firecrawl API key if set via ENV
// (PROXYLLM_FIRECRAWL_API_KEY). The key is a secret: it never enters the
// exported Config struct — assembly reads it here and hands it straight to
// the Firecrawl client.
func FirecrawlAPIKeyFromEnv() (string, bool) {
	v, ok := os.LookupEnv("PROXYLLM_FIRECRAWL_API_KEY")
	return v, ok
}
