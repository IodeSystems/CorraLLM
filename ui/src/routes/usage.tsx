import type { ReactNode } from 'react'
import { createFileRoute, redirect } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { DEFAULT_WINDOW, windowKey, windowPhrase, windowVars, type TimeWindow } from '@/TimeWindow'
import {
  Box,
  Stack,
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
import { MetricChart, StackedArea } from '@/Charts'
import type { ChartSeries } from '@/Charts'
import { C, seriesColor } from '@/theme'
import { fmtDuration, fmtInt, fmtKwh, fmtUSD } from '@/format'
import { Loading } from '@/Loading'
import { NotYet } from '@/NotYet'
import { useModelStats } from '@/ModelStats'
import { PaidComparison, type ComparisonRow } from '@/PaidComparison'

// BarCell renders a value with a proportional background bar (value / columnMax).
function BarCell({ value, max, label }: { value: number; max: number; label: string }) {
  const pct = max > 0 ? Math.max(2, (value / max) * 100) : 0
  return (
    <TableCell align="right" sx={{ position: 'relative', minWidth: 110 }}>
      <Box
        sx={{
          position: 'absolute',
          inset: 0,
          width: `${pct}%`,
          ml: 'auto',
          bgcolor: 'primary.main',
          opacity: 0.16,
          borderRadius: 0.5,
        }}
      />
      <Box sx={{ position: 'relative' }}>{label}</Box>
    </TableCell>
  )
}

// A headline number is its own kind of panel: the label IS the panel title, so
// the tile carries one value and nothing competing with it.
//
// `value` is a node, not a string, because half of what a tile has to say is not
// a number: `n/r` when the backend reported none, `not in yet` when this box has
// not answered. Both carry a tooltip of their own (see NotYet.tsx).
function StatTile({
  label,
  value,
  sub,
  hint,
}: {
  label: string
  value: ReactNode
  sub?: string
  hint?: string
}) {
  const body = (
    <Panel title={label} dense>
      <Typography variant="h5" sx={{ fontVariantNumeric: 'tabular-nums', fontWeight: 600 }}>
        {value}
      </Typography>
      {sub && (
        <Typography variant="caption" sx={{ color: C.textMuted }}>
          {sub}
        </Typography>
      )}
    </Panel>
  )
  return (
    <Box sx={{ minWidth: 160, flex: '1 1 160px' }}>
      {hint ? <Tooltip title={hint}>{body}</Tooltip> : body}
    </Box>
  )
}

type PrefillRow = {
  prefillN: unknown
  prefillFusedTps: number
  prefillRawTps: number
  prefillSecAvg: number
  prefillBuckets: readonly { label: string; n: unknown; rawTps: number; secAvg: number; newTokensAvg: number }[]
}

/**
 * The hover breakdown behind a model's prefill figure. Raw prefill by how many
 * NEW tokens each request had: the small buckets read slow because a fixed
 * per-request overhead dominates them, which is easy to mistake for a
 * regression when caching has actually cut the work.
 */
function PrefillBreakdown({ row }: { row: PrefillRow }) {
  const cell = { fontFamily: 'monospace', fontSize: 12, textAlign: 'right' as const, pl: 1.5 }
  return (
    <Box sx={{ p: 0.5 }}>
      <Typography variant="caption" sx={{ display: 'block', mb: 0.5 }}>
        Fused counts the cached prefix; raw only the new tokens processed. Both are total tokens over total
        prefill time ({fmtInt(Number(row.prefillN))} requests, {row.prefillSecAvg.toFixed(2)} s each on average).
      </Typography>
      <Box component="table" sx={{ borderCollapse: 'collapse' }}>
        <thead>
          <tr>
            <Box component="th" sx={{ ...cell, textAlign: 'left', pl: 0 }}>new tokens</Box>
            <Box component="th" sx={cell}>raw tok/s</Box>
            <Box component="th" sx={cell}>s / request</Box>
            <Box component="th" sx={cell}>n</Box>
          </tr>
        </thead>
        <tbody>
          {row.prefillBuckets.map((b) => (
            <tr key={b.label}>
              <Box component="td" sx={{ ...cell, textAlign: 'left', pl: 0 }}>{b.label}</Box>
              <Box component="td" sx={cell}>{Number(b.n) > 0 ? fmtRate(b.rawTps) : '—'}</Box>
              <Box component="td" sx={cell}>{Number(b.n) > 0 ? b.secAvg.toFixed(2) : '—'}</Box>
              <Box component="td" sx={cell}>{fmtInt(Number(b.n))}</Box>
            </tr>
          ))}
        </tbody>
      </Box>
      <Typography variant="caption" sx={{ display: 'block', mt: 0.5 }}>
        Small chunks are slow per token because of a fixed per-request overhead, not the GPU.
      </Typography>
    </Box>
  )
}

/**
 * A cache hit rate, rendered so "we never heard" cannot be mistaken for "0%".
 *
 * `cacheReports` counts requests that reported ANY hit. At zero the backend has
 * no prompt cache or does not report one — embeddings and most remote providers
 * — and printing a measured-looking 0% there invents a caching problem that does
 * not exist. It also cannot be separated from a cache that genuinely never hits,
 * which the tooltip says outright rather than papering over.
 */
function CacheCell({
  rate,
  reports,
  cached,
}: {
  rate: number
  reports: number
  cached: number
}) {
  if (!reports) {
    return (
      <TableCell align="right">
        <Tooltip title="No request here reported cached tokens — either this backend has no prompt cache (embeddings, most remote providers) or it does not report one. Not a measured 0%.">
          <span style={{ color: C.textFaint }}>n/r</span>
        </Tooltip>
      </TableCell>
    )
  }
  return (
    <TableCell align="right">
      <Tooltip title={`${fmtInt(cached)} prompt tokens served from cache instead of reprocessed, across ${fmtInt(reports)} requests that hit`}>
        <span style={{ color: rate >= 0.5 ? C.ok : rate > 0 ? undefined : C.warn }}>
          {(rate * 100).toFixed(1)}%
        </span>
      </Tooltip>
    </TableCell>
  )
}

// fmtDuration tops out at minutes, which is right for a request but unreadable
// for an accumulated total — "1282m 34s" is a number you have to do arithmetic
// on before it means anything. Headline savings get hours.
function fmtLongDuration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ${m % 60}m`
  return `${Math.floor(h / 24)}d ${h % 24}h`
}

const UsageDoc = graphql(/* GraphQL */ `
  query Usage($windowHours: Long!, $bucketMinutes: Long!, $from: Long, $to: Long) {
    corrallm {
      usageSeriesByGroup(windowHours: $windowHours, bucketMinutes: $bucketMinutes, from: $from, to: $to) {
        buckets
        groups {
          group
          points {
            requests
            costUsd
            dwellMs
            rejected
            queuedMs
          }
        }
      }
      usageSeries(windowHours: $windowHours, bucketMinutes: $bucketMinutes, from: $from, to: $to) {
        bucketMinutes
        buckets
        keys {
          key
          points {
            requests
            costUsd
            energyKwh
            dwellMs
          }
        }
      }
      usageByKey(windowHours: $windowHours, from: $from, to: $to) {
        rows {
          key
          requests
          promptTokens
          completionTokens
          dwellMs
          costUsd
          energyKwh
          cachedTokens
          cacheReports
          cacheHitRate
        }
      }
    }
  }
`)

// queueDepth is its OWN query because it is the only thing on the page nobody
// asked for. It samples the scheduler on a timer, it answers a queueing question
// rather than a traffic one, and it lives behind the collapsed detail section —
// so it must not be paid for on first paint. Splitting it out is a third of what
// "this page takes forever to load" was.
const QueueDepthDoc = graphql(/* GraphQL */ `
  query QueueDepthSeries($windowHours: Long!, $bucketMinutes: Long!, $from: Long, $to: Long) {
    corrallm {
      queueDepth(windowHours: $windowHours, bucketMinutes: $bucketMinutes, from: $from, to: $to) {
        buckets
        lanes {
          group
          points {
            avgWaiting
            maxWaiting
          }
        }
      }
    }
  }
`)

// The bucket follows the span, so a one-hour window is not one bar.
function bucketMinutesFor(window: TimeWindow): string {
  const spanMS = window.kind === 'absolute' ? window.toMS - window.fromMS : window.minutes * 60_000
  const hours = spanMS / 3_600_000
  return String(hours <= 6 ? 10 : hours <= 24 ? 60 : 240)
}

/**
 * The one usage fetch, shared by every panel on this page.
 *
 * React Query dedupes on the key, so the headline charts, the caller table and
 * the collapsed extras are one request between them. Two panels under the same
 * window control must not be able to disagree about it — which is the whole
 * reason the page has one window in the first place.
 */
function useUsage(window: TimeWindow) {
  const vars = windowVars(window)
  return useQuery({
    queryKey: ['usage', windowKey(window)],
    queryFn: () =>
      gqlClient.request(UsageDoc, {
        windowHours: vars.windowHours,
        bucketMinutes: bucketMinutesFor(window),
        from: vars.from,
        to: vars.to,
      }),
    // fallback; live updates arrive via SSE (useLiveEvents). A frozen window has
    // nothing new to fetch.
    refetchInterval: window.kind === 'absolute' ? false : 15000,
  })
}

// fmtKwh moved to format.ts when the Now page and the paid-API comparison
// needed the same rule. One copy, so an hour's energy reads identically on
// three screens.

const groupColor = (i: number) => seriesColor(i)

/**
 * THE HEADLINE: five totals and two charts, and that is deliberately all.
 *
 * The page used to open with four tiles and then EIGHT charts, which is not a
 * header — it is a gallery, and the active sessions and the request log were
 * under all of it. Two charts answer the questions a header is for: how much
 * traffic there was, and who was spending. The other six were not wrong, they
 * were just not first; they are in the collapsed detail at the foot of the page.
 *
 * The tiles are the ones the report named: tokens cached, processed, generated —
 * plus the request count and the bill. There is no tg/s or tp/s tile, and that
 * is on purpose: a rate averaged across models is not a rate, which is why the
 * schema pins both to 0 on the grand total. They are per-model columns.
 */
/** Tokens per second, at the precision a reader can use. */
function fmtRate(n: number): string {
  return n >= 100 ? Math.round(n).toString() : n.toFixed(1)
}
/** Thousands of tokens, one decimal. */
function fmtK(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(1)}k` : `${Math.round(n)}`
}
const pct = (x: number) => `${(x * 100).toFixed(1)}%`

