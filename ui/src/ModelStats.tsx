import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { DEFAULT_WINDOW, windowKey, windowPhrase, windowVars, type TimeWindow } from '@/TimeWindow'
import {
  Box,
  Button,
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
import { fmtDuration, fmtInt, fmtUSD } from '@/format'
import { Loading } from '@/Loading'

/**
 * ONE ROW PER MODEL, saying what it did and what it cost to reach it.
 *
 * This was two tables — "Utilization" (pressure: slots, rejections, waits) and
 * usage's "By model" (spend: requests, tokens, cost) — one under the other, both
 * keyed by the same model name, both over the same window. Eighteen columns
 * across two scroll containers to describe one object, and neither table could
 * answer the obvious follow-up to the other: a model turning callers away is
 * only interesting next to what it was busy doing, and a model costing a lot is
 * only interesting next to whether anyone had to wait for it.
 *
 * Twelve columns now, and the two the report asked for by name are here: tp/s
 * and tg/s, per model, where a rate is meaningful — never summed onto a total
 * row, which cannot average rates honestly (see the schema's own note on
 * promptPerSec).
 *
 * WHAT IS LOADED IS NOT ON THIS PAGE. The rung-by-rung lane ladder — which model
 * in a lane is warm, which would cold-start — is residency, and residency is
 * /machines' subject. It was the single widest thing in the old table and it
 * answered a question nobody opens Traffic to ask.
 */

const SetQueueDepthDoc = graphql(/* GraphQL */ `
  mutation SetQueueDepth($name: String!, $body: corrallm_SetModelQueueDepthInputBodyInput!) {
    corrallm {
      setModelQueueDepth(name: $name, body: $body) {
        ok
        message
      }
    }
  }
`)

const ModelStatsDoc = graphql(/* GraphQL */ `
  query ModelStats($minutes: Long!, $windowHours: Long!, $from: Long, $to: Long) {
    corrallm {
      utilization(minutes: $minutes, from: $from, to: $to) {
        rows {
          served
          lane
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
      usageRollup(windowHours: $windowHours, from: $from, to: $to) {
        rows {
          served
          requests
          promptTokens
          completionTokens
          dwellMs
          costUsd
          cachedTokens
          cacheReports
          cacheHitRate
          promptPerSec
          predictedPerSec
        }
        total {
          requests
          promptTokens
          completionTokens
          dwellMs
          costUsd
          cachedTokens
          cacheReports
          cacheHitRate
          cachedSecondsSaved
        }
      }
    }
  }
`)

/**
 * The one fetch, exported so the page's headline tiles read the same numbers.
 *
 * React Query dedupes on the key: the tiles at the top and this table are one
 * request, not two, and cannot drift apart mid-window.
 */
export function useModelStats(window: TimeWindow = DEFAULT_WINDOW) {
  const vars = windowVars(window)
  return useQuery({
    // 'activity' prefix so the SSE listener's invalidation reaches it.
    queryKey: ['activity', 'modelStats', windowKey(window)],
    queryFn: () =>
      gqlClient.request(ModelStatsDoc, {
        minutes: vars.minutes,
        windowHours: vars.windowHours,
        from: vars.from,
        to: vars.to,
      }),
    // A frozen window cannot change, so polling it is pure noise on a box whose
    // GPU time somebody is waiting for.
    refetchInterval: window.kind === 'absolute' ? false : 15000,
  })
}

type UtilRow = NonNullable<
  NonNullable<ReturnType<typeof useModelStats>['data']>['corrallm']['utilization']
>['rows'][number]
type SpendRow = NonNullable<
  NonNullable<ReturnType<typeof useModelStats>['data']>['corrallm']['usageRollup']
>['rows'][number]

export type ClashRow = Pick<
  UtilRow,
  'served' | 'configuredDepth' | 'reachableDepth' | 'depthUnreachable'
>

/** Models the window knows about, from either side, busiest first. */
function joinRows(util: readonly UtilRow[], spend: readonly SpendRow[]) {
  const byModel = new Map<string, { served: string; u?: UtilRow; s?: SpendRow }>()
  // Utilization lists models ASKED FOR; the rollup lists models that actually
  // completed something. A model that only ever turned callers away is in the
  // first and not the second, and it is the row most worth seeing — so the join
  // is a union, not a lookup from one side.
  for (const u of util) byModel.set(u.served, { served: u.served, u })
  for (const s of spend) {
    const e = byModel.get(s.served)
    if (e) e.s = s
    else byModel.set(s.served, { served: s.served, s })
  }
  return [...byModel.values()].sort((a, b) => {
    const rb = Number(b.s?.requests ?? 0) - Number(a.s?.requests ?? 0)
    return rb !== 0 ? rb : Number(b.u?.turnedAway ?? 0) - Number(a.u?.turnedAway ?? 0)
  })
}

// A dash reads as "nothing here"; a 0 reads as a measurement. Most of these
// columns are genuinely empty most of the time, so they get the dash.
function Zeroable({ n, color }: { n: number; color?: string }) {
  if (!n) return <span style={{ color: C.textFaint }}>—</span>
  return <span style={color ? { color } : undefined}>{n}</span>
}

/**
 * A tokens-per-second reading, or an honest admission that nobody reported one.
 *
 * ZERO IS NOT A MEASUREMENT HERE. Only llama.cpp backends report timings; every
 * remote provider leaves both rates at 0, and so does any model that served
 * nothing but embeddings. Printing "0.0 tg/s" against a remote provider is a
 * lie about a box that is working fine — the same trap cacheReports' "ZERO MEANS
 * UNKNOWN" exists to head off, one column over.
 */
function Rate({ v, what }: { v: number; what: string }) {
  if (!(v > 0)) {
    return (
      <Tooltip
        title={`Nothing here reported a ${what} rate. Only local llama.cpp backends report timings — a remote provider never does, and neither does an embedding model. Not a measured zero.`}
      >
        <span style={{ color: C.textFaint }}>n/r</span>
      </Tooltip>
    )
  }
  return <span style={{ fontVariantNumeric: 'tabular-nums' }}>{v.toFixed(1)}</span>
}

/**
 * TWO SETTINGS THAT CONTRADICT EACH OTHER, and the control that ends it.
 *
 * It writes THIS model's queue limit and nothing else. That was impossible until
 * the bound could be set per model: the reachable depth is one model's
 * arithmetic, so the value that stops a slow model promising a queue slot it
 * cannot honour would turn away callers of every fast model if it were set
 * box-wide — which is why this could name the fix for weeks and only describe it.
 */
export function QueueDepthClash({ rows }: { rows: readonly ClashRow[] }) {
  const qc = useQueryClient()
  const setDepth = useMutation({
    mutationFn: (v: { name: string; depth: number }) =>
      // Long crosses the wire as a string in the generated types.
      gqlClient.request(SetQueueDepthDoc, { name: v.name, body: { maxQueueDepth: String(v.depth) } }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['activity'] }),
  })
  if (rows.length === 0) return null
  const first = rows[0]
  return (
    <Box>
      <Typography variant="body2" sx={{ color: C.warn }}>
        {rows.length === 1
          ? `${first.served} has two settings that contradict each other.`
          : `${rows.length} models have two settings that contradict each other.`}{' '}
        Its queue is allowed to hold {first.configuredDepth} waiters, but the longest wait it
        permits only fits {first.reachableDepth} at the speed it is answering — so the queue never
        fills and callers time out instead of being told to come back.
      </Typography>
      <Typography variant="body2" sx={{ color: C.textMuted, mt: 0.5 }}>
        Leaving it alone costs a clear &quot;come back at 14:02&quot; — callers get a timeout
        instead. The button sets <b>this model&apos;s</b> queue limit to {first.reachableDepth}, the
        number it can actually reach. Nobody waits any longer than they do now, no other model
        changes, and a request already running is unaffected — the next caller past the limit is
        told to come back instead of waiting for a timeout. It takes effect at once.
      </Typography>
      <Box sx={{ display: 'flex', gap: 1, alignItems: 'center', mt: 1, flexWrap: 'wrap' }}>
        {rows.map((r) => (
          <Button
            key={r.served}
            size="small"
            variant="outlined"
            disabled={setDepth.isPending || Number(r.reachableDepth) < 1}
            onClick={() => setDepth.mutate({ name: r.served, depth: Number(r.reachableDepth) })}
          >
            {setDepth.isPending
              ? 'Setting…'
              : rows.length === 1
                ? `Set the queue limit to ${r.reachableDepth}`
                : `${r.served}: set to ${r.reachableDepth}`}
          </Button>
        ))}
        {/* The other lever stays a sentence, because it is box-wide and should
            not be one click: raising the wait makes callers of every model wait
            longer, and llm-bench's stall guard is derived from it. */}
        <Typography variant="caption" sx={{ color: C.textFaint }}>
          or raise the longest allowed wait on Setup — box-wide, so it slows every model&apos;s
          &quot;come back&quot; for every caller.
        </Typography>
      </Box>
      {setDepth.isError && (
        <Typography variant="body2" sx={{ color: C.warn, mt: 1 }}>
          That did not save: {String(setDepth.error)}. Nothing changed.
        </Typography>
      )}
    </Box>
  )
}

