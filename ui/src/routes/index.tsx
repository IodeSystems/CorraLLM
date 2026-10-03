import type { ReactNode } from 'react'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Box, Chip, Typography } from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { ActiveRequests } from '@/ActiveRequests'
import { Panel, PageHeader, Row } from '@/Panel'
import { C } from '@/theme'
import { NotYet } from '@/NotYet'
import { FleetPanel, type FleetResident, type FleetServer } from '@/FleetPanel'
import { fmtInt, fmtKwh, fmtUSD } from '@/format'
import { PaidComparison, type ComparisonRow } from '@/PaidComparison'

/**
 * NOW — is this box serving, and if not, what is wrong?
 *
 * The home screen answers the first question anybody arrives with, and nothing
 * else (P30). It used to also be the model catalog, the memory ledger and the
 * lane definitions: four subjects, of which a person opening the dashboard
 * because something felt wrong wanted one. The catalog is /models, the ledger is
 * /machines, the lanes are /models.
 *
 * What is left is a state sentence, the faults behind it, what the box is
 * getting DONE, and what is in flight.
 *
 * The throughput strip is a later correction to this page's own doctrine. It
 * carried a panel headed "Nothing else on this page, on purpose", which was a
 * defensible answer to "is it broken" and the wrong answer to the question an
 * owner actually opens a dashboard with — which is two questions, not one: is it
 * working, AND is it doing anything. A home screen that can only say "nothing
 * needs you" cannot distinguish a healthy busy box from a healthy idle one, and
 * those are not the same news to somebody paying for it.
 */
const NowDoc = graphql(/* GraphQL */ `
  query Now($hourAgo: Long!, $dayAgo: Long!, $now: Long!) {
    corrallm {
      # THE LAST HOUR AGAINST THE DAY BEFORE IT. Two windows of the same rollup —
      # possible only since the endpoints took absolute bounds (P30 phase B) — so
      # the page can say "slower than usual" rather than leaving a person to hold
      # two screens in their head, which is what it took to find the 2026-09-05
      # slowdown by hand.
      recent: usageRollup(from: $hourAgo, to: $now) {
        # THE RATE AND THE BASELINE, ON THE SAME FETCH. An energy figure without
        # its $/kWh is a number with no basis, and "would have cost $X" without
        # the type it was priced against is not a claim anybody can check. Both
        # are window-level, so they ride the query the tiles already make.
        costPerKwh
        baselineType
        thermal {
          mode
          ambientF
          coolTargetF
          heatTargetF
        }
        rows {
          served
          requests
          dwellMs
          cacheHitRate
          cacheReports
          promptTokens
          cachedTokens
          completionTokens
          costUsd
          # WHAT KIND OF MONEY. A single "Cost" over a mixed fleet is neither
          # electricity nor a bill; these three are the same total, split by what
          # actually charges it — and free-tier traffic is a fourth kind that
          # carries no dollars at all, so it is counted rather than summed.
          billing
          energyUsd
          energyKwh
          paidUsd
          unattributedUsd
          freeRequests
          mispricedUsd
          baselineUsd
          baselineCachedUsd
          baselineProcessedUsd
          baselineGeneratedUsd
          baselineExcluded
          baselineType
          energyBtu
          heatBtuPerHour
          thermalUsd
          promptPerSec
          predictedPerSec
        }
      }
      baseline: usageRollup(from: $dayAgo, to: $hourAgo) {
        rows {
          served
          requests
          dwellMs
          cacheHitRate
          cacheReports
          promptTokens
          cachedTokens
          promptPerSec
          predictedPerSec
        }
      }
      health {
        version
      }
      overview {
        servers {
          server
          agentStatus
          agentLastSeen
          # WHICH POOL IS THE DEVICE. Only used to say 'device' or 'system'
          # beside a bar — the word that tells a reader whether 51 GB idle is a
          # graphics card or spare RAM.
          devicePool
        }
        models {
          name
          # WHAT COULD RUN WHERE. A machine holding nothing reads very
          # differently depending on whether anything is configured to run
          # there at all, and that is the difference between idle and pointless.
          server
          remote
          # OUT OF THE SETUP, WHICH THIS PAGE USES ONLY TO STOP COUNTING IT.
          # It raises no fault here on purpose — a decision already recorded in
          # the config does not also need a card (OB-6) — but "what could run"
          # is a different question from "what is wrong", and a model nothing
          # can ever load is not an answer to it.
          disabled
          paused
          pauseReason
          pauseResumeMs
        }
        extensions {
          name
          paused
          pauseReason
        }
        refusing {
          backend
          status
          sinceMs
        }
      }
      # THE FLEET, ON THE QUERY THIS PAGE ALREADY MAKES. Everything the
      # utilisation panel reads is added to the request that was going out
      # anyway — no second fetch on the home screen, which is the thing P30 was
      # protecting. The alternative was MachineData's Config query, which is a
      # survey of every host (agent endpoints, extensions, notes, toolchains)
      # and would have cost a second round trip to render three bars.
      residency {
        stopping
        servers {
          server
          pools {
            pool
            budget
            used
          }
          # Whether being full here has COST anything. UtilizationBar's contract
          # is explicit that a percentage cannot settle that and a made-room
          # count can, so the bars are given one rather than a threshold.
          madeRoomCount
          madeRoomEvicted
          madeRoomFor
        }
        # LOCAL BOX ONLY — the attached MacBook's Metal device is not in here.
        # Read for one thing: which pool names are backed by a card, so a second
        # GPU the declaration does not name still reads as device memory. Never
        # for a number; the numbers on this page are all the fleet-wide ledger
        # above. An unavailable reading means the probe could not answer, so the
        # record says nothing about its pool either (see FleetPanel.roleOf).
        gpus {
          available
          pool
        }
        models {
          modelName
          server
          state
          refs
          remote
          # What this backend has reserved, per pool — the same ledger the bars
          # are drawn from, so the parts add up to the whole. Deliberately NOT
          # footprintMiB: that is the live process measurement, a second and
          # different reading of the same quantity, and putting it beside a
          # ledger bar would render two meanings as one. /machines shows both,
          # side by side, where the difference is the point.
          usage {
            pool
            bytes
          }
        }
      }
    }
  }
`)
/**
 * "IT FEELS SLOW" — ANSWERED, OR SAID NOT TO BE.
 *
 * Five Lenny runs asked the same thing and no screen answered it: will it come
 * right on its own? The answer was in the data the whole time. On 2026-09-05 this
 * box spent four hours reprocessing 8,000-17,000 prompt tokens per request instead
 * of ~1,500, because the callers' prompts stopped sharing a prefix with the last
 * one; mean time answering went 4.5s → 10.8s and back to 2.0s when it cleared.
 * Nothing was broken, nothing was queued, nobody was turned away — and nothing on
 * any screen said so. It took two pages and a database to work out by hand.
 *
 * This compares the last hour against the day before it, which is only possible
 * because the rollup takes absolute bounds (phase B).
 *
 * IT IS RATED IN tg/s AND tp/s, NOT SECONDS A REQUEST. Seconds a request is what
 * an operator feels, but it is a compound of three things — how much prompt came
 * in, how much of it could be reused, and how fast the box ran — and it moves when
 * ANY of them moves. Two incompatible situations push it the same direction:
 *
 *   reuse collapsed   → tg/s and tp/s hold, the prompt grew. Nothing to press.
 *   the box degraded  → tg/s FELL. Thermals, contention, something else resident.
 *
 * Generation speed is the only thing here that separates them, and until it was
 * carried on the rollup this card could not, so it asserted "nothing is broken"
 * on both — confident, and wrong on the second. The rates come from llama.cpp's
 * own timings (usage.PromptPerSec / PredictedPerSec), which is why they are 0 for
 * a remote provider: unreported, NOT a measured zero, and the card falls back to
 * seconds a request rather than claiming a speed it was never told.
 *
 * Thresholds are deliberately blunt. A fault card that cries on every wobble is
 * one nobody reads, so: a fifth of the reuse gone AND a third more time per
 * request, over enough requests to mean anything. Generation is called slow only
 * once it is off by more than a seventh, which is wider than the run-to-run
 * wobble of a busy box.
 *
 * KNOWN GAP: reuse loss is still the trigger, so a box that slowed with its reuse
 * intact — a thermal event under a steady caller — does not raise this card at
 * all. That is a second fault, not this one.
 */
