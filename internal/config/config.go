// Package config implements PROxyLLM's layered configuration system:
// defaults -> config file -> environment variables -> database overrides
// (highest precedence).
package config

// Load assembles the effective configuration from all layers: defaults,
// then the config file (docs/configuration.md layer 2), then ENV. The DB
// layer (highest precedence) applies after assembly via the settings
// overrides the server reads from the store.
func Load() (*Config, error) {
	cfg, err := applyFile(Default())
	if err != nil {
		return nil, err
	}
	return applyEnv(cfg)
}
