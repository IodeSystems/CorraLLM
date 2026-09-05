import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { DEFAULT_WINDOW, windowKey, windowPhrase, windowVars, type TimeWindow } from '@/TimeWindow'
import { Box, ToggleButton, ToggleButtonGroup, Typography } from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { StackedArea, foldSeries } from '@/Charts'
import { C } from '@/theme'
import { fmtDuration, fmtInt, fmtUSD } from '@/format'
import { Loading } from '@/Loading'

/**
 * Two views of the same traffic: who spent it, and what they spent it on.
 *
 * Neither derives from the other. "dun is 60% of cost" and "the 27B is 90% of
 * cost" are both true and answer different questions, and a page about callers
 * needs both — otherwise a heavy key looks equally heavy whether it is hammering
 * an embedder or a 27B.
 *
 * One metric selector drives both charts so they are always read on the same
 * measure. Separate charts rather than one with two y-scales: a dual axis is the
 * single most common way a chart lies about correlation.
 */

const METRICS = {
  requests: { label: 'Requests', fmt: (n: number) => fmtInt(Math.round(n)) },
  cost: { label: 'Cost', fmt: (n: number) => fmtUSD(n) },
  dwell: { label: 'Dwell', fmt: (n: number) => fmtDuration(Math.round(n)) },
} as const
type Metric = keyof typeof METRICS

const KeySeriesDoc = graphql(/* GraphQL */ `
  query KeyUsageSeries($windowHours: Long!, $bucketMinutes: Long!, $from: Long, $to: Long) {
    corrallm {
      usageSeries(windowHours: $windowHours, bucketMinutes: $bucketMinutes, from: $from, to: $to) {
        buckets
        keys {
          key
          points {
            requests
            costUsd
            dwellMs
          }
        }
      }
    }
  }
`)

const ModelSeriesDoc = graphql(/* GraphQL */ `
  query ModelUsageSeries($windowHours: Long!, $bucketMinutes: Long!, $key: String, $from: Long, $to: Long) {
    corrallm {
      usageSeriesByModel(windowHours: $windowHours, bucketMinutes: $bucketMinutes, key: $key, from: $from, to: $to) {
        buckets
        models {
          served
          points {
            requests
            costUsd
            dwellMs
          }
        }
      }
    }
  }
`)

type Point = { requests: unknown; costUsd: unknown; dwellMs: unknown }

function pick(p: Point | undefined, m: Metric): number {
  if (!p) return 0
  if (m === 'requests') return Number(p.requests)
  if (m === 'cost') return Number(p.costUsd)
  return Number(p.dwellMs)
}

/**
 * Windows, with a bucket width chosen to keep the axis around 30–50 points.
 *
 * The window is a CONTROL rather than a fixed 24h because a single burst can be
 * two orders of magnitude above the median hour — one 6,500-request embedding
 * run flattens the rest of the day to a hairline. Every fix for that on the axis
 * itself (log, clipping, a broken scale) misrepresents a stacked area, which is
 * additive by construction: the bands no longer sum to the total the reader sees.
 * Narrowing the window instead leaves every mark honestly scaled to its own
 * period.
 */
// The bucket rungs the axis used to offer as a toggle. They are now DERIVED from
// the page's window: the scaling argument above is about how wide a span each
// mark covers, and the span is chosen once, for the page, in the time control.
// A second window control inside a windowed page is exactly the drift this merge
// exists to end (plan/p30-information-architecture.md §0).
function bucketFor(spanMS: number): number {
  const hours = spanMS / 3_600_000
  if (hours <= 6) return 10
  if (hours <= 24) return 60
  return 240
}

