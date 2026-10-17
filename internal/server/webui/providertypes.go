package webui

// Provider type catalog (ADR-009): the static metadata the Providers page
// renders its type cards and per-type settings forms from, and the account
// editor builds its inputs from. Adding a provider type = one entry here +
// one parseProviderConfig case + one buildProvider case — the WebUI needs
// no code change (it renders whatever the catalog describes).

// fieldKind is the input kind the settings form renders for a field.
type fieldKind string

const (
	fieldKindText fieldKind = "text"
	fieldKindURL  fieldKind = "url"
)

// fieldDesc describes one form field of a provider type's settings or
// account config.
type fieldDesc struct {
	// Key is the config-blob key the input writes.
	Key string `json:"key"`
	// Label is the human-facing input label.
	Label string `json:"label"`
	// Kind selects the input type (text/url).
	Kind fieldKind `json:"kind"`
	// Required marks inputs the save refuses when empty.
	Required bool `json:"required"`
	// Hint is the per-field explanation under the input.
	Hint string `json:"hint"`
}

// providerTypeDesc is one catalog entry.
type providerTypeDesc struct {
	// Type is the canonical type string (providers.type column value,
	// parseProviderConfig's case).
	Type string `json:"type"`
	// Label is the display name on the card.
	Label string `json:"label"`
	// Description is the card's one-liner.
	Description string `json:"description"`
	// Kind is what the account serves (ADR-010): "llm" (routable chat
	// models — chain entries) or "web_tool" (an emulator backend — the
	// web_search/web_fetch fallback lists). web_tool accounts are never
	// LLM-chain-routable and vice versa.
	Kind string `json:"kind"`
	// Auth is how accounts authenticate: "api_key" (form input) or "oauth"
	// (device-flow sign-in button).
	Auth string `json:"auth"`
	// SettingsFields are the per-type shared settings inputs (rendered from
	// provider_type_settings).
	SettingsFields []fieldDesc `json:"settings_fields"`
	// AccountFields are the per-account config inputs (rendered from each
	// account's own config blob; type settings fill the same keys first).
	AccountFields []fieldDesc `json:"account_fields"`
	// DefaultCapabilities are the type's baseline pins (server/providers.go
	// defaultCaps) so the settings form can pre-check the boxes. nil for
	// non-LLM kinds.
	DefaultCapabilities map[string]bool `json:"default_capabilities"`
}

// Provider kinds (ADR-010).
const (
	// KindLLM marks routable chat-model providers (chain entries).
	KindLLM = "llm"
	// KindWebTool marks web-tool emulator providers (web fallback lists).
	KindWebTool = "web_tool"
)

// providerTypes is the catalog — order is the card order on the Providers
// page. The Type values match ProviderTypeCustomOpenAI/OllamaCloud/
// CodexOAuth exactly (parseProviderConfig rejects everything else).
var providerTypes = []providerTypeDesc{
	{
		Type:        ProviderTypeCustomOpenAI,
		Label:       "Custom OpenAI-compatible",
		Description: "Any OpenAI-compatible chat-completions endpoint: base URL plus API key.",
		Kind:        KindLLM,
		Auth:        "api_key",
		SettingsFields: []fieldDesc{
			{Key: "base_url", Label: "Base URL", Kind: fieldKindURL, Required: true,
				Hint: "The API root including the version prefix, e.g. https://api.example.com/v1"},
			{Key: "model_prefix", Label: "Model prefix", Kind: fieldKindText, Required: false,
				Hint: "Model identity prefix for this type, e.g. \"myhost\" → \"myhost/glm-5.3\". Empty = the type string (\"custom-openai\")."},
		},
		AccountFields:       nil, // the account is its key; settings carry the URL
		DefaultCapabilities: map[string]bool{"streaming": true, "tools": true},
	},
	{
		Type:        ProviderTypeOllamaCloud,
		Label:       "Ollama Cloud",
		Description: "The hosted Ollama API at ollama.com via its OpenAI-compatible /v1 endpoint. Multiple API keys = multiple accounts.",
		Kind:        KindLLM,
		Auth:        "api_key",
		SettingsFields: []fieldDesc{
			{Key: "base_url", Label: "Base URL", Kind: fieldKindURL, Required: false,
				Hint: "Optional override; defaults to https://ollama.com (the /v1 version prefix rides the endpoint paths)"},
			{Key: "model_prefix", Label: "Model prefix", Kind: fieldKindText, Required: false,
				Hint: "Model identity prefix for this type, e.g. \"ollamacloud\" → \"ollamacloud/glm-5.3\". Empty = the type string (\"ollama-cloud\")."},
		},
		AccountFields:       nil,
		DefaultCapabilities: map[string]bool{"streaming": true, "tools": true, "decision": true},
	},
	{
		Type:        ProviderTypeCodexOAuth,
		Label:       "ChatGPT / Codex",
		Description: "ChatGPT backend via device-flow sign-in (RFC 8628). Multiple sign-ins = multiple accounts.",
		Kind:        KindLLM,
		Auth:        "oauth",
		SettingsFields: []fieldDesc{
			{Key: "issuer", Label: "OAuth issuer", Kind: fieldKindURL, Required: true,
				Hint: "e.g. https://auth.openai.com — the issuer all accounts of this type sign in against"},
			{Key: "model_prefix", Label: "Model prefix", Kind: fieldKindText, Required: false,
				Hint: "Model identity prefix for this type. Empty = the type string (\"codex-oauth\")."},
		},
		AccountFields:       nil, // tokens arrive via the device flow, not a form
		DefaultCapabilities: map[string]bool{"streaming": true, "tools": true},
	},
	{
		Type:        ProviderTypeFirecrawl,
		Label:       "Firecrawl",
		Description: "Web-tool backend powering emulated web_search / web_fetch (server-tool fallback). Not a chat model provider.",
		Kind:        KindWebTool,
		Auth:        "api_key",
		SettingsFields: []fieldDesc{
			{Key: "base_url", Label: "Base URL", Kind: fieldKindURL, Required: false,
				Hint: "Optional override; defaults to https://api.firecrawl.dev (self-hosted instances set their root here)"},
		},
		AccountFields:       nil,
		DefaultCapabilities: nil, // web-tool accounts carry no chat capabilities
	},
	{
		Type:        ProviderTypeOllamaCloudWeb,
		Label:       "Ollama Cloud (Web Tools)",
		Description: "Ollama's hosted web-tool API at ollama.com/api powering emulated web_search / web_fetch. Not a chat model provider — a separate account type from Ollama Cloud's chat endpoint.",
		Kind:        KindWebTool,
		Auth:        "api_key",
		SettingsFields: []fieldDesc{
			{Key: "base_url", Label: "Base URL", Kind: fieldKindURL, Required: false,
				Hint: "Optional override; defaults to https://ollama.com (the endpoint paths /api/web_search and /api/web_fetch ride the root)"},
		},
		AccountFields:       nil,
		DefaultCapabilities: nil, // web-tool accounts carry no chat capabilities
	},
}

// ProviderTypeDescription returns the catalog entry for a canonical type
// (exported for the server package's emulation-presence check; the admin
// API's internal uses keep the unexported alias).
func ProviderTypeDescription(typ string) (providerTypeDesc, bool) {
	return providerTypeByID(typ)
}

// providerTypeByID returns the catalog entry for a canonical type.
func providerTypeByID(typ string) (providerTypeDesc, bool) {
	for _, pt := range providerTypes {
		if pt.Type == typ {
			return pt, true
		}
	}
	return providerTypeDesc{}, false
}
