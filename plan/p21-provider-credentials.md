# P21 — Provider credentials (multi-key providers, scoped budgets, key ACLs)

Status: **COMPLETE — P21a–g shipped, §8 and §9 resolved.** Pointer from §6 roadmap in plan.md.
Supersedes the single-credential assumption in `p16-free-aggregator.md` §4.

> This is a design doc, not a changelog. It states the problem, the shape of the
> solution, the facts it rests on, and the unknowns to close before building. It
> follows §0: each build sub-unit ships green + tested; the plan updates in the
> same commit.

## 1. Problem & insight

P16 already named the insight this rests on:

> *"free quota is enforced per ACCOUNT and those budgets are independent —
> across providers AND **across multiple accounts of the same provider**"*

The implementation did not follow it. `config.Provider` is
`{Proxy, Provides, Discover}` — **one** `proxy.headers`, therefore **one**
credential. Two OpenRouter keys today means two `providers:` entries pointing at
the same host, with nothing tying them together and no way to say "these are one
provider with two accounts."

That gap blocks four things at once:

1. **Pooling accounts of one provider** — the literal P16 insight, unbuilt.
2. **Budgets at the level the provider enforces them.** OpenRouter meters per
   API key. corrallm meters per served model name (P20, 2026-08-12), which is
   the wrong granularity in both directions: one key's spend is split across
   N discovered models, and a `usd` cap written on a discovery template becomes
   N independent caps (12 models × $200 = $2,400, not $200).
3. **Telling callers apart.** `keys:` maps a caller to a priorityGroup and
   nothing else. There is no way to say "`life-raglit` may use the work key,
   `aw3` may not."
4. **A UI for any of it**, because there is no object to render — a credential
   is not a thing corrallm models.

**This is not an OpenRouter feature.** A bespoke OpenRouter extension would
re-implement discovery, roster refresh, quota, cooldown and cost normalisation
that are already generic and already work. The missing primitive is
*credentials*, and Anthropic and Groq want it the moment a second key exists.
Build it once, generically; OpenRouter is only the first caller.

## 2. What is already true (verified 2026-08-12 — do not re-derive)

Three things that look like work and are not:

- **Cost is already normalised across providers, correctly.** `proxy.go`'s
  usage extraction carries `Cost float64 json:"cost" // provider-reported $
  (OpenRouter); 0 if absent`, and the pricing path prefers it:
  *"Provider-reported cost wins when present (OpenRouter): treat it as the
  authoritative total, keeping the table-derived class ratio."* Falls back to
  `commandCosts` coefficients when the provider reports nothing. Either way the
  result splits into cached/processed/generated, so a local GPU model and a
  remote API land in one schema. **A `usd` budget therefore charges the real
  invoice number on OpenRouter, not an estimate.**
- **Tokens likewise.** `extractUsage` normalises prompt/completion/cached plus
  `tp/s` and `tg/s` across llama.cpp and remote providers; the activity table
  stores them uniformly.
- **CORRECTION to an earlier claim in this repo's session notes:** it was said
  that "commandCosts has no `openrouter-*` entry, so those models price at
  zero." **False.** Discovered OpenRouter models take `type: chat` from the
  discovery template, and every type in use has coefficients. Nothing prices at
  zero. The real wrinkle is smaller and different: a *free* model reports
  `cost: 0`, so it falls back to `chat`'s coefficients, which are **local GPU
  energy** (`generateWattsPerToken: 0.0013`) — pricing a remote API call as if
  it burned local electricity. Wrong in principle, negligible in magnitude, and
  only on models whose true cost is zero. Fix with a `remote-free` cost type
  whose coefficients are zero; do not "fix" it by removing the fallback.

## 3. Config schema

```yaml
extensions:
  free:
    providers:
      openrouter:
        proxy:                                  # SHARED: host, port, basePath
          host: openrouter.ai
          port: 443
          basePath: /api
        credentials:
          - name: personal                      # stable id; the budget key
            headers: {authorization: "Bearer ${OPENROUTER_KEY_PERSONAL}"}
            limits:
              - {req: 20,  per: minute}
              - {usd: 50,  per: month}
            allow: [aw3, dun]                   # corrallm keys that may use it
          - name: work
            headers: {authorization: "Bearer ${OPENROUTER_KEY_WORK}"}
            limits:
              - {usd: 200, per: month}
            allow: [life-raglit]
        discover: {...}                          # per-credential at runtime, see §6
```