export function ModelStats({ window = DEFAULT_WINDOW }: { window?: TimeWindow }) {
  const q = useModelStats(window)

  const util = q.data?.corrallm.utilization?.rows ?? []
  const spend = q.data?.corrallm.usageRollup?.rows ?? []
  const rows = joinRows(util, spend)

  // TWO OF THESE COLUMNS ARE LIVE, WHATEVER THE WINDOW SAYS. `In use` and the
  // est. wait behind the Wait tooltip come from the scheduler's CURRENT
  // snapshot — there is no historical record of a slot's occupancy or of an
  // estimate that was offered. Under a frozen window they were still drawn, so a
  // page headed "a fixed span — these numbers will not change" showed an est.
  // wait identical to the live page's, to the tenth of a second. A false number
  // is worse than a missing one (OB-9), so on a fixed span they say so instead.
  const live = window.kind !== 'absolute'

  const body = q.isLoading ? (
    <Loading size={20} minHeight={120} />
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : rows.length === 0 ? (
    <Box sx={{ p: 2 }}>
      <Typography sx={{ color: 'text.secondary' }}>
        Nothing was asked for {windowPhrase(window)}.
      </Typography>
    </Box>
  ) : (
    <TableContainer>
      <Table size="small" stickyHeader>
        <TableHead>
          <TableRow>
            <TableCell>Model</TableCell>
            <TableCell align="right">Requests</TableCell>
            <TableCell align="right">
              <Tooltip title="Slots in service now, over slots that exist. A `+n` is the queue on top of them: demand, not occupancy — 1+1/1 is a full slot with somebody behind it, and it is the moment before a rejection.">
                <span>In use</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Rejected with a come-back-later in this window: the demand that did not fit. A row reading 1/1 with a number here was over capacity for the whole window — the slot state is instantaneous, this is what happened.">
                <span>Turned away</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Prompt-processing speed the backend reported, tokens/sec — how fast it READS. An unweighted mean over the requests that reported a rate; 'n/r' means none did.">
                <span>tp/s</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Generation speed the backend reported, tokens/sec — how fast it WRITES. This is the number that falls when the box itself degrades, while tp/s holds steady; a drop in both is more prompt to read, not a sick box.">
                <span>tg/s</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Prompt tokens processed — what callers sent it to read.">
                <span>Processed</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Completion tokens generated — what it wrote back.">
                <span>Generated</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Prompt tokens served from cache instead of reprocessed. 'n/r' means nothing here reported a cache — not a measured zero.">
                <span>Cached</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Mean time one request holds a slot, with the window's total underneath. The per-request figure, not the sum: a sum of dwell reads as a duration and was misread as one three times over.">
                <span>Per request</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="What requests that actually queued measurably waited before being admitted. Instant admissions and rejections are excluded — this is 'when you had to wait, how long'. Hover a value for the scheduler's own estimate and what the measured distribution says it should be.">
                <span>Wait</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">Cost</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map(({ served, u, s }) => {
            const cap = Number(u?.capacity ?? 0)
            const active = Number(u?.active ?? 0)
            const waiting = Number(u?.waiting ?? 0)
            const requests = Number(s?.requests ?? 0)
            const n = Number(u?.queuedSamples ?? 0)
            const real = Number(u?.realWaitMs ?? 0)
            const est = Number(u?.estWaitMs ?? 0)
            const cv = Number(u?.serviceCv ?? 0)
            const pk = Number(u?.pkWaitMs ?? 0)
            const promised = Number(u?.promised ?? 0)
            const notHonored = Number(u?.notHonored ?? 0)
            const early = Number(u?.early ?? 0)
            const cacheReports = Number(s?.cacheReports ?? 0)
            return (
              <TableRow key={served} hover>
                <TableCell>
                  {served}
                  {u?.lane && (
                    // Without this the same busy slot appears twice — once as the
                    // lane, once as the model it resolves to — and the column
                    // reads as two slots where there is one. What the lane is
                    // MADE OF lives on /machines; here it only needs to stop the
                    // row being misread as one backend.
                    <Tooltip title="A lane, not a backend: tried top to bottom, and its load is its rungs' load counted once. /machines has the ladder.">
                      <Chip size="small" variant="outlined" label="lane" sx={{ ml: 1 }} />
                    </Tooltip>
                  )}
                  {u?.depthUnreachable && (
                    <Tooltip
                      title={`maxQueueDepth is ${u.configuredDepth}, but maxWait only allows ${u.reachableDepth} waiter(s) at ${fmtDuration(u.serviceMeanMs)} per request on ${cap} slot(s).`}
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
                  {requests > 0 ? (
                    fmtInt(requests)
                  ) : (
                    <Tooltip title="Asked for in this window, but nothing completed — every request was turned away, refused, or is still running.">
                      <span style={{ color: C.textFaint }}>none finished</span>
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
                  {/* Promised / not honoured / came back early were three more
                      columns of mostly em-dashes. They are outcomes of the SAME
                      rejection this cell counts, so they ride its tooltip, and
                      the panel that lists them caller by caller is two panels
                      down. */}
                  <Tooltip
                    title={
                      promised + notHonored + early > 0
                        ? `${promised} still due back, ${notHonored} never returned, ${early} came back before the time we named.`
                        : 'Nobody was told to come back on this model in the window.'
                    }
                  >
                    <span>
                      <Zeroable n={Number(u?.turnedAway ?? 0)} color={C.error} />
                    </span>
                  </Tooltip>
                </TableCell>
                <TableCell align="right">
                  <Rate v={Number(s?.promptPerSec ?? 0)} what="prompt-processing" />
                </TableCell>
                <TableCell align="right">
                  <Rate v={Number(s?.predictedPerSec ?? 0)} what="generation" />
                </TableCell>
                <TableCell align="right">{requests > 0 ? fmtInt(s?.promptTokens ?? 0) : '—'}</TableCell>
                <TableCell align="right">
                  {requests > 0 ? fmtInt(s?.completionTokens ?? 0) : '—'}
                </TableCell>
                <TableCell align="right">
                  {cacheReports > 0 ? (
                    <Tooltip
                      title={`${fmtInt(s?.cachedTokens ?? 0)} of this model's prompt tokens came from cache instead of being reprocessed — ${((s?.cacheHitRate ?? 0) * 100).toFixed(1)}% of them, over ${fmtInt(cacheReports)} requests that hit.`}
                    >
                      <span style={{ color: (s?.cacheHitRate ?? 0) >= 0.5 ? C.ok : undefined }}>
                        {fmtInt(s?.cachedTokens ?? 0)}
                      </span>
                    </Tooltip>
                  ) : (
                    <Tooltip title="No request here reported cached tokens — either this backend has no prompt cache (embeddings, most remote providers) or it does not report one. Not a measured 0.">
                      <span style={{ color: C.textFaint }}>n/r</span>
                    </Tooltip>
                  )}
                </TableCell>
                <TableCell align="right">
                  {requests > 0 ? (
                    <>
                      {fmtDuration(Math.round(Number(s?.dwellMs ?? 0) / requests))}
                      <Typography variant="caption" sx={{ display: 'block', color: C.textFaint }}>
                        {fmtDuration(s?.dwellMs ?? 0)} total
                      </Typography>
                    </>
                  ) : (
                    <span style={{ color: C.textFaint }}>—</span>
                  )}
                </TableCell>
                <TableCell align="right">
                  {n > 0 ? (
                    <Tooltip
                      title={[
                        `${n} request${n === 1 ? '' : 's'} queued; longest ${fmtDuration(u?.maxWaitMs ?? 0)}.`,
                        live && est > 0
                          ? `The scheduler would tell a caller arriving now to wait ${fmtDuration(est)} — the number that goes out on Retry-After.`
                          : '',
                        Number(u?.serviceSamples ?? 0) > 0
                          ? `Request times vary ${cv.toFixed(2)}× their own average${cv >= 1 ? ', so the mean under-reads every wait estimate' : ''}.`
                          : '',
                        u?.pkSaturated
                          ? 'Arrivals outpaced service over the window — no finite steady-state wait exists.'
                          : pk > 0
                            ? `The measured distribution implies ${fmtDuration(pk)} (Pollaczek–Khinchine), a third opinion derived from neither of the other two.`
                            : '',
                      ]
                        .filter(Boolean)
                        .join(' ')}
                    >
                      {/* A mean over one or two samples is not a trend, and
                          reading it as one is how a fluke becomes a fact. */}
                      <span
                        style={{
                          color: n < 3 ? C.textFaint : cv >= 2 ? C.warn : undefined,
                        }}
                      >
                        {fmtDuration(real)}
                        {n < 3 ? ` (n=${n})` : ''}
                      </span>
                    </Tooltip>
                  ) : (
                    <Tooltip title="Nothing queued on this model in the window — every request was admitted straight away, or turned away outright.">
                      <span style={{ color: C.textFaint }}>none</span>
                    </Tooltip>
                  )}
                </TableCell>
                <TableCell align="right">{fmtUSD(s?.costUsd ?? 0)}</TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </TableContainer>
  )

  return (
    <Panel
      title="By model"
      subtitle={`What each model did ${windowPhrase(window)}, and what reaching it cost`}
      // The column glossary, which is prose and was clipping. Which sentence
      // depends on whether the window is the live one: "in use" is a reading of
      // this moment, so on a past window the column is blank for a reason the
      // reader would otherwise have to guess at (OB-9).
      note={
        live ? (
          <>
            tp/s reads, tg/s writes — &ldquo;n/r&rdquo; means no backend here reports timings,
            which is every remote provider.
          </>
        ) : (
          <>
            &ldquo;In use&rdquo; is blank: it describes this moment, and nothing recorded what it
            was then.
          </>
        )
      }
      badge={<Chip size="small" variant="outlined" label={`${rows.length} models`} />}
      flush
    >
      {/* The clash CALLOUT — the sentence and the button — is not here. It is
          the one fault on this page with a control attached, so it belongs in
          the warnings panel at the top, which reads it from this same query. The
          per-row chip stays, because that is the part that says WHICH model. */}
      {body}
    </Panel>
  )
}
