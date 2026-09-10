// Package config implements PROxyLLM's layered configuration system:
// defaults -> config file -> environment variables -> database overrides
// (highest precedence).
package config

// Load assembles the effective configuration from all layers: defaults
// first, then ENV. File and DB layers follow in later phases (file before
// ENV, DB after — DB is highest).
func Load() (*Config, error) {
	return applyEnv(Default())
}