`credentials` is additive: a provider with `proxy.headers` and no `credentials`
keeps working exactly as today (an implicit single credential named `default`).
No existing config changes.

`limits` is the P20 `LimitSet` unchanged — flat list, `{req|usd|sec, per}`,
legacy shapes still parsed. It lives in **`internal/config/limit.go`**
(`Limit`, `LimitSet`, `legacyLimits`), with the window table in
`internal/config/rate.go`.

**P21a touchpoints** — the whole phase is these four:
- `internal/config/config.go:234` — `Provider{Proxy, Provides, Discover}`, the
  struct that gains `Credentials`.
- `internal/config/config.go:612` — `FreeTier`, whose `Provider` field is a
  label today and must stay one; the credential name is the new identity.
- `internal/proxy/proxy.go:243` — the ONE `quota.SetLimits` call site, which is
  where per-credential scoping will fan out.
- The managed marshaller already round-trips this shape: on 2026-08-12 it read a
  legacy `{rpm: 20, rpd: 1000}` out of the live config and rewrote it as the
  list form unprompted. Assume the same for `credentials` and verify.

## 4. Budget scope cascade

Today: one scope, keyed by served model name. Target: **four**, any subset
declared, a request charging every declared one and gated on all of them.

| scope | key | declared at | why |
|---|---|---|---|
| model | served name | `model.freeTier.limits` | exists today (P20) |
| **credential** | `cred:<provider>/<name>` | `credentials[].limits` | **where the provider actually meters** |
| **provider** | `prov:<provider>` | `providers.<p>.limits` | "all my OpenRouter keys together ≤ $X" |
| **global** | `*` | top-level `limits:` | blast-radius guard: "this box never spends > $50/day regardless of misconfiguration" |

The ledger does not care what a key means — `chargeLocked` and `Available`
already take a key and iterate windows. The change is a **loop over resolved
scopes** instead of a single lookup, plus scope resolution for a request. The
counter, persistence, falloff and legacy-label shim all carry over untouched.

Charging is post-hoc, as P20 established: measured max single request on this
box over 30 days was **$0.0322**, i.e. 0.6% of a `$5/hour` budget. Pre-charging
an estimate is not worth needing an estimate to be wrong about.

## 5. The ACL axis (`allow:`)

New relation: **corrallm key → permitted credentials**. Orthogonal to
`keys: <key> → priorityGroup`, which stays as-is (policy, not permission).

- **Allowlist, not denylist.** Absent `allow` = every caller may use it. Present
  = only those listed. Noisier, and the failure direction is "denied" rather
  than "spent someone else's money."
- A caller with no permitted credential on a provider **falls through to the
  next backend** rather than erroring — identical to an exhausted budget, which
  the selector already handles. A directly-named model with no permitted
  credential is a 503.
- ACL is evaluated in the candidate filter beside `filterByPaused` /
  `filterByQuota`, so it composes with the existing walk instead of being a new
  exit.

## 6. Per-credential discovery

OpenRouter's `/v1/models` differs by key (free tier vs paid vs BYOK), so a
roster is a property of a **credential**, not a provider. `p.roster.Set(provider,
…)` becomes keyed by `provider/credential`.

Consequence worth stating: the same upstream model reachable by two credentials
is **one served name backed by two backends**, not two served names. That is
exactly the lane/selector shape P16 already built — the selector picks whichever
credential has budget. This is the payoff for doing it generically.

## 7. Model approval (curation ≠ discovery)

**Discovery enumerates what a credential CAN reach. Approval decides what gets
served.** Today only the former exists: `DiscoverFilter` is a set of automated
predicates — `free`, `inputModality`/`outputModality`, `minContext`, `exclude`
substrings — plus `limit: N` ordered by context descending. Anything passing them
**auto-enrols the moment the roster refreshes**, and vanishes when it churns out.
No human is in the loop, and no decision is recorded.

That was defensible while every discovered model was free: the blast radius of a
wrong enrolment was a bad answer. **P21 changes the stakes.** The point of
credentials is that some of them are paid keys, and auto-enrolling a discovered
paid model means corrallm starts spending money on a model nobody chose. The
`usd` budgets in §4 bound the damage; they do not make the decision.

Approval is also where the assumption P16 flagged and never closed gets closed:

> *"Uniform quality is an ASSUMPTION applied to every discovered model, and a
> wrong one — a 550B and a 9B nano do not deserve the same lane score. It stands
> until llm-bench measures the model; nothing here can know better."*

