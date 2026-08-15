package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Config is the corrallm domain config (the YAML file). It is the scheduler's
// declarative source of truth: served models → ordered backends, server
// capacity, priority groups, and the cost model. P0 parses and validates the
// shape; later phases give each section behavior.
//
// The schema mirrors ~/doc/plan/corrallm.md §3. Sections not yet consumed are
// still parsed so configs are forward-compatible and round-trippable.
type Config struct {
	// Include names further config files to merge into this one, relative to
	// THIS file's directory. Earlier entries load first; a later file's entry
	// wins a collision, and the including file wins over everything it includes
	// — the operator's own file is always the last word.
	//
	// This exists so corrallm can WRITE config without touching a file a human
	// owns. corrallm.yaml is dense with hard-won commentary; round-tripping it
	// through a YAML marshaller would silently delete all of it. A generated
	// file included from here is machine-owned end to end, and the hand-written
	// file stays hand-written.
	//
	// One level only: an included file may not itself include. Nesting buys
	// nothing here and costs a cycle detector plus an ordering rule nobody can
	// hold in their head.
	Include []string `yaml:"include,omitempty"`

	// CostPerKwh converts local energy → $ (cost model, P6).
	CostPerKwh float64 `yaml:"costPerKwh,omitempty"`

	// Limits budget the WHOLE box, whatever the caller or backend. The
	// blast-radius guard: "this machine never spends more than $50/day
	// regardless of what is misconfigured below". Every narrower scope can be
	// got wrong — a template that multiplies, a provider added without a cap —
	// and this is the one that cannot be routed around.
	Limits LimitSet `yaml:"limits,omitempty"`

	// CommandCosts maps a backend `type` to its cost parameters.
	CommandCosts map[string]map[string]any `yaml:"commandCosts,omitempty"`

	// Servers declares host capacity as a vector over named memory pools.
	Servers map[string]Server `yaml:"servers,omitempty"`

	// Convert is the GLOBAL default for attachment ingestion (PDFs in chat). A
	// model's own Convert block overrides it field-by-field. Empty = built-in
	// defaults (extract text).
	Convert ConvertConfig `yaml:"convert,omitempty"`

	// Extensions are integrations that serve SEVERAL model names from ONE
	// process. oidio is the motivating case: it provides stt, diarized stt, tts
	// and realtime-stt on a single port.
	//
	// Modelling those as four independent models was wrong in a way that only
	// showed up under failure. Three of them were pure proxies at the fourth's
	// port, so they had no lifecycle of their own: kill the process and the
	// three aliases returned 502 forever, because a proxy has no cmd and cannot
	// spawn the thing it depends on. Only the one privileged member could
	// revive it. They also each carried separate residency accounting for
	// memory that is allocated exactly once.
	//
	// An extension makes the sharing explicit: one cmd, one process, one
	// reservation, and every model it provides loads and unloads with it.
	Extensions map[string]Extension `yaml:"extensions,omitempty"`

	// Models maps a served model name → exactly one serving path (a spawned cmd
	// or a proxy target) + residency policy. Fallback across models is a lane.
	Models map[string]Model `yaml:"models,omitempty"`

	// Lanes are named, ordered fallback lists over model names. Requesting a
	// lane name allows substitution across its members (walked best-quality
	// first, gated by the caller group's acceptDegrade/qualityFloor); requesting
	// a model name pins exactly that model.
	Lanes map[string]Lane `yaml:"lanes,omitempty"`

	// PriorityGroups bundle scheduling policy; a key maps to exactly one group.
	PriorityGroups map[string]PriorityGroup `yaml:"priorityGroups,omitempty"`

	// Keys maps a caller identity → priorityGroup name.
	Keys map[string]string `yaml:"keys,omitempty"`

	// UnknownKeys is the policy for callers this config has never heard of.
	//
	// It was implicit and therefore unstateable: an unrecognized key silently
	// resolved to "default", which is a POLICY (accept anyone at weight 1)
	// wearing the clothes of a fallback. Writing it down makes it reviewable,
	// and makes "should a stranger be served at all" a question the operator
	// answers rather than one the code answers by omission.
	UnknownKeys UnknownKeyPolicy `yaml:"unknownKeys,omitempty"`

	// Scheduler holds global admission knobs (queue bounds).
	Scheduler SchedulerConfig `yaml:"scheduler,omitempty"`

	// discovered holds models contributed at runtime by a provider's `discover`
	// block, keyed by served name. Guarded because the refresh loop writes it
	// while requests read it. Never populated by Load — the file is the static
	// truth, this is the live addition to it.
	mu         sync.RWMutex
	discovered map[string]Model
	// discoveredBy records WHICH credentials saw each discovered model.
	//
	// Two accounts of one provider do not see the same catalogue — OpenRouter's
	// /v1/models differs by key (free tier vs paid vs BYOK) — so a union would
	// offer a model on a credential that cannot serve it, and every request
	// routed there would 404. model → set of credential names.
	discoveredBy map[string]map[string]bool
	// selections are the models an operator chose off a provider's directory,
	// with where they put them. See selection.go for why they share the
	// discovered overlay rather than living beside it.
	selections []Selection
	// fetched is what each refresh last contributed, keyed
	// provider\x00credential. Kept SEPARATELY from `discovered` because
	// `discovered` is a derived union of this and the selections, and a union
	// cannot be un-merged: without the inputs there was no way to withdraw one
	// contributor without also withdrawing the other's.
	fetched map[string]map[string]Model
}

// SetDiscovered replaces the models contributed by one provider. Replacing
// wholesale (rather than merging) is what lets a model that has churned OUT of
// the provider's free set disappear on the next pass.
//
// Equivalent to SetDiscoveredFor with the provider's implicit single credential.
func (c *Config) SetDiscovered(provider string, models map[string]Model) {
	c.SetDiscoveredFor(provider, DefaultCredentialName, models)
}

// SetDiscoveredFor replaces what ONE CREDENTIAL of a provider contributes.
//
// Per credential rather than per provider because the catalogues genuinely
// differ: a free-tier key and a paid key of the same account see different
// model lists. Recording the union would offer a model on a credential that
// cannot serve it, and the walk would route there and 404 — a failure that
// looks like the provider being flaky rather than a config error.
//
// A model several credentials can serve keeps ONE served name, backed by
// several candidates; expandCredentials emits only the ones that saw it.
func (c *Config) SetDiscoveredFor(provider, credential string, models map[string]Model) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetched == nil {
		c.fetched = map[string]map[string]Model{}
	}
	c.fetched[provider+"\x00"+credential] = models
	c.rebuildDiscoveredLocked()
}

// rebuildDiscoveredLocked recomputes the runtime overlay from its two inputs:
// what refreshes fetched, and what the operator selected.
//
// A full rebuild rather than an incremental patch, because `discovered` is a
// UNION and a union cannot be un-merged. Withdrawing one contributor
// incrementally means knowing which of them put each entry there, and getting
// that bookkeeping subtly wrong is invisible — it looks like a model that
// stayed a bit too long, or vanished a bit too early. Recomputing is a few
// hundred map writes on a path that runs at refresh interval, and it is
// obviously correct.
//
// Requires c.mu held for writing.
func (c *Config) rebuildDiscoveredLocked() {
	c.discovered = make(map[string]Model, len(c.discovered))
	c.discoveredBy = make(map[string]map[string]bool, len(c.discoveredBy))
	for key, models := range c.fetched {
		credential := key[strings.Index(key, "\x00")+1:]
		for name, m := range models {
			// A declared model always wins: discovery must never silently
			// redefine something the operator wrote down.
			if _, static := c.Models[name]; static {
				continue
			}
			c.discovered[name] = m
			if c.discoveredBy[name] == nil {
				c.discoveredBy[name] = map[string]bool{}
			}
			c.discoveredBy[name][credential] = true
		}
	}
	// Selections go on top: a chosen model outranks the same name arriving from
	// a filter, because the operator named an exact upstream id and the filter
	// only matched a pattern.
	c.applySelectionsLocked()
}

// DiscoveredServableBy reports whether a discovered model is reachable with the
// named credential.
func (c *Config) DiscoveredServableBy(model, credential string) bool {
	return c.discoveredServableBy(model, credential)
}

// discoveredServableBy reports whether a discovered model is reachable with the
// named credential. A model that is not discovered at all (declared, or a glob
// template) is unconstrained — this only narrows what discovery contributed.
func (c *Config) discoveredServableBy(model, credential string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	by, ok := c.discoveredBy[model]
	if !ok {
		return true
	}
	return by[credential]
}

// Discovered returns a copy of the runtime-contributed models.
func (c *Config) Discovered() map[string]Model {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]Model, len(c.discovered))
	for k, v := range c.discovered {
		out[k] = v
	}
	return out
}

// AllModels is the served registry: everything declared in the file plus
// whatever discovery has contributed. Use this anywhere the full catalog is
// meant (listings, capability views); c.Models alone omits discovered models.
func (c *Config) AllModels() map[string]Model {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]Model, len(c.Models)+len(c.discovered))
	for k, v := range c.Models {
		out[k] = v
	}
	for k, v := range c.discovered {
		if _, static := out[k]; !static {
			out[k] = v
		}
	}
	return out
}

// SchedulerConfig bounds queueing so saturated callers get a fast, informative
// 429 to shape against instead of a long blocking wait (the llama-swap fork's
// maxWait / maxQueueDepth contract). Zero values keep the prior behavior:
// MaxWait 0 → bounded only by the request context; MaxQueueDepth 0 → unbounded.
type SchedulerConfig struct {
	// MaxWait bounds how long a caller may sit in the queue before being handed
	// a 429 with a real Retry-After.
	//
	// It is not only a fairness knob: it is what makes "queued" distinguishable
	// from "hung" to a CLIENT. In-queue time inside an accepted request is
	// invisible from outside — it just looks like a slow response — so without
	// a bound, silence has no ceiling and no caller-side stall detection can
	// work. llm-bench's stall guard is derived from this value
	// (TestStallTimeoutClearsTheQueueBound); raising it without raising that
	// makes the bench kill healthy queued requests.
	MaxWait       string `yaml:"maxWait,omitempty"`       // e.g. "15s": queue wait before a 429
	MaxQueueDepth int    `yaml:"maxQueueDepth,omitempty"` // reject once this many already wait on a backend
}

