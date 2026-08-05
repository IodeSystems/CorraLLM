import { useQuery } from '@tanstack/react-query'
import {
  Box,
  Chip,
  CircularProgress,
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
    <Box sx={{ p: 2 }}>
      <CircularProgress />
    </Box>
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : rows.length === 0 ? (
    <Box sx={{ p: 2 }}>
      <Typography color="text.secondary">
        Nothing has been asked for in the last {minutes} minutes.
      </Typography>
    </Box>
  ) : (
    <TableContainer>
      <Table size="small" stickyHeader>
        <TableHead>
          <TableRow>
            <TableCell>Model</TableCell>
            <TableCell align="right">In use</TableCell>
            <TableCell align="right">Queue</TableCell>
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
                    <Chip
                      size="small"
                      variant={active >= cap ? 'filled' : 'outlined'}
                      color={active >= cap ? 'warning' : 'default'}
                      label={`${active} / ${cap}`}
                    />
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