A human approving a model is the cheapest moment to set its real `quality`,
`contextPerRequest` and lane membership.

### State, and where it lives

A discovered model is in exactly one state per **(credential, upstream id)**:

| state | meaning | served? |
|---|---|---|
| `pending` | discovered, no decision yet | no |
| `approved` | a human said yes | yes |
| `rejected` | a human said no | no, and never re-prompt |
| `churned` | was approved, has left the provider's roster | no, decision retained |

Three properties this must have, each learned from something that already bit:

1. **Decisions persist and survive roster refresh.** A rejected model that
   reappears next refresh must stay rejected, or the UI becomes a treadmill.
   State belongs in SQLite beside the quota counters, not in the managed YAML —
   it is operational state, not declared intent (the same argument `Pause`
   makes for itself in `internal/proc/pause.go`).
2. **`churned` retains the decision.** A model that leaves and returns keeps its
   approval; P16e's refresh already detects churn and marks backends stale, so
   this is a state transition on existing machinery, not new detection.
3. **Approval is per credential, not per provider.** The same upstream id may be
   wanted on the free key and refused on the paid one — that IS the distinction
   §6 introduces, and collapsing it would defeat it.

### ❓ Open: default posture for a newly discovered model

Two coherent policies, and the choice is the user's:

- **auto-approve, human vetoes** — today's behaviour. Zero friction, and safe
  while everything is free. On a paid credential it spends before anyone looks.
- **pending by default, human approves** — nothing serves until chosen. Safe,
  but a provider with a churning roster generates a steady approval queue.

Recommended: **per-credential policy**, defaulting on whether the credential's
declared limits contain a `usd` window. A credential with a spend budget is by
definition one that can cost money, so `approvalRequired: true` is the honest
default there and `false` where everything is free. Explicitly overridable:

```yaml
credentials:
  - name: work
    limits: [{usd: 200, per: month}]
    approvalRequired: true      # default: true, because a usd window exists
```

This keeps the current zero-friction behaviour for the free roster that exists
today, and refuses to spend on an unreviewed model the moment a paid key appears.

### UI

The approval queue is the natural landing page for §10's provider view: pending
models per credential, with the provider's advertised context/pricing/modality
alongside the fields a human sets on approval (quality, lane membership).
Rejections need to be visible and reversible, or a mis-click is permanent and
invisible.

## 8. Lane assignment (and the load-order blocker)

Approval should also place a model in a lane — that is the moment a human knows
what the model is for. Two things stand in the way, one easy and one not.

### The easy half: the ACL interaction is already solved

If a lane contains a model backed by a credential the caller may not use, the
caller **skips that member and walks on**. That is the same fall-through §5
specifies for a denied credential and the selector already implements for an
exhausted budget — a lane is an ordered list and a member that cannot serve this
caller is simply not a candidate. No new exit, no error.

Consequence worth stating plainly: **a lane is not a permission boundary.** Two
callers requesting the same lane can legitimately be served by different
backends, because they have different credential access. That is the feature, not
a bug, but it means "which model answered?" must stay visible per request — see
the attribution risk in §12.

### ✅ RESOLVED: membership by selector (option 2)

`config.go:1617` rejects a lane member naming an unknown model:

```go
return fmt.Errorf("lane %q member %d: unknown model %q", name, i, mem.Model)
```

Lane membership is validated at **config load**; discovery runs later on the
refresh loop and lands in a dynamic overlay. The live config already documents
the consequence as a known limitation:

> *"Groq and Cerebras are members of the `free` lane above; OpenRouter's
> discovered models are not (lane membership validates at config load, before any
> refresh) — pin those by name."*

So today a discovered model can only be reached by pinning its name. **Assigning
one to a lane from an approval UI is impossible until this is resolved**, and it
is the single biggest unknown in this document.

Three candidate shapes, in ascending order of work:

1. **Deferred membership.** Lane members may name a model that does not exist at
   load; validation warns instead of failing, and the member is inert until the
   name resolves. Cheapest, and it weakens a check that currently catches real
   typos — a misspelled member would silently never serve.
2. **Membership by selector, not by name.** A lane declares
   `{provider: openrouter, approved: true}` rather than a list of ids, and
   resolves against the live roster. Typo-proof by construction and it survives
   churn, but it is a new concept in the lane schema and changes what a lane
   *is*.
