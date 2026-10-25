package server

// Live provider construction: at startup and on
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
//     backend), Auth client built against the issuer with the
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
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tubeof/proxyllm/internal/httpguard"
	"github.com/tubeof/proxyllm/internal/provider"
	"github.com/tubeof/proxyllm/internal/provider/codex"
	"github.com/tubeof/proxyllm/internal/provider/custom"
	"github.com/tubeof/proxyllm/internal/provider/ollamacloud"
	"github.com/tubeof/proxyllm/internal/routing"
	"github.com/tubeof/proxyllm/internal/server/webui"
	"github.com/tubeof/proxyllm/internal/storage"
	"github.com/tubeof/proxyllm/internal/tools/firecrawl"
	"github.com/tubeof/proxyllm/internal/tools/ollamaweb"
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
		if !row.Enabled {
			// A disabled account is not constructed at all: it leaves the
			// registry, and with it the type's chain expansion, the health
			// picture, the model sync and the web-tool emulator pool. Its
			// tokens and model rows stay in the store, so switching it back
			// on restores it whole (`server.SetProviderEnabled` reloads this
			// set through the admin API).
			continue
		}
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
// upstreamHeaderTimeoutMSKey is the operator's shared override for the
// chat upstreams' response-header wall (ADR-018). It lives in the settings
// KV like every other operator setting; a provider write re-applies it when
// the live set is reconstructed.
const upstreamHeaderTimeoutMSKey = "upstream_response_header_timeout_ms"

// resolveUpstreamHeaderTimeout reads the response-header override. A nil
// option means the shared five-minute default — absent, malformed, and
// non-positive values all degrade to it (the settings-read contract: store
// noise never changes how a provider is built).
func resolveUpstreamHeaderTimeout(ctx context.Context, store *storage.Store) httpguard.ClientOption {
	if store == nil {
		return nil
	}
	v, ok, err := store.GetConfigValue(ctx, upstreamHeaderTimeoutMSKey)
	if err != nil || !ok {
		return nil
	}
	ms, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || ms <= 0 {
		return nil
	}
	return httpguard.WithHeaderTimeout(time.Duration(ms) * time.Millisecond)
}

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

	// ADR-018: the header wall is operator-tunable, read per construction
	// (the live set is rebuilt on every provider write, so a settings change
	// applies on the next reload). nil = shared five-minute default.
	headerOpts := []httpguard.ClientOption{}
	if opt := resolveUpstreamHeaderTimeout(ctx, store); opt != nil {
		headerOpts = append(headerOpts, opt)
	}

	switch row.Type {
	case webui.ProviderTypeFirecrawl:
		// ADR-010 web_tool kind: the account IS an emulator backend. The
		// key rides the encrypted token store; a keyless row constructs
		// and fails at call time with the documented upstream error (the
		// same honest-state contract as keyless custom accounts).
		emu := firecrawl.NewEmulator(cfg.BaseURL, providerAPIKey(ctx, logger, store, row.ID), nil)
		return nil, emu, nil

	case webui.ProviderTypeOllamaCloudWeb:
		// The second web-tool backend (ADR-015): Ollama's hosted
		// web_search/web_fetch API. Same web_tool contract as Firecrawl —
		// credential from the encrypted token store, keyless rows construct
		// and fail at call time.
		emu := ollamaweb.NewEmulator(cfg.BaseURL, providerAPIKey(ctx, logger, store, row.ID), nil)
		return nil, emu, nil

	case webui.ProviderTypeCustomOpenAI:
		return custom.New(row.ID, cfg.BaseURL, providerAPIKey(ctx, logger, store, row.ID), capsFromConfig(cfg, row.Type), headerOpts...), nil, nil

	case webui.ProviderTypeOllamaCloud:
		// Empty base_url is a legal save: the provider defaults to the
		// hosted endpoint.
		return ollamacloud.NewWithLogger(row.ID, cfg.BaseURL, providerAPIKey(ctx, logger, store, row.ID), capsFromConfig(cfg, row.Type), logger, headerOpts...), nil, nil

	case webui.ProviderTypeCodexOAuth:
		// The codex provider speaks the ChatGPT backend; the
		// issuer from config drives the OAuth client. The auth
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
			// Model discovery announces the type's codex CLI version; empty
			// selects the provider's built-in pin.
			ClientVersion: cfg.ClientVersion,
			// Streaming transport with guarded redirects — the OAuth access
			// token rides the Bearer header on every dispatch.
			HTTP: httpguard.NewStreamingClient(headerOpts...),
			// ADR-012: the backend has no usage endpoint — live response
			// headers are the account's only usage source. The write runs
			// off the dispatch path (a snapshot races no request; last
			// write wins).
			RateLimitSink: func(rl codex.RateLimits) {
				go persistRateLimits(store, row.ID, rl)
			},
		}), nil, nil

	default:
		// ParseProviderConfig rejects unknown types, so this is a
		// construction-table drift, not operator input.
		return nil, nil, errors.New("no construction path for type " + row.Type)
	}
}

