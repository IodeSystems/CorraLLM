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
import { fmtDuration, fmtInt, fmtUSD } from '@/format'
import { Loading } from '@/Loading'
import { useModelStats } from '@/ModelStats'

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
function StatTile({
  label,
  value,
  sub,
  hint,
}: {
  label: string
  value: string
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

const fmtKwh = (k: number) =>
  !Number.isFinite(k) || k === 0 ? '—' : k < 1 ? `${(k * 1000).toFixed(1)} Wh` : `${k.toFixed(3)} kWh`

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
export function TrafficHeadline({ window = DEFAULT_WINDOW }: { window?: TimeWindow }) {
  const q = useUsage(window)
  // Totals ride the By-model query rather than a usageRollup of our own: same
  // fetch, so a tile and the table under it cannot drift apart mid-window.
  const total = useModelStats(window).data?.corrallm.usageRollup?.total

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

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
      <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
        <StatTile label="Requests" value={fmtInt(total?.requests ?? 0)} />
        <StatTile
          label="Processed"
          value={fmtInt(total?.promptTokens ?? 0)}
          sub="prompt tokens read"
          hint="Every prompt token the backends were handed in this window, cached ones included."
        />
        <StatTile
          label="Generated"
          value={fmtInt(total?.completionTokens ?? 0)}
          sub="completion tokens written"
        />
        <StatTile
          label="Cached"
          value={reports > 0 ? fmtInt(total?.cachedTokens ?? 0) : 'n/r'}
          sub={
            reports > 0
              ? `${((total?.cacheHitRate ?? 0) * 100).toFixed(1)}% of prompt, ~${fmtLongDuration(
                  (total?.cachedSecondsSaved ?? 0) * 1000,
                )} saved`
              : 'nothing reported a cache'
          }
          hint="Prompt tokens the backends served from cache instead of reprocessing, and their share of all prompt tokens — weighted by tokens across every model, not an average of per-model rates. 'n/r' means nothing reported a cache at all, which is not a measured zero. The saved figure estimates the prompt-processing time avoided at the observed tokens/sec, so it moves with batch size and context."
        />
        <StatTile label="Cost" value={fmtUSD(total?.costUsd ?? 0)} />
      </Stack>

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
