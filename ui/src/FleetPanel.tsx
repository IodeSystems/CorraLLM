import { Box, Chip, Typography } from '@mui/material'
import { Panel, Row } from '@/Panel'
import { UtilizationBar } from '@/UtilizationBar'
import { NotYet } from '@/NotYet'
import { residentColor } from '@/MachineData'
import { C } from '@/theme'
import { fmtBytes } from '@/format'

/**
 * WHERE THE MODELS ARE — how much of the hardware is spoken for, and by what.
 *
 * The owner asked for "models in residency on devices, so we can get an idea of
 * fleet utilisation". That is two facts the home screen did not carry: how full
 * each machine is, and WHICH model is filling it. Now had the second fact in its
 * own query already and used none of it — `residency.models` was read for a
 * count of readies and thrown away.
 *
 * WHY IT IS HERE AND NOT ONLY ON /machines. The state line says "1 of 12 models
 * loaded across 2 machines", which on this box is true and hides the thing worth
 * seeing: one card is at 96% while a second card and an entire attached machine
 * hold nothing. A fleet can be nearly full and almost entirely idle at the same
 * time, and no single number says so — that takes one bar per pool.
 *
 * WHY IT DOES NOT REPLACE /machines, deliberately. This is a census: it reads,
 * it does not act. No click-through on the row, no agent chip, no concurrency
 * limit, no per-machine controls, and nothing about the toolchain. The one thing
 * it shows that /machines does not is WHAT IS HOLDING each pool, in the open —
 * /machines has that string in the bar's tooltip, which OB-10 says was never
 * written. Everything else here is a shorter view of that page's own row, on
 * purpose: a reader who recognises the shape knows where the rest of it is.
 *
 * IT IS THE SCHEDULER'S LEDGER, NOT A DEVICE READING. `residency.gpus` is
 * LOCAL-BOX-ONLY — the enrolled MacBook's 51 GB of Metal does not appear in it —
 * so a panel built on measured cards would show one machine and look complete.
 * `residency.servers[].pools` covers the whole fleet, and it is always known, so
 * every bar here is measured-and-present and none of them can be "not measured".
 * The measured overlay (what the card itself reports, and the `available: false`
 * that says the probe could not answer) stays on /machines, which pairs a card
 * to a pool under rules that only hold locally. The cards are used here for ONE
 * thing — deciding whether a pool is device memory — and never for a number.
 */

/** A pool of one machine as the scheduler accounts for it. */
export type FleetPool = {
  pool: string
  budget: number
  used: number
}
/** A resident backend and what it holds, per pool, on its machine. */
export type FleetResident = {
  modelName: string
  server: string
  state: string
  refs: number
  usage: { pool: string; bytes: number }[]
}
export type FleetServer = {
  server: string
  /** The pool the machine's device memory is declared as. */
  devicePool: string
  /** up | down | unknown | local. */
  agentStatus: string
  pools: FleetPool[]
  /** Loads today that had to unload something first. 0 is the normal case. */
  madeRoomCount: number
  madeRoomEvicted: string
  madeRoomFor: string
}

// One clause, no sentence break: this is a label, not prose (Panel.tsx, and
// `corrallm/subtitle-is-a-label` enforces the half a machine can check).
const SUBTITLE = 'What each machine is holding, of what the scheduler may fill'

// The band above the body, where the two things a reader would otherwise get
// wrong go: which models are missing from this list, and that a full bar is not
// by itself a fault (UtilizationBar's own note says why at length).
const NOTE =
  'Models a provider runs are not here — they hold nothing on your machines. How full a pool is is not a fault on its own; Machines has each box in full, with what being full has cost it.'

/**
 * Whether being full HERE has cost anything, in the only terms that settle it.
 *
 * MachineData exports `madeRoomNote`, which is the same sentence — but it takes
 * a whole `Machine`, a shape this page deliberately does not build (it would
 * mean pulling in the /machines query). Widening that signature is the right fix
 * and belongs in that file, not this one.
 */