export function KeyCharts({
  filterKey,
  window: pageWindow = DEFAULT_WINDOW,
}: {
  filterKey?: string
  window?: TimeWindow
}) {
  const [metric, setMetric] = useState<Metric>('requests')
  const spanMS =
    pageWindow.kind === 'absolute'
      ? pageWindow.toMS - pageWindow.fromMS
      : pageWindow.minutes * 60_000
  const bucketMinutes = bucketFor(spanMS)
  const wv = windowVars(pageWindow)
  const vars = {
    windowHours: wv.windowHours,
    bucketMinutes: String(bucketMinutes),
    from: wv.from,
    to: wv.to,
  }
  const live = pageWindow.kind === 'absolute' ? false : 60000

  const byKey = useQuery({
    queryKey: ['usage', 'series-by-key', windowKey(pageWindow), bucketMinutes],
    queryFn: () => gqlClient.request(KeySeriesDoc, vars),
    // Only meaningful unfiltered: on a single key's page there is one band, and
    // "who spent it" is already answered by the page you are on.
    enabled: !filterKey,
    refetchInterval: live,
  })
  const byModel = useQuery({
    queryKey: ['usage', 'series-by-model', windowKey(pageWindow), bucketMinutes, filterKey ?? ''],
    queryFn: () => gqlClient.request(ModelSeriesDoc, { ...vars, key: filterKey || undefined }),
    refetchInterval: live,
  })

  const loading = byModel.isLoading || (!filterKey && byKey.isLoading)
  const error = byModel.error || byKey.error
  if (loading) {
    return <Loading size={20} minHeight={120} />
  }
  if (error) {
    return (
      <Box sx={{ p: 2 }}>
        <Typography color="error">{String(error)}</Typography>
      </Box>
    )
  }

  const keyData = byKey.data?.corrallm.usageSeries
  const modelData = byModel.data?.corrallm.usageSeriesByModel
  const buckets = (modelData?.buckets ?? keyData?.buckets ?? []).map(Number)

  const keySeries = foldSeries(
    (keyData?.keys ?? []).map((k) => ({
      label: k.key || '(unkeyed)',
      values: (k.points ?? []).map((p) => pick(p, metric)),
    })),
  )
  const modelSeries = foldSeries(
    (modelData?.models ?? []).map((m) => ({
      label: m.served,
      values: (m.points ?? []).map((p) => pick(p, metric)),
    })),
  )

  const f = METRICS[metric].fmt
  const spanLabel = windowPhrase(pageWindow)

  const selector = (
    <ToggleButtonGroup
      size="small"
      exclusive
      value={metric}
      onChange={(_, v) => v && setMetric(v as Metric)}
    >
      {(Object.keys(METRICS) as Metric[]).map((m) => (
        <ToggleButton key={m} value={m} sx={{ textTransform: 'none', py: 0.25, px: 1 }}>
          {METRICS[m].label}
        </ToggleButton>
      ))}
    </ToggleButtonGroup>
  )

  const nothing =
    keySeries.length === 0 && modelSeries.length === 0 ? (
      <Typography sx={{ color: C.textFaint, p: 2 }}>No traffic {spanLabel}.</Typography>
    ) : null

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      {/* Filters in one row above the charts, not repeated per panel. */}
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5, flexWrap: 'wrap' }}>
        {selector}
        <Typography variant="caption" sx={{ color: C.textMuted }}>
          {bucketMinutes >= 60 ? `${bucketMinutes / 60}h` : `${bucketMinutes}m`} buckets — hover for
          exact values
        </Typography>
      </Box>
      {nothing}
      <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 2 }}>
        {!filterKey && keySeries.length > 0 && (
          <Box sx={{ flex: '1 1 420px', minWidth: 340 }}>
            <StackedArea
              title={`${METRICS[metric].label} by caller`}
              series={keySeries}
              fmtTotal={f}
              buckets={buckets}
              collapsibleLegend
            />
          </Box>
        )}
        {modelSeries.length > 0 && (
          <Box sx={{ flex: '1 1 420px', minWidth: 340 }}>
            <StackedArea
              title={
                filterKey
                  ? `${METRICS[metric].label} by model for ${filterKey}`
                  : `${METRICS[metric].label} by model`
              }
              series={modelSeries}
              fmtTotal={f}
              buckets={buckets}
              collapsibleLegend
            />
          </Box>
        )}
      </Box>
    </Box>
  )
}
