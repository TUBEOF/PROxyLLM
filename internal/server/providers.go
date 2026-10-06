package server

// Live provider construction (Task 5 Gap #2, plan §5.2): at startup and on
// every admin-API provider write, the stored provider rows become live
// provider implementations — the WebUI-edited provider routes traffic
// without a restart.
//
// Construction rules:
//   - custom-openai: base_url from config, key from the encrypted token
//     store (kind "access_token"). Missing key is legal: the provider
//     still routes, the upstream answers the auth failure.
//   - ollama-cloud: base_url defaults to the package's DefaultBaseURL
//     when the config leaves it empty.
//   - codex-oauth: issuer from config, backend URL pinned (chatgpt.com
//     backend per X7), Auth client built against the issuer with the
//     provider's own ID — so token rows align with the provider row and
//     the device flow started in the WebUI refreshes THIS provider.
//
// A row that fails construction is skipped with an error log — a single
// bad row never takes the server down (the admin API re-validates on
// every write, so a bad row can only exist from a pre-validation
// database).

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/codex"
	"github.com/tubeof/proxyllm/internal/provider/custom"
	"github.com/tubeof/proxyllm/internal/provider/ollamacloud"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/server/webui"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/tools/firecrawl"
)

// loadProviders builds live providers from the stored rows. Errors are
// logged per row (skip + continue); the returned set is every row that
// constructed. A store failure returns the error — the caller decides
// whether that is startup-fatal or a reload-keep-last-known-good.
//
// ADR-009: each row's effective config is the type settings blob overlaid
// by the row's own config (account fields win). Both sides were validated
// per type on their write paths; the static parse below re-checks the
// merged shape. A type-settings read failure logs and degrades to empty —
// never a startup death on store noise.
//
// ADR-010: web_tool-kind rows (firecrawl) construct Emulators instead of
// chat providers; they are returned separately WITH their account IDs so
// the emulator pool can apply the stored web_search/web_fetch fallback
// KVs (the lists name ACCOUNT ids — pool order is the stored order). An
// LLM row and a web_tool row never cross.
func loadProviders(ctx context.Context, store *storage.Store, logger *slog.Logger) ([]provider.Provider, []NamedEmulator, error) {
	rows, err := store.ListProviders(ctx)
	if err != nil {
		return nil, nil, err
	}
	typeSettings, err := store.GetProviderTypeSettings(ctx)
	if err != nil {
		logger.Error("provider type settings unreadable — constructing from account config only",
			"error", err)
		typeSettings = map[string]json.RawMessage{}
	}
	out := make([]provider.Provider, 0, len(rows))
	var emus []NamedEmulator
	for _, row := range rows {
		p, emu, err := buildProvider(ctx, store, row, typeSettings, logger)
		if err != nil {
			logger.Error("provider row not constructable — skipping (fix it in the WebUI)",
				"provider_id", row.ID, "type", row.Type, "error", err)
			continue
		}
		if emu != nil {
			emus = append(emus, NamedEmulator{ID: row.ID, Emulator: emu})
			continue
		}
		out = append(out, p)
	}
	return out, emus, nil
}

// NamedEmulator pairs one web-tool emulator with its account ID (the
// providers row the account was built from): the stored
// web_search_fallback / web_fetch_fallback KVs name these IDs.
type NamedEmulator struct {
	ID       string
	Emulator routing.Emulator
}

// buildProvider constructs one row into its live form (ADR-010): an LLM
// provider, or — for web_tool kinds — an Emulator with a nil provider
// return. The config is parsed with the STATIC variant of the write-path
// validator: full shape checks, but no DNS resolution — rows were already
// SSRF-guarded when they were written, and a transient DNS failure at
// reload must never drop a live provider. The parse input is the ADR-009
// merged config (type settings ⊕ own fields, own wins).
func buildProvider(ctx context.Context, store *storage.Store, row storage.Provider,
	typeSettings map[string]json.RawMessage, logger *slog.Logger) (provider.Provider, routing.Emulator, error) {
	merged, err := webui.MergeProviderConfig(typeSettings[row.Type], row.Config)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := webui.ParseProviderConfigStatic(row.Type, merged)
	if err != nil {
		return nil, nil, err
	}

	switch row.Type {
	case webui.ProviderTypeFirecrawl:
		// ADR-010 web_tool kind: the account IS an emulator backend. The
		// key rides the encrypted token store; a keyless row constructs
		// and fails at call time with the documented upstream error (the
		// same honest-state contract as keyless custom accounts).
		emu := firecrawl.NewEmulator(cfg.BaseURL, providerAPIKey(ctx, logger, store, row.ID), nil)
		return nil, emu, nil

	case webui.ProviderTypeCustomOpenAI:
		return custom.New(row.ID, cfg.BaseURL, providerAPIKey(ctx, logger, store, row.ID), capsFromConfig(cfg, row.Type)), nil, nil

	case webui.ProviderTypeOllamaCloud:
		// Empty base_url is a legal save: the provider defaults to the
		// hosted endpoint.
		return ollamacloud.NewWithLogger(row.ID, cfg.BaseURL, providerAPIKey(ctx, logger, store, row.ID), capsFromConfig(cfg, row.Type), logger), nil, nil

	case webui.ProviderTypeCodexOAuth:
		// The codex provider speaks the ChatGPT backend (X7/X19); the
		// issuer from config drives the OAuth client (X1). The auth
		// client is keyed to THIS provider's ID so the token rows and
		// the device flow the WebUI started align.
		authc := codex.NewAuthClient(codex.AuthConfig{
			Issuer:     cfg.Issuer,
			Store:      store,
			ProviderID: row.ID,
			HTTP:       httpguard.NewClient(0),
		})
		return codex.New(codex.ProviderConfig{
			ID:           row.ID,
			BaseURL:      codexBaseURL,
			Capabilities: capsFromConfig(cfg, row.Type),
			Auth:         authc,
			// Streaming transport with guarded redirects — the OAuth access
			// token rides the Bearer header on every dispatch.
			HTTP: httpguard.NewStreamingClient(),
		}), nil, nil

	default:
		// ParseProviderConfig rejects unknown types, so this is a
		// construction-table drift, not operator input.
		return nil, nil, errors.New("no construction path for type " + row.Type)
	}
}

