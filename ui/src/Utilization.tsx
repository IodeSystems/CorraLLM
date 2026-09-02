import { useQuery } from '@tanstack/react-query'
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
  query Utilization($minutes: Long!) {
    corrallm {
      utilization(minutes: $minutes) {
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

export function Utilization({ minutes = 60 }: { minutes?: number }) {
  const q = useQuery({
    // 'activity' prefix so the SSE listener's invalidation reaches it.
    queryKey: ['activity', 'utilization', minutes],
    queryFn: () => gqlClient.request(UtilizationDoc, { minutes: String(minutes) }),
    refetchInterval: 10000,
  })

  const rows = q.data?.corrallm.utilization?.rows ?? []

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
        Nothing has been asked for in the last {minutes} minutes.
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
                <span>CV</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="What the measured distribution implies the wait should be: ρ/(1−ρ)·E[S]·(1+CV²)/2 (Pollaczek–Khinchine). A third opinion, derived from neither the scheduler's estimate nor the recorded waits. Assumes steady state, which a bursty hour is not — read it as an order of magnitude.">
                <span>Theory</span>
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
                  {r.depthUnreachable && (
                    // Two settings that contradict each other. Worth saying out
                    // loud: it means every rejection here will be a timeout, and
                    // the configured depth describes a queue that cannot form.
                    <Tooltip
                      title={`maxQueueDepth is ${r.configuredDepth}, but maxWait only allows ${r.reachableDepth} waiter(s) at ${fmtDuration(r.serviceMeanMs)} per request on ${cap} slot(s). The depth bound never binds — callers always time out first.`}
                    >
                      <Chip
                        size="small"
                        color="warning"
                        variant="outlined"
                        label={`depth ${r.configuredDepth} unreachable`}
                        sx={{ ml: 1 }}
                      />
                    </Tooltip>
                  )}
                </TableCell>
                <TableCell align="right">
                  {cap > 0 ? (
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
                  <Zeroable n={waiting} color={waiting > 0 ? C.warn : undefined} />
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
                  {est > 0 ? fmtDuration(est) : <span style={{ color: C.textFaint }}>—</span>}
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
      subtitle={`Models asked for in the last ${minutes} minutes — live load, promises made, and what waiting actually cost`}
      badge={<Chip size="small" variant="outlined" label={`${rows.length} models`} />}
      flush
    >
      {body}
    </Panel>
  )
}