type Rollup = {
  served: string
  requests: number
  dwellMs: number
  cacheHitRate: number
  cacheReports: number
  promptTokens?: number
  cachedTokens?: number
  // Backend-reported, and 0 means NOTHING REPORTED ONE. Same trap as
  // cacheReports: reading an unreported rate as a measured 0 invents a
  // catastrophic slowdown for every remote provider on the box.
  promptPerSec?: number
  predictedPerSec?: number
}

function slowdownFaults(recent: Rollup[], baseline: Rollup[]): Fault[] {
  const out: Fault[] = []
  const before = new Map(baseline.map((r) => [r.served, r]))
  for (const now of recent) {
    const was = before.get(now.served)
    if (!was) continue
    // cacheReports === 0 means NOTHING REPORTED A CACHE, not a measured 0% — an
    // embedding backend or a remote provider never reports one, and reading that
    // as a miss rate invents a problem (the schema says so in as many words).
    if (now.cacheReports === 0 || was.cacheReports === 0) continue
    if (now.requests < 20 || was.requests < 100) continue

    const nowMean = now.dwellMs / Math.max(1, now.requests)
    const wasMean = was.dwellMs / Math.max(1, was.requests)
    const reuseLost = was.cacheHitRate - now.cacheHitRate
    if (reuseLost < 0.2 || nowMean < wasMean * 1.33) continue

    const pct = (x: number) => `${Math.round(x * 100)}%`
    const secs = (ms: number) => `${(ms / 1000).toFixed(1)} s`
    // One formatter for both rates: tg/s lands near 40 and wants a decimal, tp/s
    // lands in the thousands and does not.
    const rate = (x: number) => (x >= 100 ? Math.round(x).toLocaleString() : x.toFixed(1))
    const reread = (r: Rollup) =>
      r.promptTokens != null && r.cachedTokens != null
        ? Math.round((r.promptTokens - r.cachedTokens) / Math.max(1, r.requests))
        : null

    const nowTg = now.predictedPerSec ?? 0
    const wasTg = was.predictedPerSec ?? 0
    const nowTp = now.promptPerSec ?? 0
    const wasTp = was.promptPerSec ?? 0
    // BOTH windows must have reported, or there is nothing to compare. A backend
    // that reported an hour ago and not now has changed what it is, not how fast
    // it is, and comparing across that is worse than saying nothing.
    const rated = nowTg > 0 && wasTg > 0 && nowTp > 0 && wasTp > 0
    // Generation itself slowed. This is the box, not the callers.
    const boxSlowed = rated && nowTg < wasTg * 0.85

    const nowRe = reread(now)
    const wasRe = reread(was)
    const reread_ =
      nowRe != null && wasRe != null
        ? `, over ${nowRe.toLocaleString()} tokens re-read per request against ${wasRe.toLocaleString()}`
        : nowRe != null
          ? `, about ${nowRe.toLocaleString()} tokens re-read per request`
          : ''

    // Rated where the backend told us, seconds a request where it did not. The
    // fallback is not a lesser card: it is the honest one when the only number
    // anybody measured is wall time.
    const what = rated
      ? `${now.served} is generating at ${rate(nowTg)} tg/s against ${rate(wasTg)}, reading prompt at ${rate(nowTp)} tp/s against ${rate(wasTp)}${reread_}`
      : `${now.served} is answering more slowly than usual — ${secs(nowMean)} a request against ${secs(wasMean)} over the last day${reread_} (this backend reports no tg/s/tp/s, so the cause is not separable here)`

    out.push({
      key: `slow:${now.served}`,
      scope: 'here',
      severity: 'warning',
      what,
      whose: boxSlowed ? 'This box.' : 'This box, and nothing on it is broken.',
      next: boxSlowed
        ? `Generation itself slowed, so this is not the callers' prompts — a shorter prompt would not fix it. Check the device's thermals and what else is resident on it. Reuse is down too (${pct(now.cacheHitRate)} against ${pct(was.cacheHitRate)}), which is adding to the wait but is not the cause.`
        : `Callers are sending prompts that share less with the one before, so more of each prompt has to be read again: ${pct(now.cacheHitRate)} of it is being reused now against ${pct(was.cacheHitRate)} normally.` +
          (rated ? ' The box is running at its normal speed.' : '') +
          ' Nothing to press — it comes right on its own when they settle back onto one conversation.',
    })
  }
  return out
}