// Extension is an integration corrallm hosts. The concept is deliberately OPEN:
// today an extension is a process that contributes models (`provides`), but the
// shape is meant to grow — a request/response listener or transformer, for
// instance, would be another capability declared on the same block rather than a
// parallel concept. Keep additions as new optional capability fields; do not
// narrow the type's meaning to "a process that serves models".
//
// It carries exactly the fields that describe a local lifecycle; everything
// per-model (type, quality, maxConcurrent, modalities) stays on the models it
// provides.
//
// RAMUsage is the whole extension's footprint, counted ONCE no matter how many
// of its models are in play — which is the honest accounting, since they are the
// same resident bytes.
type Extension struct {
	Cmd        string            `yaml:"cmd,omitempty"`
	Server     string            `yaml:"server,omitempty"`
	RAMUsage   map[string]string `yaml:"ramUsage,omitempty"`
	Swap       *Swap             `yaml:"swap,omitempty"`
	Proxy      yaml.Node         `yaml:"proxy,omitempty"` // the port every provided model forwards to
	Persistent bool              `yaml:"persistent,omitempty"`
	Sticky     *Sticky           `yaml:"sticky,omitempty"`

	// Notes is free-text kept ABOUT this entry, carried in config and shown in
	// the UI beside it. See Model.Notes.
	Notes string `yaml:"notes,omitempty"`

	// Providers lets ONE extension span several upstreams, each with its own
	// endpoint and credentials. The free-tier aggregator is the motivating case:
	// "free" is a single integration, but Groq, Cerebras and OpenRouter are three
	// hosts with three keys, so they cannot share a proxy target.
	//
	// Served names are "<provider>-<key>", so the provider — not the extension —
	// is the prefix. Mutually exclusive with the extension-level proxy/provides
	// pair, which is the shorthand for the common single-provider case (oidio,
	// claude) and behaves exactly as if the extension declared one provider named
	// after itself.
	Providers map[string]Provider `yaml:"providers,omitempty"`

	// Provides declares the models this extension serves. The KEY is the id the
	// backend knows the model by; the SERVED name is "<extension>-<key>", so
	// oidio's `stt-diarize` is served as `oidio-stt-diarize` and the upstream
	// rewrite is implied rather than repeated.
	//
	// The models are created here because the extension is what creates them —
	// declaring them separately in `models:` and pointing each back at the
	// extension duplicated the relationship in both directions and let the two
	// disagree.
	//
	// Values carry only per-model fields (type, quality, maxConcurrent,
	// modalities …). Lifecycle belongs to the extension.
	Provides map[string]Model `yaml:"provides,omitempty"`
}

// Provider is one upstream within an extension: an endpoint, its credentials,
// and the models reached through it.
type Provider struct {
	Proxy    yaml.Node        `yaml:"proxy,omitempty"`
	Provides map[string]Model `yaml:"provides,omitempty"`

	// Credentials are the accounts held against this endpoint. Empty means one
	// implicit credential using the provider's own proxy headers, which is
	// every config written before this existed — see CredentialList.
	Credentials []Credential `yaml:"credentials,omitempty"`

	// Limits budget every credential of this provider TOGETHER: "all my
	// OpenRouter keys combined stay under $X". A per-credential budget cannot
	// express that, and N per-credential budgets multiply instead of capping.
	Limits LimitSet `yaml:"limits,omitempty"`

	// Discover contributes models from the provider's own catalog instead of a
	// hand-written list. `provides` is a static declaration; this is the same
	// thing enumerated at runtime, for a roster that churns.
	Discover *Discover `yaml:"discover,omitempty"`

	// Manual declares that this provider's models are chosen ONE AT A TIME from
	// its directory (see selection.go) rather than admitted by a filter or
	// written out in `provides`.
	//
	// This is the right setting for a provider with a large catalogue — nobody
	// wants OpenRouter's four hundred models enrolled by a filter that guessed.
	//
	// It is a flag rather than an inference because the alternative is to stop
	// rejecting providers that contribute nothing, and "contributes no models"
	// catches a real class of typo — a provider block that was half-written.
	// Writing the intent down keeps that check while making the third way of
	// contributing legal. Nothing reads it at runtime: the selections do the
	// work, and this only says they are expected.
	Manual bool `yaml:"manual,omitempty"`
}

// Discover enumerates a provider's /v1/models and contributes the rows that
// pass Filter, each shaped by Template.
//
// Kept OUT of config load: Load must stay pure and offline, so discovery runs on
// the roster refresh loop and lands in a dynamic overlay. A provider that
// declares discover but has never refreshed contributes nothing rather than
// blocking startup.
type Discover struct {
	Filter   DiscoverFilter `yaml:"filter,omitempty"`
	Template Model          `yaml:"template,omitempty"`
	// Limit caps how many models one provider may contribute (0 = unlimited),
	// ordered by context length descending so a cap keeps the largest.
	Limit int `yaml:"limit,omitempty"`
}

// DiscoverFilter narrows a provider's catalog. Everything is optional; an empty
// filter accepts every row, which is almost never what you want from a catalog
// of hundreds.
type DiscoverFilter struct {
	// Free keeps only rows the provider offers at no cost.
	Free bool `yaml:"free,omitempty"`
	// InputModality / OutputModality must match exactly when set (e.g. "text").
	// This is what keeps a music-generation model out of a chat lane: OpenRouter
	// prices Lyria at zero, so the free test alone would enroll it.
	InputModality  string `yaml:"inputModality,omitempty"`
	OutputModality string `yaml:"outputModality,omitempty"`
	// MinContext drops rows with a smaller advertised window.
	MinContext int `yaml:"minContext,omitempty"`
	// Exclude drops any id containing one of these substrings.
	Exclude []string `yaml:"exclude,omitempty"`
}

// Admits reports whether one catalogue row passes this filter.
//
// Takes primitives rather than a freeroster.Entry so config keeps no dependency
// on the fetcher. It lives here, not next to the refresh loop, because two
// callers now ask the same question — the loop deciding what to enrol, and the
// catalogue browser marking which rows discovery would have taken anyway — and
// a browser that disagreed with the loop about the same filter would be worse
// than no marking at all.
func (f DiscoverFilter) Admits(id string, free bool, inMod, outMod string, contextLength int) bool {
	if f.Free && !free {
		return false
	}
	if f.InputModality != "" && inMod != f.InputModality {
		return false
	}
	if f.OutputModality != "" && outMod != f.OutputModality {
		return false
	}
	if f.MinContext > 0 && contextLength < f.MinContext {
		return false
	}
	for _, x := range f.Exclude {
		if x != "" && strings.Contains(id, x) {
			return false
		}
	}
	return true
}

// Server declares a host's capacity as a vector over named memory pools.
type Server struct {
	Pools         map[string]string `yaml:"pools,omitempty"`   // pool → size (e.g. "24GB")
	Reserve       map[string]string `yaml:"reserve,omitempty"` // headroom kept free per pool
	MaxConcurrent int               `yaml:"maxConcurrent,omitempty"`

	// DevicePool names the pool that holds DEVICE (accelerator) memory on this
	// server — the pool a MEASURED footprint is charged against, and the one the
	// VRAM budget is computed for. Defaults to "gpu0", which is what the whole
	// manager hardcoded before this existed.
	//
	// It is not cosmetic. A measured footprint is written to this pool by name;
	// point it at a pool the server does not declare and every measured model
	// there is charged against a budget of zero, which surfaces as a PERMANENT
	// capacity error — a 503 that reads like a backend fault rather than a
	// pool-naming one. A unified-memory host (Apple silicon) has no discrete
	// VRAM and must set this to its single "system" pool.
	DevicePool string `yaml:"devicePool,omitempty"`

	// Devices binds a pool to the PHYSICAL card whose memory it budgets:
	// pool name → device selector (a GPU UUID, a unique prefix of one, or a PCI
	// bus id). Absent means this server has at most one device pool and needs no
	// disambiguation — every single-GPU config keeps working untouched.
	//
	// A selector rather than an index, because the two orderings a host offers
	// disagree and BOTH move when hardware changes. Adding a slower second card
	// to this box put it at nvidia-smi index 0 (lowest PCI bus — it sits behind
	// the chipset) while CUDA, and so llama.cpp, kept the original card as
	// device 0. Everything that probed "the first GPU" silently switched to
	// describing the new card: the dashboard reported a 10 GiB device under a
	// pool budgeted for 32 GiB, and the slot auto-tuner began sizing against
	// VRAM that belonged to a different piece of hardware. A UUID cannot do
	// that.
	//
	// Note what this does NOT do: it is bookkeeping, not placement. corrallm
	// does not rewrite a cmd to pin it to a card — the cmd's own device flag
	// decides where the process actually lands, and a cmd that disagrees with
	// its declared pool will be charged to the wrong ledger. Declaring the pool
	// is how you tell corrallm where you already sent it.
	Devices map[string]string `yaml:"devices,omitempty"`

	// Agent binds this server to another machine running `corrallm agent`.
	// Absent (nil) means this box, spawned locally — byte-identical to the
	// behavior every existing config already gets.
	Agent *AgentBinding `yaml:"agent,omitempty"`

	// NoProcessMemory marks a host that cannot attribute memory to a single
	// process — macOS, which has no nvidia-smi equivalent. Set by enrollment
	// from what the agent reports; settable by hand for a host corrallm has not
	// met.
	//
	// It changes a rule rather than just a measurement: on such a host a model's
	// declared ramUsage stops being advisory and becomes the ONLY size anyone
	// has, so Validate requires it. Without that requirement the model gets no
	// profile, "reserve the whole pool then measure" never measures, and the
	// host silently serves one model at a time forever.
	NoProcessMemory bool `yaml:"noProcessMemory,omitempty"`

	// Notes is free-text the operator keeps ABOUT this entry, carried in the
	// config and surfaced in the UI beside it.
	//
	// It exists because the hand-written config was half commentary — why a
	// model is failover rather than a degrade tier, which build trap cost a
	// day, why a limit is the number it is — and a machine-owned file written
	// by a marshaller cannot keep YAML comments. Losing that knowledge is the
	// one irreversible part of letting the GUI own config, so it becomes a
	// field rather than a comment.
	Notes string `yaml:"notes,omitempty"`
}

// AgentBinding locates the machine backing a server.
//
// Endpoints is a LIST because one agent legitimately has several addresses AT
// THE SAME TIME — a NAT/LAN address on the daemon's own network, a VPN address
// when both ends are on the VPN, and possibly an external one — and which of
// them works depends on where the daemon is sitting, not on which is "right".
// A single `url` would force a config edit every time the topology moved.
//
// Order is preference. Selection is first-listed today; per-endpoint
// reachability (probe, prefer what answers, fall through) arrives with the
// remote client, and is the reason this is a list of endpoints rather than a
// list of hostnames — each may need its own scheme and port.
type AgentBinding struct {
	Endpoints []string `yaml:"endpoints,omitempty"`
	// Token authenticates this daemon to the agent. ${ENV} is expanded at load,
	// like proxy headers — never a literal secret in tracked YAML.
	Token string `yaml:"token,omitempty"`
}

// Host returns the preferred endpoint's host[:port], or "" if none parses.
// Used to point a model's data plane at the agent's machine instead of the
// primary's loopback.
// BaseURL is the agent's own address — host AND port — which is where data-plane
// traffic is now sent, as opposed to Host() which answers only "which machine".
//
// Returns the first endpoint that parses, matching Host()'s existing choice. The
// control plane probes the full list and remembers what worked; this does not,
// so a machine whose first-listed endpoint is unreachable serves traffic through
// an address the spawner already knows is dead. Pre-existing (Host() picked the
// same way), and worth fixing by having the data plane consult the same
// reachability the RemoteHost already discovered.
func (a *AgentBinding) BaseURL() *url.URL {
	if a == nil {
		return nil
	}
	for _, e := range a.Endpoints {
		if u, err := url.Parse(strings.TrimSpace(e)); err == nil && u.Host != "" {
			c := *u
			c.Path, c.RawQuery, c.Fragment = "", "", ""
			if c.Scheme == "" {
				c.Scheme = "http"
			}
			return &c
		}
	}
	return nil
}