function madeRoom(s: FleetServer): string {
  if (s.madeRoomCount <= 0) {
    return 'Nothing has been unloaded here today to fit something else, so however full this reads, it has cost nothing.'
  }
  const traded =
    s.madeRoomEvicted && s.madeRoomFor
      ? ` Last unloaded ${s.madeRoomEvicted} to fit ${s.madeRoomFor}.`
      : ''
  return `Made room ${s.madeRoomCount} ${s.madeRoomCount === 1 ? 'time' : 'times'} today — a load here had to unload something first.${traded}`
}

/**
 * device | system | device + system — what this pool IS, in the same words
 * /machines uses for it.
 *
 * The declared `devicePool` names ONE pool per machine, so a second card — this
 * box's idle RTX 3080 on `gpu1` — is a device pool that the declaration does not
 * identify. A measured card says which pool it backs, which settles it, but only
 * for the local box: two machines here both call a pool `gpu0`, and binding by
 * pool name alone would hand box1's 5090 to the MacBook.
 *
 * `available: false` is "the probe could not answer", and the schema says to
 * ignore the rest of that record — POOL INCLUDED. So an unreadable card cannot
 * tell us anything about its pool, and the word is dropped rather than falling
 * through to "system", which would be a wrong answer wearing the shape of a fact.
 */
function roleOf(s: FleetServer, pool: string, bound: Set<string>, probeIncomplete: boolean) {
  const local = s.agentStatus === 'local'
  const isDevice = pool === s.devicePool || (local && bound.has(pool))
  if (isDevice) {
    // A unified-memory box (Apple silicon) has one pool that is both.
    return s.pools.length === 1 ? 'device + system' : 'device'
  }
  return local && probeIncomplete ? undefined : 'system'
}

/** This box first, then by name — a stable order, because these numbers move. */
function order(a: FleetServer, b: FleetServer): number {
  return (
    Number(b.agentStatus === 'local') - Number(a.agentStatus === 'local') ||
    a.server.localeCompare(b.server)
  )
}

/** The device pool first: it is the scarce one, and what eviction fights over. */
function poolOrder(s: FleetServer, bound: Set<string>) {
  const rank = (p: string) =>
    p === s.devicePool ? 0 : s.agentStatus === 'local' && bound.has(p) ? 1 : 2
  return (a: FleetPool, b: FleetPool) => rank(a.pool) - rank(b.pool) || a.pool.localeCompare(b.pool)
}

function MachineLine({ s, resident, declared }: { s: FleetServer; resident: FleetResident[]; declared: number }) {
  const busy = resident.reduce((n, r) => n + r.refs, 0)
  // A MACHINE HOLDING NOTHING IS NOT A MACHINE IN TROUBLE. corrallm loads on
  // demand, so an idle box with an empty card is the normal resting state — it
  // gets plain muted words and an empty track, never a colour and never the
  // word "none" on its own, which reads as a failure to somebody scanning.
  const held =
    resident.length === 0
      ? declared === 0
        ? 'holding nothing · no models are configured to run here'
        : `holding nothing · ${declared} model${declared === 1 ? '' : 's'} can run here`
      : `${resident.length} of ${declared} models here loaded${busy > 0 ? ` · ${busy} in flight` : ''}`
  return (
    <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
      <Typography variant="subtitle2">{s.server}</Typography>
      <Typography variant="caption" sx={{ color: C.textFaint }}>
        {s.agentStatus === 'local' ? 'this box' : s.agentStatus === 'up' ? 'attached' : null}
      </Typography>
      {/* OB-9: a reading that has stopped updating says so. The ledger is still
          the scheduler's own accounting and is not wrong — but it is no longer
          being corrected by the machine it describes, and a bar that looks live
          when it is not is the thing that rule exists to stop. */}
      {s.agentStatus !== 'local' && s.agentStatus !== 'up' && (
        <Typography variant="caption" sx={{ color: C.warn }}>
          not reporting in — this is the last accounting, not a live reading
        </Typography>
      )}
      <Box sx={{ flexGrow: 1 }} />
      <Typography variant="caption" sx={{ color: C.textMuted }}>
        {held}
      </Typography>
    </Box>
  )
}