/**
 * WHAT IS WRONG RIGHT NOW — the panel this page did not have.
 *
 * The first Lenny run (plan.md §6) walked nine screens looking for one true
 * sentence to put in a team chat and found none: every coloured thing said
 * green, and the one machine that WAS down was discoverable only as a stack
 * trace on the Hosts page, three screens in. The facts were all in the product;
 * no screen collected them.
 *
 * Each fault says three things, because a fault that says only the first is the
 * "red thing you cannot act on" that OB-6 forbids:
 *   what   — in the operator's words, naming the object
 *   whose  — this box, another machine, or a person who did it on purpose
 *   next   — the one thing to do about it
 *
 * It is deliberately built from what the Overview query already knows plus agent
 * reachability. A fault sourced from a page nobody opens is how this got missed.
 */
type FaultBody = {
  key: string
  what: string
  whose: string
  next: string
  when?: string
  severity: 'error' | 'warning' | 'info'
  // WHERE THE NEXT THING IS, when it is a control somewhere rather than
  // something to go and understand. OB-8 asks a screen to name the next thing
  // AND be able to start it; a fault whose `next` describes a button on another
  // page and leaves the reader to find it has only done the first half.
  where?: { label: string; to: string }
}

// WHOSE MACHINE, and it decides the headline. Lenny run 3: the box spent its
// one "Struggling" on a laptop that had never reported in, and never said
// anything about the machine actually serving the team — so a person came
// looking for "is my box broken" and left unable to say. A fault somewhere
// else is still a fault; it is not this box struggling.
//
// AND WHAT KIND OF THING IT IS, which is a SEPARATE question the state line
// used to answer by guessing. Everything scoped 'attached' was counted as a
// machine, so a Groq credential answering 403 made the one sentence this page
// owes on arrival read "1 attached machine is not [serving normally]" — while
// /machines correctly said every machine was reachable. A reader sent to look
// for a down box would have found none, because there was none: the thing that
// was not serving is a provider, which the rest of the product calls a provider
// (OB-3 — the screen speaks one word for one thing, on every page).
//
// It is a discriminated union rather than an optional field so the omission
// cannot recur: a new 'attached' fault does not compile until it says which.
type AttachedFault = FaultBody & { scope: 'attached'; subject: 'machine' | 'provider' }
type Fault = (FaultBody & { scope: 'here' }) | AttachedFault