func (a *AgentBinding) Host() string {
	if a == nil {
		return ""
	}
	for _, e := range a.Endpoints {
		if u, err := url.Parse(strings.TrimSpace(e)); err == nil && u.Host != "" {
			return u.Hostname()
		}
	}
	return ""
}

// poolNames lists a pool map's keys, sorted, for error messages.
func poolNames(pools map[string]string) []string {
	out := make([]string, 0, len(pools))
	for k := range pools {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DevicePoolFor names the pool holding device memory on a server, defaulting to
// the historical "gpu0". An unknown server also yields the default: callers are
// on the spawn path, where the server has already been validated.
func (c *Config) DevicePoolFor(server string) string {
	if s, ok := c.Servers[server]; ok && strings.TrimSpace(s.DevicePool) != "" {
		return s.DevicePool
	}
	return "gpu0"
}

// DeviceSelectorFor names the physical card backing one pool on a server, or ""
// when the pool is not bound to a device. "" is the answer for a plain `system`
// pool and for every single-GPU config that never declared `devices:` — both
// are correct states, not missing data, so callers fall back rather than fail.
func (c *Config) DeviceSelectorFor(server, pool string) string {
	s, ok := c.Servers[server]
	if !ok {
		return ""
	}
	return strings.TrimSpace(s.Devices[pool])
}

// DevicePoolsFor lists the pools on a server that hold device memory, sorted.
//
// It is the `devices:` keys when the operator declared them, and otherwise the
// single DevicePoolFor answer — so a config that predates multi-GPU support
// reports exactly the one pool it always had.
func (c *Config) DevicePoolsFor(server string) []string {
	if s, ok := c.Servers[server]; ok && len(s.Devices) > 0 {
		return poolNames(s.Devices)
	}
	return []string{c.DevicePoolFor(server)}
}

// DevicePoolsNamedBy lists the DEVICE pools a ramUsage vector actually draws
// from on a server, sorted. Pools that are not device-backed (`system`) are not
// included, and neither are pools the server does not declare.
//
// The three answers mean three different things, and the caller must
// distinguish them rather than take the first element:
//
//   - one pool — the card that holds this model; the only case where a measured
//     footprint can be attributed;
//   - none — the model draws no device memory it declared (a CPU-only backend
//     such as oidio), or declares nothing at all;
//   - several — a multi-GPU split. A measurement is one number, and how it
//     divides between cards is not recoverable from it, so charging the total
//     to either pool over-commits that card by whatever the other holds.
func (c *Config) DevicePoolsNamedBy(server string, ramUsage map[string]string) []string {
	devPools := map[string]bool{}
	for _, p := range c.DevicePoolsFor(server) {
		devPools[p] = true
	}
	var out []string
	for pool := range ramUsage {
		if devPools[pool] {
			out = append(out, pool)
		}
	}
	sort.Strings(out)
	return out
}

// Model is a served name with exactly ONE serving path: either a spawned local
// process (`cmd` + the port it binds in `proxy`) or a standalone proxy target
// (remote/paid endpoint, or another process's surface). The same weights served
// two ways = two models with distinct names; fallback across them is a lane.
type Model struct {
	Sticky     *Sticky `yaml:"sticky,omitempty"`
	Persistent bool    `yaml:"persistent,omitempty"`
	// Convert overrides the global attachment-ingestion config for this model
	// (e.g. a vision model rasterizes PDFs to images instead of extracting text).
	Convert *ConvertConfig `yaml:"convert,omitempty"`

	// Extension names the integration that provides this model. Mutually
	// exclusive with cmd/proxy/server/ramUsage/swap: those are the extension's,
	// and Resolve copies them down so the rest of the system sees an ordinary
	// model. What makes the sharing real is ProcKey, which routes every model of
	// one extension to a single Process.
	Extension string `yaml:"extension,omitempty"`

	// ProviderName is the upstream WITHIN the extension that serves this model
	// (the `providers:` key), equal to the extension name when the extension
	// declares a single implicit provider. Computed, never authored.
	//
	// Distinct from the Provider() method, which infers a provider from the proxy
	// host; this is the declared grouping.
	ProviderName string `yaml:"-"`

	// ExtensionHosted is computed, never authored: true when the extension runs a
	// local process. Only a HOSTED extension's models share a ProcKey — a remote
	// integration (Anthropic, Groq) has no process to share, and forcing them onto
	// one Process would pool their admission slots for no reason.
	ExtensionHosted bool `yaml:"-"`

	// Sampling holds this model's per-mode samplers, substituted into a chat
	// request for whichever fields the CALLER did not set. Nil = substitute
	// nothing, which is what every model did before this existed.
	//
	// Deliberately NOT merged with the cmd's own sampler flags: those are the
	// backend's floor and apply to /upstream traffic that bypasses this rewrite,
	// so keeping them independent means a direct-to-backend request still gets a
	// sane sampler rather than none.
	Sampling *SamplingConfig `yaml:"sampling,omitempty"`

	// Aliases are ADDITIONAL served names that resolve to this model. They exist
	// for version cutovers: a caller that hardcodes last quarter's model id keeps
	// working while the id it names starts meaning the new model.
	//
	// An alias resolves to the CANONICAL name, unlike a glob template which keeps
	// the requested id. The difference is not cosmetic. A glob's members are
	// distinct remote models that merely share a spelling, so each deserves its
	// own metrics row; an alias is a second name for one process holding one
	// reservation, and letting it carry the requested name would split residency
	// and pool accounting across two ids for memory allocated exactly once —
	// the same mistake the Extensions comment above was written about.
	//
	// Aliases may not shadow a model or lane name (Validate rejects it). That is
	// not a limitation so much as the reason the feature is coherent: resolution
	// returns on an exact Models hit, so an alias colliding with a live model
	// would never fire, and a silently dead alias during a cutover is worse than
	// a load error. To hand an old id to a new model, RENAME the old entry — the
	// old model stays pinnable under its new name, and rollback is moving one
	// alias line between two entries.
	Aliases []string `yaml:"aliases,omitempty"`

	// Upstream is the model id the BACKEND knows this model by, when that differs
	// from the served name. corrallm is the naming authority — renaming the four
	// oidio models to oidio-* is a corrallm decision — but oidio still serves them
	// as stt/stt-diarize/tts/realtime-stt, so the name is swapped on the way out.
	//
	// Same role as a proxy object's `model:` (Groq's id rewrite), expressed on the
	// model because an extension's provided models share ONE proxy target and each
	// needs a different upstream id.
	Upstream string `yaml:"upstream,omitempty"`

	Cmd    string `yaml:"cmd,omitempty"`    // spawn it (local model); empty → pure proxy
	Server string `yaml:"server,omitempty"` // which server it draws capacity from (cmd only)

	// Placements are the ways this model can be RUN: a box plus the command
	// that runs it there. Empty means the legacy shape — cmd/server/proxy on
	// the model itself — which normalises to exactly one placement, so an
	// existing config means what it always did. See placement.go.
	//
	// The model stays the routing identity (name, quality, cost class); how it
	// gets served is separate, and there can be more than one way. Two boxes,
	// or one box running two quantisations: not interchangeable, and differing
	// in footprint, context and capability.
	Placements []Placement `yaml:"placements,omitempty"`
	// RAMUsage is an ADVISORY bootstrap hint, not a fact anything relies on.
	//
	// A measured profile supersedes it the moment one exists (see
	// proc.effectiveUsage), and a model with neither is treated as needing the
	// whole pool for one spawn so it can be measured alone. Declaring it only
	// saves that first heavy eviction.
	//
	// It is advisory because every hand-written size on this box was wrong: the
	// gpu0 pool understated the real card by 2 GB, ternary-bonsai-27b's ramUsage
	// by 7 GB, Qwen3-6-27B-MPT's by 1 GB, and nomic-embed-text's OVER-stated by
	// 262 MiB. They stayed invisible because the errors cancelled — until
	// measurement made one side honest and the arithmetic stopped working.
	RAMUsage map[string]string `yaml:"ramUsage,omitempty"` // per-pool footprint vector (cmd only)
	Swap     *Swap             `yaml:"swap,omitempty"`     // measured load cost (cmd only)
	Proxy    yaml.Node         `yaml:"proxy,omitempty"`    // forward target: number | "host:port" | {host,port,headers}
	Type     string            `yaml:"type,omitempty"`     // cost class: chat | embed | openrouter | …
	// Quality is the relative rank used for lane ordering and degrade gating.
	// FLOAT, not int: a tier often has to be slotted BETWEEN two existing ones —
	// an MLX 4-bit port of a model that already sits at 2 is better than the 27B
	// at 1 and worse than the original, and "1.5" is the only honest way to say
	// so without renumbering the whole ladder. Integers keep working unchanged;
	// 1 parses as 1.0.
	//
	// Before this was a float, yaml silently TRUNCATED `quality: 1.5` to 1 and
	// validated clean, so the model quietly tied the tier below the one it was
	// meant to beat.
	Quality float64 `yaml:"quality,omitempty"`

	// Notes is free-text the operator keeps ABOUT this entry, carried in the
	// config and surfaced in the UI beside it.
	//
	// It exists because the hand-written config was half commentary — why a
	// model is failover rather than a degrade tier, which build trap cost a
	// day, why a limit is the number it is — and a machine-owned file written
	// by a marshaller cannot keep YAML comments. Losing that knowledge is the
	// one irreversible part of letting the GUI own config, so it becomes a
	// field rather than a comment.
	Notes string `yaml:"notes,omitempty"`

	// MaxConcurrent is the model's admission slots (the fairshare capacity
	// unit). For a local llama-server this mirrors --parallel. Default 1.
	MaxConcurrent int `yaml:"maxConcurrent,omitempty"`

	// ContextPerRequest is the context window each REQUEST must get, in tokens.
	//
	// llama.cpp's --ctx-size is a TOTAL divided across --parallel slots, so
	// raising concurrency silently shrinks the window every request sees. That
	// inverts how anyone actually reasons about it: the context length is a
	// requirement ("this model must serve 220k-token prompts") and concurrency
	// is the free variable you discover by tuning.
	//
	// When set, corrallm computes the spawned --ctx-size as
	// ContextPerRequest * slots, so the declared window is preserved by
	// construction and SLOTS become what gets reduced under VRAM pressure. If
	// not even one slot fits, that is reported loudly rather than served
	// quietly at a shorter window.
	//
	// Unset (0) keeps llama.cpp's native meaning: whatever --ctx-size the cmd
	// says is the total, divided by slots. Existing configs are unaffected
	// until they opt in.
	ContextPerRequest int `yaml:"contextPerRequest"`
	// MaxTokens caps the completion length this (often smaller/degraded) model
	// will be asked for: when a lane request degrades onto it, its max_tokens is
	// clamped to this value (P7). 0 = no cap.
	MaxTokens int `yaml:"maxTokens,omitempty"`
	// Modalities declares the input modalities this model accepts, each with
	// optional client-facing metadata (see ModalitySpec). Keys: text|image|audio.
	// Replaces the old coarse modality bucket. Unset → inferred: {audio} for audio
	// cost types, else {text}. Note: llama.cpp auto-loads the mmproj sibling from a
	// vision repo (no --mmproj flag), so `image` is declared here, not detected.
	Modalities map[string]ModalitySpec `yaml:"modalities,omitempty"`

	// FreeTier marks a remote backend as a P16 free-tier member and carries its
	// quota policy. Nil for local/paid models. See plan/p16-free-aggregator.md.
	FreeTier *FreeTier `yaml:"freeTier,omitempty"`
}

// FreeTier is a remote free backend's quota policy (P16).
type FreeTier struct {
	// Provider is a LABEL only (display, privacy defaults) — never the budget
	// key. The budget is per backend (one definition = one key), so two keys for
	// the same provider are two independent budgets.
	Provider string `yaml:"provider,omitempty"`
	// Private excludes this backend when a request is marked sensitive (some free
	// providers train on prompts). Advisory guardrail, not a guarantee.
	Private bool `yaml:"private,omitempty"`
	// Cap self-throttles BELOW the provider's own limit — leave headroom, avoid a
	// hard 429, or be a good citizen. 0 = no cap (use the provider's full limit).
	// Applied to the same two windows the provider reports; the effective budget
	// is min(provider-remaining, cap-minus-used). Header-tracked backends only.
	Cap FreeCap `yaml:"cap,omitempty"`

	// Limits declares this backend's own budgets, counted locally rather than
	// learned from a response — for a COUNTER-MODE backend that returns no
	// X-Ratelimit-* headers (OpenRouter), and for any spend cap at all, which no
	// provider reports.
	//
	//	limits:
	//	  - {req: 20,  per: minute}
	//	  - {req: 1000, per: day}
	//	  - {usd: 200, per: month}
	//
	// The pre-list shape ({rpm: 20, rpd: 1000}) still parses — see LimitSet.
	// Any entry makes the backend counter-mode. Empty = untracked.
	//
	// This is the ONE scope keyed by BACKEND rather than by caller, which is what
	// makes "this provider never exceeds $200/month, whoever calls it" sayable;
	// a priorityGroup limit bounds a caller across every backend instead.
	Limits LimitSet `yaml:"limits,omitempty"`

	// Refresh opts this backend into P16e roster refresh: corrallm periodically
	// pulls the provider's /v1/models, and if this backend's model has churned out
	// of the free set (gone paid or removed) marks it stale so the selector routes
	// around it. For providers whose free roster is volatile (OpenRouter);
	// stable-roster providers (Groq) leave it off.
	Refresh bool `yaml:"refresh,omitempty"`
}

// FreeCap self-throttles the two rate-limit windows below the provider's limit.
type FreeCap struct {
	Requests int `yaml:"requests,omitempty"` // e.g. 800 of a provider's 1000/day
	Tokens   int `yaml:"tokens,omitempty"`   // e.g. 10000 of a provider's 12000/min
}

// FreeLimits was the pre-list shape of FreeTier.Limits. Retained only so an
// existing config parses; LimitSet.UnmarshalYAML translates it. New config
// should use the list form.
type FreeLimits struct {
	RPM int `yaml:"rpm,omitempty"` // requests per minute (e.g. OpenRouter :free = 20)
	RPD int `yaml:"rpd,omitempty"` // requests per day (50 under $10 lifetime, else 1000)
}

// ModalitySpec is optional client-facing metadata for one accepted input
// modality. Only the fields relevant to that modality are set: image uses
// maxResolution + formats, audio uses formats, text may cap generation with
// maxTokens; the rest stay zero and are omitted from output.
type ModalitySpec struct {
	MaxResolution int      `yaml:"maxResolution,omitempty" json:"maxResolution,omitempty"` // image: longest-edge px cap
	Formats       []string `yaml:"formats,omitempty" json:"formats,omitempty"`             // image/audio: accepted encodings
	MaxTokens     int      `yaml:"maxTokens,omitempty" json:"maxTokens,omitempty"`         // text: generation-length cap
}

// KnownModalities is the accepted set of modality keys (typo guard in Validate).
var KnownModalities = map[string]bool{"text": true, "image": true, "audio": true}

// EffectiveModalities returns the model's declared modalities, or a single
// inferred default when none are configured: "audio" when audioDefault (an audio
// cost type), else "text". Callers pass audioDefault because audio-ness lives in
// the cost model, not config.
// ProbedModalities, when set, overrides what the config declares.
//
// A package-level hook rather than a parameter because EffectiveModalities is
// called from five places across three packages, all of which want the same
// answer and none of which should have to know where it comes from. Set once at
// startup by whatever owns the probe records; nil leaves behaviour exactly as
// it was.
//
// Probed BEATS declared deliberately. A declaration is what somebody thought to
// write; a probe is what the backend said when asked. Where they disagree the
// backend is right, and treating the absence of a declaration as a negative is
// what caused vision-capable models to be skipped for vision probes — the one
// mechanism that could have established the capability was gated on someone
// having already claimed it.
var ProbedModalities func(served string) (map[string]ModalitySpec, bool)

func (m Model) EffectiveModalities(served string, audioDefault bool) map[string]ModalitySpec {
	// The served name is passed rather than stored on the Model because a Model
	// is a map VALUE — it genuinely does not know what it is called, and every
	// caller here already does.
	if ProbedModalities != nil && served != "" {
		if got, ok := ProbedModalities(served); ok && len(got) > 0 {
			return got
		}
	}
	if len(m.Modalities) > 0 {
		return m.Modalities
	}
	d := "text"
	if audioDefault {
		d = "audio"
	}
	return map[string]ModalitySpec{d: {}}
}

// Slots returns the model's concurrency capacity, defaulting to 1.
func (m Model) Slots() int {
	if m.MaxConcurrent > 0 {
		return m.MaxConcurrent
	}
	return 1
}

// Lane is a named, ordered fallback list over model names.
type Lane struct {
	Members []LaneMember `yaml:"members,omitempty"`
	// Notes is free-text kept ABOUT this lane, carried in config and shown in
	// the UI beside it. See Model.Notes.
	Notes string `yaml:"notes,omitempty"`
}

// LaneMember references a model by name, optionally overriding its residency
// stickiness when it was loaded on this lane's behalf (e.g. a fallback member
// unloads sooner than when requested directly). YAML accepts a plain string
// (`- gemma-4-12b`) or an object (`- {model: gemma-4-12b, sticky: {ttl: 120s}}`).
type LaneMember struct {
	Model  string  `yaml:"model"`
	Sticky *Sticky `yaml:"sticky,omitempty"`

	// Provider makes this member a SELECTOR rather than a name: every model
	// contributed by that provider joins the lane, resolved against the live
	// roster instead of the set that happened to exist at config load.
	//
	// Named membership cannot express a DISCOVERED model at all. Membership is
	// validated at load; discovery runs later on the refresh loop, so a
	// discovered id is always "unknown" when the check runs — which is why the
	// live config carries the note "OpenRouter's discovered models are not [lane
	// members] ... pin those by name".
	//
	// The two alternatives both make membership a NAME, and a name is exactly
	// what keeps disappearing on a roster with `refresh: true`. Tolerating
	// unknown names handles churn by going quiet, which is indistinguishable
	// from a typo; having the UI rewrite `lanes:` on every refresh puts
	// operational state in the document, which is what Pause refused to do. A
	// selector says what is WANTED, so churn stops being an event to handle.
	Provider string `yaml:"provider,omitempty"`

	// Server scopes a selector to ONE host. Without it, "the same model" on two
	// hosts is one undifferentiated pool, and a lane cannot express the thing
	// lanes exist for: try the local copy first, fall to the attached box, fall
	// to a paid remote. A local Qwen and a remote Qwen are different backends
	// with different latency, cost and failure modes — collapsing them makes
	// the fallback ladder meaningless.
	//
	// Combines with Provider: both set means both must match.
	Server string `yaml:"server,omitempty"`

	// Device scopes further, to models drawing on one memory pool of that host
	// (gpu0, gpu1, system). Two cards on one box are not interchangeable — a
	// 5090 and a 3080 differ by 3x in bandwidth — so "the local copy" is
	// sometimes a per-DEVICE statement, not a per-host one.
	Device string `yaml:"device,omitempty"`

	// MinQuality drops contributed models below this rank, so a selector does
	// not have to accept everything a provider happens to publish.
	MinQuality float64 `yaml:"minQuality,omitempty"`
}

// IsSelector reports whether this member resolves against the roster rather
// than naming one model. Any scoping field makes it one.
func (lm LaneMember) IsSelector() bool {
	return lm.Model == "" && (lm.Provider != "" || lm.Server != "" || lm.Device != "")
}

// matches reports whether a model satisfies every scope this selector declares.
// Unset fields do not constrain, so `{provider: openrouter}` still means "all of
// that provider's" and `{server: box1}` means "everything hosted here".
func (lm LaneMember) matches(m Model) bool {
	if lm.Provider != "" && m.ProviderName != lm.Provider {
		return false
	}
	if lm.Server != "" && m.Server != lm.Server {
		return false
	}
	if lm.Device != "" {
		if _, ok := m.RAMUsage[lm.Device]; !ok {
			return false
		}
	}
	return m.Quality >= lm.MinQuality
}

// selectorMembers expands one selector into concrete candidates.
//
// Ordered by quality descending, then name, so the lane's walk is deterministic
// across restarts and across a roster that gained or lost entries — an
// expansion whose order depended on map iteration would reshuffle the fallback
// ladder on every boot.
func (c *Config) selectorMembers(lm LaneMember) []Candidate {
	var out []Candidate
	add := func(name string, m Model) {
		if !lm.matches(m) {
			return
		}
		out = append(out, Candidate{Name: name, Model: m, Sticky: lm.Sticky})
	}
	for name, m := range c.Models {
		add(name, m)
	}
	c.mu.RLock()
	for name, m := range c.discovered {
		if _, declared := c.Models[name]; !declared {
			add(name, m)
		}
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model.Quality != out[j].Model.Quality {
			return out[i].Model.Quality > out[j].Model.Quality
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// UnmarshalYAML lets a member be a scalar model name or the object form.
func (lm *LaneMember) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		lm.Model = n.Value
		return nil
	}
	type raw LaneMember // avoid recursion
	var r raw
	if err := n.Decode(&r); err != nil {
		return err
	}
	*lm = LaneMember(r)
	return nil
}

// Candidate is one resolved serving option for a served name: the model, its
// name (process identity + audit key), and an optional lane-member sticky
// override applied when this candidate is loaded via the lane.
type Candidate struct {
	Name   string
	Model  Model
	Sticky *Sticky // nil → the model's own sticky applies

	// Credential is the account this candidate forwards with, when its provider
	// declares more than the implicit one. nil means "the model's proxy as
	// written" — every model that predates credentials, and every provider that
	// declares none.
	//
	// One served name expands to one candidate PER credential, which is what
	// makes several accounts of one provider a fallback ladder rather than
	// several served names: the walk already skips a candidate with no budget,
	// so the selector picks whichever account can still pay.
	Credential *Credential
}

// ProcKey is this candidate's process identity. Credential-backed candidates
// must not share one: they are distinct upstreams (different auth, different
// budget), and collapsing them would make the second one reuse the first's
// connection and quota.
func (c Candidate) ProcKey() string {
	if c.Credential == nil {
		return c.Name
	}
	return c.Name + "@" + c.Credential.Name
}

// QuotaKey is the budget identity to gate and charge this candidate against.
//
// A credential-backed candidate keys on its CREDENTIAL, because that is the
// level a provider actually meters: one API key's spend is one budget, however
// many of its models you route to. Keying on the served model instead splits a
// single key's quota across N discovered models and lets a per-model cap
// multiply — 12 models x $200 is a $2,400 ceiling, not a $200 one.
func (c Candidate) QuotaKey() string {
	if c.Credential == nil {
		return c.Name
	}
	return c.Credential.ScopeKey(c.Model.ProviderName)
}

// GlobalScopeKey is the budget identity of the whole box.
const GlobalScopeKey = "*"

// ProviderScopeKey is a provider's joint budget across all its credentials.
func ProviderScopeKey(provider string) string { return "prov:" + provider }

// QuotaScopes lists every budget this candidate is charged against and gated
// on, narrowest first.
//
// A request consumes from ALL of them and is refused if ANY is exhausted,
// which is what makes the cascade a cascade rather than four independent
// counters: a credential under its own cap still cannot spend past its
// provider's, and nothing can spend past the box's.
//
// Only scopes that EXIST are returned — a config declaring no global limit does
// not get a global counter — so the cost is proportional to what was asked for.
func (c Candidate) QuotaScopes(cfg *Config) []string {
	out := []string{c.Name} // the model's own budget, which predates all of this
	if c.Credential != nil {
		out = append(out, c.Credential.ScopeKey(c.Model.ProviderName))
		if cfg != nil && c.Model.ProviderName != "" {
			if ext, ok := cfg.Extensions[c.Model.Extension]; ok {
				if pv, ok := ext.Providers[c.Model.ProviderName]; ok && len(pv.Limits) > 0 {
					out = append(out, ProviderScopeKey(c.Model.ProviderName))
				}
			}
		}
	}
	if cfg != nil && len(cfg.Limits) > 0 {
		out = append(out, GlobalScopeKey)
	}
	return out
}

// Target resolves where this candidate forwards, with its credential's headers
// merged over the provider's shared ones.
func (c Candidate) Target() (*ProxyTarget, error) {
	t, err := c.Model.ProxyTarget()
	if err != nil || c.Credential == nil {
		return t, err
	}
	merged := *t
	merged.Headers = c.Credential.MergedHeaders(t.Headers)
	if cmd := c.Credential.AuthTokenCommand; cmd != "" {
		merged.AuthTokenCommand = cmd
	}
	return &merged, nil
}

// expandCredentials turns one candidate into one per declared credential,
// leaving it untouched when its provider declares none. Order follows the
// config so the walk is deterministic.
func (c *Config) expandCredentials(in []Candidate) []Candidate {
	out := make([]Candidate, 0, len(in))
	for _, cand := range in {
		ext, ok := c.Extensions[cand.Model.Extension]
		if !ok || cand.Model.ProviderName == "" {
			out = append(out, cand)
			continue
		}
		pv, ok := ext.Providers[cand.Model.ProviderName]
		if !ok || len(pv.Credentials) == 0 {
			out = append(out, cand)
			continue
		}
		for i := range pv.Credentials {
			cr := pv.Credentials[i]
			// A credential that did not see this model in its own catalogue
			// cannot serve it; offering it would route a request to a 404.
			if !c.discoveredServableBy(cand.Name, cr.Name) {
				continue
			}
			dup := cand
			dup.Credential = &cr
			// No second gate here. There used to be one — a discovered model on
			// an approvalRequired credential needed a recorded yes — and it is
			// gone with the approval queue: a model is contributed by a filter
			// the operator wrote or by a selection the operator made, and both
			// are already the decision. Asking again afterwards was asking
			// twice.
			out = append(out, dup)
		}
	}
	return out
}

// ResolveServed maps a request's served name to its ordered candidates: a lane
// name yields its members (fallback allowed), a model name yields exactly that
// model (pinned). Unknown lane members are skipped (Validate rejects them at
// load; skipping keeps a hand-built Config safe).
func (c *Config) ResolveServed(served string) ([]Candidate, bool) {
	if lane, ok := c.Lanes[served]; ok {
		cands := make([]Candidate, 0, len(lane.Members))
		for _, mem := range lane.Members {
			if mem.IsSelector() {
				// Explicit members keep their exact declared position; a
				// selector expands in place, so the two forms compose in one
				// ordered ladder rather than competing.
				cands = append(cands, c.selectorMembers(mem)...)
				continue
			}
			m, ok := c.Models[mem.Model]
			if !ok {
				continue
			}
			cands = append(cands, Candidate{Name: mem.Model, Model: m, Sticky: mem.Sticky})
		}
		// Models placed INTO this lane by a selection join after its declared
		// members, at the priority chosen when they were placed. Declared
		// membership is an operator's explicit ladder and keeps the front of
		// it; a selection is a later, per-model addition rather than a
		// reordering of what was written down.
		cands = append(cands, c.selectedLaneMembers(served)...)
		return c.expandCredentials(cands), len(cands) > 0
	}
	if m, ok := c.Models[served]; ok {
		return c.expandCredentials([]Candidate{{Name: served, Model: m}}), true
	}
	// An alias is a declared, exact, hand-written name, so it outranks both a
	// discovered id and a glob. It yields the CANONICAL name, not the requested
	// one, so residency and metrics stay on one id — see Model.Aliases.
	//
	// Linear over Models rather than a prebuilt index: Validate guarantees at
	// most one model claims any given alias, the map is small and static after
	// load, and the glob branch below already walks it. An index here would need
	// invalidating against discovery for no measurable gain.
	for canonical, m := range c.Models {
		if slices.Contains(m.Aliases, served) {
			return c.expandCredentials([]Candidate{{Name: canonical, Model: m}}), true
		}
	}
	// Discovered models sit between the declared set and the glob templates: a
	// hand-written model still wins, but a concrete discovered id beats a
	// wildcard that merely happens to match it.
	c.mu.RLock()
	dm, dok := c.discovered[served]
	c.mu.RUnlock()
	if dok {
		return c.expandCredentials([]Candidate{{Name: served, Model: dm}}), true
	}
	// Template models: a model key containing '*' is a glob pattern. When no
	// exact model or lane matches, a served name matching a pattern resolves to
	// that template — but the Candidate keeps the REQUESTED name (so metrics,
	// residency, and audit log the concrete id), and since a pure-proxy
	// template leaves ProxyTarget.Model empty, the requested id forwards to the
	// upstream unchanged and the provider's own model matrix validates it.
	// e.g. "claude-opus-*" catches every dated Opus variant without enumeration.
	// Deterministic by sorted pattern key when several could match.
	patterns := make([]string, 0)
	for name := range c.Models {
		if strings.Contains(name, "*") {
			patterns = append(patterns, name)
		}
	}
	sort.Strings(patterns)
	for _, p := range patterns {
		if globMatch(p, served) {
			return []Candidate{{Name: served, Model: c.Models[p]}}, true
		}
	}
	return nil, false
}

// globMatch reports whether s matches a '*'-wildcard pattern (each '*' matches
// any run, including empty). Plain byte matching — no path/slash semantics, so
// it works on model ids like "claude-opus-5" and "vendor/model-name" alike.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s // no wildcard → exact
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// ConvertConfig governs how attached files (currently PDFs) in a chat request are
// ingested before reaching the model. Resolved per request as built-in defaults →
// global `convert:` → the model's `convert:`, each field overriding the last.
// Zero/empty fields inherit; OCR is a pointer so a model can force it false.
// SamplingProfile is one mode's sampler, as the model's own card states it.
//
// Every field is a POINTER because 0 is a meaningful value for most of them —
// temperature 0 is greedy, presencePenalty 0 is off, minP 0 is off — and a plain
// float could not tell "the operator set it to 0" from "the operator said
// nothing". An unset field is never sent, which is what lets a caller keep
// control of the knobs the config has no opinion about.
type SamplingProfile struct {
	Temperature      *float64 `yaml:"temperature,omitempty"`
	TopP             *float64 `yaml:"topP,omitempty"`
	TopK             *int     `yaml:"topK,omitempty"`
	MinP             *float64 `yaml:"minP,omitempty"`
	PresencePenalty  *float64 `yaml:"presencePenalty,omitempty"`
	FrequencyPenalty *float64 `yaml:"frequencyPenalty,omitempty"`
	RepeatPenalty    *float64 `yaml:"repeatPenalty,omitempty"`
}

// Empty reports whether this profile would send nothing.
func (p SamplingProfile) Empty() bool {
	return p.Temperature == nil && p.TopP == nil && p.TopK == nil && p.MinP == nil &&
		p.PresencePenalty == nil && p.FrequencyPenalty == nil && p.RepeatPenalty == nil
}

// SamplingConfig carries the per-MODE samplers for one model.
//
// It exists because a reasoning model has two right answers and the server flag
// can only hold one. Qwen3.8's card, for instance, asks for temperature 1.0 /
// top_p 0.95 / presence_penalty 0.0 while thinking and 0.7 / 0.80 / 1.5 while
// not — so a llama-server launched with one set is running the wrong sampler
// every time a caller flips the mode per request, which llama.cpp lets them do
// (chat_template_kwargs.enable_thinking overrides --reasoning).
//
// That mismatch is invisible: it degrades output, it does not error. Corrallm is
// the only component that knows both which model a request resolved to (after
// aliases, lanes and globs) and what that model's card recommends, which is why
// this lives here and not in each client.
type SamplingConfig struct {
	// Thinking and Instruct are the two modes' profiles. Either may be empty,
	// in which case nothing is substituted for that mode.
	Thinking SamplingProfile `yaml:"thinking,omitempty"`
	Instruct SamplingProfile `yaml:"instruct,omitempty"`

	// Default names the profile to use when the request expresses no opinion
	// about thinking: "instruct" (the zero value) or "thinking". It must match
	// how the backend was actually launched — a model started with
	// `--reasoning off` and a Default of "thinking" would hand every unmarked
	// request the wrong sampler, which is the exact failure this type exists to
	// prevent.
	Default string `yaml:"default,omitempty"`
}

// ProfileFor returns the profile for a mode: thinking when think is true.
func (s SamplingConfig) ProfileFor(think bool) SamplingProfile {
	if think {
		return s.Thinking
	}
	return s.Instruct
}

// DefaultThinking reports which mode an unmarked request gets.
func (s SamplingConfig) DefaultThinking() bool { return s.Default == "thinking" }

type ConvertConfig struct {
	PDF      string `yaml:"pdf,omitempty"`      // text | vision | off
	DPI      int    `yaml:"dpi,omitempty"`      // rasterization DPI (vision/OCR)
	Quality  int    `yaml:"quality,omitempty"`  // JPEG quality 1–100 (vision)
	Format   string `yaml:"format,omitempty"`   // jpeg | png (vision page images)
	MaxPages int    `yaml:"maxPages,omitempty"` // page cap (vision rasterize / OCR)
	MaxChars int    `yaml:"maxChars,omitempty"` // injected-text cap (text)
	OCR      *bool  `yaml:"ocr,omitempty"`      // OCR fallback for scanned PDFs (text)
}

// DefaultConvert is the built-in base every resolution starts from.
func DefaultConvert() ConvertConfig {
	on := true
	return ConvertConfig{PDF: "text", DPI: 200, Quality: 85, Format: "jpeg", MaxPages: 20, MaxChars: 400000, OCR: &on}
}

// Merge returns c with the set (non-zero) fields of over applied on top.
func (c ConvertConfig) Merge(over ConvertConfig) ConvertConfig {
	if over.PDF != "" {
		c.PDF = over.PDF
	}
	if over.DPI != 0 {
		c.DPI = over.DPI
	}
	if over.Quality != 0 {
		c.Quality = over.Quality
	}
	if over.Format != "" {
		c.Format = over.Format
	}
	if over.MaxPages != 0 {
		c.MaxPages = over.MaxPages
	}
	if over.MaxChars != 0 {
		c.MaxChars = over.MaxChars
	}
	if over.OCR != nil {
		c.OCR = over.OCR
	}
	return c
}

// OCREnabled reports whether the OCR fallback is on (defaults to true if unset).
func (c ConvertConfig) OCREnabled() bool { return c.OCR == nil || *c.OCR }

// ConvertFor resolves the effective ingestion config for a served name: the
// global default overridden by the model's own block. A lane inherits its
// first (top-preference) member's block — conversion happens once, before the
// member walk, so the primary member's needs win.
func (c *Config) ConvertFor(global ConvertConfig, served string) ConvertConfig {
	eff := global
	name := served
	if lane, ok := c.Lanes[served]; ok && len(lane.Members) > 0 {
		name = lane.Members[0].Model
	}
	if m, ok := c.Models[name]; ok && m.Convert != nil {
		eff = eff.Merge(*m.Convert)
	}
	return eff
}

// Sticky keeps a model warm after last use and resists eviction (residency, P4).
type Sticky struct {
	TTL       string `yaml:"ttl,omitempty"`
	EvictCost string `yaml:"evictCost,omitempty"` // low | medium | high

	// IdleUnload unloads a backend that has gone quiet for this long, without
	// waiting for anything else to want its memory.
	//
	// TTL alone does NOT do this. TTL is an eviction PRIORITY — sortVictims
	// puts ttl-expired processes at the front of the queue — so a model whose
	// TTL lapsed hours ago stays resident until some other model needs the
	// card. That is deliberate and mostly right: holding weights in VRAM costs
	// a couple of watts, and unloading a model that is requested again a minute
	// later pays an unload AND a cold load for nothing.
	//
	// What it costs is LATENCY PLACEMENT. When the next model finally does need
	// the card, its first request pays eviction plus cold load serially. Setting
	// idleUnload moves the eviction to a moment when nobody is waiting.
	//
	// Empty = never (the pre-existing behaviour). A value at or below TTL is
	// honoured with a warning: the backend unloads before the eviction ordering
	// ever consults its TTL, which makes the TTL moot rather than wrong.
	IdleUnload string `yaml:"idleUnload,omitempty"`
}

// MaxQuality returns the highest Quality among the candidates (0 if none/unset)
// — the top of a served name's quality ladder (P7).
func MaxQuality(cands []Candidate) float64 {
	top := 0.0
	for _, c := range cands {
		if c.Model.Quality > top {
			top = c.Model.Quality
		}
	}
	return top
}

// Capability classifies a backend cost-class `type` into the operation it serves,
// the same convention modality is inferred from. STT and TTS are kept DISTINCT
// (speech-in vs speech-out) — never lumped as "audio". Drives /v1/models,
// /v1/capabilities, and the dashboard so clients/LLMs pick the right model.
func Capability(typ string) string {
	t := strings.ToLower(typ)
	switch {
	case strings.Contains(t, "realtime"):
		// Live ws transcription (/v1/realtime) — a distinct delivery from batch
		// STT, so the catalog/console can route to the right surface without a
		// separate "modes" field.
		return "audio.realtime"
	case strings.Contains(t, "tts") || strings.Contains(t, "speech"):
		return "audio.tts"
	case strings.Contains(t, "stt") || strings.Contains(t, "asr") ||
		strings.Contains(t, "whisper") || strings.Contains(t, "transcri") || strings.Contains(t, "parakeet"):
		return "audio.stt"
	case strings.Contains(t, "embed"):
		return "embeddings"
	case strings.Contains(t, "rerank"):
		return "rerank"
	default:
		return "chat"
	}
}

// ModelCapability is a served model's capability, inferred from its cost type.
func ModelCapability(m Model) string {
	return Capability(m.Type)
}

// Swap is the measured cost of loading a backend (residency input, P4). P6 adds
// LoadWatts so the load's energy can be priced (loadSeconds × loadWatts → $);
// absent, only its latency feeds scheduling.
type Swap struct {
	LoadSeconds float64 `yaml:"loadSeconds,omitempty"`
	LoadWatts   float64 `yaml:"loadWatts,omitempty"`
}

// PriorityGroup is the single policy unit (fairshare + saturation behavior).
type PriorityGroup struct {
	Weight        int               `yaml:"weight,omitempty"`
	ShareCurrency string            `yaml:"shareCurrency,omitempty"` // requests | dwell | cost
	Interruptible bool              `yaml:"interruptible,omitempty"`
	OnSaturated   map[string]Stage  `yaml:"onSaturated,omitempty"` // backend type → stage policy
	Limits        map[string]string `yaml:"limits,omitempty"`      // group-wide TCO caps
	// AcceptDegrade opts the group into quality-degrade fall-through (P7): when
	// set, the group may be served by lower-quality backends in the model's list
	// (down to QualityFloor). Default false → the group is served only by the
	// model's highest-quality tier; below that it backs off (reject/queue) rather
	// than be served a worse model.
	AcceptDegrade bool `yaml:"acceptDegrade,omitempty"`
	// QualityFloor is the lowest backend quality the group will accept when it
	// does degrade (0 = no floor). Ignored unless AcceptDegrade is set.
	QualityFloor float64 `yaml:"qualityFloor,omitempty"`
	// PreferResident makes the group best-effort against what is already loaded:
	// among the backends it accepts (quality-wise), any that are currently
	// resident (a warm process) are tried first, in quality order, before any
	// cold-load candidate. Only when no accepted backend is resident does it fall
	// to the normal quality-first cold-load ladder. Keeps a latency-sensitive lane
	// (a concierge) on whatever chat model is hot instead of cold-loading a bigger
	// one and re-hogging the box. Independent of AcceptDegrade (though pairing them
	// is what lets the lane ride a degraded-but-resident tier).
	PreferResident bool `yaml:"preferResident,omitempty"`
}

// AcceptsQuality reports whether the group may be served by a backend of quality
// q, given the model's top-tier quality. A non-degrading group accepts only the
// top tier; a degrading group accepts down to its QualityFloor (P7).
func (g PriorityGroup) AcceptsQuality(q, topQuality float64) bool {
	// A model's own top tier is always acceptable — the floor only gates
	// degrading BELOW the best when a better tier exists. Without this, a group
	// with QualityFloor>0 rejects any model whose whole ladder sits under the
	// floor (e.g. audio backends default to quality 0), emptying the walk →
	// "no backend available".
	if q >= topQuality {
		return true
	}
	if !g.AcceptDegrade {
		return false // no degrade → only the top tier
	}
	return q >= g.QualityFloor // degrade down to the floor
}

// EffectiveWeight returns the group's fairshare weight, defaulting to 1.
func (g PriorityGroup) EffectiveWeight() int {
	if g.Weight > 0 {
		return g.Weight
	}
	return 1
}

// StageFor returns the saturation stage for a backend type, falling back to the
// group's "default" stage, then to a reject stage if neither is declared.
func (g PriorityGroup) StageFor(backendType string) Stage {
	if s, ok := g.OnSaturated[backendType]; ok && !s.IsZero() {
		return s
	}
	if s, ok := g.OnSaturated["default"]; ok && !s.IsZero() {
		return s
	}
	return Stage{Reject: true}
}

// UnknownKeyPolicy governs callers whose key is not in Keys.
type UnknownKeyPolicy struct {
	// Allow, when explicitly false, refuses an unrecognized key outright
	// instead of serving it in the fallback group. Default TRUE: corrallm has
	// always accepted any key, and flipping that by omission would lock out
	// every caller on an upgrade.
	Allow *bool `yaml:"allow,omitempty"`
	// Group is where an unrecognized caller lands. Empty = "default".
	Group string `yaml:"group,omitempty"`
}

// Allowed reports whether an unrecognized key may be served.
func (p UnknownKeyPolicy) Allowed() bool { return p.Allow == nil || *p.Allow }

// FallbackGroup is the group an unrecognized caller resolves to.
func (p UnknownKeyPolicy) FallbackGroup() string {
	if p.Group != "" {
		return p.Group
	}
	return "default"
}

// ResolveGroup maps a caller key to its priority group. An empty/unknown key, or
// a key whose group is absent, resolves to the UnknownKeys fallback group
// ("default" unless configured), synthesized as weight-1 reject-on-saturation
// if the config omits it.
//
// Recognized reports whether the key was actually in Keys, so a caller can tell
// "assigned to this group" from "fell through to it" — the distinction the
// enrollment UI is built on, and one this function used to erase.
func (c *Config) ResolveGroup(key string) (name string, g PriorityGroup) {
	name, g, _ = c.resolveGroup(key)
	return name, g
}

// ResolveGroupRecognized is ResolveGroup plus whether the key was configured.
func (c *Config) ResolveGroupRecognized(key string) (string, PriorityGroup, bool) {
	return c.resolveGroup(key)
}

func (c *Config) resolveGroup(key string) (name string, g PriorityGroup, recognized bool) {
	name = c.Keys[key]
	if name == "" {
		name = c.UnknownKeys.FallbackGroup()
	}
	_, recognized = c.Keys[key]
	if grp, ok := c.PriorityGroups[name]; ok {
		return name, grp, recognized
	}
	return c.UnknownKeys.FallbackGroup(), PriorityGroup{Weight: 1}, recognized
}

// Load reads and parses the corrallm YAML config at path. A missing file yields
// an empty (valid) config so the proxy can boot with env-only configuration.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.mergeIncludes(path); err != nil {
		return nil, err
	}
	// Before Validate: resolution fills in cmd/server/proxy from the extension,
	// and the existing model rules ("cmd set but no server") must judge the
	// resolved model, not the sparse one the author wrote.
	c.projectFirstPlacement()
	if err := c.resolveExtensions(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// mergeIncludes folds every file named in c.Include into c, resolving relative
// paths against the including file's directory.
//
// Precedence, from weakest to strongest: the first include, later includes,
// then c itself. The operator's own file always wins, so a generated include
// can never quietly redefine something a human wrote down — the same rule
// SetDiscovered follows for runtime-contributed models.
//
// Only the map-shaped sections merge. A scalar or struct section in an included
// file is REJECTED rather than ignored: silently dropping a costPerKwh someone
// wrote is the kind of thing that is discovered months later via a wrong bill.
func (c *Config) mergeIncludes(path string) error {
	if len(c.Include) == 0 {
		return nil
	}
	dir := filepath.Dir(path)

	// Accumulate the includes among themselves first, last-wins, THEN let c win
	// over the result. Folding straight into c would invert the include order:
	// c is already populated, so "skip what exists" would make the FIRST include
	// beat every later one.
	var acc Config
	for _, inc := range c.Include {
		p := inc
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			// Unlike the top-level config, a named include must exist: the
			// operator asked for it by name, and booting without it would serve
			// a silently smaller config.
			return fmt.Errorf("config %s: include %q: %w", path, inc, err)
		}
		var in Config
		if err := yaml.Unmarshal(b, &in); err != nil {
			return fmt.Errorf("parse include %s: %w", p, err)
		}
		if len(in.Include) > 0 {
			return fmt.Errorf("include %s: nested include is not supported (one level only)", p)
		}
		if err := in.rejectNonMergeable(p); err != nil {
			return err
		}
		overwriteMap(&acc.Servers, in.Servers)
		overwriteMap(&acc.Extensions, in.Extensions)
		overwriteMap(&acc.Models, in.Models)
		overwriteMap(&acc.Lanes, in.Lanes)
		overwriteMap(&acc.PriorityGroups, in.PriorityGroups)
		overwriteMap(&acc.Keys, in.Keys)
		overwriteMap(&acc.CommandCosts, in.CommandCosts)
	}

	mergeMap(&c.Servers, acc.Servers)
	mergeMap(&c.Extensions, acc.Extensions)
	mergeMap(&c.Models, acc.Models)
	mergeMap(&c.Lanes, acc.Lanes)
	mergeMap(&c.PriorityGroups, acc.PriorityGroups)
	mergeMap(&c.Keys, acc.Keys)
	mergeMap(&c.CommandCosts, acc.CommandCosts)
	return nil
}

// rejectNonMergeable fails an included file that sets a section which only the
// top-level config may set, rather than dropping it on the floor.
func (c *Config) rejectNonMergeable(p string) error {
	var bad []string
	if c.CostPerKwh != 0 {
		bad = append(bad, "costPerKwh")
	}
	if !reflect.DeepEqual(c.Convert, ConvertConfig{}) {
		bad = append(bad, "convert")
	}
	if !reflect.DeepEqual(c.Scheduler, SchedulerConfig{}) {
		bad = append(bad, "scheduler")
	}
	if len(bad) > 0 {
		return fmt.Errorf("include %s: sets %s — only the top-level config may set these (merging is per-model, not global)",
			p, strings.Join(bad, ", "))
	}
	return nil
}

// mergeMap copies src into *dst for keys *dst does not already hold — dst is
// the stronger side and keeps what it has.
func mergeMap[V any](dst *map[string]V, src map[string]V) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]V, len(src))
	}
	for k, v := range src {
		if _, exists := (*dst)[k]; exists {
			continue
		}
		(*dst)[k] = v
	}
}

