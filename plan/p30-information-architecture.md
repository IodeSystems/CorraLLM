# P30 — information architecture: one page, one question

> Opened 2026-09-04, out of the first Lenny run (`plan.md` §6, harness in `done.md`
> § UX verification) and a full inventory of all 18 dashboard routes.
> **Status: ◐ phase A landed 2026-09-05; B–D open.** What moved so far: the nav rail's
> labels, the `Lane`→`Group` column header (§5), and phase A (§6).

## 0. The finding under the findings

Lenny's ten defects are mostly symptoms of one structural fact:

> **Pages are named after the data they render — Activity, Usage, Groups, Keys, Hosts,
> Providers — not after the questions people arrive with.** So a question is answered in
> pieces on four pages, and no page answers one question completely.

Three measurements out of the inventory, none of which is a matter of taste:

**1. One fact renders on up to four pages, in four shapes.** A caller's cost and request
count appears on `/keys` (charts + roster), `/keys/$key` (stat row + charts), `/usage`
("By key", "By key over time") and `/activity` (the log, filtered by key). Nothing says
which is authoritative, and they do not agree, because —

**2. Every panel carries its own time window and there is no page-level control.**

| panel | window |
|---|---|
| Utilization, Come back later, Journeys | last 60 minutes |
| Caller service profiles, all of `/usage` | last 24 hours |
| `KeyCharts` on `/keys` | 6h / 24h / 7d, its own toggle |
| Recent / the activity log | the newest 100 rows, whenever they were |
| Bench panels | per run, whenever that ran |

This is the real reason Lenny could not get back to 09:00: **the product has no time
axis, only per-panel constants.** A time filter on the activity table would have been
the pointwise fix and would still have left the other five panels arguing.

**3. The UI contradicts its own schema on the word *lane*.** `/activity`'s in-flight table has a
column headed **`Lane`** whose cell is the caller's **priority group** (`r.group`).
`/providers` and Overview have panels headed **`Lanes`** whose rows are **ordered
fallback lists of models** (`chat`, `free`). `/groups` calls the first one "Priority
groups" and `/keys` calls it a lane in its own comments. `plan.md` §3 defines a lane as the fallback list and
`priorityGroup` as the caller's policy unit — so the config is right and that column header
is simply wrong. A person who learns either meaning is wrong on the other page. Canon: *a
shared word is only safe when the objects are the same.* See §5.

Duplication the inventory also names, for the record: `ACTIVE REQUESTS` renders on both
Overview and Activity; `/usage`'s "Resident models" and per-server pool bars are memory
subjects on a cost page; `/groups`' "Backend load" and `/activity`'s "Utilization" are
the same pressure measured twice; `/hosts` shows one machine a chip reading `unknown`
and, further down, `connection refused`.

## 1. Who reads this, and what they arrive asking

| | who | the question they arrive with | today they must visit |
|---|---|---|---|
| **A** | the owner on a bad day (Lenny run 1) | *Is it broken, is it me, will it clear?* | Overview, Activity, Hosts, History |
| **B** | the same person, tuning | *Where is the capacity going and what should I change?* | Overview, Usage, Groups, Hosts, Bench |
| **C** | a caller handed a key | *What can I call, what am I allowed, did my calls work?* | Overview, Keys, Usage, Quota |
| **D** | whoever changes it | *Add a model / a host / a provider; what changed and can I undo it?* | Providers, Hosts, History, Config |

**A is the first-run audience and the one the product currently serves worst.** Nothing on
the home screen answers "is it broken", and the one sentence that tries reads
`ok · 1fb3df6-dirty`.

## 2. The rule this proposes

1. **One page owns one question**, and its title is that question's subject.
2. **A fact has one home.** Everywhere else links to it. Two renderings of one number
   will drift, and one of them will be wrong on the day it matters.
3. **The time window belongs to the page, not the panel.** One control, every panel obeys.
4. **The home screen answers the first question anybody has**, and nothing else.
5. **Name the object, every time.** Never a bare "lane".

## 3. Current → proposed

Ten nav entries become six, plus the lab. Every proposed name is a subject somebody
arrives asking about.

| proposed | absorbs | the question it owns |
|---|---|---|
| **Now** `/` | Overview minus memory + catalog; `ACTIVE REQUESTS` (its only home); the failures panel that does not exist yet | *Is it serving right now, and if not, what is wrong and whose is it?* |
| **Traffic** `/traffic` | `/activity` (utilization, come back later, journeys, service profiles, recent) + all of `/usage` — **one page, one time axis** | *What has it been doing, over a window I choose?* |
| **Models** `/models`, `/m/<name>` | Overview's modality catalog + `LOADED` + fallback-chain lists; `/providers`' "Assigned models" and "Local" | *What can be called, what state is it in, is it any good?* |
| **Callers** `/callers`, `/callers/<key>` | `/keys` + `/keys/$key` + `/groups` (priority groups, reservations) | *Who is asking, what are they allowed, what did they get?* |
| **Machines** `/machines` | `/hosts` (hosts, agents, tooling) + Overview's `MEMORY` + `/usage`'s "Resident models" and pool bars | *What hardware exists, what is holding it, is any of it unreachable?* |
| **Setup** `/setup` | `/providers` (integrations, credentials) + `/history` + `/quota` renamed **provider budgets** | *What is configured, what changed, can I undo it?* |
| **Bench** `/bench/*` | unchanged | *Which model is worth its VRAM?* |