function faultsOf(
  servers: readonly { server: string; agentStatus: string; agentLastSeen: string }[],
  resident: readonly { modelName: string; server: string; state: string }[],
  pools: readonly { server: string; pools: readonly { pool: string; budget: string; used: string }[] }[],
  models: readonly { name: string; paused: boolean; pauseReason: string; pauseResumeMs: string }[],
  extensions: readonly { name: string; paused: boolean; pauseReason: string }[],
  refusing: readonly { backend: string; status: number | string; sinceMs: number | string }[],
): Fault[] {
  const out: Fault[] = []

  // A machine that stopped reporting in. 'local' is this box and has no agent;
  // 'unknown' means it has never reported at all, which reads to a person as the
  // same thing — the models there will not start.
  for (const s of servers) {
    // 'away' is a machine the operator marked as coming and going: its
    // absence is expected, so it is not a fault.
    if (s.agentStatus === 'up' || s.agentStatus === 'local' || s.agentStatus === 'away') continue
    const last = Number(s.agentLastSeen)
    out.push({
      key: `agent:${s.server}`,
      scope: 'attached',
      subject: 'machine',
      severity: s.agentStatus === 'down' ? 'error' : 'warning',
      what: `${s.server} is not reporting in`,
      whose: 'That machine, not this one.',
      next: 'Check it is awake and on the network. Nothing new can start there until it reports in; anything already running there is left alone.',
      // Last-seen is not persisted: 0 means "not since corrallm started", and
      // saying "never" about a machine that reported in all week is false.
      when:
        last > 0
          ? `last heard from at ${new Date(last).toLocaleTimeString()}`
          : 'not heard from since corrallm started',
    })
  }

  // A backend that tried to start and did not.
  for (const m of resident) {
    if (m.state !== 'failed') continue
    out.push({
      key: `failed:${m.modelName}`,
      scope: 'here',
      severity: 'error',
      what: `${m.modelName} failed to start on ${m.server}`,
      whose: 'This box.',
      next: `Open ${m.modelName} and read its log — the reason it gave is there.`,
    })
  }

  // No room left. Nothing is broken; nothing new fits either, and that is the
  // difference between "busy" and "broken" a person cannot otherwise see.
  for (const s of pools) {
    for (const p of s.pools) {
      const budget = Number(p.budget)
      const used = Number(p.used)
      if (budget <= 0 || used < budget) continue
      out.push({
        key: `full:${s.server}/${p.pool}`,
        scope: 'here',
        severity: 'warning',
        what: `${s.server} has no room left in ${p.pool}`,
        whose: 'This box — it is full, not broken.',
        next: 'Requests for a model that is not already loaded will wait or be turned away until something unloads.',
      })
    }
  }

  // Paused on purpose. Listed because a person who did not do it cannot tell it
  // from a fault, and the resume time is the whole answer to "will it clear?".
  const pausedNote = (reason: string, resumeMs: string) => {
    const at = Number(resumeMs)
    const when = at > 0 ? `Resumes at ${new Date(at).toLocaleTimeString()}.` : 'It stays paused until somebody resumes it.'
    return reason ? `${when} Reason given: “${reason}”.` : when
  }
  for (const m of models) {
    if (!m.paused) continue
    out.push({
      key: `paused:${m.name}`,
      scope: 'here',
      severity: 'info',
      what: `${m.name} is paused — it is not serving anybody`,
      whose: 'Somebody here paused it on purpose.',
      next: pausedNote(m.pauseReason, m.pauseResumeMs),
    })
  }
  for (const e of extensions) {
    if (!e.paused) continue
    out.push({
      key: `paused-ext:${e.name}`,
      scope: 'here',
      severity: 'info',
      what: `${e.name} is paused — every model it serves is unavailable`,
      whose: 'Somebody here paused it on purpose.',
      next: pausedNote(e.pauseReason, ''),
    })
  }
  // A backend refusing outright — 401/402/403, the provider saying no in a way
  // retrying cannot fix. It reads as a fault here and not only on the budgets
  // page because that page is under Setup and the owner never opens it: a
  // remote rung answered 402 for five days and no screen anybody visits said so.
  //
  // 'attached', not 'here': the box is serving. Scoping it to this box would
  // make the headline say the machine is struggling, which would be false and
  // is exactly the error run 3 caught.
  for (const r of refusing) {
    // A PAUSED BACKEND ALREADY HAS ITS CARD, AND IT IS THE RIGHT ONE.
    //
    // The ledger cannot clear a refusal for a backend nobody calls — the clear
    // only fires on a successful response — so a paused rung keeps its row
    // forever. Raising this fault too put the SAME backend on this panel twice:
    // "X is turning down every request" in warning, directly above "X is paused"
    // in info. The owner paused it from the banner and got both: "it still shows
    // errors, and it shows that it is paused - kinda pointless".
    //
    // The paused card wins because it is the one with something to say: the
    // pause reason is prefilled with the refusal that caused it, so it already
    // reads "paused — refusing every request, 403 since <date>". Two cards for
    // one backend is how a fault panel becomes something people scroll past.
    if (models.some((m) => m.name === r.backend && m.paused)) continue
    const status = Number(r.status ?? 0) || 0
    const days = Math.floor(Math.max(0, Date.now() - Number(r.sinceMs ?? 0)) / 86400000)
    out.push({
      key: `refusing:${r.backend}`,
      scope: 'attached',
      // Not a machine. It is a credential at somebody else's API, and calling
      // it one sent a reader hunting for a down box on a fleet where every box
      // was up.
      subject: 'provider',
      severity: 'warning',
      what: `${r.backend} is turning down every request${
        status === 402
          ? ' — the account is unpaid'
          : status === 401
            ? ' — the key is not accepted'
            : status === 403
              ? ' — the key is not permitted to use it'
              : ` — it answers ${status}`
      }`,
      whose: 'The provider that runs it, not this box.',
      // TWO FIXES, IN WORDS THE READER OWNS. "Drop it from its lane" named a
      // thing the home screen never introduces and OB-3 keeps off labels
      // outright: "I don't own a rung and I don't know what a lane is." The
      // second half now describes the action instead of naming the object, and
      // points at the panel that does it — which Setup titles "Model lanes" and
      // explains, in the same words, as models tried in order.
      // "Take it out of its fallback list" was an instruction to edit
      // configuration by hand, and it went stale the moment Setup grew a Pause
      // for exactly this: pausing is reversible, records why, and leaves the
      // config alone, which the edit does none of. The sentence names the
      // action and the link lands on it.
      //
      // The button itself stays on Setup rather than being repeated here. This
      // panel is a census of unlike things — a machine, a full pool, a
      // slowdown, a pause somebody set on purpose — and it is readable because
      // every row has the same shape. One row carrying a control makes the
      // others look inert, and invites a control on each of them, which is the
      // accretion P30 took off this page. A pause also has to show what it will
      // cost before it is pressed, and that is counted from the fallback lists
      // and their ladders — which Now does not fetch, and the one thing P30
      // protected here is this page's single round trip.
      next:
        status === 402
          ? 'Fund the account, or stop calling it — pausing it is reversible and leaves your config alone. Nothing here is waiting on it.'
          : 'Check the credential, or stop calling it — pausing it is reversible and leaves your config alone. Nothing here is waiting on it.',
      where: { label: 'Pause it on Setup', to: '/setup' },
      when:
        days >= 1
          ? `it has been like this for ${days} day${days === 1 ? '' : 's'}`
          : 'it started refusing today',
    })
  }
  return out
}

/**
 * The one sentence this page owes on arrival.
 *
 * It replaces a chip reading `${health.status} · ${health.version}` that was
 * hardcoded `color="success"` — and whose text carried no information either:
 * the schema says status is *"always 'ok' when the process is serving"*, so a
 * page that renders at all always said ok, in green, including on a box with a
 * machine down. The state has to be computed from things that can actually be
 * false. The build stamp stays, demoted: operators need it, it is not the
 * headline.
 */
/**
 * The half of the state sentence about things that are not this box.
 *
 * It used to be one count and one noun: every 'attached' fault was "an attached
 * machine". A UX walk read *"This machine is serving normally. 1 attached
 * machine is not, and it is below"* on a fleet where /machines said both boxes
 * were reachable — and it was right, because the thing not serving was
 * `groq-gpt-oss-120b`, a provider credential. The one sentence this page owes on
 * arrival named the wrong kind of object, and sent a reader to the one screen
 * that could not show it.
 *
 * Machines and providers are said separately because they are fixed in
 * different places: a machine on /machines, a provider's credential on Setup.
 * Neither is described in terms of the other.
 */
function attachedPhrase(machines: number, providers: number): string {
  const m =
    machines > 0
      ? `${machines} attached machine${machines === 1 ? ' is' : 's are'} not`
      : ''
  const p =
    providers > 0
      ? `${providers} provider${providers === 1 ? ' is' : 's are'} turning down requests`
      : ''
  if (m && p) return `${m}, and ${p}. They are all below.`
  const one = (m || p) && machines + providers === 1
  return `${m || p}, and ${one ? 'it is' : 'they are'} below.`
}