// overwriteMap copies src into *dst, src winning — used between includes, where
// the later file is the stronger side.
func overwriteMap[V any](dst *map[string]V, src map[string]V) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]V, len(src))
	}
	for k, v := range src {
		(*dst)[k] = v
	}
}

// resolveExtensions copies each extension's lifecycle fields onto the models it
// provides, so everything downstream sees an ordinary spawned model. Sharing is
// expressed by ProcKey, not by these fields.
// projectFirstPlacement mirrors placement[0] onto the legacy model fields.
//
// Ninety-odd call sites still read Model.Cmd/Server/Proxy/RAMUsage, and they
// are the whole runtime: admission, residency, target resolution,
// reconciliation. Until those are placement-aware, a model written with a
// placements: list would validate and then fail to serve — Server would be
// empty, so it would be treated as a pure proxy with no local process.
//
// Projecting the first placement makes the new syntax work TODAY for the
// single-placement case, which is every existing config re-expressed. More than
// one placement is refused in Validate rather than silently serving only the
// first, because a config that says two boxes and quietly uses one is worse
// than a config that will not load.
func (c *Config) projectFirstPlacement() {
	for name, m := range c.Models {
		// Only a single-placement model projects. With several, the runtime
		// CHOOSES per load (proc.selectPlacement) and projecting one of them
		// onto the model would make read surfaces claim a placement that a
		// given request never used.
		if len(m.Placements) != 1 {
			continue
		}
		p := m.PlacementList()[0]
		m.Server = p.Server
		m.Cmd = p.Cmd
		m.Proxy = p.Proxy
		if len(p.RAMUsage) > 0 {
			m.RAMUsage = p.RAMUsage
		}
		if p.MaxConcurrent > 0 {
			m.MaxConcurrent = p.MaxConcurrent
		}
		if p.ContextPerRequest > 0 {
			m.ContextPerRequest = p.ContextPerRequest
		}
		if p.Swap != nil {
			m.Swap = p.Swap
		}
		c.Models[name] = m
	}
}