3. **Approval writes the config.** The UI adds the resolved id to `lanes:` via
   the existing config API, so membership stays static and validated. Honest and
   requires no engine change — but it churns the managed YAML on every roster
   change, which is exactly what `Pause` refused to do for operational state.

**DECIDED (2026-08-14): (2), keeping named membership for pinned models**, so
the two forms do not compete.

    lanes:
      free:
        members:
          - groq-llama-70b            # explicit: keeps its declared position
          - {provider: openrouter}    # selector: resolves against the live roster
          - {provider: openrouter, minQuality: 3}   # or a filtered slice

Why not the other two: both make membership a NAME, and a name is exactly what
keeps disappearing on a roster with `refresh: true`. Tolerating unknown names
handles churn by going quiet, which is indistinguishable from a typo; having the
UI rewrite `lanes:` on every refresh puts operational state in the document,
which is what Pause refused to do. A selector says what is WANTED, so churn
stops being an event to handle.

A selector expands IN PLACE, so explicit members keep their exact declared
position and the two forms compose into one ordered ladder. Expansion orders by
quality descending then name — map iteration order would reshuffle the fallback
ladder on every restart. Validation skips selectors (the roster is empty at load,
which is the whole point) but still REJECTS an unknown named member, so the typo
check the first alternative would have weakened is intact.

STILL TO BUILD (P21e2): per-model lane choice and ordering at approval time. The
selector is blanket — every model a provider contributes joins. Choosing *which*
lanes a model joins, and where in the ladder, needs the approval record to hang
that state on.

## 9. ✅ RESOLVED: secrets live in ~/.corrallm/credentials

corrallm **currently never persists a secret**. Config holds references only —
`${OPENROUTER_API_KEY}`, or `authTokenCommand` running a shell command
per-request. Expansion happens at load; the stored YAML keeps the literal.

That is load-bearing, not incidental: **`/api/v1/config/{kind}/{name}/yaml`
serves config as YAML**. Today it returns a reference. A UI that stores keys
makes that endpoint a secret-disclosure surface, along with every config backup
and every time a human pastes config into a chat window.

| option | UX | exposure | verdict |
|---|---|---|---|
| references only (UI manages *names*) | weakest — still edit env/systemd | none | preserves current property |
| **separate secrets file, `0600`, never served** | good | file only | **recommended** |
| SQLite | good | DB (unencrypted, but not `cat`-ed or pasted) | acceptable |
| inline in managed YAML | best | **served by the admin API** | argue against |

**DECIDED (2026-08-14): the secrets file**, at `~/.corrallm/credentials`,
`key=value` in the same properties-lite format the operator knobs use.

Simpler than the `credentialRef` indirection sketched above, because config
already references secrets and always has:

    headers: {authorization: "Bearer ${OPENROUTER_KEY_WORK}"}

`${...}` expansion now consults the store first and the process environment
second, so there is NO schema change, nothing to migrate, and the document
`/api/v1/config/*` serves still contains only references. A credential added by
a UI is a line in this file rather than an edit to the thing the API hands out.

File-first precedence, because the store is the deliberate managed source — a
value someone just typed must take effect, and env-first would let an ambient
variable win silently. Shadowing is warned about by name at load, so the
precedence is discoverable rather than folklore.

A group- or world-readable file is REFUSED at startup, the way ssh refuses a
private key, rather than loaded with a warning nobody reads: failing is loud and
fixable (the error names `chmod 600`), loading is silent and permanent. Verified
live — 0644 aborts startup with that message, 0600 loads, missing is a no-op so
every deployment predating this keeps resolving from the environment.

## 10. UI

Plumbing exists; the provider-shaped view does not.

- ✅ `ui/src/routes/config.tsx`, `ModelForm.tsx`, and the API the session drove
  all day (`config/{kind}/{name}/yaml`, `upsertModel`, `trialModel`,
  `probeModel`).
- ❌ `kind` is `model|server|lane|group|extension|key` — an extension is edited
  as a **raw YAML blob**. No per-provider form, no credential list, no
  model-picker showing what a key can reach, no spend-vs-budget display.
- Cost/token normalisation being solved (§2) means the UI **renders existing
  data**; no new metering to build. `/api/v1/quota` already exposes per-key
  windows with used/limit/blocked.

## 11. Phased build order (each a green, tested sub-unit per §0)