function BoxState(props: {
  faults: Fault[]
  stopping: number
  ready: number
  configured: number
  /**
   * Models in the config that are out of the setup, used for ONE sentence.
   *
   * "Not set up" with nothing configured means go and configure something. With
   * everything configured and taken out, it means the opposite — the work is
   * done and one decision undid it — and sending that reader to Setup to add
   * models they already have is the dead end OB-8 forbids.
   */
  outOfSetup: number
  machines: number
  version: string
}) {
  const { faults, stopping, ready, configured, outOfSetup, machines, version } = props
  const bad = faults.filter((f) => f.severity !== 'info' && f.scope === 'here').length
  // Counted by WHAT THEY ARE, not just where they are. One number covering both
  // is what made a remote credential read as a machine.
  const away = faults.filter(
    (f): f is AttachedFault => f.severity !== 'info' && f.scope === 'attached',
  )
  const downMachines = away.filter((f) => f.subject === 'machine').length
  const badProviders = away.filter((f) => f.subject === 'provider').length
  const elsewhere = away.length
  // "Serving" with nothing loaded is TRUE here and has to be said carefully:
  // corrallm loads on demand, so an idle box with zero resident models is
  // healthy and the next request will start one. What is NOT serving is a box
  // with no models configured at all — saying "Serving" there would be the
  // false-belief case OB-5 exists to forbid.
  const word =
    stopping > 0 ? 'Stopping' : bad > 0 ? 'Struggling' : configured === 0 ? 'Not set up' : 'Serving'
  const color = word === 'Serving' ? C.ok : word === 'Struggling' ? C.error : C.warn
  const detail =
    word === 'Stopping'
      ? `${stopping} backend${stopping === 1 ? ' is' : 's are'} shutting down.`
      : bad > 0
        ? `${bad} thing${bad === 1 ? '' : 's'} below need${bad === 1 ? 's' : ''} looking at.`
        : configured === 0
          ? outOfSetup > 0
            ? `Every model in your configuration is out of the setup, so nothing can be served. Models lists ${outOfSetup === 1 ? 'it' : 'them'}, and puts ${outOfSetup === 1 ? 'it' : 'them'} back.`
            : 'No models are configured, so nothing can be served yet. Start on Setup.'
          : // THIS box is fine and something else is not: say both, in that order.
            // "Struggling" for a machine that is not serving anybody sends a
            // person hunting for a fault on the one that is.
            //
            // AND THE WORD FOR IT IS THE WORD THE REST OF THE PRODUCT USES. A
            // machine is a box you own; a provider is a credential at somebody
            // else's API. Both can stop serving, they are fixed in different
            // places, and only one of them is findable on /machines.
            elsewhere > 0
            ? `This machine is serving normally. ${attachedPhrase(downMachines, badProviders)}`
            : ready === 0
              ? `Nothing is loaded right now — the next request loads one of ${configured} models. Nothing needs you.`
              : `${ready} of ${configured} models loaded across ${machines} machine${machines === 1 ? '' : 's'}. Nothing needs you.`
  return (
    <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
      <Typography variant="body1" sx={{ color, fontWeight: 700 }}>
        {word}
      </Typography>
      <Typography variant="body2" sx={{ color: C.textMuted }}>
        — {detail}
      </Typography>
      <Typography variant="caption" sx={{ color: C.textFaint }}>
        build {version}
      </Typography>
    </Box>
  )
}

/**
 * The same line, before the box has answered — because the page owes a sentence
 * on ARRIVAL (OB-5), and arrival is the moment it has nothing.
 *
 * Serving, struggling and stopped are the three states this line may report, and
 * none of them is true while the query is out. So it reports the fourth thing
 * that is true: that it is asking. Muted, never the green/amber/red of a state,
 * because the colour is what gets read first and a colour here would be a
 * verdict. It also says the panels below are not readings yet, which is the part
 * a person who stops at the first line needs to know.
 */
function BoxStateAsking() {
  return (
    <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
      <Typography variant="body1" sx={{ color: C.textMuted, fontWeight: 700 }}>
        Checking
      </Typography>
      <Typography variant="body2" sx={{ color: C.textMuted }}>
        — asking this box how it is. Nothing below is a reading yet, and nothing here says it is
        healthy.
      </Typography>
    </Box>
  )
}

/**
 * What the box is GETTING DONE — the second question an owner arrives with.
 *
 * Every number carries its unit and its window (OB-4); the window is said once,
 * on the panel, rather than five times. Counts are the last hour because that is
 * the span a person means by "at the moment" when something felt wrong — the
 * same window the slowdown comparison above already reads.
 *
 * Deliberately not a chart. A chart of the last hour invites reading a trend off
 * six points, and what this answers is "how much", not "which way" — Traffic
 * owns the shape over time and says so at the end of the strip (OB-8).
 */
/**
 * Compact for a headline number. "46,840,478 in" is exact and unreadable at a
 * glance, which is the only way this strip is read; "46.8M in" is the same fact
 * in the shape a person can take in without counting digit groups. Traffic keeps
 * the exact figures, where someone is looking rather than glancing.
 */
function compact(n: number): string {
  if (!Number.isFinite(n)) return '—'
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)}B`
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`
  return String(Math.round(n))
}

