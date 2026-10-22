import { createFileRoute, redirect } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { DEFAULT_WINDOW, windowKey, windowVars, type TimeWindow } from '@/TimeWindow'
import { Box, Stack, Typography } from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { MetricChart, StackedArea } from '@/Charts'
import type { ChartSeries } from '@/Charts'
import { C, seriesColor } from '@/theme'
import { fmtDuration, fmtInt, fmtKwh } from '@/format'
import { Loading } from '@/Loading'

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
        <Typography variant="overline" sx={{ color: C.textMuted, display: 'block', mb: 1, textTransform: 'none', fontSize: 13, letterSpacing: 0 }}>
          Priority groups
        </Typography>
        <Stack spacing={2}>
          {anyRejections ? (
            <StackedArea
              title="Rejections (429) by group"
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
            title="Average queue wait per request"
            series={waitSeries}
            fmt={(n) => fmtDuration(n)}
            buckets={groupBuckets}
          />
          {depth.isLoading ? (
            <Loading size={20} minHeight={80} />
          ) : anyDepth ? (
            <StackedArea
              title="Queue depth by group"
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
        <Typography variant="overline" sx={{ color: C.textMuted, display: 'block', mb: 1, textTransform: 'none', fontSize: 13, letterSpacing: 0 }}>
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
              title="Slot time"
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
    throw redirect({ to: '/' })
  },
})
