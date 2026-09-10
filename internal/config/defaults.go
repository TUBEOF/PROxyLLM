package config

// Config is the effective, assembled configuration.
type Config struct {
	Server  ServerConfig
	Storage StorageConfig
	Log     LogConfig
	Auth    AuthConfig
}

// ServerConfig holds HTTP listener settings.
type ServerConfig struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
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
// it can only be disabled via the explicit ENV opt-out.
type AuthConfig struct {
	// Disabled reflects PROXYLLM_AUTH_DISABLED=1. Never default true.
	Disabled bool
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