func (c *Config) resolveExtensions() error {
	for name, ext := range c.Extensions {
		hosted := ext.Cmd != ""
		// A hosted extension runs a process here and needs a pool to draw from. A
		// remote one is a shared endpoint + credentials, with no local lifecycle —
		// residency knobs on it would describe something that does not exist.
		if hosted && ext.Server == "" {
			return fmt.Errorf("extension %q: cmd set but no server", name)
		}
		if !hosted {
			if ext.Server != "" || len(ext.RAMUsage) > 0 || ext.Swap != nil || ext.Persistent || ext.Sticky != nil {
				return fmt.Errorf("extension %q: server/ramUsage/swap/persistent/sticky need a cmd (they describe a local process)", name)
			}
		}
		if _, clash := c.Models[name]; clash {
			return fmt.Errorf("extension %q: name collides with a model", name)
		}

		// Normalize both shapes to a provider list. The extension-level
		// proxy/provides pair is one provider named after the extension.
		provs := map[string]Provider{}
		switch {
		case len(ext.Providers) > 0:
			if !ext.Proxy.IsZero() || len(ext.Provides) > 0 {
				return fmt.Errorf("extension %q: use either providers, or a top-level proxy+provides, not both", name)
			}
			if hosted {
				return fmt.Errorf("extension %q: providers describe several upstreams; a cmd serves exactly one", name)
			}
			for pn, pv := range ext.Providers {
				if pv.Proxy.IsZero() {
					return fmt.Errorf("extension %q provider %q: needs proxy", name, pn)
				}
				// `discover` and `manual` are the other two ways to contribute
				// models, so a provider with either is complete — it just
				// contributes nothing until the first refresh, or until someone
				// picks a model off its catalogue.
				if len(pv.Provides) == 0 && pv.Discover == nil && !pv.Manual {
					return fmt.Errorf("extension %q provider %q: contributes no models (needs provides, discover, or manual: true to pick from its catalogue by hand)", name, pn)
				}
				if err := validateCredentials(name, pn, pv); err != nil {
					return err
				}
				provs[pn] = pv
			}
		default:
			if ext.Proxy.IsZero() {
				return fmt.Errorf("extension %q: needs proxy (the target its models forward to)", name)
			}
			if len(ext.Provides) == 0 {
				// `provides` and `providers` are currently the only capabilities an
				// extension can declare. When others exist (listeners, transformers),
				// this accepts an extension that contributes no models.
				return fmt.Errorf("extension %q: declares no capabilities (provides/providers are currently the only ones)", name)
			}
			provs[name] = Provider{Proxy: ext.Proxy, Provides: ext.Provides}
		}

		if c.Models == nil {
			c.Models = map[string]Model{}
		}
		for pn, pv := range provs {
			for id, pm := range pv.Provides {
				if id == "" {
					return fmt.Errorf("extension %q provider %q: empty model id", name, pn)
				}
				if pm.Cmd != "" || !pm.Proxy.IsZero() || pm.Server != "" || len(pm.RAMUsage) > 0 ||
					pm.Swap != nil || pm.Persistent || pm.Sticky != nil || pm.Extension != "" {
					return fmt.Errorf("extension %q: model %q sets cmd/proxy/server/ramUsage/swap/persistent/sticky/extension; those belong to the extension", name, id)
				}
				served := pn + "-" + id
				if _, clash := c.Models[served]; clash {
					return fmt.Errorf("extension %q: provided model %q collides with a declared model", name, served)
				}
				pm.Extension, pm.ProviderName, pm.ExtensionHosted = name, pn, hosted
				pm.Proxy = pv.Proxy
				if pm.Upstream == "" && hosted {
					// A hosted backend knows the model by the key. A remote provider's id
					// is arbitrary (Groq's "llama-3.3-70b-versatile"), so it must be
					// stated — either here as `upstream:` or on the proxy object.
					pm.Upstream = id
				}
				c.Models[served] = pm
			}
		}
	}
	for name, m := range c.Models {
		if m.Extension == "" {
			continue
		}
		if _, ok := c.Extensions[m.Extension]; !ok {
			return fmt.Errorf("model %q: unknown extension %q", name, m.Extension)
		}
	}
	return nil
}