// persistRateLimits maps one codex rate-limit snapshot into the account's
// ADR-012 usage blob. The default family fills the legacy session/weekly
// mirrors; every family (including extra limit ids) and the credit balance
// become uniform windows. Unreported values stay zero/nil — the blob renders
// them as unknown, never invented. A storage failure drops the snapshot
// silently: usage display is not a dispatch concern, and the next response
// writes a fresh one anyway.
func persistRateLimits(store *storage.Store, providerID string, rl codex.RateLimits) {
	blob := storage.AccountUsageBlob{FetchedAt: time.Now().Unix()}
	families := rl.Families
	if len(families) == 0 && (rl.PrimaryUsedPct != nil || rl.SecondaryUsedPct != nil ||
		rl.PrimaryResetAt > 0 || rl.SecondaryResetAt > 0) {
		// Backward compatibility for callers that still fill only the
		// legacy mirrors (the pre-window shape).
		families = []codex.RateLimitFamily{{
			ID: "codex", Name: "Codex",
			Primary: &codex.RateLimitWindow{
				UsedPct: rl.PrimaryUsedPct, WindowMinutes: rl.PrimaryWindowMinutes,
				ResetAt: rl.PrimaryResetAt,
			},
			Secondary: &codex.RateLimitWindow{
				UsedPct: rl.SecondaryUsedPct, WindowMinutes: rl.SecondaryWindowMinutes,
				ResetAt: rl.SecondaryResetAt,
			},
		}}
	}
	addWindow := func(key, label string, w *codex.RateLimitWindow) {
		if w == nil {
			return
		}
		window := storage.UsageWindow{
			Key: key, Label: label, Kind: "percent",
			ResetAt: w.ResetAt, WindowSeconds: w.WindowMinutes * 60,
			Source: rl.Source,
		}
		if w.UsedPct != nil {
			used := clampUsagePct(*w.UsedPct)
			window.UsedPct = &used
			remaining := 100 - used
			window.RemainingPct = &remaining
		}
		if window.UsedPct == nil && window.ResetAt == 0 && window.WindowSeconds == 0 {
			return
		}
		blob.Windows = append(blob.Windows, window)
	}
	for _, fam := range families {
		prefix := "codex"
		if fam.ID != "" && fam.ID != "codex" {
			prefix = "codex." + fam.ID
		}
		addWindow(prefix+".primary", codexWindowLabel(fam, fam.Primary, "Session"), fam.Primary)
		addWindow(prefix+".secondary", codexWindowLabel(fam, fam.Secondary, "Weekly"), fam.Secondary)
	}
	if rl.Credits != nil {
		window := storage.UsageWindow{
			Key: "codex.credits", Label: "Credits", Kind: "credits", Unit: "credits",
			Unlimited: rl.Credits.Unlimited, Source: rl.Source,
		}
		if rl.Credits.Balance != nil {
			window.Remaining = rl.Credits.Balance
		} else if rl.Credits.Raw != "" {
			window.Note = rl.Credits.Raw
		}
		blob.Windows = append(blob.Windows, window)
	}
	// Legacy mirrors for the default family (the old session/weekly shape).
	for _, fam := range families {
		if fam.ID != "" && fam.ID != "codex" {
			continue
		}
		if fam.Primary != nil {
			blob.SessionPct = fam.Primary.UsedPct
			blob.SessionReset = fam.Primary.ResetAt
		}
		if fam.Secondary != nil {
			blob.WeeklyPct = fam.Secondary.UsedPct
			blob.WeeklyReset = fam.Secondary.ResetAt
		}
		break
	}
	if len(blob.Windows) == 0 && blob.SessionPct == nil && blob.WeeklyPct == nil &&
		blob.SessionReset == 0 && blob.WeeklyReset == 0 {
		return
	}
	_ = store.PutAccountUsage(context.Background(), providerID, blob)
}