// capsFromConfig resolves the provider's capability set: per-type defaults
// (the baseline every provider of this class actually speaks) overridden by
// the operator's explicit pins from the config blob.
func capsFromConfig(cfg webui.ProviderConfig, typ string) provider.Capabilities {
	caps := defaultCaps(typ)
	if cfg.Capabilities == nil {
		return caps
	}
	// ParseProviderConfig validated the key names; unknown keys cannot
	// reach here.
	for name, on := range cfg.Capabilities {
		switch name {
		case "streaming":
			caps.Streaming = on
		case "tools":
			caps.Tools = on
		case "vision":
			caps.Vision = on
		case "web_search":
			caps.WebSearch = on
		case "web_fetch":
			caps.WebFetch = on
		case "code_execution":
			caps.CodeExecution = on
		case "thinking":
			caps.Thinking = on
		}
	}
	return caps
}

// defaultCaps is the baseline capability set per provider type — what a
// freshly saved provider of this class can do before the operator pins
// anything. Web-search/fetch/code-execution/thinking stay off by default:
// capability routing sends such requests to providers that truly support
// them, and Firecrawl emulation covers the gap (plan §6.2).
func defaultCaps(typ string) provider.Capabilities {
	switch typ {
	case webui.ProviderTypeCodexOAuth:
		// The ChatGPT backend speaks Responses-shaped requests with
		// streaming and tools (X19/X23); thinking is Claude-only
		// semantics translated per §6.4, so it stays off here.
		return provider.Capabilities{Streaming: true, Tools: true}
	case webui.ProviderTypeOllamaCloud:
		return provider.Capabilities{Streaming: true, Tools: true}
	default:
		// custom-openai: a conservative, honest default — streaming and
		// tools are the OpenAI-compat baseline (O2/O3); the operator pins
		// vision/server tools only when the upstream really has them.
		return provider.Capabilities{Streaming: true, Tools: true}
	}
}

// providerAPIKey reads the provider's access_token from the encrypted
// token store. No row is legal (the provider routes without a key — the
// upstream answers the auth failure; never a startup death).
func providerAPIKey(ctx context.Context, logger *slog.Logger, store *storage.Store, providerID string) string {
	tok, err := store.GetProviderToken(ctx, providerID, "access_token")
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			// A transient store failure degrades to "no key" rather than
			// failing construction — the error is loud in the log.
			logger.ErrorContext(ctx, "provider token read failed — routing without a key",
				"provider_id", providerID, "error", err)
		}
		return ""
	}
	return string(tok.Secret)
}

// webToolAccountsPresent returns a bool-closure over the R5 emulation
// default (httpapi.Params.HasWebToolAccounts): whether ANY stored account
// is a web_tool kind (firecrawl). The function is read at assembly and on
// every provider reload — a per-request store query is avoided (the
// emulation default must not add latency to every chat dispatch).
// A store failure reads as false — the emulation default degrades to
// native, never a request failure.
func webToolAccountsPresent(store *storage.Store) func() bool {
	return func() bool {
		rows, err := store.ListProviders(context.Background())
		if err != nil {
			return false
		}
		for _, row := range rows {
			if desc, isType := webui.ProviderTypeDescription(row.Type); isType && desc.Kind == webui.KindWebTool {
				return true
			}
		}
		return false
	}
}

// codexBaseURL is the ChatGPT backend root (X7/X19) every codex provider
// instance pins — the backend URL is not operator configuration.
const codexBaseURL = "https://chatgpt.com/backend-api"
