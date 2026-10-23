package config

// Config is the effective, assembled configuration.
type Config struct {
	Server    ServerConfig
	Storage   StorageConfig
	Log       LogConfig
	Auth      AuthConfig
	Firecrawl FirecrawlConfig
}

// ServerConfig holds HTTP listener settings.
type ServerConfig struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string

	// TrustedProxies lists the reverse proxies (CIDRs or bare IPs) whose
	// X-Forwarded-Proto/X-Forwarded-Host headers the WebUI's origin check
	// may trust — the setup a TLS terminator in front requires. Empty (the
	// default) trusts no one: forwarded headers are never honored then.
	// Validation happens where the set is consumed (auth.ParseTrustedProxies
	// fails startup on a bad entry).
	TrustedProxies []string
}

// StorageConfig holds SQLite settings.
type StorageConfig struct {
	// Path is the SQLite database file path.
	Path string

	// MasterKeyPath points to the envelope-encryption master key file
	// (0600). If empty and no ENV key is set, a key is generated on first
	// start.
	MasterKeyPath string
}

// LogConfig holds logging settings.
type LogConfig struct {
	// Level is a slog level string: debug, info, warn, error.
	Level string

	// Format is "json" (structured, default) or "text" (development).
	Format string
}

// AuthConfig holds authentication settings. Authentication is default-ON;
// it can only be disabled via the explicit ENV opt-out (PROXYLLM_AUTH_DISABLED)
// — the config-file layer refuses an `auth` group outright, so no file and no
// console switch can turn it off. Never default true.
type AuthConfig struct {
	// Disabled reflects PROXYLLM_AUTH_DISABLED=1.
	Disabled bool
}

// FirecrawlConfig holds the web-tool fallback settings. Only
// non-secret surface lives here; the API key is read straight from ENV
// (FirecrawlAPIKeyFromEnv) so config dumps can never leak it — the same
// discipline as the storage master key.
type FirecrawlConfig struct {
	// BaseURL overrides the Firecrawl API host; empty selects the
	// client's documented default. Intended for self-hosted instances and
	// tests.
	BaseURL string
}

// Default returns the built-in defaults. Every layer above overrides these.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Addr: ":8080",
		},
		Storage: StorageConfig{
			Path:          "data/proxyllm.db",
			MasterKeyPath: "data/master.key",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
		Auth: AuthConfig{
			Disabled: false,
		},
	}
}