// DiscoverTargets lists every (extension, provider, spec) that opted into
// catalog discovery, with the provider's proxy target resolved. The refresh loop
// needs the target to know where to fetch and which credentials to send.
type DiscoverTarget struct {
	Extension string
	Provider  string
	// Credential names the account this catalogue was fetched with. Discovery
	// runs once PER credential because the catalogues differ by key, and the
	// result is recorded against this name so the walk only offers a model on
	// the accounts that can actually serve it.
	Credential string
	Spec      *Discover
	Target    *ProxyTarget
	// ProxyNode is the provider's proxy block as written, so a discovered model
	// can be given the same target the declared ones get. ProxyTarget is resolved
	// and cannot round-trip back to YAML.
	ProxyNode yaml.Node
}

// DiscoverTargets returns the providers with a `discover` block.
func (c *Config) DiscoverTargets() []DiscoverTarget {
	var out []DiscoverTarget
	names := make([]string, 0, len(c.Extensions))
	for n := range c.Extensions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, en := range names {
		ext := c.Extensions[en]
		provs := make([]string, 0, len(ext.Providers))
		for pn := range ext.Providers {
			provs = append(provs, pn)
		}
		sort.Strings(provs)
		for _, pn := range provs {
			pv := ext.Providers[pn]
			if pv.Discover == nil {
				continue
			}
			base, err := (Model{Proxy: pv.Proxy}).ProxyTarget()
			if err != nil {
				continue
			}
			// One target per credential: same endpoint, different auth, and
			// therefore a different catalogue. A provider declaring none yields
			// exactly one with the implicit default, so nothing changes for a
			// config that predates credentials.
			for _, cr := range pv.CredentialList() {
				t := *base
				if len(cr.Headers) > 0 || cr.AuthTokenCommand != "" {
					t.Headers = cr.MergedHeaders(base.Headers)
					if cr.AuthTokenCommand != "" {
						t.AuthTokenCommand = cr.AuthTokenCommand
					}
				}
				out = append(out, DiscoverTarget{
					Extension: en, Provider: pn, Credential: cr.Name,
					Spec: pv.Discover, Target: &t, ProxyNode: pv.Proxy,
				})
			}
		}
	}
	return out
}

