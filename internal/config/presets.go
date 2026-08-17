package config

import "sort"

// Provider presets: the endpoints "add a provider" can fill in for you.
//
// Every field here is something a person would otherwise have to look up in a
// vendor's docs and get exactly right — host, the path prefix in front of /v1,
// which header carries the key. A wrong basePath fails as a 404 from an
// endpoint that plainly exists, which is the least diagnosable way to be wrong,
// so this table is the cure for the most common way adding a provider goes bad.
//
// A preset is a STARTING POINT, never a constraint: the form fills from it and
// every field stays editable, so a vendor moving a path costs an edit rather
// than a release. "Custom endpoint" is the absence of a preset, not a special
// case.
//
// VERIFIED 2026-08-15 by probing each `https://<host><basePath>/v1/models`:
// every entry below answered 200 (public catalogue) or 401/403 (route exists,
// wants a key). Re-probe before trusting one that starts failing — a vendor
// silently moving its base path is exactly the failure this table absorbs.
//
// Deliberately ABSENT, both for the same reason — corrallm builds
// `basePath + "/v1/..."`, and neither endpoint's OpenAI surface can be reached
// that way. These are not missing rows; they are why the API field exists
// before anything reads it.
//
//   Google Gemini — rooted at /v1beta/openai/, so the models list is
//   /v1beta/openai/models. No basePath produces that.
//
//   Z.ai (Zhipu GLM) — OpenAI-compatible at /api/paas/V4/. basePath
//   "/api/paas" yields /api/paas/v1/..., which EXISTS but is the legacy
//   surface: probed 2026-08-17, an unauthenticated POST to
//   /api/paas/v1/chat/completions answers HTTP 200 with
//   {"code":1001,...,"success":false} — not an OpenAI error shape. That is
//   worse than a 404, because a 200 reads as success and the body would be
//   parsed as a completion. Adding it as a preset would hand someone a
//   provider that fails silently.

// PresetAPIOpenAI marks an endpoint that speaks OpenAI's wire format at
// <basePath>/v1/*. It is the only value the proxy handles today; the field
// exists so a future adapter (Anthropic Messages, Gemini generateContent) is a
// new value and a translation layer, not a schema migration.
const PresetAPIOpenAI = "openai"

// Catalog kinds: what it takes to enumerate a provider's models.
const (
	// CatalogPublic — /v1/models answers without credentials, so the catalogue
	// can be browsed BEFORE a key is stored. Only OpenRouter and DeepInfra.
	CatalogPublic = "public"
	// CatalogAuthed — the route exists but wants a key. Browsing has to wait
	// until a credential is saved, and the UI says so rather than showing an
	// empty table that looks like a broken provider.
	CatalogAuthed = "authed"
)

// PresetFilter is the discovery filter a preset suggests. A catalogue of 300
// models with no filter is not a starting point, it is a hazard: the defaults
// here are what makes "pick a provider, save" a safe thing to do.
type PresetFilter struct {
	Free       bool
	MinContext int
	Limit      int
}

// ProviderPreset is one known endpoint.
type ProviderPreset struct {
	ID    string
	Label string
	// Group buckets the typeahead: aggregators, labs, local. Purely
	// presentational, but a flat list of twenty endpoints is a list nobody
	// reads.
	Group    string
	Host     string
	Port     int
	BasePath string
	API      string
	// AuthHeader/AuthScheme describe how the key is presented. Nearly always
	// "authorization" + "Bearer"; named per preset because the exceptions are
	// what cost an afternoon.
	AuthHeader string
	AuthScheme string
	// SecretRef is the suggested credential-store entry name, so two accounts
	// of one provider get obviously-different names instead of KEY and KEY2.
	SecretRef string
	// NeedsSecret is false for local endpoints, which take no key at all — and
	// a form that demands one for llama.cpp is a form people work around.
	NeedsSecret bool
	Catalog     string
	Docs        string
	Notes       string
	// PickByHand says this provider's catalogue is too large to enrol by
	// filter. A filter over four hundred models is a guess, and the ones it
	// guesses wrong are billable — so the add form starts these on "choose them
	// yourself" and the suggested filter below is only what the DIRECTORY
	// pre-filters to, not what gets imported.
	PickByHand bool
	Filter     PresetFilter
}