- ✅ **P21a** `credentials` schema + implicit-`default` back-compat. Config-only,
  no behaviour change. `Provider.CredentialList()` always returns at least one,
  so callers never branch on "does this provider have credentials"; a provider
  declaring none gets a synthesised `default` and every pre-existing config is
  untouched. Credential headers MERGE over the provider's, so shared ones
  (`anthropic-version`) stay declared once and only auth repeats. Validation
  refuses an unnamed or duplicated name — names key persisted budget counters,
  so a duplicate would silently share a budget. Round-trip verified twice: in a
  unit test, and live against the running managed marshaller, which rewrites
  the file on every edit and would otherwise delete a field it could parse but
  not re-emit. `${ENV}` references stay literal through both.
- ✅ **P21b** Credential as a routing target: one served name, N credential-backed
  backends; selector picks by budget. Closes the P16 insight.
  - ✅ *config layer.* `ResolveServed` expands one served name into one
    Candidate per declared credential (order follows config, so the walk is
    deterministic). `Candidate.Target()` merges the credential's headers OVER
    the provider's shared ones and overrides authTokenCommand; the provider's
    own target is never written back into. `Candidate.ProcKey()` appends
    `@<credential>` so two accounts cannot share a process — they are distinct
    upstreams with distinct auth and distinct budgets, and collapsing them would
    make the second reuse the first's connection and quota. A provider with no
    declared credentials yields exactly one Candidate with a nil Credential, so
    every existing config resolves as before.
  - ✅ *routing wire-up.* `EnsureReady` takes the credential — it is the ONE
    door every load comes through, so merging there covers inference, realtime
    and passthrough alike. The process key gains `@<credential>` and the target
    gets the credential's headers merged over the provider's (copied first: a
    write-back would leak one account's key into the next request resolving the
    same model). A nil credential leaves both untouched, so nothing that
    persisted the historical key is orphaned.
    Budget keying moved with it: `Candidate.QuotaKey()` returns the credential
    scope, and gating, `ObserveResponse` and `Charge` all use it. Verified live —
    two credentials on the real openrouter provider registered as
    `cred:openrouter/personal [req/minute 20]` and `cred:openrouter/work
    [usd/month 200]`, two independent budgets.
    KNOWN: `SetLimits` runs at proxy construction only, so a credential added by
    a live config edit needs a restart to register. Pre-existing — per-model
    freeTier limits behave the same way.
- ✅ **P21c** Scope cascade (§4). `Candidate.QuotaScopes(cfg)` returns the
  budgets a request answers to, narrowest first; it is charged against ALL of
  them and refused if ANY is exhausted. Only DECLARED scopes appear, so cost is
  proportional to what was configured. Provider limits (`providers.<p>.limits`)
  and a box-wide guard (top-level `limits:`) are new; the ledger keys on strings
  and did not change. Verified live: two credentials at $10/$50 bounded jointly
  by their provider at $100 — which per-credential budgets alone cannot express,
  and N of them multiply rather than cap.
- ✅ **P21d** ACL (§5): `filterByCredential` sits beside filterByPaused/
  filterByQuota so it composes with the walk. Unlike filterByQuota it does NOT
  fall back to the unfiltered list when everything is dropped — an exhausted
  budget is temporary and serving anyway is arguable, a permission is not.
  Applied BEFORE the budget filter, so an exhausted-but-permitted account cannot
  mask a forbidden one.
- ✅ **P21e** Per-credential discovery (§6). `DiscoverTargets()` emits one target
  per credential, each carrying that credential's merged auth, because the
  catalogues genuinely differ by key (free tier vs paid vs BYOK).
  `SetDiscoveredFor(provider, credential, models)` records WHICH credentials saw
  each model, and expansion offers a model only on the accounts that can serve
  it — a union would route to a 404 that looks like provider flakiness rather
  than a config error. Retraction is scoped to the refreshing credential, so two
  accounts refreshing in turn do not erase each other, and a model disappears
  only when NO credential still sees it. Verified live: `discovered models
  provider=openrouter credential=default kept=10 of=413`.
- ✅ **P21e2** Model approval state + `approvalRequired` policy (§7).
  `model_approval` in SQLite, keyed (provider, credential, model) — the same
  upstream id can be wanted on one account and refused on another, because a
  paid key's model is a spending decision the free key's is not. Persisted for a
  sharper reason than config-vs-state: a rejection that did not survive a
  restart puts the model back in the queue on the next refresh and asks the
  operator the same question forever. `approvalRequired` is per credential and
  OFF by default, so turning it on for a paid key leaves a free roster working
  untouched, and DECLARED models are never gated — asking someone to approve
  what they just wrote down would be theatre.