// ServedName turns a provider's own model id into a served name under this
// provider: "nvidia/nemotron-3-super-120b-a12b:free" under provider "openrouter"
// becomes "openrouter-nvidia-nemotron-3-super-120b-a12b".
//
// The ":free" marker is dropped because it is a pricing tier, not part of the
// identity — and if the model later goes paid, the served name should not have
// to change. The original id is kept as Upstream, so what reaches the provider
// is always its own id.
func ServedName(provider, id string) string {
	n := strings.TrimSuffix(id, ":free")
	n = strings.ReplaceAll(n, "/", "-")
	n = strings.ReplaceAll(n, ":", "-")
	return provider + "-" + n
}

// Effective returns a served model as the PROCESS layer must see it: for an
// extension-provided model, the extension's lifecycle fields overlaid onto it.
//
// This overlay exists only at spawn/residency time. The stored model keeps no
// cmd, so nothing else in the system can mistake it for something it can start
// or evict on its own.
func (c *Config) Effective(served string) (Model, bool) {
	m, ok := c.Models[served]
	if !ok {
		return Model{}, false
	}
	if m.Extension == "" {
		return m, true
	}
	ext, ok := c.Extensions[m.Extension]
	if !ok || ext.Cmd == "" {
		return m, true // remote: nothing to spawn, nothing to reserve
	}
	m.Cmd, m.Server, m.RAMUsage = ext.Cmd, ext.Server, ext.RAMUsage
	m.Swap, m.Persistent, m.Sticky = ext.Swap, ext.Persistent, ext.Sticky
	return m, true
}