// presets is the table. Ordered by group then label at read time, never here —
// source order is for reading, not for the UI.
var presets = []ProviderPreset{
	// --- Aggregators and inference clouds: large catalogues, the best fit for
	// browse-and-approve.
	{
		ID: "openrouter", PickByHand: true, Label: "OpenRouter", Group: "aggregator",
		Host: "openrouter.ai", Port: 443, BasePath: "/api",
		SecretRef: "OPENROUTER_API_KEY", Catalog: CatalogPublic,
		Docs:  "https://openrouter.ai/docs",
		Notes: "300+ models with pricing and a churning free tier. Catalogue is public — browse it before storing a key.",
		// Free + a minimum window as the DIRECTORY's opening filter. Not an
		// import rule: enrolling by filter here means a guess over hundreds of
		// models, several of which bill.
		Filter: PresetFilter{Free: true, MinContext: 8192, Limit: 12},
	},
	{
		ID: "groq", Label: "Groq", Group: "aggregator",
		Host: "api.groq.com", Port: 443, BasePath: "/openai",
		SecretRef: "GROQ_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://console.groq.com/docs",
		Notes:  "Serves OpenAI's surface under /openai, not at the root.",
		Filter: PresetFilter{MinContext: 8192, Limit: 12},
	},
	{
		ID: "cerebras", Label: "Cerebras", Group: "aggregator",
		Host: "api.cerebras.ai", Port: 443,
		SecretRef: "CEREBRAS_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://inference-docs.cerebras.ai",
		Filter: PresetFilter{MinContext: 8192, Limit: 12},
	},
	{
		ID: "together", PickByHand: true, Label: "Together AI", Group: "aggregator",
		Host: "api.together.xyz", Port: 443,
		SecretRef: "TOGETHER_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://docs.together.ai",
		Filter: PresetFilter{MinContext: 8192, Limit: 12},
	},
	{
		ID: "fireworks", PickByHand: true, Label: "Fireworks AI", Group: "aggregator",
		Host: "api.fireworks.ai", Port: 443, BasePath: "/inference",
		SecretRef: "FIREWORKS_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://docs.fireworks.ai",
		Notes:  "OpenAI surface lives under /inference.",
		Filter: PresetFilter{MinContext: 8192, Limit: 12},
	},
	{
		ID: "deepinfra", PickByHand: true, Label: "DeepInfra", Group: "aggregator",
		Host: "api.deepinfra.com", Port: 443,
		SecretRef: "DEEPINFRA_API_KEY", Catalog: CatalogPublic,
		Docs:   "https://deepinfra.com/docs",
		Notes:  "Their docs give the base URL as /v1/openai; /v1 serves the same OpenAI surface and is the shape this proxy builds.",
		Filter: PresetFilter{MinContext: 8192, Limit: 12},
	},
	{
		ID: "nebius", PickByHand: true, Label: "Nebius AI Studio", Group: "aggregator",
		Host: "api.studio.nebius.com", Port: 443,
		SecretRef: "NEBIUS_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://docs.nebius.com/studio/inference",
		Filter: PresetFilter{MinContext: 8192, Limit: 12},
	},
	{
		ID: "hyperbolic", PickByHand: true, Label: "Hyperbolic", Group: "aggregator",
		Host: "api.hyperbolic.xyz", Port: 443,
		SecretRef: "HYPERBOLIC_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://docs.hyperbolic.xyz",
		Filter: PresetFilter{MinContext: 8192, Limit: 12},
	},

	{
		ID: "qwen-intl", PickByHand: true, Label: "Qwen / DashScope (international)", Group: "aggregator",
		Host: "dashscope-intl.aliyuncs.com", Port: 443, BasePath: "/compatible-mode",
		SecretRef: "DASHSCOPE_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://www.alibabacloud.com/help/en/model-studio/compatibility-of-openai-with-dashscope",
		Notes:  "Alibaba Model Studio. OpenAI surface under /compatible-mode. Qwen models are periodically offered free — the catalogue reports no pricing, so a free filter cannot see that; check their console.",
		Filter: PresetFilter{Limit: 12},
	},
	{
		ID: "qwen-cn", PickByHand: true, Label: "Qwen / DashScope (China)", Group: "aggregator",
		Host: "dashscope.aliyuncs.com", Port: 443, BasePath: "/compatible-mode",
		SecretRef: "DASHSCOPE_API_KEY_CN", Catalog: CatalogAuthed,
		Docs:   "https://help.aliyun.com/zh/model-studio/developer-reference/compatibility-of-openai-with-dashscope",
		Notes:  "The China-region endpoint of the same service; keys are NOT interchangeable with the international one.",
		Filter: PresetFilter{Limit: 12},
	},

	// --- First-party labs: small catalogues, real money.
	{
		ID: "openai", PickByHand: true, Label: "OpenAI", Group: "lab",
		Host: "api.openai.com", Port: 443,
		SecretRef: "OPENAI_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://platform.openai.com/docs",
		Notes:  "The catalogue includes embedding, audio and image models — filter, or approve by hand.",
		Filter: PresetFilter{Limit: 12},
	},
	{
		ID: "anthropic", Label: "Anthropic", Group: "lab",
		Host: "api.anthropic.com", Port: 443,
		SecretRef: "ANTHROPIC_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://docs.claude.com/en/api/openai-sdk",
		Notes:  "Via Anthropic's OpenAI-compatible layer at /v1/chat/completions. The native Messages API is a different surface and is not what this reaches.",
		Filter: PresetFilter{Limit: 12},
	},
	{
		ID: "deepseek", Label: "DeepSeek", Group: "lab",
		Host: "api.deepseek.com", Port: 443,
		SecretRef: "DEEPSEEK_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://api-docs.deepseek.com",
		Filter: PresetFilter{Limit: 12},
	},
	{
		ID: "mistral", Label: "Mistral", Group: "lab",
		Host: "api.mistral.ai", Port: 443,
		SecretRef: "MISTRAL_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://docs.mistral.ai",
		Filter: PresetFilter{Limit: 12},
	},
	{
		ID: "xai", Label: "xAI (Grok)", Group: "lab",
		Host: "api.x.ai", Port: 443,
		SecretRef: "XAI_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://docs.x.ai",
		Filter: PresetFilter{Limit: 12},
	},
	{
		ID: "perplexity", Label: "Perplexity", Group: "lab",
		Host: "api.perplexity.ai", Port: 443,
		SecretRef: "PERPLEXITY_API_KEY", Catalog: CatalogAuthed,
		Docs:   "https://docs.perplexity.ai",
		Filter: PresetFilter{Limit: 12},
	},

	// --- Local and self-hosted: no key, and the port is the interesting field.
	{
		ID: "ollama", Label: "Ollama", Group: "local",
		Host: "127.0.0.1", Port: 11434, NeedsSecret: false,
		Catalog: CatalogPublic, Docs: "https://docs.ollama.com/openai",
		Notes:  "Point host at the machine running it — 127.0.0.1 only works if that is this box.",
		Filter: PresetFilter{Limit: 0},
	},
	{
		ID: "lmstudio", Label: "LM Studio", Group: "local",
		Host: "127.0.0.1", Port: 1234, NeedsSecret: false,
		Catalog: CatalogPublic, Docs: "https://lmstudio.ai/docs/app/api",
		Filter: PresetFilter{Limit: 0},
	},
	{
		ID: "vllm", Label: "vLLM", Group: "local",
		Host: "127.0.0.1", Port: 8000, NeedsSecret: false,
		Catalog: CatalogPublic, Docs: "https://docs.vllm.ai",
		Filter: PresetFilter{Limit: 0},
	},
	{
		ID: "llamacpp", Label: "llama.cpp server", Group: "local",
		Host: "127.0.0.1", Port: 8080, NeedsSecret: false,
		Catalog: CatalogPublic, Docs: "https://github.com/ggml-org/llama.cpp/tree/master/tools/server",
		Notes:  "One server usually serves ONE model. corrallm's own backends already cover this — reach for it when another box is running llama.cpp you do not manage.",
		Filter: PresetFilter{Limit: 0},
	},
}