/** What is in this pool, named — the fact /machines keeps in a tooltip. */
function Holders({ holders }: { holders: { r: FleetResident; bytes: number }[] }) {
  if (holders.length === 0) {
    return (
      <Typography variant="caption" sx={{ color: C.textFaint }}>
        nothing loaded
      </Typography>
    )
  }
  return (
    <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap', alignItems: 'center' }}>
      {holders.map(({ r, bytes }) => (
        <Box key={r.modelName} sx={{ display: 'flex', gap: 0.5, alignItems: 'center' }}>
          <Typography variant="caption" sx={{ color: C.text }}>
            {r.modelName}
          </Typography>
          <Typography variant="caption" sx={{ color: C.textFaint, fontVariantNumeric: 'tabular-nums' }}>
            {fmtBytes(bytes)}
          </Typography>
          {/* Only when it is NOT ready. A chip reading "ready" on every row of a
              working fleet is a chip nobody reads. */}
          {r.state !== 'ready' && (
            <Chip size="small" label={r.state} color={residentColor(r.state)} />
          )}
        </Box>
      ))}
    </Box>
  )
}

export function FleetPanel(props: {
  /** Nothing has answered yet: the panel renders, the bars do not. */
  pending?: boolean
  servers?: FleetServer[]
  resident?: FleetResident[]
  /** Non-remote declared models, by machine — what COULD run there. */
  declaredByServer?: Map<string, number>
  /** Pools a measured local card says it backs. Classification only, never a number. */
  boundPools?: Set<string>
  /** A local card whose probe failed, so its pool cannot be classified. */
  probeIncomplete?: boolean
}) {
  const {
    pending = false,
    servers = [],
    resident = [],
    declaredByServer = new Map<string, number>(),
    boundPools = new Set<string>(),
    probeIncomplete = false,
  } = props

  if (pending) {
    return (
      <Panel title="Where the models are" subtitle={SUBTITLE} dense>
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          <NotYet /> — nothing has reported what it is holding. This is not a fleet holding nothing.
        </Typography>
      </Panel>
    )
  }

  if (servers.length === 0) {
    return (
      <Panel title="Where the models are" subtitle={SUBTITLE} dense>
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          No machines are declared, so nothing can be held anywhere. Machines is where a box is
          added.
        </Typography>
      </Panel>
    )
  }

  return (
    <Panel title="Where the models are" subtitle={SUBTITLE} note={NOTE} flush>
      {[...servers].sort(order).map((s) => {
        const here = resident.filter((r) => r.server === s.server)
        const note = madeRoom(s)
        return (
          <Row key={s.server}>
            <MachineLine s={s} resident={here} declared={declaredByServer.get(s.server) ?? 0} />
            {s.pools.length === 0 ? (
              <Typography variant="caption" sx={{ color: C.textFaint }}>
                No pools declared, so the scheduler will not admit anything here.
              </Typography>
            ) : (
              <Box
                sx={{
                  display: 'grid',
                  gridTemplateColumns: { xs: '1fr', md: 'repeat(auto-fit, minmax(240px, 1fr))' },
                  gap: 1.5,
                  mt: 1,
                }}
              >
                {[...s.pools].sort(poolOrder(s, boundPools)).map((p) => {
                  const holders = here
                    .map((r) => ({ r, bytes: r.usage.find((u) => u.pool === p.pool)?.bytes ?? 0 }))
                    .filter((h) => h.bytes > 0)
                  return (
                    <Box key={p.pool} sx={{ minWidth: 0 }}>
                      <UtilizationBar
                        label={p.pool}
                        role={roleOf(s, p.pool, boundPools, probeIncomplete)}
                        used={p.used}
                        capacity={p.budget}
                        note={note}
                      />
                      <Box sx={{ mt: 0.5 }}>
                        <Holders holders={holders} />
                      </Box>
                    </Box>
                  )
                })}
              </Box>
            )}
          </Row>
        )
      })}
    </Panel>
  )
}
