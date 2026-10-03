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
import { fmtDuration, fmtInt, fmtKwh, fmtUSD } from '@/format'
import { SuppressButton } from '@/WarningControls'

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

const SetMaxWaitDoc = graphql(/* GraphQL */ `
  mutation SetMaxWait($name: String!, $body: corrallm_SetModelMaxWaitInputBodyInput!) {
    corrallm {
      setModelMaxWait(name: $name, body: $body) {
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
          allowedWaitMs
          waitForDepthMs
        }
      }
      usageRollup(windowHours: $windowHours, from: $from, to: $to) {
        # The rate every energy figure below was computed at, and the paid type
        # the comparison prices against. Window-level, so they belong here and
        # not on each row.
        costPerKwh
        thermal {
          mode
          roomF
          outdoorF
          coolTargetF
          heatTargetF
          coolBlocked
          coolFactorNow
          coolFanCfm
        }
        baselineType
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
          # How full the context was: every turn, and each conversation's last.
          ctxRequests
          ctxRequestAvg
          ctxRequestP90
          ctxSessions
          ctxTailAvg
          ctxTailP90
          # What kind of money this row is, and what the same tokens would have
          # been billed at on a paid API.
          type
          billing
          energyUsd
          energyKwh
          paidUsd
          unattributedUsd
          # Free-tier traffic carries no dollars — that is what free means — so
          # the count is what says the $0 was measured, and mispricedUsd is what
          # the store had charged it before the kind existed.
          freeRequests
          mispricedUsd
          baselineUsd
          baselineCachedUsd
          baselineProcessedUsd
          baselineGeneratedUsd
          baselineExcluded
          # Which paid type priced THIS row: the model's costEquivalent, else the default.
          baselineType
          # Per-request speed and cache distributions, and the heat local work gave off.
          tgN
          tgAvg
          tgP10
          tgP90
          tpN
          tpAvg
          tpP10
          tpP90
          cacheN
          cacheAvg
          cacheP90
          energyBtu
          heatBtuPerHour
          thermalUsd
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
          energyUsd
          energyKwh
          paidUsd
          unattributedUsd
          freeRequests
          mispricedUsd
          baselineUsd
          ctxRequests
          ctxRequestAvg
          ctxRequestP90
          ctxSessions
          ctxTailAvg
          ctxTailP90
          tgN
          tgAvg
          tgP10
          tgP90
          tpN
          tpAvg
          tpP10
          tpP90
          cacheN
          cacheAvg
          cacheP90
          energyBtu
          heatBtuPerHour
          thermalUsd
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
  | 'served'
  | 'configuredDepth'
  | 'reachableDepth'
  | 'depthUnreachable'
  | 'allowedWaitMs'
  | 'waitForDepthMs'
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
 * TWO SETTINGS THAT CONTRADICT EACH OTHER, and the controls that end it.
 *
 * A queue clash is one model's arithmetic — reachable depth is capacity ×
 * maxWait / meanService — so both ways out are per model: lower THIS model's
 * queue limit to what its wait allows, or raise THIS model's wait to what its
 * limit needs. Neither touches the box-wide settings or any other model. The
 * wait used to be named here as a box-wide Setup change and left as a
 * sentence; a per-model maxWait override made it a button.
 *
 * Lowering the limit is not offered when the reachable depth is 0: a limit of
 * 0 means "inherit the box-wide limit", so the button would write the clash
 * straight back.
 */
export function QueueDepthClash({ rows }: { rows: readonly ClashRow[] }) {
  const qc = useQueryClient()
  const onSuccess = () => qc.invalidateQueries({ queryKey: ['activity'] })
  const setDepth = useMutation({
    mutationFn: (v: { name: string; depth: number }) =>
      // Long crosses the wire as a string in the generated types.
      gqlClient.request(SetQueueDepthDoc, { name: v.name, body: { maxQueueDepth: String(v.depth) } }),
    onSuccess,
  })
  const setWait = useMutation({
    mutationFn: (v: { name: string; waitMs: number }) =>
      gqlClient.request(SetMaxWaitDoc, {
        name: v.name,
        body: { maxWait: `${Math.round(v.waitMs / 1000)}s` },
      }),
    onSuccess,
  })
  if (rows.length === 0) return null
  const busy = setDepth.isPending || setWait.isPending
  const error = setDepth.error ?? setWait.error
  return (
    <Box>
      <Typography variant="body2" sx={{ color: C.warn }}>
        {rows.length === 1
          ? `${rows[0].served} has two settings that contradict each other.`
          : `${rows.length} models have two settings that contradict each other.`}{' '}
        A model&apos;s queue limit says how many callers may wait; its longest allowed wait says
        how long each may wait. When the wait runs out before the queue fills, callers time out
        instead of being told when to come back.
      </Typography>
      <Typography variant="body2" sx={{ color: C.textMuted, mt: 0.5 }}>
        Two fixes, each for that model only. <b>Lower the queue limit</b> to what the wait allows:
        the next caller past it is told to come back at a set time. <b>Raise the longest wait</b>{' '}
        to what the limit needs: every waiter fits, and each may wait up to the new time. Other
        models keep their settings. A request already running is unaffected. Both take effect at
        once.
      </Typography>
      {rows.map((r) => {
        const reach = Number(r.reachableDepth)
        const need = Number(r.waitForDepthMs)
        return (
          <Box key={r.served} sx={{ mt: 1 }}>
            <Typography variant="body2">
              <b>{r.served}</b>: limit {r.configuredDepth}, longest wait{' '}
              {Number(r.allowedWaitMs) > 0 ? fmtDuration(Number(r.allowedWaitMs)) : 'unbounded'}, which
              fits {reach} at the speed it is answering.
              {reach < 1 && ' It cannot hold even one waiter, so only a longer wait fixes it.'}
            </Typography>
            <Box sx={{ display: 'flex', gap: 1, alignItems: 'center', mt: 0.5, flexWrap: 'wrap' }}>
              {reach >= 1 && (
                <Button
                  size="small"
                  variant="outlined"
                  disabled={busy}
                  onClick={() => setDepth.mutate({ name: r.served, depth: reach })}
                >
                  {setDepth.isPending ? 'Setting…' : `Lower the queue limit to ${reach}`}
                </Button>
              )}
              {need > 0 && (
                <Button
                  size="small"
                  variant="outlined"
                  disabled={busy}
                  onClick={() => setWait.mutate({ name: r.served, waitMs: need })}
                >
                  {setWait.isPending ? 'Setting…' : `Raise the longest wait to ${fmtDuration(need)}`}
                </Button>
              )}
              <SuppressButton kind="queue-clash" subject={r.served} />
            </Box>
          </Box>
        )
      })}
      {error && (
        <Typography variant="body2" sx={{ color: C.warn, mt: 1 }}>
          That did not save: {String(error)}. Nothing changed.
        </Typography>
      )}
    </Box>
  )
}

/**
 * The word under a cost figure saying what kind of money it is.
 *
 * Electricity carries its kWh: a dollar figure derived from energy without the
 * energy is the derivation hidden, and the rate is in the header of the panel
 * this table sits under.
 */
function billingWord(billing: string, kwh: number, excluded: string): string {
  switch (billing) {
    case 'energy':
      // Below a tenth of a watt-hour fmtKwh prints "0.0 Wh", which reads as a
      // measured nothing rather than as a rounding. The word alone is truer.
      return kwh >= 0.0001 ? `${fmtKwh(kwh)} of power` : 'electricity'
    case 'paid':
      return 'paid per token'
    case 'free':
      // NOT "free" on its own. Under a $0 that word reads as "we got away with
      // it"; what a reader needs is WHOSE hardware did the work, because that is
      // the thing that is true of the row and the thing that could change.
      return 'free tier — not our hardware'
    case 'unattributed':
      // "Unattributed" answers "which kind of money"; for a model that has left
      // the config there is a more useful answer, and it names the fix.
      return excluded === 'unknown-model' ? 'model not in the config' : 'unattributed'
    default:
      return ''
  }
}

function billingHint(billing: string, type: string, rate: number, mispriced = 0): string {
  const t = type ? ` Backend type: ${type}.` : ' The model is no longer in the config.'
  switch (billing) {
    case 'energy':
      return `Electricity: token work → watt-hours → kWh${rate > 0 ? ` × $${rate}/kWh` : ''}. Cached prompt tokens cost nothing here — they come from the KV cache with no recompute.${t}`
    case 'paid':
      return `A provider's bill: cached, processed and generated tokens each at their own $/token rate.${t}`
    case 'free':
      // The fix these rows used to be told to make was "declare a paid type",
      // and for a free tier that would have been a lie in the other direction.
      // The hint says what the $0 rests on — a declaration in config — so a
      // reader can go and check it rather than trust the word.
      return (
        `$0 because the provider charges nothing, not because a rate is missing. This model is served off this box and its config declares a free tier (freeTier:), so there is no electricity of ours and no invoice of theirs.` +
        (mispriced > 0
          ? ` ${fmtUSD(mispriced)} had been recorded against it from this box's watt-hour coefficients; that is not charged here.`
          : '') +
        t
      )
    case 'unattributed':
      if (type === '') {
        return "This served name is not in the config any more, so nothing says what priced these rows or where they ran. They survive here because activity is retained for 30 days and a model can be removed inside that."
      }
      return `Neither electricity nor a bill. This model is served by a machine corrallm does not run, its type declares no per-token rates, and nothing says it is free either — so the figure is this box's watt-hour coefficients applied to somebody else's hardware. Say what it charges: a type with cachedCostFactor / processCostFactor / generateCostFactor if it bills, or freeTier: on the model if it does not.${t}`
    default:
      return t.trim()
  }
}