function Throughput({
  pending = false,
  machines = 0,
  ready = 0,
  configured = 0,
  requests = 0,
  promptTokens = 0,
  cachedTokens = 0,
  completionTokens = 0,
  cacheReports = 0,
  energyUsd = 0,
  energyKwh = 0,
  paidUsd = 0,
  unattributedUsd = 0,
  freeRequests = 0,
  mispricedUsd = 0,
  costPerKwh = 0,
}: {
  /** Nothing has been counted yet: the strip renders, the numbers do not. */
  pending?: boolean
  machines?: number
  ready?: number
  configured?: number
  requests?: number
  promptTokens?: number
  cachedTokens?: number
  completionTokens?: number
  cacheReports?: number
  energyUsd?: number
  energyKwh?: number
  paidUsd?: number
  unattributedUsd?: number
  /** How many requests cost nothing. The free-tier tile's dollar figure is $0
   *  by definition, so this is what says whether the $0 was measured. */
  freeRequests?: number
  mispricedUsd?: number
  costPerKwh?: number
}) {
  // Idle is a fact, not a blank. A strip of zeroes reads as broken; saying so in
  // words is the difference between "nothing is wrong" and "nothing is here".
  //
  // And "not asked yet" is a third thing again, which is why `pending` is not
  // folded into `idle`: "none" is a count of zero requests that somebody took,
  // "not in yet" is no count at all (NotYet.tsx).
  const idle = requests === 0
  const stats: { label: string; value: ReactNode; sub?: string }[] = [
    {
      label: 'Machines',
      value: pending ? <NotYet /> : String(machines),
      sub: pending ? undefined : machines === 1 ? 'this box' : 'this box and attached',
    },
    {
      label: 'Models loaded',
      value: pending ? <NotYet /> : `${ready} of ${configured}`,
      sub: 'the rest load on demand',
    },
    {
      label: 'Requests',
      value: pending ? <NotYet /> : idle ? 'none' : fmtInt(requests),
      sub: 'in the last hour',
    },
    {
      // CACHED · PROCESSED · GENERATED — the three classes tokens are always
      // measured in, and the vocabulary the cost model, the metrics and the
      // owner all already use. "in · out" was two of the three with the
      // interesting one folded into the first: promptTokens INCLUDES the cached
      // prefix, so "in" was silently the sum of the work done and the work
      // avoided, and the cache read — the number that says whether prefix
      // caching is working at all — was queried here and never shown.
      label: 'Tokens',
      value: pending ? (
        <NotYet />
      ) : idle ? (
        '—'
      ) : (
        `${cacheReports > 0 ? compact(cachedTokens) : 'n/r'} · ${compact(
          Math.max(0, promptTokens - cachedTokens),
        )} · ${compact(completionTokens)}`
      ),
      // The label strip is what makes the three readable; without it the value
      // is three numbers in an order nobody was told.
      sub:
        pending || idle
          ? 'cached · processed · generated'
          : cacheReports > 0
            ? `cached · processed · generated — ${((cachedTokens / Math.max(1, promptTokens)) * 100).toFixed(0)}% of prompt from cache`
            : 'cached · processed · generated — nothing reported a cache',
    },
    {
      // ELECTRICITY, SAID SO, WITH ITS RATE. A local backend's cost IS energy:
      // token work → Wh → kWh × $/kWh, and the screen never said either half.
      // The rate is on the tile rather than in a tooltip because a cost figure
      // whose rate is invisible cannot be checked (OB-4, OB-10).
      label: 'Electricity',
      value: pending ? <NotYet /> : fmtUSD(energyUsd),
      sub:
        costPerKwh > 0
          ? `${fmtKwh(energyKwh)} at $${costPerKwh}/kWh`
          : 'no $/kWh configured — every energy figure is $0 by omission',
    },
    {
      // AND THE PART THAT IS NOT ELECTRICITY. The fleet is mixed: the anthropic
      // types bill per token and a total over both kinds is neither. A counted
      // $0 here is a real answer ("nothing went to a paid provider this hour"),
      // which is why the tile is not hidden when it is zero.
      label: 'Paid APIs',
      value: pending ? <NotYet /> : fmtUSD(paidUsd),
      sub: paidUsd > 0 ? 'billed per token by a provider' : 'nothing billed per token',
    },
  ]
  // A THIRD KIND: SOMEBODY ELSE'S HARDWARE, AT NO CHARGE. The only tile here
  // whose $0 is the answer rather than the absence of one — the provider charges
  // nothing, so nothing is what it cost. That is why it is gated on the request
  // COUNT and not on the dollars: $0 over seven requests is a reading, $0 over
  // none is an empty category, and only the second belongs off the page.
  //
  // The sub-label says whose hardware, because that is the fact a reader can
  // check and the one that can change — a free tier is a gift, not a property of
  // this box, and it ends without warning.
  if (freeRequests > 0) {
    stats.push({
      label: 'Free tier',
      value: fmtUSD(0),
      sub:
        mispricedUsd > 0
          ? `${fmtInt(freeRequests)} requests on another provider, at no charge — ${fmtUSD(mispricedUsd)} of it had been billed as ours`
          : `${fmtInt(freeRequests)} requests on another provider, at no charge`,
    })
  }
  // A FOURTH, AND IT IS A CONFIG GAP RATHER THAN A READING. An energy-priced
  // type on a model corrallm does not run, with nothing declaring what that host
  // charges: the tokens were processed on somebody else's hardware and then
  // charged against this box's watt-hour coefficients. Not electricity anybody
  // paid here, not the provider's rate, and not declared free either. It gets a
  // tile only when it is nonzero — a permanent $0 tile teaches nobody anything,
  // and this one names an action (OB-6).
  if (unattributedUsd > 0) {
    stats.push({
      label: 'Unattributed',
      value: fmtUSD(unattributedUsd),
      sub: 'off-box models priced as our own energy — say what they charge, or declare their free tier',
    })
  }
  return (
    <Panel
      title="What it is getting done"
      subtitle="Counted over the last hour"
      // The window stays in the header, because a number without its window is
      // not a number (OB-4) and the head of the sentence survives a clip. Where
      // to get another window is the next step, and goes where it is readable.
      note="Traffic has the same numbers over any span you pick."
      dense
    >
      <Row>
        <Box sx={{ display: 'flex', gap: 4, flexWrap: 'wrap', py: 0.5 }}>
          {stats.map((s) => (
            <Box key={s.label} sx={{ minWidth: 120 }}>
              <Typography variant="caption" sx={{ color: C.textFaint, display: 'block' }}>
                {s.label}
              </Typography>
              <Typography variant="h6" sx={{ lineHeight: 1.2 }}>
                {s.value}
              </Typography>
              {s.sub && (
                <Typography variant="caption" sx={{ color: C.textMuted }}>
                  {s.sub}
                </Typography>
              )}
            </Box>
          ))}
        </Box>
      </Row>
    </Panel>
  )
}

function WhatIsWrong({ faults }: { faults: Fault[] }) {
  if (!faults.length) return null
  return (
    <Panel
      title="What needs looking at"
      badge={<Chip size="small" label={faults.length} />}
      subtitle="What it is, whose it is, and the one thing to do about it"
      flush
    >
      {faults.map((f) => (
        <Row key={f.key}>
          <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
            <Typography
              variant="body2"
              sx={{
                fontWeight: 600,
                color: f.severity === 'error' ? C.error : f.severity === 'warning' ? C.warn : C.text,
              }}
            >
              {f.what}
            </Typography>
            {f.when && (
              <Typography variant="caption" sx={{ color: C.textFaint }}>
                {f.when}
              </Typography>
            )}
          </Box>
          <Typography variant="body2" sx={{ color: C.textMuted, mt: 0.25 }}>
            {f.whose} {f.next}{' '}
            {/* Unstyled on purpose: theme.ts paints a router Link as text that
                happens to be clickable rather than accent blue. */}
            {f.where && <Link to={f.where.to}>{f.where.label}</Link>}
          </Typography>
        </Row>
      ))}
    </Panel>
  )
}