**Redirects, not deletions.** `/activity`, `/usage`, `/keys`, `/groups`, `/hosts`,
`/providers`, `/history`, `/quota` all keep resolving. The repo already does this for
`/config → /hosts` and `/model → /m/<name>`; links live in notes and chat history.

**`/quota` moves and is renamed.** It is the free-tier ledger — *upstream* budgets learned
from provider rate-limit headers — and it sat in the nav where a person looking for "what
is my box about to run out of" would find it and be answered about something else. That
question gets answered on **Now** and **Machines** instead; the ledger belongs with the
providers it describes.

**`/m/<name>/activity` is deleted or given its outlet** (`plan.md` §7): today the URL
resolves and silently renders the parent's Info tab.

## 4. What the home screen owes on arrival

In this order, and little else:

1. **One sentence, in plain words: serving · struggling · stopped, and since when.**
   Today: a chip hardcoded `color="success"` reading `${status} · ${version}`
   (`index.tsx:882`) — green whatever is happening, with a git hash as the most prominent
   thing on the page.
2. **What is wrong right now**, each item saying whose problem it is and what to do:
   a host not reporting in, a model that failed to start, a provider exhausted, a backend
   at capacity. This panel does not exist today; its content is scattered across Hosts,
   Activity and Quota, and Lenny found the machine that is actually down only by reading a
   stack trace on page three.
3. **What is in flight** — the one live panel, and its only home.
4. **What changed recently** — the last config revision and any toolchain change, because
   *"is it me?"* is the second question everybody asks and today it is four pages away.
5. **A way into Traffic at a time I choose**, including this morning.

Off the home screen: the memory ledger (→ Machines), the full model catalog (→ Models),
fallback-chain definitions (→ Models).

## 5. The vocabulary — decided already, and the UI diverged

This looked like a naming decision. It is not: `plan.md` §3 has settled it since P14.

> **lane** = a served name that is *"a named, ordered fallback list over model names"*.
> **priorityGroup / group** = *"the single policy unit — a key maps to exactly one group"*.

The schema is unambiguous and the config uses both words correctly. **The UI contradicts
it in two places**, which is a bug with a known right answer, not a decision:

- `ActiveRequests.tsx:137` heads a column **`Lane`**; `:174` fills it with `r.group`. It is
  the caller's group. **Fix: head it `Group`.**
- `/keys` describes group assignment as "which lane is this key in" in its own prose and
  component names (`KeyLaneActions`). **Fix: group, throughout.**

Then, everywhere else, **name the object** — *caller group*, *model lane* — because the two
words still rhyme and a bare one is how this came back. No migration, no config change, and
nothing here needs a call from you.

*(Checked against the box before being asked, per `plan.md` §0. It is the third question in
a row here that answered itself in the running system.)*

## 6. Sequencing

Each phase is a shippable slice; the plan's Definition of done applies to each.

- **A — ✅ the home screen answers its question (2026-09-05).** Shipped: a computed
  serving/struggling/stopping/not-set-up sentence with the build stamp demoted; a
  **"What needs looking at"** panel that collects the faults scattered across three pages
  (a machine not reporting in, a backend that failed to start, a pool with no room left, a
  model or extension paused on purpose) — each saying what it is, **whose** it is and the one
  thing to do; the memory ledger moved to `/hosts`, where the declared budget already lived;
  and `ACTIVE REQUESTS` deduped to one home. No route changes, no backend work.
  *Verified on a throwaway daemon loaded with a redacted copy of the live config: the panel
  correctly reported `carlsmacbookpro is not reporting in`.*
  **Not fixed by phase A, deliberately:** the model catalog is still ~80% of the home screen.
  It moves to Models in phase C.
- **B — Traffic: merge `/activity` + `/usage` behind one time axis. — decision resolved
  2026-09-05: merge.** The only phase with real backend work: the activity and rollup queries
  take an explicit from/to instead of each panel's constant, and every panel on the page obeys
  one control. Fixes "I cannot get back to 09:00" properly, and collapses the per-key and
  per-model numbers that today disagree because each is measured over a different window.
- **C — Callers, Machines, Setup; nav to seven; old URLs redirect.**
- **D — the vocabulary pass** (decision above), plus the consequence lines on `Unload`,
  `Probe` and `Restore`, which are cheap and independent of all of the above.

**How we will know it worked:** Lenny run 2, same scenario, same walk — plus the consumer
scenario from `icebox.md`, because until two scenarios walk the same screens and fail
DIFFERENTLY, the constant is an assertion. **Report the composition, not the verdict.**
Expect the verdict to stay "no" for several runs and expect some scores to go DOWN: fixing
fear exposes the next problem, and that is the method working.

## 7. What this does not change

The engine, the proxy, the scheduler, the config schema (under recommendation (c)), and
Bench. This is a reorganization of surfaces plus one new query parameter.

## 8. Decisions that need you

Two, not three — the vocabulary answered itself against the schema (§5).

1. **Whether Traffic is one page or two.** Merging `/activity` and `/usage` is the single
   biggest change here. The argument for: they are the same subject at two windows, and the
   duplication is where the numbers disagree. The argument against: "what is happening" and
   "what did it cost" are different jobs, and one page with a time control may just be a
   longer page.
2. **Audience C.** A caller with a key currently gets the operator's dashboard with the
   dangerous buttons still on it. Out of scope here; it is either a filtered view or a
   separate surface, and that is a bigger decision than a reorganization.