- ✅ **P21e3** Lane membership for discovered models (§8). Two complementary
  mechanisms, neither sufficient alone:
  - *selector* — `{provider, server, device, minQuality}` says "everything
    matching this", resolved against the live roster. Scoped by HOST and DEVICE
    as well as provider, because a local Qwen, the same family on an attached
    box, and a paid remote are different backends with different latency, cost
    and failure modes; a lane exists to order them, not to pool them.
  - *approval placement* — `{lane, order}` recorded per model says "this one, in
    these lanes, at this position". The per-model half a blanket selector cannot
    express.
  Declared members keep the front of the ladder; approved additions follow in
  their chosen order. An approval's `quality` also replaces the discovery
  template's uniform guess, which p16 flagged as "an ASSUMPTION applied to every
  discovered model, and a wrong one".
- ✅ **P21f** Secrets store: `~/.corrallm/credentials`, file-first `${...}`
  resolution, 0600 enforced. No schema change — config keeps holding references.
- ✅ **P21g** Approvals API + UI. `GET /api/v1/approvals` returns decisions AND
  the discovered models still awaiting one, composed server-side so no client
  has to diff the roster against the decisions and get it subtly different.
  `POST /api/v1/approvals/decide` records approved/rejected with lanes and
  positions and re-installs the table immediately. The page pairs the decision
  with its placement in one click — approving and then separately placing would
  leave a model serving from nowhere in between.
  Also: a **Providers** page with credential CRUD. The §9 objection turned out
  to be narrower than stated — the property is that the API never SERVES a
  secret, and writing one is a different operation. `POST /api/v1/secrets` is
  write-only with no read counterpart, so a form can set a value and see that it
  EXISTS (`hasSecret`) without any endpoint able to return it. Verified: after
  storing a secret and creating a provider referencing it, the value appears in
  neither `/api/v1/providers` nor the config YAML — config holds
  `Bearer ${NAME}` and nothing else.
- ✅ **P21h** Catalogue browsing + provider presets + hand-picked models
  (2026-08-15). Closes the gap P21g left: approvals gated what a FILTER had
  already chosen, so a model the filter excluded was unreachable except by
  loosening the filter and admitting a hundred others with it. The catalogue was
  fetched, narrowed, and the remainder discarded with no record it existed.
  - **`internal/config/presets.go`** — 18 verified OpenAI-compatible endpoints
    (host, port, basePath, auth header, suggested filter, docs), served over
    `GET /api/v1/providers/presets`. In Go so `validate`, the CLI and the UI
    share one table and a corrected path ships with the binary. Each carries
    `api: "openai"` — the seam a non-OpenAI adapter becomes a value of rather
    than a schema change. Gemini is deliberately absent for exactly that
    reason: its OpenAI surface is rooted at `/v1beta/openai/`, and no basePath
    makes `basePath + "/v1/models"` reach it.
  - **`GET /api/v1/providers/catalog`** — one credential's `/v1/models` live,
    annotated with what corrallm already decided per row (state, enrolled,
    passesFilter, declared, conflictsWith). Live rather than cached, because the
    cache holds only the survivors and the point is the rows that did not
    survive.
  - **Picks** (`internal/config/pick.go`) — an approval carrying the provider's
    own `upstream` id ENROLS a model instead of merely gating one. They live in
    the same overlay as discovered models (every downstream reader already knows
    it) and are re-applied after each `SetDiscoveredFor`, which replaces a
    credential's contribution wholesale — without that a hand-picked model
    vanished within one refresh interval, looking like the provider had dropped
    it.
  - **`manual: true` on a provider** — the third way to contribute models, so
    "contributes nothing" stays an error and a half-written provider block is
    still caught.
  - **Bug found and fixed**: `reloadInto` built a fresh Config and installed it
    without re-seeding approvals, so ANY config write through the UI silently
    reset every decision until the next decide or restart — un-rejecting
    rejections and (once picks existed) dropping every hand-picked model.
    `api.InstallApprovals` is now the single install path, shared by startup and
    reload.
  - Verified live on an isolated daemon: 413-row OpenRouter catalogue, 10
    passing the filter; picking the paid, filter-excluded `x-ai/grok-4.20` put it
    in `/v1/models`, and it survived a config write, a discovery refresh and a
    restart.