export function ModelStats({ window = DEFAULT_WINDOW }: { window?: TimeWindow }) {
  const q = useModelStats(window)

  const util = q.data?.corrallm.utilization?.rows ?? []
  const spend = q.data?.corrallm.usageRollup?.rows ?? []
  const rows = joinRows(util, spend)
  const rate = q.data?.corrallm.usageRollup?.costPerKwh ?? 0

  // TWO OF THESE COLUMNS ARE LIVE, WHATEVER THE WINDOW SAYS. `In use` and the
  // est. wait behind the Wait tooltip come from the scheduler's CURRENT
  // snapshot — there is no historical record of a slot's occupancy or of an
  // estimate that was offered. Under a frozen window they were still drawn, so a
  // page headed "a fixed span — these numbers will not change" showed an est.
  // wait identical to the live page's, to the tenth of a second. A false number
  // is worse than a missing one (OB-9), so on a fixed span they say so instead.
  const live = window.kind !== 'absolute'

  const body = q.isLoading ? (
    // A sentence, not a spinner. A spinner says "wait"; this says what is being
    // waited on and that the emptiness above it is not a reading — which is the
    // half a reader takes away if they stop here.
    <Box sx={{ p: 2 }}>
      <Typography variant="body2" sx={{ color: C.textFaint }}>
        Asking this box what each model did {windowPhrase(window)} — nothing has been counted yet.
      </Typography>
    </Box>
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
            {/* CACHED · PROCESSED · GENERATED, in the order the work happens.
                Cached used to sit third, AFTER a column headed "Processed" that
                carried promptTokens — which includes the cached prefix, so the
                two columns overlapped and the header said otherwise. */}
            <TableCell align="right">
              <Tooltip title="Prompt tokens served from cache instead of reprocessed, and their share of this model's prompt. 'n/r' means nothing here reported a cache — not a measured zero.">
                <span>Cached</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Prompt tokens the backend really recomputed: prompt minus cached. The prompt total counts the cached prefix too, so this is the one that measures work.">
                <span>Processed</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Completion tokens generated — what it wrote back.">
                <span>Generated</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Context tokens (prompt + reply) each chat turn carried, averaged over every turn, with the 90th percentile under it. Every turn re-sends the whole conversation, so one turn's size IS the conversation's size at that point — and a long conversation counts once per turn.">
                <span>Context / turn</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Context tokens at each conversation's LAST turn in the window, averaged over conversations, with the 90th percentile under it. Each turn overwrites the one before, so a conversation counts once. A conversation whose message count drops — restarted, or its history compacted — counts as a new one.">
                <span>Context / conversation end</span>
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
            <TableCell align="right">
              <Tooltip title="Heat this model's local work gave off: its metered electricity × 3,412 BTU/kWh, with the window's average BTU/h under it. Token energy only — idle draw, CPU and PSU losses are not counted, so real heat is higher. A dash for a model that ran somewhere else.">
                <span>Heat</span>
              </Tooltip>
            </TableCell>
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
                    <Tooltip title="A fallback list, not one backend: its models are tried top to bottom, and the load here is theirs counted once. /machines shows what it is made of. The config file calls this a lane.">
                      <Chip size="small" variant="outlined" label="fallback list" sx={{ ml: 1 }} />
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
                <TableCell align="right">
                  {cacheReports > 0 ? (
                    <Tooltip
                      title={`${fmtInt(s?.cachedTokens ?? 0)} of this model's prompt tokens came from cache instead of being reprocessed — ${((s?.cacheHitRate ?? 0) * 100).toFixed(1)}% of them, over ${fmtInt(cacheReports)} requests that hit.`}
                    >
                      <span style={{ color: (s?.cacheHitRate ?? 0) >= 0.5 ? C.ok : undefined }}>
                        {fmtInt(s?.cachedTokens ?? 0)}
                        {/* THE HIT RATE, ON THE ROW. Whether prefix caching is
                            working is the question a cache column is asked, and
                            cachedTokens alone cannot answer it — 47M means
                            nothing without the prompt it is a share of. It rides
                            the cell it qualifies rather than a column of its own
                            (the table is already thirteen wide). */}
                        <Typography
                          variant="caption"
                          sx={{ display: 'block', color: C.textFaint }}
                        >
                          {((s?.cacheHitRate ?? 0) * 100).toFixed(0)}% of prompt
                        </Typography>
                      </span>
                    </Tooltip>
                  ) : (
                    <Tooltip title="No request here reported cached tokens — either this backend has no prompt cache (embeddings, most remote providers) or it does not report one. Not a measured 0.">
                      <span style={{ color: C.textFaint }}>n/r</span>
                    </Tooltip>
                  )}
                </TableCell>
                <TableCell align="right">
                  {requests > 0
                    ? fmtInt(
                        Math.max(0, Number(s?.promptTokens ?? 0) - Number(s?.cachedTokens ?? 0)),
                      )
                    : '—'}
                </TableCell>
                <TableCell align="right">
                  {requests > 0 ? fmtInt(s?.completionTokens ?? 0) : '—'}
                </TableCell>
                <TableCell align="right">
                  <CtxCell
                    avg={Number(s?.ctxRequestAvg ?? 0)}
                    p90={Number(s?.ctxRequestP90 ?? 0)}
                    n={Number(s?.ctxRequests ?? 0)}
                    noun="turn"
                  />
                </TableCell>
                <TableCell align="right">
                  <CtxCell
                    avg={Number(s?.ctxTailAvg ?? 0)}
                    p90={Number(s?.ctxTailP90 ?? 0)}
                    n={Number(s?.ctxSessions ?? 0)}
                    noun="conversation"
                  />
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
                {/* THE COST, AND WHAT KIND OF COST IT IS. Per model the answer is
                    one word, so it goes under the figure rather than into a
                    tooltip (OB-10): electricity is kWh this fleet paid for, paid
                    is a provider's invoice, free tier is somebody else's hardware
                    at no charge, and unattributed is none of the three. */}
                <TableCell align="right">
                  <Tooltip
                    title={billingHint(
                      s?.billing ?? '',
                      s?.type ?? '',
                      rate,
                      Number(s?.mispricedUsd ?? 0),
                    )}
                  >
                    <span>
                      {fmtUSD(s?.costUsd ?? 0)}
                      <Typography variant="caption" sx={{ display: 'block', color: C.textFaint }}>
                        {billingWord(
                          s?.billing ?? '',
                          Number(s?.energyKwh ?? 0),
                          s?.baselineExcluded ?? '',
                        )}
                      </Typography>
                    </span>
                  </Tooltip>
                </TableCell>
                {/* HEAT, in the room this box is in. Only energy-billed rows have
                    any: a paid or remote backend heated somebody else's room. */}
                <TableCell align="right">
                  {Number(s?.energyBtu ?? 0) > 0 ? (
                    <>
                      {fmtInt(Math.round(Number(s?.energyBtu)))} BTU
                      {Number(s?.heatBtuPerHour ?? 0) > 0 && (
                        <Typography variant="caption" sx={{ display: 'block', color: C.textFaint }}>
                          ≈ {fmtInt(Math.round(Number(s?.heatBtuPerHour)))} BTU/h
                        </Typography>
                      )}
                      {Number(s?.thermalUsd ?? 0) !== 0 && (
                        <Typography variant="caption" sx={{ display: 'block', color: C.textFaint }}>
                          {Number(s?.thermalUsd) > 0
                            ? `+${fmtUSD(Number(s?.thermalUsd))} cooling`
                            : `${fmtUSD(-Number(s?.thermalUsd))} heating saved`}
                        </Typography>
                      )}
                    </>
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
      title="By model"
      subtitle={`What each model did ${windowPhrase(window)}, and what reaching it cost`}
      // The column glossary, which is prose and was clipping. Which sentence
      // depends on whether the window is the live one: "in use" is a reading of
      // this moment, so on a past window the column is blank for a reason the
      // reader would otherwise have to guess at (OB-9).
      note={
        <>
          {live ? (
            <>
              tp/s reads, tg/s writes — &ldquo;n/r&rdquo; means no backend here reports timings,
              which is every remote provider.
            </>
          ) : (
            <>
              &ldquo;In use&rdquo; is blank: it describes this moment, and nothing recorded what
              it was then.
            </>
          )}{' '}
          {/* THE RATE, ONCE, FOR THE WHOLE COLUMN. Every "electricity" figure in
              the Cost column is kWh × this, and thirteen rows each repeating it
              would be thirteen copies of one fact. Said here rather than in a
              tooltip because a cost whose rate is invisible cannot be checked. */}
          {rate > 0 ? (
            <>Electricity is billed at ${rate}/kWh.</>
          ) : (
            <>No $/kWh is configured, so every electricity figure is $0 by omission.</>
          )}
        </>
      }
      // "0 models" while the query is still out is the same lie the tiles above
      // used to tell: the count has not arrived, and a badge is read before the
      // body that would have explained it. The body says which it is.
      badge={
        <Chip
          size="small"
          variant="outlined"
          label={q.data ? `${rows.length} models` : 'not in yet'}
        />
      }
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

/** Thousands of tokens, one decimal: "35.8k tokens" reads at a glance where "35,821" does not. */
function fmtKTokens(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(1)}k tokens` : `${Math.round(n)} tokens`
}

/**
 * One context reading: the mean, the p90 under it, and how many it is over.
 * A dash when nothing in the window named a conversation — embeddings, audio,
 * or a backend whose requests carry no messages.
 */
function CtxCell({ avg, p90, n, noun }: { avg: number; p90: number; n: number; noun: string }) {
  if (n === 0) {
    return (
      <Tooltip title="No chat turn here named a conversation in this window — embeddings, audio, or requests with no messages.">
        <span style={{ color: C.textFaint }}>—</span>
      </Tooltip>
    )
  }
  return (
    <Tooltip
      title={`Over ${fmtInt(n)} ${noun}${n === 1 ? '' : 's'}: mean ${fmtInt(Math.round(avg))} tokens, 90th percentile ${fmtInt(p90)} tokens.`}
    >
      <span>
        {fmtKTokens(avg)} avg
        <Typography variant="caption" sx={{ display: 'block', color: C.textFaint }}>
          p90 {fmtKTokens(p90)}
        </Typography>
      </span>
    </Tooltip>
  )
}
