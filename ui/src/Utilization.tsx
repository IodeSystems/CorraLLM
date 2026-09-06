import { useQuery } from '@tanstack/react-query'
import { DEFAULT_WINDOW, windowKey, windowPhrase, windowVars, type TimeWindow } from '@/TimeWindow'
import {
  Box,
  Chip,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { Panel } from '@/Panel'
import { C } from '@/theme'
import { fmtDuration } from '@/format'
import { Loading } from '@/Loading'

/**
 * Per-model pressure: what each model is doing now, and what arriving at it cost.
 *
 * Per-model rather than one box-wide figure, because capacity is not fungible —
 * a saturated 27B summed with an idle embedder into "1/6 in use" describes
 * neither, and hides the only thing worth knowing (which one is full). The rows
 * are the models actually ASKED FOR in the window; a model nobody called is
 * absent, not 0% utilized.
 *
 * The two wait columns are the point of the panel. `est` is the scheduler's own
 * live projection — literally what a caller arriving now would be told to wait
 * if we refused them. `real` is what requests that queued measurably waited.
 * They come from different machinery, and a persistent gap between them means
 * the number we hand callers does not describe this box.
 */

const UtilizationDoc = graphql(/* GraphQL */ `
  query Utilization($minutes: Long!, $from: Long, $to: Long) {
    corrallm {
      utilization(minutes: $minutes, from: $from, to: $to) {
        minutes
        rows {
          served
          lane
          members {
            model
            state
            remote
            spawnable
            paused
            pauseReason
            capacity
            active
            waiting
          }
          capacity
          active
          waiting
          promised
          notHonored
          early
          turnedAway
          estWaitMs
          realWaitMs
          maxWaitMs
          queuedSamples
          serviceMeanMs
          serviceCv
          serviceSamples
          rho
          pkWaitMs
          pkSaturated
          configuredDepth
          reachableDepth
          depthUnreachable
        }
      }
    }
  }
`)

type LaneMember = {
  model: string
  state: string
  remote: boolean
  spawnable: boolean
  paused: boolean
  pauseReason: string
  capacity: number | string
  active: number | string
  waiting: number | string
}

// What one rung is doing, and whether it could take work if asked.
//
// Three outcomes, and they are what the lane chip exists to distinguish:
// `warm` serves the next caller immediately, `cold` serves it after a spawn,
// and `blocked` never serves it at all — a lane of four models with three
// paused is one model wearing a lane's name.
function rungStatus(m: LaneMember): { kind: 'warm' | 'cold' | 'blocked'; text: string; color: string } {
  if (m.paused) {
    return {
      kind: 'blocked',
      text: m.pauseReason ? `paused — ${m.pauseReason}` : 'paused — the walk skips it',
      color: C.warn,
    }
  }
  // Residency is not a fact about a host we do not run: a remote rung is always
  // available and there is nothing to load, so it is neither warm nor cold.
  if (m.remote) return { kind: 'warm', text: 'remote — nothing to load', color: C.accent }
  switch (m.state) {
    case 'ready':
      return { kind: 'warm', text: 'loaded', color: C.ok }
    case 'loading':
      return { kind: 'cold', text: 'loading now', color: C.warn }
    case 'evicting':
      return { kind: 'cold', text: 'unloading', color: C.warn }
    case 'stopping':
      return { kind: 'blocked', text: 'stopping — a load is refused until it exits', color: C.warn }
    case 'failed':
      return { kind: 'cold', text: 'last load failed', color: C.error }
    default:
      return {
        kind: 'cold',
        text: m.spawnable ? 'not loaded — spawns on demand' : 'not loaded',
        color: C.textFaint,
      }
  }
}

// A long ladder (a pool lane resolves to a dozen) would run off the screen as a
// tooltip. The warm rungs are the answer to "what is loaded", so they are never
// the ones truncated.
const RUNGS_SHOWN = 8

function LaneTooltip({ lane, members }: { lane: string; members: readonly LaneMember[] }) {
  const rows = members.map((m) => ({ m, s: rungStatus(m) }))
  const warm = rows.filter((r) => r.s.kind === 'warm').length
  const cold = rows.filter((r) => r.s.kind === 'cold').length
  const blocked = rows.length - warm - cold
  // Ladder order is kept — it is the order the walk tries them in, and a list
  // sorted by state would quietly contradict the line above it. Truncation
  // takes from the middle instead: a warm rung past the cut is the one fact
  // this tooltip exists to show, so it is shown out of place rather than lost.
  const head = rows.slice(0, RUNGS_SHOWN)
  const tailWarm = rows.slice(RUNGS_SHOWN).filter((r) => r.s.kind === 'warm')
  const hidden = rows.length - head.length - tailWarm.length
  const line = ({ m, s }: (typeof rows)[number]) => {
    const cap = Number(m.capacity)
    const active = Number(m.active)
    const waiting = Number(m.waiting)
    return [
      <Typography key={`${m.model}-n`} variant="caption" sx={{ color: s.color }}>
        {s.kind === 'warm' ? '\u25cf' : s.kind === 'cold' ? '\u25cb' : '\u2298'} {m.model}
      </Typography>,
      <Typography key={`${m.model}-s`} variant="caption" sx={{ color: C.textMuted }}>
        {s.text}
      </Typography>,
      <Typography key={`${m.model}-l`} variant="caption" sx={{ color: C.textFaint }}>
        {cap > 0 ? `${active}${waiting > 0 ? `+${waiting}` : ''} / ${cap}` : ''}
      </Typography>,
    ]
  }
  return (
    <Box sx={{ py: 0.5 }}>
      <Typography variant="caption" sx={{ display: 'block', mb: 0.75 }}>
        <b>{lane}</b> is a lane, not a backend — tried top to bottom, and its load is these
        rungs&apos; load counted once.
      </Typography>
      <Box
        sx={{ display: 'grid', gridTemplateColumns: 'auto 1fr auto', columnGap: 1.5, rowGap: 0.25 }}
      >
        {head.map(line)}
        {hidden > 0 && (
          <Typography
            key="hidden"
            variant="caption"
            sx={{ gridColumn: '1 / -1', color: C.textFaint }}
          >
            … {hidden} more not shown
          </Typography>
        )}
        {tailWarm.map(line)}
      </Box>
      <Typography variant="caption" sx={{ display: 'block', mt: 0.75, color: C.textMuted }}>
        {warm === 0
          ? cold === 0
            ? 'Nothing here can serve a request.'
            : `Nothing is loaded — the next caller pays a cold start on one of ${cold}.`
          : `${warm} ready now, ${cold} more could load.`}
        {blocked > 0 && ` ${blocked} shut out.`}
      </Typography>
    </Box>
  )
}

// A dash reads as "nothing here"; a 0 reads as a measurement. Most of these
// columns are genuinely empty most of the time, so they get the dash.
function Zeroable({ n, color }: { n: number; color?: string }) {
  if (!n) return <span style={{ color: C.textFaint }}>—</span>
  return <span style={color ? { color } : undefined}>{n}</span>
}

export function Utilization({ window = DEFAULT_WINDOW }: { window?: TimeWindow }) {
  const vars = windowVars(window)
  const q = useQuery({
    // 'activity' prefix so the SSE listener's invalidation reaches it.
    queryKey: ['activity', 'utilization', windowKey(window)],
    queryFn: () => gqlClient.request(UtilizationDoc, { minutes: vars.minutes, from: vars.from, to: vars.to }),
    // A frozen window cannot change, so polling it is pure noise on a box whose
    // GPU time somebody is waiting for.
    refetchInterval: window.kind === 'absolute' ? false : 10000,
  })

  const rows = q.data?.corrallm.utilization?.rows ?? []

  // THREE OF THESE COLUMNS ARE LIVE, WHATEVER THE WINDOW SAYS. `In use`, `Queue`
  // and `Est. wait` come from the scheduler's CURRENT snapshot — there is no
  // historical record of a slot's occupancy or of an estimate that was offered.
  // Under a frozen window they were still being drawn, so a page headed "a fixed
  // span — these numbers will not change" showed an est. wait identical to the
  // live page's, to the tenth of a second. Lenny run 3 caught it by reading the
  // two screens against each other, which is the only way it was visible: each
  // screen alone was plausible. A false number is worse than a missing one
  // (OB-9), so on a fixed span these three say so instead.
  const live = window.kind !== 'absolute'

  // WHOSE PROBLEM IT IS AND WHAT TO DO, once, above the table. The chip alone was
  // orange text neither persona could act on — "nobody says whose fault or what
  // to do" (the operator), "it doesn't say whose settings, which settings, or
  // what to do about it" (the caller). OB-6: a fault names whose it is and the
  // next thing to do, or it is not shown as a fault.
  const clashing = rows.filter((r) => r.depthUnreachable)

  const body = q.isLoading ? (
    <Loading size={20} minHeight={120} />
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : rows.length === 0 ? (
    <Box sx={{ p: 2 }}>
      <Typography sx={{
        color: "text.secondary"
      }}>
        Nothing was asked for {windowPhrase(window)}.
      </Typography>
    </Box>
  ) : (
    <TableContainer>
      <Table size="small" stickyHeader>
        <TableHead>
          <TableRow>
            <TableCell>Model</TableCell>
            <TableCell align="right">
              <Tooltip title="Slots in service now, over slots that exist. A `+n` is the queue on top of them: demand, not occupancy — 1+1/1 is a full slot with somebody behind it, and it is the moment before a rejection.">
                <span>In use</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">Queue</TableCell>
            <TableCell align="right">
              <Tooltip title="Rejected with a come-back-later in this window: the demand that did not fit. A row reading 1/1 with a queue of 0 and a number here was over capacity for the whole window — the slot state is instantaneous, this is what happened.">
                <span>Turned away</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Promises still outstanding: told to come back at a time that has not arrived yet. These are scheduled arrivals the queue depth cannot see.">
                <span>Promised</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="We told them to come back and the time passed with no return — callers we may simply have driven off.">
                <span>Not honored</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Came back BEFORE the time we gave them — ignoring Retry-After and adding load early.">
                <span>Early</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="What the scheduler would tell a caller arriving right now to wait — its own live estimate, the same number that goes out on Retry-After.">
                <span>Est. wait</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="What requests that actually queued measurably waited before being admitted. Instant admissions and rejections are excluded — this is 'when you had to wait, how long'.">
                <span>Real wait</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Coefficient of variation of service time. Above 1 the mean is tail-dominated, and the scheduler's position×mean estimate under-predicts badly.">
                <span>How even</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="What the measured distribution implies the wait should be: ρ/(1−ρ)·E[S]·(1+CV²)/2 (Pollaczek–Khinchine). A third opinion, derived from neither the scheduler's estimate nor the recorded waits. Assumes steady state, which a bursty hour is not — read it as an order of magnitude.">
                <span>Wait the maths expects</span>
              </Tooltip>
            </TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((r) => {
            const cap = Number(r.capacity)
            const active = Number(r.active)
            const waiting = Number(r.waiting)
            const est = Number(r.estWaitMs)
            const real = Number(r.realWaitMs)
            const n = Number(r.queuedSamples)
            const cv = Number(r.serviceCv)
            const pk = Number(r.pkWaitMs)
            return (
              <TableRow key={r.served} hover>
                <TableCell>
                  {r.served}
                  {r.lane &&
                    (() => {
                      // Without this the same busy slot appears twice — once as
                      // the lane, once as the model it resolves to — and the
                      // column reads as two slots where there is one.
                      //
                      // The count is on the CHIP because "lane" alone said only
                      // that the row was an aggregate — not of what, nor whether
                      // any of it was warm. A lane reading 0/1 in use is one
                      // model idling or four models none of which is loaded, and
                      // those are opposite answers for the next caller.
                      const members: readonly LaneMember[] = r.members ?? []
                      const warm = members.filter((m) => rungStatus(m).kind === 'warm').length
                      return (
                        <Tooltip
                          title={<LaneTooltip lane={r.served} members={members} />}
                          slotProps={{ tooltip: { sx: { maxWidth: 460 } } }}
                        >
                          <Chip
                            size="small"
                            variant="outlined"
                            color={warm === 0 ? 'warning' : 'default'}
                            label={`lane ${warm}/${members.length} ready`}
                            sx={{ ml: 1 }}
                          />
                        </Tooltip>
                      )
                    })()}
                  {/* TWO SETTINGS THAT CONTRADICT EACH OTHER, said in words.
                      It read `depth 8 unreachable`, which Lenny run 4 could not
                      tell apart from a machine being unreachable — two meanings
                      of "unreachable" on adjacent screens (OB-3). And it is a
                      CONFIGURATION fact, not a measurement, so it says which:
                      drawn identically on a frozen window it looked like a
                      reading of that hour (OB-9).
                      The arithmetic behind it stays in the tooltip for whoever
                      wants it; the sentence does not need it. */}
                  {r.depthUnreachable && (
                    <Tooltip
                      title={`maxQueueDepth is ${r.configuredDepth}, but maxWait only allows ${r.reachableDepth} waiter(s) at ${fmtDuration(r.serviceMeanMs)} per request on ${cap} slot(s).`}
                    >
                      <Chip
                        size="small"
                        color="warning"
                        variant="outlined"
                        // On a frozen window this is still a SETTING, not a
                        // reading of that hour — run 5 could not tell which, and
                        // it was the one thing drawn identically on both (OB-9).
                        label={live ? 'settings clash' : 'settings clash (a setting, not a reading)'}
                        sx={{ ml: 1 }}
                      />
                    </Tooltip>
                  )}
                </TableCell>
                <TableCell align="right">
                  {!live ? (
                    // NOT the same dash as a zero elsewhere in this row. Run 5:
                    // "same glyph, two different reasons, no visual difference
                    // between them" (OB-9).
                    <span style={{ color: C.textFaint, fontSize: 11 }}>not recorded</span>
                  ) : cap > 0 ? (
                    <Tooltip
                      title={
                        waiting > 0
                          ? `${active} in service and ${waiting} queued against ${cap} slot(s) — demand is ${active + waiting}.`
                          : `${active} in service against ${cap} slot(s).`
                      }
                    >
                      <Chip
                        size="small"
                        variant={active >= cap ? 'filled' : 'outlined'}
                        color={active + waiting > cap ? 'error' : active >= cap ? 'warning' : 'default'}
                        label={waiting > 0 ? `${active}+${waiting} / ${cap}` : `${active} / ${cap}`}
                      />
                    </Tooltip>
                  ) : (
                    // No live scheduler state: the model was called in the window
                    // but nothing has been admitted on it since this process
                    // started. Reporting 0/0 would imply zero capacity.
                    <Tooltip title="No admissions on this model since startup — no live slot state to report.">
                      <span style={{ color: C.textFaint }}>—</span>
                    </Tooltip>
                  )}
                </TableCell>
                <TableCell align="right">
                  {live ? (
                    <Zeroable n={waiting} color={waiting > 0 ? C.warn : undefined} />
                  ) : (
                    <span style={{ color: C.textFaint, fontSize: 11 }}>not recorded</span>
                  )}
                </TableCell>
                <TableCell align="right">
                  <Zeroable n={Number(r.turnedAway)} color={C.error} />
                </TableCell>
                <TableCell align="right">
                  <Zeroable n={Number(r.promised)} />
                </TableCell>
                <TableCell align="right">
                  <Zeroable n={Number(r.notHonored)} color={C.warn} />
                </TableCell>
                <TableCell align="right">
                  <Zeroable n={Number(r.early)} color={C.warn} />
                </TableCell>
                <TableCell align="right">
                  {live ? (
                    est > 0 ? (
                      fmtDuration(est)
                    ) : (
                      <span style={{ color: C.textFaint }}>—</span>
                    )
                  ) : (
                    <span style={{ color: C.textFaint, fontSize: 11 }}>not recorded</span>
                  )}
                </TableCell>
                <TableCell align="right">
                  {n > 0 ? (
                    <Tooltip
                      title={`${n} request${n === 1 ? '' : 's'} queued; longest ${fmtDuration(r.maxWaitMs)}`}
                    >
                      {/* A mean over one or two samples is not a trend, and
                          reading it as one is how a fluke becomes a fact. */}
                      <span style={n < 3 ? { color: C.textFaint } : undefined}>
                        {fmtDuration(real)}
                        {n < 3 ? ` (n=${n})` : ''}
                      </span>
                    </Tooltip>
                  ) : (
                    <span style={{ color: C.textFaint }}>—</span>
                  )}
                </TableCell>
                <TableCell align="right">
                  {Number(r.serviceSamples) > 0 ? (
                    <Tooltip
                      title={`mean service ${fmtDuration(r.serviceMeanMs)} over ${r.serviceSamples} requests; utilization ${(Number(r.rho) * 100).toFixed(0)}%`}
                    >
                      <span style={{ color: cv >= 2 ? C.warn : undefined }}>{cv.toFixed(2)}</span>
                    </Tooltip>
                  ) : (
                    <span style={{ color: C.textFaint }}>—</span>
                  )}
                </TableCell>
                <TableCell align="right">
                  {r.pkSaturated ? (
                    <Tooltip title="Arrivals outpaced service over the window — utilization reached 1 and no finite steady-state wait exists.">
                      <span style={{ color: C.warn }}>saturated</span>
                    </Tooltip>
                  ) : pk > 0 ? (
                    <span>{fmtDuration(pk)}</span>
                  ) : (
                    <span style={{ color: C.textFaint }}>—</span>
                  )}
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </TableContainer>
  )

  return (
    <Panel
      title="Utilization"
      subtitle={
        live
          ? `Models asked for ${windowPhrase(window)} — live load, promises made, and what waiting actually cost. "How even" near 1 means request times vary as much as they average; well above 1 means a few long ones dominate, and every wait estimate under-reads.`
          : `Models asked for ${windowPhrase(window)}. In use, Queue and Est. wait are blank: they describe this moment, and nothing recorded what they were then.`
      }
      badge={<Chip size="small" variant="outlined" label={`${rows.length} models`} />}
      flush
    >
      {clashing.length > 0 && (
        <Box sx={{ px: 2, py: 1.5, borderTop: `1px solid ${C.border}` }}>
          <Typography variant="body2" sx={{ color: C.warn }}>
            {clashing.length === 1
              ? `${clashing[0].served} has two settings that contradict each other.`
              : `${clashing.length} models have two settings that contradict each other.`}{' '}
            Its queue is allowed to hold {clashing[0].configuredDepth} waiters, but the longest
            wait it permits only fits {clashing[0].reachableDepth} at the speed it is answering —
            so the queue never fills and callers time out instead of being told to come back.
          </Typography>
          <Typography variant="body2" sx={{ color: C.textMuted, mt: 0.5 }}>
            Yours to change, and nothing is broken meanwhile: raise the wait it permits or lower
            the queue it advertises, on <b>Setup</b> under the model. Leaving it alone costs a
            clear "come back at 14:02" — callers get a timeout instead.
          </Typography>
        </Box>
      )}
      {body}
    </Panel>
  )
}