- ✅ **P21i — approvals removed; one concept, SELECTION** (2026-08-15). P21h left
  two vocabularies in place: an approval queue (pending/approved/rejected, gated
  per credential by `approvalRequired`) and a hand-pick that rode on top of it.
  Against OpenRouter's four hundred models a queue of questions is not a control,
  it is a chore, and the second concept was only ever there to serve the first.
  Both collapse into: **you look at a provider's directory and assign models,
  optionally into a lane at a priority.**
  - `model_approval` → `model_selection`, with **no state column**. Presence is
    the predicate; "reject" and "change your mind" are the same DELETE. Approved
    rows migrate across, pending/rejected are dropped (with the gate gone a
    rejection holds nothing back). Migration + a second open past the dropped
    table are unit-tested; the runner now tolerates "no such table" for exactly
    this shape.
  - The `servesUnderApproval` gate and `Credential.ApprovalRequired` are gone. An
    old config carrying the field still loads and it is ignored.
  - API: `listApprovals`/`decideApproval` → `listSelections` / `assignModel` /
    `unassignModel`. UI: the Approvals page and its nav entry are deleted; the
    directory dialog is Add/Remove; Providers grows an **Assigned models** panel.
  - **Bug found by a new test**: contributions were applied ADDITIVELY, so
    unselecting a model left it serving — `discovered` is a union of the fetch
    and the selections, and a union cannot be un-merged. Inputs are now kept
    separately (`Config.fetched`) and `discovered` is REBUILT from both on every
    change. Also fixes the symmetric case nobody had hit: a model dropped from a
    filter while another credential still saw it.
  - Presets gain `PickByHand`, true for the large-catalogue providers
    (openrouter, together, fireworks, deepinfra, nebius, hyperbolic, openai): the
    add form starts them on "choose them yourself", and their suggested filter is
    what the DIRECTORY opens pre-filtered to, not an import rule.
  - Verified end to end on an isolated daemon and in the UI: assign with
    lane+priority → serves; survives config write and restart; unassign →
    out of service immediately, with the filter's own 10 models untouched.

All phases shipped. What is deliberately NOT built:
  - Provider-level spend rollup in the UI. The budgets exist and are enforced;
    only the visualisation is missing.
  - Editing an EXISTING provider's discovery filter from the form. Upsert keeps
    `provides`/`discover` when a provider already exists, so a form save cannot
    silently drop a hand-written model list — but it also cannot change the
    filter, which stays a YAML edit.
  - `SetLimits` still runs at proxy construction, so a credential added by a
    live config edit needs a restart before its budget registers. Pre-existing;
    per-model freeTier limits behave the same way.

## 12. Risks & non-goals

- **Risk: scope fan-out multiplies writes.** Four scopes × N windows = up to a
  dozen counter rows per request. Mitigated by the WAL switch (2026-08-12:
  15.2ms → 0.052ms per write, measured), but batch the flush if it shows up.
- **Risk: `usd` accuracy depends on the provider reporting cost.** Where it does
  not, budgets enforce against `commandCosts` coefficients — an estimate. Say so
  in the UI rather than implying the number is authoritative everywhere.
- **Risk: a shared served name across credentials makes "which key paid for
  this?" ambiguous in the activity log.** Record the credential on the activity
  row, or the spend view cannot be reconciled against a provider invoice.
- **Non-goal: billing-cycle budgets.** Windows are sliding falloff counters;
  `month` is 30 days sliding, deliberately (no month-end cliff, no thundering
  burst after a reset). Calendar-cycle semantics need a different mechanism.
- **Non-goal: hiding a provider's own routing.** P16 §7 already decided to
  reconcile with OpenRouter's routing, not duplicate it.

---

**next** — write P21a (schema + back-compat), which is unblocked and touches
only `internal/config`.
**risks** — see §12; the sharpest is credential attribution in the activity log,
which is cheap now and expensive to retrofit.
**blocking decisions** — §9 secrets location (user); P21f/g wait on it. §8 lane
membership shape for discovered models; P21e3 waits on it, and it is the bigger
unknown of the two.
**optional extensions** — `remote-free` cost type (§2); batching the counter
flush (§12); per-credential health/latency in the selector.
**assumptions made** — that OpenRouter meters per API key rather than per
account; verify before P21c, since it decides whether `credential` or a new
`account` scope is the right budget key.
