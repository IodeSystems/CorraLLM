import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Box, Chip, CircularProgress, Typography } from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { ActiveRequests } from '@/ActiveRequests'
import { Panel, PageHeader, Row } from '@/Panel'
import { C } from '@/theme'
import { Loading } from '@/Loading'

/**
 * NOW — is this box serving, and if not, what is wrong?
 *
 * The home screen answers the first question anybody arrives with, and nothing
 * else (P30). It used to also be the model catalog, the memory ledger and the
 * lane definitions: four subjects, of which a person opening the dashboard
 * because something felt wrong wanted one. The catalog is /models, the ledger is
 * /machines, the lanes are /models.
 *
 * What is left is deliberately three panels: a state sentence, the faults behind
 * it, and what is in flight.
 */
const NowDoc = graphql(/* GraphQL */ `
  query Now {
    corrallm {
      health {
        version
      }
      overview {
        servers {
          server
          agentStatus
          agentLastSeen
        }
        models {
          name
          paused
          pauseReason
          pauseResumeMs
        }
        extensions {
          name
          paused
          pauseReason
        }
      }
      residency {
        stopping
        servers {
          server
          pools {
            pool
            budget
            used
          }
        }
        models {
          modelName
          server
          state
        }
      }
    }
  }
`)
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
type Fault = {
  key: string
  what: string
  whose: string
  next: string
  when?: string
  severity: 'error' | 'warning' | 'info'
  // WHOSE MACHINE, and it decides the headline. Lenny run 3: the box spent its
  // one "Struggling" on a laptop that had never reported in, and never said
  // anything about the machine actually serving the team — so a person came
  // looking for "is my box broken" and left unable to say. A fault somewhere
  // else is still a fault; it is not this box struggling.
  scope: 'here' | 'attached'
}

function faultsOf(
  servers: readonly { server: string; agentStatus: string; agentLastSeen: string }[],
  resident: readonly { modelName: string; server: string; state: string }[],
  pools: readonly { server: string; pools: readonly { pool: string; budget: string; used: string }[] }[],
  models: readonly { name: string; paused: boolean; pauseReason: string; pauseResumeMs: string }[],
  extensions: readonly { name: string; paused: boolean; pauseReason: string }[],
): Fault[] {
  const out: Fault[] = []

  // A machine that stopped reporting in. 'local' is this box and has no agent;
  // 'unknown' means it has never reported at all, which reads to a person as the
  // same thing — the models there will not start.
  for (const s of servers) {
    if (s.agentStatus === 'up' || s.agentStatus === 'local') continue
    const last = Number(s.agentLastSeen)
    out.push({
      key: `agent:${s.server}`,
      scope: 'attached',
      severity: s.agentStatus === 'down' ? 'error' : 'warning',
      what: `${s.server} is not reporting in`,
      whose: 'That machine, not this one.',
      next: 'Check it is awake and on the network. Nothing new can start there until it reports in; anything already running there is left alone.',
      when: last > 0 ? `last heard from at ${new Date(last).toLocaleTimeString()}` : 'it has never reported in',
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
function BoxState(props: {
  faults: Fault[]
  stopping: number
  ready: number
  configured: number
  machines: number
  version: string
}) {
  const { faults, stopping, ready, configured, machines, version } = props
  const bad = faults.filter((f) => f.severity !== 'info' && f.scope === 'here').length
  const elsewhere = faults.filter((f) => f.severity !== 'info' && f.scope === 'attached').length
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
          ? 'No models are configured, so nothing can be served yet. Start on Setup.'
          : // THIS box is fine and something else is not: say both, in that order.
            // "Struggling" for a machine that is not serving anybody sends a
            // person hunting for a fault on the one that is.
            elsewhere > 0
            ? `This machine is serving normally. ${elsewhere} attached machine${elsewhere === 1 ? ' is' : 's are'} not, and ${elsewhere === 1 ? 'it is' : 'they are'} below.`
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
            {f.whose} {f.next}
          </Typography>
        </Row>
      ))}
    </Panel>
  )
}


function Now() {
  const q = useQuery({
    queryKey: ['now'],
    queryFn: () => gqlClient.request(NowDoc),
    refetchInterval: 10000,
  })

  if (q.isLoading) return <Loading />
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
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
  if (!c) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }
  const ov = c.overview
  const res = c.residency
  const models = ov?.models ?? []
  const stopping = (res?.stopping ?? []).length
  const readyCount = (res?.models ?? []).filter((m) => m.state === 'ready').length

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
  )

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Now">
        <BoxState
          faults={faults}
          stopping={stopping}
          ready={readyCount}
          configured={models.length}
          machines={(ov?.servers ?? []).length}
          version={c.health?.version ?? 'unknown'}
        />
      </PageHeader>

      {/* Anything wrong comes first — before what is running, before what could
          run. This is the panel a person opens the dashboard to read. */}
      <WhatIsWrong faults={faults} />

      {/* What the box is doing right now. This is in-flight's ONE home: it used
          to render here AND on /activity, so two pages showed the same live
          table and neither was the authority. */}
      <ActiveRequests />

      {/* Nothing else. What can be called is /models, what the hardware holds is
          /machines, what it has been doing is /traffic. A person who opened this
          page because something felt wrong has their answer above, and a way to
          each of those in the nav. */}
      {faults.length === 0 && (
        <Panel title="Nothing else on this page, on purpose" dense>
          <Row>
            <Typography variant="body2" sx={{ color: C.textMuted }}>
              This page says whether the box is serving and what needs looking at. What can be
              called is <b>Models</b>, what the hardware is holding is <b>Machines</b>, and what it
              has been doing is <b>Traffic</b>.
            </Typography>
          </Row>
        </Panel>
      )}
    </Box>
  )
}

export const Route = createFileRoute('/')({ component: Now })
