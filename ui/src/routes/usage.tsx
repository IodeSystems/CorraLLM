import { createFileRoute, redirect } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { DEFAULT_WINDOW, windowKey, windowVars, type TimeWindow } from '@/TimeWindow'
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
      usageRollup(windowHours: $windowHours, from: $from, to: $to) {
        windowHours
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
          cachedSecondsSaved
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
      queueDepth(windowHours: $windowHours, bucketMinutes: $bucketMinutes) {
        lanes {
          group
          points {
            avgWaiting
            maxWaiting
          }
        }
      }
      usageSeriesByGroup(windowHours: $windowHours, bucketMinutes: $bucketMinutes) {
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

/**
 * The cost-and-callers half of Traffic (P30 phase B).
 *
 * This was the `/usage` page. It is now a section of `/traffic`, under that
 * page's one time control, because "what is happening" and "what did it cost"
 * were the same subject measured over two different windows — which is how the
 * per-key numbers here came to disagree with the per-key numbers on the activity
 * side. `/usage` still resolves; it redirects.
 *
 * What left with the merge: the per-server pool bars and the Resident models
 * table. Both answer "what does the hardware have", which is /hosts' question,
 * and the memory ledger there already says the same budget/used WITH attribution.
 */
export function UsagePanels({ window = DEFAULT_WINDOW }: { window?: TimeWindow }) {
  const vars = windowVars(window)
  const spanMS =
    window.kind === 'absolute' ? window.toMS - window.fromMS : window.minutes * 60_000
  // The bucket follows the span, so a one-hour window is not one bar.
  const bucketMinutes = spanMS / 3_600_000 <= 6 ? 10 : spanMS / 3_600_000 <= 24 ? 60 : 240
  const q = useQuery({
    queryKey: ['usage', windowKey(window)],
    queryFn: () =>
      gqlClient.request(UsageDoc, {
        windowHours: vars.windowHours,
        bucketMinutes: String(bucketMinutes),
        from: vars.from,
        to: vars.to,
      }),
    // fallback; live updates arrive via SSE (useLiveEvents). A frozen window has
    // nothing new to fetch.
    refetchInterval: window.kind === 'absolute' ? false : 15000,
  })

  if (q.isLoading) {
    return <Loading />
  }
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Typography color="error">{String(q.error)}</Typography>
      </Box>
    )
  }

  const rollup = q.data?.corrallm.usageRollup
  const rollupRows = rollup?.rows ?? []
  const total = rollup?.total
  const byKey = q.data?.corrallm.usageByKey?.rows ?? []
  const kMax = {
    cost: Math.max(0, ...byKey.map((r) => r.costUsd)),
    req: Math.max(0, ...byKey.map((r) => Number(r.requests))),
    energy: Math.max(0, ...byKey.map((r) => r.energyKwh)),
    dwell: Math.max(0, ...byKey.map((r) => Number(r.dwellMs))),
  }
  const fmtKwh = (k: number) =>
    !Number.isFinite(k) || k === 0 ? '—' : k < 1 ? `${(k * 1000).toFixed(1)} Wh` : `${k.toFixed(3)} kWh`

  const pgroups = q.data?.corrallm.usageSeriesByGroup?.groups ?? []
  const groupColor = (i: number) => seriesColor(i)
  const groupSeries: ChartSeries[] = pgroups.map((g, i) => ({
    key: g.group,
    color: groupColor(i),
    values: g.points.map((p) => Number(p.requests)),
  }))
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

  const depthGroups = q.data?.corrallm.queueDepth?.lanes ?? []
  const depthSeries: ChartSeries[] = depthGroups.map((l, i) => ({
    key: l.group,
    color: groupColor(i),
    values: l.points.map((p) => p.avgWaiting),
  }))
  const anyDepth = depthSeries.some((s) => s.values.some((v) => v > 0))

  const seriesKeys = q.data?.corrallm.usageSeries?.keys ?? []
  const mkSeries = (sel: (p: {
    requests: string
    costUsd: number
    energyKwh: number
    dwellMs: string
  }) => number): ChartSeries[] =>
    seriesKeys.map((k, i) => ({
      key: k.key,
      color: seriesColor(i),
      values: k.points.map(sel),
    }))

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
      <Stack direction="row" spacing={2} useFlexGap sx={{
        flexWrap: "wrap"
      }}>
        <StatTile label="Requests" value={fmtInt(total?.requests ?? 0)} />
        <StatTile label="Prompt tokens" value={fmtInt(total?.promptTokens ?? 0)} />
        <StatTile label="Completion tokens" value={fmtInt(total?.completionTokens ?? 0)} />
        <StatTile label="Cost" value={fmtUSD(total?.costUsd ?? 0)} />
        <StatTile
          label="Cache hit"
          value={
            Number(total?.cacheReports ?? 0) > 0
              ? `${((total?.cacheHitRate ?? 0) * 100).toFixed(1)}%`
              : 'n/r'
          }
          sub={
            Number(total?.cacheReports ?? 0) > 0
              ? `${fmtInt(total?.cachedTokens ?? 0)} tok, ~${fmtLongDuration(
                  (total?.cachedSecondsSaved ?? 0) * 1000,
                )} saved`
              : 'nothing reported a cache'
          }
          hint="Share of prompt tokens the backends served from cache instead of reprocessing, weighted by tokens across every model — not an average of per-model rates. The saved figure estimates the prompt-processing time avoided at the observed tokens/sec, so it moves with batch size and context."
        />
      </Stack>

      <Panel title="By model" flush>
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Model</TableCell>
                <TableCell align="right">Requests</TableCell>
                <TableCell align="right">Prompt</TableCell>
                <TableCell align="right">Completion</TableCell>
                {/* The per-request figure, not the sum: a sum of dwell reads as
                    a duration and was misread as one in three runs (OB-4). The
                    total is still there, under it. */}
                <TableCell align="right">Time per request</TableCell>
                <TableCell align="right">
                  <Tooltip title="Prompt tokens served from cache as a share of all prompt tokens. 'n/r' means nothing reported a cache here — not a measured zero.">
                    <span>Cached</span>
                  </Tooltip>
                </TableCell>
                <TableCell align="right">Cost</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {rollupRows.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={7}>
                    <Typography sx={{
                      color: "text.secondary"
                    }}>No usage in window.</Typography>
                  </TableCell>
                </TableRow>
              ) : (
                rollupRows.map((r) => (
                  <TableRow key={r.served} hover>
                    <TableCell>{r.served}</TableCell>
                    <TableCell align="right">{fmtInt(r.requests)}</TableCell>
                    <TableCell align="right">{fmtInt(r.promptTokens)}</TableCell>
                    <TableCell align="right">{fmtInt(r.completionTokens)}</TableCell>
                    <TableCell align="right">
                      {fmtDuration(
                        Number(r.requests) > 0
                          ? Math.round(Number(r.dwellMs) / Number(r.requests))
                          : 0,
                      )}
                      <Typography variant="caption" sx={{ display: 'block', color: C.textFaint }}>
                        {fmtDuration(r.dwellMs)} total
                      </Typography>
                    </TableCell>
                    <CacheCell
                      rate={r.cacheHitRate}
                      reports={Number(r.cacheReports)}
                      cached={Number(r.cachedTokens)}
                    />
                    <TableCell align="right">{fmtUSD(r.costUsd)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Panel>

      <Box>
        <Typography variant="overline" sx={{ color: C.textMuted, display: 'block', mb: 1 }}>
          Priority groups
        </Typography>
        {groupSeries.length === 0 ? (
          <Typography sx={{
            color: "text.secondary"
          }}>No usage in window.</Typography>
        ) : (
          <Stack spacing={2}>
            <StackedArea title="Throughput — requests/bucket (stacked)" series={groupSeries} fmtTotal={fmtInt} />
            {anyRejections ? (
              <StackedArea
                title="Queue pressure — 429s/bucket by group"
                series={rejectSeries}
                fmtTotal={fmtInt}
              />
            ) : (
              <Typography variant="caption" sx={{
                color: "text.secondary"
              }}>
                No rejections in window — no group is being starved.
              </Typography>
            )}
            <MetricChart title="Avg queue wait / request" series={waitSeries} fmt={(n) => fmtDuration(n)} />
            {anyDepth ? (
              <StackedArea
                title="Queue depth — avg waiting by group (sampled)"
                series={depthSeries}
                fmtTotal={(n) => n.toFixed(1)}
              />
            ) : (
              <Typography variant="caption" sx={{
                color: "text.secondary"
              }}>
                Queue depth: no group has queued requests in the sampled window.
              </Typography>
            )}
          </Stack>
        )}
      </Box>

      <Box>
        <Typography variant="overline" sx={{ color: C.textMuted, display: 'block', mb: 1 }}>
          By key over time
        </Typography>
        {seriesKeys.length === 0 ? (
          <Typography sx={{
            color: "text.secondary"
          }}>No usage in window.</Typography>
        ) : (
          <Stack direction="row" spacing={2} useFlexGap sx={{
            flexWrap: "wrap"
          }}>
            <MetricChart title="Cost ($)" series={mkSeries((p) => p.costUsd)} fmt={fmtUSD} />
            <MetricChart title="Requests" series={mkSeries((p) => Number(p.requests))} fmt={fmtInt} />
            <MetricChart title="Energy" series={mkSeries((p) => p.energyKwh)} fmt={fmtKwh} />
            <MetricChart
              title="Time"
              series={mkSeries((p) => Number(p.dwellMs))}
              fmt={(n) => fmtDuration(n)}
            />
          </Stack>
        )}
      </Box>

      <Panel title="By key" subtitle="Bars are relative to the column max" flush>
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
                    <Typography sx={{
                      color: "text.secondary"
                    }}>No keyed usage in window.</Typography>
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
    </Box>
  );
}

// The old address, kept: links live in notes and chat history, and a 404 for a
// page that moved is the worst possible answer. Same trick /config already uses.
export const Route = createFileRoute('/usage')({
  beforeLoad: () => {
    throw redirect({ to: '/traffic' })
  },
})