// ProviderPresets returns the table, group- then label-ordered, with the
// defaults every entry omits filled in. Callers get a complete record so no
// consumer has to re-implement "blank means Bearer".
func ProviderPresets() []ProviderPreset {
	out := make([]ProviderPreset, 0, len(presets))
	for _, p := range presets {
		if p.API == "" {
			p.API = PresetAPIOpenAI
		}
		if p.AuthHeader == "" {
			p.AuthHeader = "authorization"
		}
		if p.AuthScheme == "" {
			p.AuthScheme = "Bearer"
		}
		if p.Port == 0 {
			p.Port = 443
		}
		// Every hosted preset names a secret; the local ones name none and mean
		// it. Deriving the flag from that keeps the two from disagreeing.
		p.NeedsSecret = p.SecretRef != ""
		out = append(out, p)
	}
	groupRank := map[string]int{"aggregator": 0, "lab": 1, "local": 2}
	sort.SliceStable(out, func(i, j int) bool {
		gi, gj := groupRank[out[i].Group], groupRank[out[j].Group]
		if gi != gj {
			return gi < gj
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// ProviderPresetByID finds one, or false.
func ProviderPresetByID(id string) (ProviderPreset, bool) {
	for _, p := range ProviderPresets() {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderPreset{}, false
}