/**
 * SPEED AND CACHE, AS DISTRIBUTIONS (P47).
 *
 * The By-model table carries per-model MEANS, and the rollup refuses to sum a
 * rate across models, so the page had no global speed figure and no tail at
 * all. These are per-request distributions computed over every request in the
 * window: the mean is what a caller typically saw, p10 is the slow tail — where
 * a slowdown shows first — and p90 the fast one. Cache and context are about
 * how much, so they carry the mean and p90.
 *
 * Per model only when more than one model served: with one, the table would
 * repeat the tiles row for row.
 */
export function SpeedCacheHeadline({ window = DEFAULT_WINDOW }: { window?: TimeWindow }) {
  const q = useModelStats(window)
  const roll = q.data?.corrallm.usageRollup
  const total = roll?.total
  const rows = (roll?.rows ?? []).filter(
    (r) => Number(r.tgN) > 0 || Number(r.cacheN) > 0 || Number(r.ctxRequests) > 0,
  )
  const counted = total != null
  const speedTile = (label: string, n: number, avg: number, p10: number, p90: number, hint: string) => (
    <StatTile
      label={label}
      value={!counted ? <NotYet /> : n > 0 ? `${fmtRate(avg)} tok/s` : 'n/r'}
      sub={
        counted && n > 0
          ? `avg · slowest 10% under ${fmtRate(p10)} · fastest 10% over ${fmtRate(p90)} tok/s · ${fmtInt(n)} requests`
          : counted
            ? 'no backend reported timings'
            : undefined
      }
      hint={hint}
    />
  )
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
        {speedTile(
          'Decode (writing)',
          Number(total?.tgN ?? 0),
          Number(total?.tgAvg ?? 0),
          Number(total?.tgP10 ?? 0),
          Number(total?.tgP90 ?? 0),
          'Generation speed the backend reported, tokens/sec, per request. The mean is what a caller typically saw; the slowest-10% mark is where a slowdown shows first. Long contexts decode slower, so this moves with how much each request carried.',
        )}
        <StatTile
          label="Prefill, fused (cache included)"
          value={!counted ? <NotYet /> : Number(total?.prefillN ?? 0) > 0 ? `${fmtRate(Number(total?.prefillFusedTps))} tok/s` : 'n/r'}
          sub={
            counted && Number(total?.prefillN ?? 0) > 0
              ? `raw ${fmtRate(Number(total?.prefillRawTps))} tok/s new tokens · ${Number(total?.prefillSecAvg).toFixed(2)} s prefill per request · ${pct(Number(total?.cacheHitRate ?? 0))} of prompt tokens cached · ${fmtInt(Number(total?.prefillN))} requests`
              : counted
                ? 'no backend reported timings'
                : undefined
          }
          hint="Prompt tokens INCLUDING the cached prefix, divided by the time spent in prefill — how fast a caller's whole prompt was effectively read. Raw is the same time over only the tokens the GPU processed. Both are token-weighted: total tokens over total seconds, not a mean of per-request rates (that mean is dominated by cache hits reading a few hundred tokens behind a fixed per-request overhead)."
        />
        <StatTile
          label="Prefill, raw by new tokens"
          value={
            !counted ? (
              <NotYet />
            ) : Number(total?.prefillN ?? 0) > 0 ? (
              <Box component="span" sx={{ fontSize: 13, fontFamily: 'monospace', lineHeight: 1.5, display: 'block' }}>
                {(total?.prefillBuckets ?? []).map((b) => (
                  <Box component="span" key={b.label} sx={{ display: 'block' }}>
                    {b.label.padEnd(7)} {Number(b.n) > 0 ? `${fmtRate(b.rawTps)} tok/s  n ${fmtInt(Number(b.n))}` : '—'}
                  </Box>
                ))}
              </Box>
            ) : (
              'n/r'
            )
          }
          hint={`Raw prefill split by how many NEW (uncached) tokens each request had; n is the number of requests. Small chunks are dominated by fixed per-request overhead (context-checkpoint copies on this hybrid model); large chunks show what the GPU can do. Prefill time per request: ${(total?.prefillBuckets ?? []).map((b) => `${b.label} ${Number(b.n) > 0 ? b.secAvg.toFixed(2) + ' s' : '—'}`).join(', ')}.`}
        />
        <StatTile
          label="Cache hit"
          value={!counted ? <NotYet /> : Number(total?.cacheN ?? 0) > 0 ? `${pct(Number(total?.cacheAvg))} avg` : 'n/r'}
          sub={
            counted && Number(total?.cacheN ?? 0) > 0
              ? `per request · p90 ${pct(Number(total?.cacheP90))} · ${pct(Number(total?.cacheHitRate ?? 0))} of all prompt tokens`
              : counted
                ? 'nothing reported a cache'
                : undefined
          }
          hint="Share of each request's prompt served from the KV cache instead of reprocessed, averaged over requests; the last figure weights by tokens instead. Only models that reported any cache hit are counted, so a backend without a prompt cache does not read as 0%."
        />
        <StatTile
          label="Context / turn"
          value={!counted ? <NotYet /> : Number(total?.ctxRequests ?? 0) > 0 ? `${fmtK(Number(total?.ctxRequestAvg))} tokens avg` : '—'}
          sub={
            counted && Number(total?.ctxRequests ?? 0) > 0
              ? `p90 ${fmtK(Number(total?.ctxRequestP90))} tokens · conversation end ${fmtK(Number(total?.ctxTailAvg))} avg, p90 ${fmtK(Number(total?.ctxTailP90))} · ${fmtInt(Number(total?.ctxSessions))} conversations, ${(total?.turnsAvg ?? 0).toFixed(1)} turns avg (p90 ${fmtInt(Number(total?.turnsP90))}, max ${fmtInt(Number(total?.turnsMax))}), ${pct(Number(total?.ctxSessions) > 0 ? Number(total?.singleTurnSessions) / Number(total?.ctxSessions) : 0)} single-turn`
              : undefined
          }
          hint="Prompt + reply tokens per chat turn (each turn re-sends the conversation, so this IS the conversation's size then), and at each conversation's last turn. Turns: served requests per conversation; single-turn conversations never came back, the rest are repeat callers. A conversation whose message count drops (restarted or compacted) counts as a new one."
        />
      </Stack>
      {rows.length > 1 && (
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Model</TableCell>
                <TableCell align="right">Decode tok/s avg (p10 – p90)</TableCell>
                <TableCell align="right">Prefill tok/s, fused / raw</TableCell>
                <TableCell align="right">Cache hit avg (p90)</TableCell>
                <TableCell align="right">Context / turn avg (p90), tokens</TableCell>
                <TableCell align="right">Turns / conversation avg (p90, max)</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {rows.map((r) => (
                <TableRow key={r.served}>
                  <TableCell>{r.served}</TableCell>
                  <TableCell align="right">
                    {Number(r.tgN) > 0 ? `${fmtRate(Number(r.tgAvg))} (${fmtRate(Number(r.tgP10))} – ${fmtRate(Number(r.tgP90))})` : 'n/r'}
                  </TableCell>
                  <TableCell align="right">
                    {Number(r.prefillN) > 0 ? (
                      <Tooltip title={<PrefillBreakdown row={r} />} placement="left">
                        <Box component="span" sx={{ borderBottom: `1px dotted ${C.textMuted}`, cursor: 'help' }}>
                          {fmtRate(Number(r.prefillFusedTps))} / {fmtRate(Number(r.prefillRawTps))}
                        </Box>
                      </Tooltip>
                    ) : (
                      'n/r'
                    )}
                  </TableCell>
                  <TableCell align="right">
                    {Number(r.cacheN) > 0 ? `${pct(Number(r.cacheAvg))} (${pct(Number(r.cacheP90))})` : 'n/r'}
                  </TableCell>
                  <TableCell align="right">
                    {Number(r.ctxRequests) > 0 ? `${fmtK(Number(r.ctxRequestAvg))} (${fmtK(Number(r.ctxRequestP90))})` : '—'}
                  </TableCell>
                  <TableCell align="right">
                    {Number(r.ctxSessions) > 0
                      ? `${r.turnsAvg.toFixed(1)} (${fmtInt(Number(r.turnsP90))}, ${fmtInt(Number(r.turnsMax))}) · ${pct(Number(r.singleTurnSessions) / Number(r.ctxSessions))} single`
                      : '—'}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      )}
    </Box>
  )
}

export function TrafficHeadline({ window = DEFAULT_WINDOW }: { window?: TimeWindow }) {
  const q = useUsage(window)
  // Totals ride the By-model query rather than a usageRollup of our own: same
  // fetch, so a tile and the table under it cannot drift apart mid-window.
  const stats = useModelStats(window)
  const total = stats.data?.corrallm.usageRollup?.total

  const groupBuckets = (q.data?.corrallm.usageSeriesByGroup?.buckets ?? []).map(Number)
  const keyBuckets = (q.data?.corrallm.usageSeries?.buckets ?? []).map(Number)
  const pgroups = q.data?.corrallm.usageSeriesByGroup?.groups ?? []
  const groupSeries: ChartSeries[] = pgroups.map((g, i) => ({
    key: g.group,
    color: groupColor(i),
    values: g.points.map((p) => Number(p.requests)),
  }))
  const seriesKeys = q.data?.corrallm.usageSeries?.keys ?? []
  const costSeries: ChartSeries[] = seriesKeys.map((k, i) => ({
    key: k.key,
    color: seriesColor(i),
    values: k.points.map((p) => p.costUsd),
  }))

  const reports = Number(total?.cacheReports ?? 0)
  const rate = stats.data?.corrallm.usageRollup?.costPerKwh ?? 0
  const baselineType = stats.data?.corrallm.usageRollup?.baselineType ?? ''
  // processed = prompt − cached. The store records prompt_tokens INCLUDING the
  // cached prefix, so this is the only figure that says what was recomputed.
  const processedTokens = Math.max(
    0,
    Number(total?.promptTokens ?? 0) - Number(total?.cachedTokens ?? 0),
  )
  const comparisonRows: ComparisonRow[] = (stats.data?.corrallm.usageRollup?.rows ?? []).map(
    (r) => ({
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
    }),
  )
  // NOTHING HAS BEEN COUNTED UNTIL THE TOTAL ARRIVES. `?? 0` on these five put a
  // measured-looking zero on every tile for as long as the query took — six
  // seconds on a box busy enough to be worth opening the page for — and the
  // strip is the part of the page that gets read. The Cached tile already knew
  // the difference; the other four now say it the same way (NotYet.tsx).
  //
  // `total == null` covers a query still in flight AND one that failed, and the
  // sentence behind the token is true of both: this box has not returned a
  // figure. A failure also says so in words under the tiles.
  const counted = total != null

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
      <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
        <StatTile label="Requests" value={counted ? fmtInt(total.requests) : <NotYet />} />
        {/* CACHED · PROCESSED · GENERATED, in that order, because that is the
            order the work happens in and the vocabulary every other layer
            already speaks. The Cached tile used to come THIRD and Processed
            carried promptTokens — which INCLUDES the cached prefix, so the tile
            headed "Processed" was the sum of the tokens processed and the tokens
            deliberately not processed. Processed is now prompt − cached, the
            same subtraction the Now page's re-read figure uses. */}
        <StatTile
          label="Cached"
          value={!counted ? <NotYet /> : reports > 0 ? fmtInt(total.cachedTokens) : 'n/r'}
          sub={
            !counted
              ? undefined
              : reports > 0
                ? `${(total.cacheHitRate * 100).toFixed(1)}% of prompt, ~${fmtLongDuration(
                    total.cachedSecondsSaved * 1000,
                  )} saved`
                : 'nothing reported a cache'
          }
          hint="Prompt tokens the backends served from cache instead of reprocessing, and their share of all prompt tokens — weighted by tokens across every model, not an average of per-model rates. 'n/r' means nothing reported a cache at all, which is not a measured zero. The saved figure estimates the prompt-processing time avoided at the observed tokens/sec, so it moves with batch size and context."
        />
        <StatTile
          label="Processed"
          value={counted ? fmtInt(processedTokens) : <NotYet />}
          sub="prompt tokens actually read"
          hint="Prompt tokens the backends really recomputed: promptTokens minus the cached prefix. The prompt total itself counts the cached tokens too, so this is the one of the two that measures work done."
        />
        <StatTile
          label="Generated"
          value={counted ? fmtInt(total.completionTokens) : <NotYet />}
          sub="completion tokens written"
        />
        {/* ELECTRICITY AND A PROVIDER'S BILL, NOT ONE "COST". The fleet is mixed:
            local backends bill kWh × $/kWh and the anthropic types bill per
            token, and one figure over both is neither. */}
        <StatTile
          label="Electricity"
          value={counted ? fmtUSD(total.energyUsd) : <NotYet />}
          sub={
            rate > 0 ? `${fmtKwh(total?.energyKwh ?? 0)} at $${rate}/kWh` : 'no $/kWh configured'
          }
          hint="What local backends spent in energy: token work → watt-hours → kWh × the configured electricity rate. Cached prompt tokens contribute nothing here — they are served from the KV cache with no recompute."
        />
        <StatTile
          label="Paid APIs"
          value={counted ? fmtUSD(total.paidUsd) : <NotYet />}
          sub={
            !counted ? undefined : total.paidUsd > 0 ? 'billed per token' : 'nothing billed per token'
          }
          hint="What backends with a per-token rate table cost in this window. A counted $0 means no request went to one — not that the figure is missing."
        />
        {/* SOMEBODY ELSE'S HARDWARE, AT NO CHARGE — a third kind of money, and
            the only one whose figure is $0 on purpose. It needs the request
            count beside it: $0 over 7 requests is a reading, $0 over none is an
            empty category, and only the second should be off the page. Shown
            whenever there was free traffic, hidden when there was not. */}
        {counted && Number(total.freeRequests) > 0 && (
          <StatTile
            label="Free tier"
            value={fmtUSD(0)}
            sub={`${fmtInt(Number(total.freeRequests))} request(s) — not our hardware`}
            hint={`Requests served by a remote backend whose config declares a free tier (freeTier:) and whose type names no per-token rates. $0 is the price the provider charges, not a missing coefficient — which is what separates this from Unattributed.${
              Number(total.mispricedUsd) > 0
                ? ` ${fmtUSD(Number(total.mispricedUsd))} had been recorded against these rows from this box's watt-hour coefficients and is charged to no kind here; that figure shrinks to $0 as those rows age out of the 30-day window.`
                : ''
            } What this traffic WOULD have cost is in the comparison below — it is the traffic that question is most worth asking about.`}
          />
        )}
        {/* Only when nonzero: an energy-priced type on a model corrallm does not
            run, with nothing said about what it charges. Neither electricity paid
            here nor the provider's rate, and the fix is a config declaration,
            which the sub-label names (OB-6). */}
        {counted && total.unattributedUsd > 0 && (
          <StatTile
            label="Unattributed"
            value={fmtUSD(total.unattributedUsd)}
            sub="say what these cost"
            hint="Requests served by a machine corrallm does not run, priced with this box's watt-hour coefficients because their backend type declares no per-token rates and nothing declares them free. The figure is neither electricity anybody paid here nor the provider's bill. Fix it by saying what the backend charges: a type with cachedCostFactor / processCostFactor / generateCostFactor if it bills, or freeTier: on the model if it does not."
          />
        )}
      </Stack>

      {/* How fast and how much reuse: speed and cache distributions, right under
          the totals and above the comparison and the charts. */}
      <SpeedCacheHeadline window={window} />

      {/* The same comparison the home screen shows, over whatever window this
          page is set to. One component so the two cannot drift; the scope
          sentence and the baseline's name travel with it. */}
      <PaidComparison
        when={windowPhrase(window)}
        baselineType={baselineType}
        costPerKwh={rate}
        thermal={stats.data?.corrallm.usageRollup?.thermal ?? null}
        rows={comparisonRows}
        pending={!counted}
      />

      {/* A tile that says "not in yet" forever is a screen that has stopped
          updating without saying so (OB-9), and the fetch these five ride on
          had nowhere to report a failure. */}
      {stats.error && (
        <Typography variant="body2" sx={{ color: C.warn }}>
          The totals above did not arrive: {String(stats.error)}. Nothing was counted, and nothing
          above is a reading.
        </Typography>
      )}

      {q.isLoading ? (
        <Loading />
      ) : q.error ? (
        <Typography color="error">{String(q.error)}</Typography>
      ) : (
        <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
          {groupSeries.length === 0 ? (
            <Panel title="Requests over time" dense>
              <Typography variant="body2" sx={{ color: 'text.secondary' }}>
                Nothing ran {windowPhrase(window)}.
              </Typography>
            </Panel>
          ) : (
            <Box sx={{ flex: '1 1 380px', minWidth: 320 }}>
              <StackedArea
                title="Requests over time, by priority group"
                series={groupSeries}
                fmtTotal={fmtInt}
                buckets={groupBuckets}
                collapsibleLegend
              />
            </Box>
          )}
          {costSeries.length > 0 && (
            <MetricChart
              title="Cost over time, by caller"
              series={costSeries}
              fmt={fmtUSD}
              buckets={keyBuckets}
            />
          )}
        </Stack>
      )}
    </Box>
  )
}

/** Who spent it. One row per caller key, bars relative to the column max. */
export function CallerSpend({ window = DEFAULT_WINDOW }: { window?: TimeWindow }) {
  const q = useUsage(window)
  const byKey = q.data?.corrallm.usageByKey?.rows ?? []
  const kMax = {
    cost: Math.max(0, ...byKey.map((r) => r.costUsd)),
    req: Math.max(0, ...byKey.map((r) => Number(r.requests))),
    energy: Math.max(0, ...byKey.map((r) => r.energyKwh)),
    dwell: Math.max(0, ...byKey.map((r) => Number(r.dwellMs))),
  }
  return (
    <Panel title="By caller" subtitle="Bars are relative to the column max" flush>
      <TableContainer>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Key</TableCell>
              <TableCell align="right">Cost</TableCell>
              <TableCell align="right">Requests</TableCell>
              <TableCell align="right">Energy</TableCell>
              <TableCell align="right">Time</TableCell>
              <TableCell align="right">
                <Tooltip title="How much of this caller's prompt was served from cache — a property of how it prompts. A stable system prefix reuses cache; a shuffled one never does.">
                  <span>Cached</span>
                </Tooltip>
              </TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {byKey.length === 0 ? (
              <TableRow>
                <TableCell colSpan={6}>
                  <Typography sx={{ color: 'text.secondary' }}>
                    {q.isLoading ? 'Loading…' : 'No keyed usage in window.'}
                  </Typography>
                </TableCell>
              </TableRow>
            ) : (
              byKey.map((r) => (
                <TableRow key={r.key || '(unkeyed)'} hover>
                  <TableCell>{r.key || '(unkeyed)'}</TableCell>
                  <BarCell value={r.costUsd} max={kMax.cost} label={fmtUSD(r.costUsd)} />
                  <BarCell value={Number(r.requests)} max={kMax.req} label={fmtInt(r.requests)} />
                  <BarCell value={r.energyKwh} max={kMax.energy} label={fmtKwh(r.energyKwh)} />
                  <BarCell value={Number(r.dwellMs)} max={kMax.dwell} label={fmtDuration(r.dwellMs)} />
                  <CacheCell
                    rate={r.cacheHitRate}
                    reports={Number(r.cacheReports)}
                    cached={Number(r.cachedTokens)}
                  />
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </TableContainer>
    </Panel>
  )
}

/**
 * The six charts that used to be above the fold.
 *
 * Nothing here is deleted, and nothing here is first. Rejections, queue wait and
 * queue depth describe the scheduler's behaviour, not the traffic's; requests,
 * energy and time per caller are second cuts of the cost chart in the header.
 * They render only when somebody opens the section — which is also when the
 * queue-depth query is issued at all.
 */
export function MoreCharts({ window = DEFAULT_WINDOW }: { window?: TimeWindow }) {
  const q = useUsage(window)
  const vars = windowVars(window)
  const depth = useQuery({
    queryKey: ['usage', 'queueDepth', windowKey(window)],
    queryFn: () =>
      gqlClient.request(QueueDepthDoc, {
        windowHours: vars.windowHours,
        bucketMinutes: bucketMinutesFor(window),
        from: vars.from,
        to: vars.to,
      }),
    refetchInterval: window.kind === 'absolute' ? false : 15000,
  })

  if (q.isLoading) return <Loading />

  const groupBuckets = (q.data?.corrallm.usageSeriesByGroup?.buckets ?? []).map(Number)
  const keyBuckets = (q.data?.corrallm.usageSeries?.buckets ?? []).map(Number)
  const pgroups = q.data?.corrallm.usageSeriesByGroup?.groups ?? []

  const rejectSeries: ChartSeries[] = pgroups.map((g, i) => ({
    key: g.group,
    color: groupColor(i),
    values: g.points.map((p) => Number(p.rejected)),
  }))
  // Avg queue wait per request, per group, per bucket (ms).
  const waitSeries: ChartSeries[] = pgroups.map((g, i) => ({
    key: g.group,
    color: groupColor(i),
    values: g.points.map((p) => {
      const reqs = Number(p.requests)
      return reqs > 0 ? Number(p.queuedMs) / reqs : 0
    }),
  }))
  const anyRejections = rejectSeries.some((s) => s.values.some((v) => v > 0))

  const depthBuckets = (depth.data?.corrallm.queueDepth?.buckets ?? []).map(Number)
  const depthSeries: ChartSeries[] = (depth.data?.corrallm.queueDepth?.lanes ?? []).map((l, i) => ({
    key: l.group,
    color: groupColor(i),
    values: l.points.map((p) => p.avgWaiting),
  }))
  const anyDepth = depthSeries.some((s) => s.values.some((v) => v > 0))

  const seriesKeys = q.data?.corrallm.usageSeries?.keys ?? []
  const mkSeries = (
    sel: (p: { requests: string; costUsd: number; energyKwh: number; dwellMs: string }) => number,
  ): ChartSeries[] =>
    seriesKeys.map((k, i) => ({ key: k.key, color: seriesColor(i), values: k.points.map(sel) }))

  return (
    <Stack spacing={3}>
      <Box>
        <Typography variant="overline" sx={{ color: C.textMuted, display: 'block', mb: 1 }}>
          Priority groups
        </Typography>
        <Stack spacing={2}>
          {anyRejections ? (
            <StackedArea
              title="Queue pressure — 429s/bucket by group"
              series={rejectSeries}
              fmtTotal={fmtInt}
              buckets={groupBuckets}
            />
          ) : (
            <Typography variant="caption" sx={{ color: 'text.secondary' }}>
              No rejections in window — no group is being starved.
            </Typography>
          )}
          <MetricChart
            title="Avg queue wait / request"
            series={waitSeries}
            fmt={(n) => fmtDuration(n)}
            buckets={groupBuckets}
          />
          {depth.isLoading ? (
            <Loading size={20} minHeight={80} />
          ) : anyDepth ? (
            <StackedArea
              title="Queue depth — avg waiting by group (sampled)"
              series={depthSeries}
              fmtTotal={(n) => n.toFixed(1)}
              // Its OWN buckets: queueDepth samples the scheduler rather than
              // aggregating activity rows, so it can return a different number
              // of them. Borrowing the group series' timestamps would label this
              // plot with another query's clock.
              buckets={depthBuckets}
            />
          ) : (
            <Typography variant="caption" sx={{ color: 'text.secondary' }}>
              Queue depth: no group has queued requests in the sampled window.
            </Typography>
          )}
        </Stack>
      </Box>

      <Box>
        <Typography variant="overline" sx={{ color: C.textMuted, display: 'block', mb: 1 }}>
          By caller over time
        </Typography>
        {seriesKeys.length === 0 ? (
          <Typography sx={{ color: 'text.secondary' }}>No usage in window.</Typography>
        ) : (
          <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
            <MetricChart
              title="Requests"
              series={mkSeries((p) => Number(p.requests))}
              fmt={fmtInt}
              buckets={keyBuckets}
            />
            <MetricChart
              title="Energy"
              series={mkSeries((p) => p.energyKwh)}
              fmt={fmtKwh}
              buckets={keyBuckets}
            />
            <MetricChart
              title="Time"
              series={mkSeries((p) => Number(p.dwellMs))}
              fmt={(n) => fmtDuration(n)}
              buckets={keyBuckets}
            />
          </Stack>
        )}
      </Box>
    </Stack>
  )
}

// The old address, kept: links live in notes and chat history, and a 404 for a
// page that moved is the worst possible answer. Same trick /config already uses.
export const Route = createFileRoute('/usage')({
  beforeLoad: () => {
    throw redirect({ to: '/traffic' })
  },
})