// codexWindowLabel names a window from its length. The default family keeps
// the familiar Session/Weekly names; extra families prefix their own name.
func codexWindowLabel(fam codex.RateLimitFamily, w *codex.RateLimitWindow, fallback string) string {
	label := fallback
	if w != nil && w.WindowMinutes > 0 {
		switch {
		case w.WindowMinutes == 300:
			label = "5-hour"
		case w.WindowMinutes == 10080 || w.WindowMinutes == 7*24*60:
			label = "Weekly"
		case w.WindowMinutes%1440 == 0:
			label = fmt.Sprintf("%dd", w.WindowMinutes/1440)
		case w.WindowMinutes%60 == 0:
			label = fmt.Sprintf("%dh", w.WindowMinutes/60)
		default:
			label = fmt.Sprintf("%dm", w.WindowMinutes)
		}
	}
	if fam.Name != "" && fam.ID != "" && fam.ID != "codex" {
		return fam.Name + " · " + label
	}
	return label
}

func clampUsagePct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// modelVisionLookup is the router's model-row vision source (ADR-014
// learning): whether the row for (provider, wire
// model) has learned whether that model can see images.
//
// A missing row, an unreadable blob, or a row that simply never formed an
// opinion all report known=false — "no opinion". That is deliberately
// permissive: the router keeps such a candidate, so the no-pre-stream-gate
// behavior still holds for every model the sync has not classified. Callers
// never see a store error here; a read failure must not become a routing
// failure.
func modelVisionLookup(store *storage.Store) func(ctx context.Context, providerID, model string) (canSeeImages, known bool) {
	return func(ctx context.Context, providerID, model string) (bool, bool) {
		row, err := store.GetModelByProviderAndName(ctx, providerID, model)
		if err != nil {
			return false, false
		}
		return modelCapabilityFlag(row.Capabilities, "vision")
	}
}

// modelCapabilityFlag reads one boolean flag out of a model row's capabilities
// blob.
//
// Two-step on purpose: the blob carries a `pinned` OBJECT next to the boolean
// flags (the operator-override map ADR-014's learning writes and honors), so a
// plain map[string]bool decode fails outright on every row that has ever
// learned anything — which is exactly the rows this gate exists for.
func modelCapabilityFlag(raw json.RawMessage, name string) (value, known bool) {
	if len(raw) == 0 {
		return false, false
	}
	var blob map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blob); err != nil {
		return false, false
	}
	b, ok := blob[name]
	if !ok {
		return false, false
	}
	var v bool
	if err := json.Unmarshal(b, &v); err != nil {
		return false, false
	}
	return v, true
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
		case "decision":
			caps.Decision = on
		}
	}
	return caps
}

// defaultCaps is the baseline capability set per provider type — what a
// freshly saved provider of this class can do before the operator pins
// anything. Web-search/fetch/code-execution/thinking stay off by default:
// capability routing sends such requests to providers that truly support
// them, and Firecrawl emulation covers the gap.
func defaultCaps(typ string) provider.Capabilities {
	switch typ {
	case webui.ProviderTypeCodexOAuth:
		// The ChatGPT backend speaks Responses-shaped requests with
		// streaming and tools; thinking is Claude-only
		// semantics, so it stays off here.
		return provider.Capabilities{Streaming: true, Tools: true}
	case webui.ProviderTypeOllamaCloud:
		// The hosted API serves the decision surface (System One / Jev at
		// /v1/systemone — clef:cloud) with the same account credential the
		// chat path uses, so the flag is on by default.
		return provider.Capabilities{Streaming: true, Tools: true, Decision: true}
	default:
		// custom-openai: a conservative, honest default — streaming and
		// tools are the OpenAI-compat baseline; the operator pins
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

// codexBaseURL is the ChatGPT backend root every codex provider
// instance pins — the backend URL is not operator configuration.
const codexBaseURL = "https://chatgpt.com/backend-api"