function Now() {
  // Rounded to the minute so the query key is stable between renders — an exact
  // Date.now() would make every render a cache miss and refetch in a loop.
  const nowMs = Math.floor(Date.now() / 60_000) * 60_000
  const q = useQuery({
    queryKey: ['now', nowMs],
    queryFn: () =>
      gqlClient.request(NowDoc, {
        now: String(nowMs),
        hourAgo: String(nowMs - 3_600_000),
        dayAgo: String(nowMs - 25 * 3_600_000),
      }),
    refetchInterval: 10000,
  })

  if (q.error) {
    return (
      <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
        <PageHeader title="Now" />
        <Panel title="This machine did not answer">
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            The dashboard could not reach corrallm on this box. It is either restarting, stopped, or
            not reachable from here — nothing below is current. {String(q.error)}
          </Typography>
        </Panel>
      </Box>
    )
  }

  const c = q.data?.corrallm
  // THE PAGE, WHILE IT IS STILL BEING ANSWERED — not a spinner on an empty
  // frame. This screen used to return `<Loading />` here, so on a box slow
  // enough to be worth looking at (6 s to answer, reproducibly) the first thing
  // an owner saw was a nav rail and nothing else: "the one thing I came here for
  // and it's blank". Traffic has always degraded properly — header, window
  // picker and panel chrome first, values as they land — so this was an
  // inconsistency, not an unsolved problem (OB-5).
  //
  // Same shape, same order, same panels; every number says it has not arrived
  // rather than showing a zero, and the state line says so in one sentence at
  // the top. What is NOT here is a "What needs looking at" placeholder: an empty
  // fault panel is the false all-clear this page exists to stop, and the state
  // line above already says nothing below is a reading.
  if (!c) {
    return (
      <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
        <PageHeader title="Now">
          <BoxStateAsking />
        </PageHeader>
        <Throughput pending />
        <FleetPanel pending />
        <ActiveRequests />
      </Box>
    )
  }
  const ov = c.overview
  const res = c.residency
  // THE DENOMINATOR IS WHAT CAN SERVE, not what is written down. This page says
  // "1 of 12 models loaded" and "the next request loads one of 12", and a model
  // out of the setup is never resolved, never spawned and never preloaded — so
  // counting it makes both sentences promises the box cannot keep, and hides
  // the one it can. Faults are untouched: being out of the setup is a decision
  // somebody already made and recorded, not something needing attention here.
  const allModels = ov?.models ?? []
  const models = allModels.filter((m) => !m.disabled)
  const outOfSetup = allModels.length - models.length
  const stopping = (res?.stopping ?? []).length
  const readyCount = (res?.models ?? []).filter((m) => m.state === 'ready').length

  const slow = slowdownFaults(
    (c.recent?.rows ?? []).map((r) => ({
      served: r.served,
      requests: Number(r.requests),
      dwellMs: Number(r.dwellMs),
      cacheHitRate: r.cacheHitRate,
      cacheReports: Number(r.cacheReports),
      promptTokens: Number(r.promptTokens),
      cachedTokens: Number(r.cachedTokens),
      promptPerSec: r.promptPerSec,
      predictedPerSec: r.predictedPerSec,
    })),
    (c.baseline?.rows ?? []).map((r) => ({
      served: r.served,
      requests: Number(r.requests),
      dwellMs: Number(r.dwellMs),
      cacheHitRate: r.cacheHitRate,
      cacheReports: Number(r.cacheReports),
      promptTokens: Number(r.promptTokens),
      cachedTokens: Number(r.cachedTokens),
      promptPerSec: r.promptPerSec,
      predictedPerSec: r.predictedPerSec,
    })),
  )

  const faults = faultsOf(
    (ov?.servers ?? []).map((x) => ({
      server: x.server,
      agentStatus: x.agentStatus,
      agentLastSeen: String(x.agentLastSeen),
    })),
    (res?.models ?? []).map((m) => ({ modelName: m.modelName, server: m.server, state: m.state })),
    (res?.servers ?? []).map((x) => ({
      server: x.server,
      pools: x.pools.map((p) => ({ pool: p.pool, budget: String(p.budget), used: String(p.used) })),
    })),
    models.map((m) => ({
      name: m.name,
      paused: !!m.paused,
      pauseReason: m.pauseReason ?? '',
      pauseResumeMs: String(m.pauseResumeMs ?? 0),
    })),
    (ov?.extensions ?? []).map((e) => ({
      name: e.name,
      paused: !!e.paused,
      pauseReason: e.pauseReason ?? '',
    })),
    (ov?.refusing ?? []).map((r) => ({
      backend: r.backend,
      status: r.status,
      sinceMs: String(r.sinceMs),
    })),
  )

  const all = [...slow, ...faults]

  // THE FLEET, JOINED ONCE. The declared side (which pool is the device, what
  // could run where) comes from overview; the live side (budget, held, what made
  // room) from residency; the holders from the resident list. All three are in
  // the one response above.
  const num = (v: string | number | null | undefined) => Number(v ?? 0)
  const liveByServer = new Map((res?.servers ?? []).map((s) => [s.server, s]))
  const fleetServers: FleetServer[] = (ov?.servers ?? []).map((s) => {
    const live = liveByServer.get(s.server)
    return {
      server: s.server,
      devicePool: s.devicePool ?? '',
      agentStatus: s.agentStatus,
      pools: (live?.pools ?? []).map((p) => ({
        pool: p.pool,
        budget: num(p.budget),
        used: num(p.used),
      })),
      madeRoomCount: num(live?.madeRoomCount),
      madeRoomEvicted: live?.madeRoomEvicted ?? '',
      madeRoomFor: live?.madeRoomFor ?? '',
    }
  })
  // A remote model is served by a host corrallm does not run. It holds no
  // residency and must never be counted as loaded on one of your machines.
  const fleetResident: FleetResident[] = (res?.models ?? [])
    .filter((m) => !m.remote)
    .map((m) => ({
      modelName: m.modelName,
      server: m.server,
      state: m.state,
      refs: num(m.refs),
      usage: m.usage.map((u) => ({ pool: u.pool, bytes: num(u.bytes) })),
    }))
  const declaredByServer = new Map<string, number>()
  for (const m of models) {
    if (m.remote || !m.server) continue
    declaredByServer.set(m.server, (declaredByServer.get(m.server) ?? 0) + 1)
  }
  const gpus = res?.gpus ?? []
  const boundPools = new Set(
    gpus.filter((g) => g.available && g.pool).map((g) => g.pool as string),
  )
  const probeIncomplete = gpus.some((g) => !g.available)

  // Summed across served names, because the strip answers "what is this BOX
  // doing" — per-model is what Traffic is for.
  const hour = c.recent?.rows ?? []
  const totals = hour.reduce(
    (a, r) => ({
      requests: a.requests + Number(r.requests ?? 0),
      promptTokens: a.promptTokens + Number(r.promptTokens ?? 0),
      cachedTokens: a.cachedTokens + Number(r.cachedTokens ?? 0),
      completionTokens: a.completionTokens + Number(r.completionTokens ?? 0),
      cacheReports: a.cacheReports + Number(r.cacheReports ?? 0),
      // Summed per KIND, never into one figure. The three are the same
      // costUsd the row already carries, split by what charges it.
      energyUsd: a.energyUsd + Number(r.energyUsd ?? 0),
      energyKwh: a.energyKwh + Number(r.energyKwh ?? 0),
      paidUsd: a.paidUsd + Number(r.paidUsd ?? 0),
      unattributedUsd: a.unattributedUsd + Number(r.unattributedUsd ?? 0),
      // Free traffic is COUNTED, not summed: its dollar figure is $0 by
      // definition, so the count is the only thing that distinguishes a measured
      // $0 from no such traffic at all.
      freeRequests: a.freeRequests + Number(r.freeRequests ?? 0),
      mispricedUsd: a.mispricedUsd + Number(r.mispricedUsd ?? 0),
    }),
    {
      requests: 0,
      promptTokens: 0,
      cachedTokens: 0,
      completionTokens: 0,
      cacheReports: 0,
      energyUsd: 0,
      energyKwh: 0,
      paidUsd: 0,
      unattributedUsd: 0,
      freeRequests: 0,
      mispricedUsd: 0,
    },
  )
  // The comparison reads the same rows the strip does, so the two cannot
  // disagree about the hour.
  const comparisonRows: ComparisonRow[] = hour.map((r) => ({
    served: r.served,
    billing: r.billing ?? '',
    requests: Number(r.requests ?? 0),
    cachedTokens: Number(r.cachedTokens ?? 0),
    promptTokens: Number(r.promptTokens ?? 0),
    completionTokens: Number(r.completionTokens ?? 0),
    energyUsd: Number(r.energyUsd ?? 0),
    baselineUsd: Number(r.baselineUsd ?? 0),
    baselineCachedUsd: Number(r.baselineCachedUsd ?? 0),
    baselineProcessedUsd: Number(r.baselineProcessedUsd ?? 0),
    baselineGeneratedUsd: Number(r.baselineGeneratedUsd ?? 0),
    baselineExcluded: r.baselineExcluded ?? '',
    baselineType: r.baselineType ?? '',
    energyBtu: Number(r.energyBtu ?? 0),
    heatBtuPerHour: Number(r.heatBtuPerHour ?? 0),
    thermalUsd: Number(r.thermalUsd ?? 0),
  }))

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Now">
        <BoxState
          faults={all}
          stopping={stopping}
          ready={readyCount}
          configured={models.length}
          outOfSetup={outOfSetup}
          machines={(ov?.servers ?? []).length}
          version={c.health?.version ?? 'unknown'}
        />
      </PageHeader>

      {/* Anything wrong comes first — before what is running, before what could
          run. This is the panel a person opens the dashboard to read. */}
      <WhatIsWrong faults={all} />

      {/* What the box is doing right now. This is in-flight's ONE home: it used
          to render here AND on /activity, so two pages showed the same live
          table and neither was the authority. */}
      {/* What it is GETTING DONE, under what is wrong with it. Both are the
          home screen's business: an owner arrives asking whether the box works
          AND whether it is doing anything, and a page that answered only the
          first could not tell a healthy busy box from a healthy idle one. */}
      <Throughput
        machines={(ov?.servers ?? []).length}
        ready={readyCount}
        configured={models.length}
        requests={totals.requests}
        promptTokens={totals.promptTokens}
        cachedTokens={totals.cachedTokens}
        completionTokens={totals.completionTokens}
        cacheReports={totals.cacheReports}
        energyUsd={totals.energyUsd}
        energyKwh={totals.energyKwh}
        paidUsd={totals.paidUsd}
        unattributedUsd={totals.unattributedUsd}
        freeRequests={totals.freeRequests}
        mispricedUsd={totals.mispricedUsd}
        costPerKwh={c.recent?.costPerKwh ?? 0}
      />

      {/* WHAT RUNNING IT HERE IS WORTH, directly under what it cost. The strip
          above says the electricity; this says what the same tokens would have
          been billed at on a paid API, and names which one. It sits below rather
          than inside the strip because its scope is a sentence, not a tile's
          sub-label: the comparison covers token-billed traffic on our own
          hardware and nothing else, and a figure whose scope is invisible is the
          one thing worse than no figure. */}
      <PaidComparison
        when="in the last hour"
        baselineType={c.recent?.baselineType ?? ''}
        costPerKwh={c.recent?.costPerKwh ?? 0}
        thermal={c.recent?.thermal ?? null}
        rows={comparisonRows}
        pending={q.isLoading}
      />

      {/* WHERE THE HARDWARE IS GOING, directly under the two tiles that raise
          the question. The strip says "2 machines" and "1 of 12 models loaded";
          this is the same two facts with a shape, and it is the only thing on
          the page that can show a fleet which is simultaneously nearly full and
          almost entirely idle. It sits BELOW what the box is getting done,
          because an owner asks "is it working" and "is it doing anything"
          before "where is it doing it" — and above what is in flight, which is
          the most volatile thing here and stays last. */}
      <FleetPanel
        servers={fleetServers}
        resident={fleetResident}
        declaredByServer={declaredByServer}
        boundPools={boundPools}
        probeIncomplete={probeIncomplete}
      />

      <ActiveRequests />
    </Box>
  )
}

export const Route = createFileRoute('/')({ component: Now })