// ExtensionModels lists the served models an extension provides, sorted so
// callers that must pick one pick the same one every time.
func (c *Config) ExtensionModels(ext string) []string {
	var out []string
	for name, m := range c.Models {
		if m.Extension == ext {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ProcKey is the identity of the PROCESS backing a served model. Models of one
// extension share it, which is what makes them load and unload together; every
// other model is its own process and keys by its own name.
//
// The "extension:" prefix cannot collide with a model name: resolveExtensions
// rejects a model named after an extension, and a served name containing a colon
// is not addressable as a model.
func (m Model) ProcKey(served string) string {
	if m.Extension != "" && m.ExtensionHosted {
		return "extension:" + m.Extension
	}
	return served
}

// Validate checks structural invariants that must hold before scheduling can
// run. P0 enforces only what's cheap and unambiguous; richer checks land with
// the phases that consume each section.
func (c *Config) Validate() error {
	for srvName, srv := range c.Servers {
		if _, err := ParseSizes(srv.Pools); err != nil {
			return fmt.Errorf("server %q pools: %w", srvName, err)
		}
		if _, err := ParseSizes(srv.Reserve); err != nil {
			return fmt.Errorf("server %q reserve: %w", srvName, err)
		}
		// A devicePool naming a pool the server does not declare would charge
		// every measured footprint against a zero budget, and the model would go
		// permanently unschedulable with a 503 that looks like a backend fault.
		// Catch it here, where the message can say what is actually wrong.
		if dp := strings.TrimSpace(srv.DevicePool); dp != "" {
			if _, ok := srv.Pools[dp]; !ok {
				return fmt.Errorf("server %q: devicePool %q is not one of its pools %v", srvName, dp, poolNames(srv.Pools))
			}
		}
		// A devices entry has the same failure mode as devicePool one level
		// down: it names the card a pool's budget describes, so a key that is
		// not a pool binds a real GPU to a ledger nobody charges against, and
		// two pools sharing a selector double-count one card's VRAM.
		seenSel := map[string]string{}
		for pool, sel := range srv.Devices {
			if _, ok := srv.Pools[pool]; !ok {
				return fmt.Errorf("server %q: devices key %q is not one of its pools %v", srvName, pool, poolNames(srv.Pools))
			}
			sel = strings.TrimSpace(sel)
			if sel == "" {
				return fmt.Errorf("server %q: devices[%q] is empty — name the card by UUID or PCI bus id", srvName, pool)
			}
			if prev, dup := seenSel[strings.ToLower(sel)]; dup {
				return fmt.Errorf("server %q: pools %q and %q both claim device %q — one card cannot back two budgets",
					srvName, prev, pool, sel)
			}
			seenSel[strings.ToLower(sel)] = pool
		}
		// An agent binding that cannot be dialled is worse than none: the models
		// on that server would be admitted and then fail at spawn, one request
		// at a time. Fail at load instead.
		_ = srvName
		if a := srv.Agent; a != nil {
			if len(a.Endpoints) == 0 {
				return fmt.Errorf("server %q: agent declared with no endpoints (list at least one, e.g. http://host:6503)", srvName)
			}
			for i, e := range a.Endpoints {
				u, err := url.Parse(strings.TrimSpace(e))
				if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
					return fmt.Errorf("server %q: agent endpoint %d (%q) must be an absolute http(s) URL with a host", srvName, i, e)
				}
			}
		}
	}
	for name, m := range c.Models {
		// A serving path can now live on a PLACEMENT rather than on the model
		// itself, which is the whole point of placements: the model is the
		// routing identity and the placement says how it is served. A model
		// with placements has as many paths as it has placements.
		if m.Cmd == "" && m.Proxy.IsZero() && len(m.Placements) == 0 {
			return fmt.Errorf("model %q: needs cmd (spawned) or proxy (standalone proxy model), or a placements: list; legacy backends: lists are no longer supported — flatten to one path per model and compose fallbacks as a lane", name)
		}
		if m.Cmd != "" && m.Server == "" {
			return fmt.Errorf("model %q: cmd set but no server", name)
		}
		if m.Cmd == "" {
			// A pure proxy model has no local lifecycle: residency knobs are
			// meaningless on it and almost certainly a config mistake.
			if m.Sticky != nil || m.Persistent || len(m.RAMUsage) > 0 || m.Swap != nil || m.Server != "" {
				return fmt.Errorf("model %q: sticky/persistent/ramUsage/swap/server only apply to cmd models", name)
			}
		}
		for k := range m.Modalities {
			if !KnownModalities[k] {
				return fmt.Errorf("model %q: unknown modality %q (want text|image|audio)", name, k)
			}
		}
		// Placement rules first: a badly-shaped placement list makes every
		// downstream check ambiguous about which placement it is complaining of.
		if err := m.ValidatePlacements(name); err != nil {
			return err
		}

		for _, pl := range m.PlacementList() {
			if pl.Server == "" {
				continue
			}
			srv, ok := c.Servers[pl.Server]
			if !ok {
				return fmt.Errorf("model %q placement %q: unknown server %q", name, pl.Name, pl.Server)
			}
			for pool := range pl.RAMUsage {
				if _, ok := srv.Pools[pool]; !ok {
					return fmt.Errorf("model %q placement %q: ramUsage pool %q not declared on server %q",
						name, pl.Name, pool, pl.Server)
				}
			}
		}
		if m.Server != "" && m.Cmd != "" {
			// NO ramUsage REQUIREMENT. It used to be mandatory on a host that
			// could not measure per-process memory, because "reserve the whole
			// pool, then measure" never reached the measuring part there and the
			// server silently became one-model-at-a-time.
			//
			// That was a workaround for a missing implementation, not a rule
			// worth keeping. Unified-memory hosts CAN attribute memory to a
			// process group — the resident set is the footprint — so the
			// measure-and-govern path works there like everywhere else, and
			// sampleVRAMPeak records the MAXIMUM observed rather than a spot
			// reading, which is what catches a backend that loads and frees an
			// mmproj mid-life.
			//
			// Requiring a hand-written number bought nothing that measurement
			// does not do better: every declaration observed in this config was
			// wrong (16GB declared against 33.7GB measured; 16GB against 23GB on
			// bonsai), and a wrong declaration is worse than an absent one
			// because absence is honest and triggers the conservative path.
		}
		if m.Server != "" {
			srv, ok := c.Servers[m.Server]
			if !ok {
				return fmt.Errorf("model %q: unknown server %q", name, m.Server)
			}
			if _, err := ParseSizes(m.RAMUsage); err != nil {
				return fmt.Errorf("model %q ramUsage: %w", name, err)
			}
			for pool := range m.RAMUsage {
				if _, ok := srv.Pools[pool]; !ok {
					return fmt.Errorf("model %q: ramUsage pool %q not declared on server %q",
						name, pool, m.Server)
				}
			}
		}
	}
	for name, lane := range c.Lanes {
		if _, clash := c.Models[name]; clash {
			return fmt.Errorf("lane %q: name collides with a model", name)
		}
		if len(lane.Members) == 0 {
			return fmt.Errorf("lane %q: no members", name)
		}
		for i, mem := range lane.Members {
			if mem.IsSelector() {
				// Deliberately NOT checked against the roster: the roster is
				// empty at load and fills on the refresh loop, which is the
				// whole reason this form exists. A selector matching nothing
				// contributes nothing, exactly like a provider that has not
				// refreshed yet.
				continue
			}
			if mem.Model == "" {
				return fmt.Errorf("lane %q member %d: needs a model name or a provider selector", name, i)
			}
			if _, ok := c.Models[mem.Model]; !ok {
				return fmt.Errorf("lane %q member %d: unknown model %q", name, i, mem.Model)
			}
		}
	}
	// A misspelled sampling default must not load. It has no error surface at
	// request time — an unrecognised value simply reads as "instruct" — so every
	// unmarked request would quietly get the wrong mode's sampler, which is the
	// precise failure this block of config exists to prevent.
	for _, name := range func() []string {
		out := make([]string, 0, len(c.Models))
		for n := range c.Models {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}() {
		s := c.Models[name].Sampling
		if s == nil {
			continue
		}
		switch s.Default {
		case "", "instruct", "thinking":
		default:
			return fmt.Errorf("model %q: sampling.default %q must be \"instruct\" or \"thinking\"", name, s.Default)
		}
		if s.Thinking.Empty() && s.Instruct.Empty() {
			return fmt.Errorf("model %q: sampling block declares no profile; remove it or give it one", name)
		}
	}

	// Aliases are checked after models AND lanes, because an alias can collide
	// with either. Sorted so a config with two faults always reports the same
	// one — map order would otherwise make the error flap between loads and
	// send an operator chasing a moving target mid-cutover.
	claimedBy := make(map[string]string, len(c.Models))
	modelNames := make([]string, 0, len(c.Models))
	for name := range c.Models {
		modelNames = append(modelNames, name)
	}
	sort.Strings(modelNames)
	for _, name := range modelNames {
		for _, a := range c.Models[name].Aliases {
			switch {
			case a == "":
				return fmt.Errorf("model %q: empty alias", name)
			case strings.Contains(a, "*"):
				// Only a MODEL KEY may glob; alias matching is exact, so a
				// starred alias would load clean and silently never match.
				return fmt.Errorf("model %q: alias %q may not contain '*' (only a model key can be a glob template)", name, a)
			case a == name:
				return fmt.Errorf("model %q: alias %q repeats the model's own name", name, a)
			}
			if _, clash := c.Models[a]; clash {
				return fmt.Errorf("model %q: alias %q collides with a model (rename that model; resolution returns on an exact model hit, so the alias would never fire)", name, a)
			}
			if _, clash := c.Lanes[a]; clash {
				return fmt.Errorf("model %q: alias %q collides with a lane (a lane is resolved first, so the alias would never fire)", name, a)
			}
			if prev, dup := claimedBy[a]; dup {
				return fmt.Errorf("model %q: alias %q already claimed by model %q", name, a, prev)
			}
			claimedBy[a] = name
		}
	}
	for key, grp := range c.Keys {
		if _, ok := c.PriorityGroups[grp]; !ok {
			return fmt.Errorf("key %q: unknown priorityGroup %q", key, grp)
		}
	}
	return nil
}

// LoadBytesForTest parses config from bytes with the same resolution Load does.
// Exported for tests in other packages that need a fully resolved Config.
func LoadBytesForTest(b []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	c.projectFirstPlacement()
	if err := c.resolveExtensions(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
