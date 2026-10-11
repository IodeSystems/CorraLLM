import { useQuery } from '@tanstack/react-query'
import { Box, Typography } from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { Panel } from '@/Panel'
import { MacdChart, MetricChart, StackedArea } from '@/Charts'
import { C, seriesColor } from '@/theme'
import { fmtDuration } from '@/format'

/**
 * How busy each GPU was, and how long callers waited for a first token.
 *
 * BUSY is the share of time at least one request was being served on the pool
 * — the union of service spans, so two models on one card at once count once.
 * Queue and cold-load time are not busy: the GPU was not working for that
 * request yet. An idle GPU while work was waiting is the waste this shows.
 *
 * FIRST TOKEN splits into the wait in corrallm's queue, the wait for a cold
 * model load, and active time (prompt processing up to the first byte).
 * Streamed requests only: a non-streamed reply's first byte is the whole answer.
 */
const GpuUtilDoc = graphql(/* GraphQL */ `
  query GpuUtilization {
    corrallm {
      hour: gpuUtilization(windowHours: 1, bucketMinutes: 60) {
        pools {
          server
          pool
          busyPct
        }
      }
      trend: gpuUtilization(windowHours: 48, bucketMinutes: 15) {
        buckets
        pools {
          server
          pool
          busy
        }
      }
      day: gpuUtilization(windowHours: 24, bucketMinutes: 15) {
        bucketMinutes
        buckets
        pools {
          server
          pool
          models
          busyPct
          busy
          ttftN
          queueMs
          loadMs
          activeMs
          ttftCount
          queueMsAvg
          activeMsAvg
          ttftMsP90
          ttftMsP99
          activeMsP90
          activeMsP99
        }
      }
    }
  }
`)

export function useGpuUtilization() {
  return useQuery({
    queryKey: ['gpuUtilization'],
    queryFn: () => gqlClient.request(GpuUtilDoc),
    refetchInterval: 60000,
  })
}

/** Busy share per `server/pool`, last hour and last 24 hours, 0..1. */
export function useGpuBusy(): Map<string, { hour: number; day: number }> {
  const { data } = useGpuUtilization()
  const out = new Map<string, { hour: number; day: number }>()
  const c = data?.corrallm
  for (const p of c?.day?.pools ?? []) {
    out.set(`${p.server}/${p.pool}`, { hour: 0, day: p.busyPct })
  }
  for (const p of c?.hour?.pools ?? []) {
    const k = `${p.server}/${p.pool}`
    out.set(k, { hour: p.busyPct, day: out.get(k)?.day ?? 0 })
  }
  return out
}

const pct = (n: number) => `${Math.round(n * 100)}%`

// MACD on busy share, standard 12/26/9 periods. At 15-minute buckets the fast
// average spans 3 h, the slow ("typical") 6.5 h, the signal 2.25 h.
const FAST = 12
const SLOW = 26
const SIGNAL = 9

/** Exponential moving average, seeded with the first value. */
function ema(v: number[], period: number): number[] {
  const a = 2 / (period + 1)
  const out: number[] = []
  v.forEach((x, i) => out.push(i === 0 ? x : a * x + (1 - a) * out[i - 1]))
  return out
}

/**
 * Fast and slow averages of busy %, the MACD line (fast − slow) and its signal,
 * computed over the 48 h series so the averages are warmed up, then cut to the
 * 24 h axis by timestamp. MACD above 0 = busier than typical; MACD above its
 * signal (green bars) = the gap is widening, i.e. accelerating.
 */
function macdFor(trendBuckets: number[], trendBusy: number[], dayBuckets: number[]) {
  const pctSeries = trendBusy.map((v) => v * 100)
  const fast = ema(pctSeries, FAST)
  const slow = ema(pctSeries, SLOW)
  const macd = fast.map((f, i) => f - slow[i])
  const signal = ema(macd, SIGNAL)
  const off = trendBuckets.indexOf(dayBuckets[0])
  if (off < 0 || off + dayBuckets.length > trendBuckets.length) return null
  const cut = (v: number[]) => v.slice(off, off + dayBuckets.length)
  return { fast: cut(fast), slow: cut(slow), macd: cut(macd), signal: cut(signal) }
}
const secs = (ms: number) => fmtDuration(Math.round(ms))

export function GpuUtilizationPanel() {
  const { data, isLoading } = useGpuUtilization()
  const day = data?.corrallm?.day
  const hour = data?.corrallm?.hour
  const buckets = (day?.buckets ?? []).map(Number)
  const pools = day?.pools ?? []
  const trend = data?.corrallm?.trend
  const trendBuckets = (trend?.buckets ?? []).map(Number)
  return (
    <Panel
      title="GPU utilization and time to first token"
      subtitle="Last 24 hours, 15-minute buckets"
      note="Busy: at least one request being served on the GPU; waiting in the queue or for a load is not busy. First token: streamed requests only, split into waiting in corrallm's queue, waiting for a cold model load, and active prompt processing up to the first byte."
      dense
    >
      {isLoading ? (
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          Loading…
        </Typography>
      ) : pools.length === 0 ? (
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          No GPU served anything in the last 24 hours.
        </Typography>
      ) : (
        pools.map((p) => {
          const h = hour?.pools.find((x) => x.server === p.server && x.pool === p.pool)
          const tp = trend?.pools.find((x) => x.server === p.server && x.pool === p.pool)
          const m = tp ? macdFor(trendBuckets, tp.busy, buckets) : null
          const n = Number(p.ttftCount)
          return (
            <Box key={`${p.server}/${p.pool}`} sx={{ mb: 2 }}>
              <Box sx={{ display: 'flex', gap: 1, alignItems: 'baseline', flexWrap: 'wrap' }}>
                <Typography variant="subtitle2">
                  {p.server} · {p.pool}
                </Typography>
                <Typography variant="caption" sx={{ color: C.textMuted }}>
                  busy {pct(h?.busyPct ?? 0)} last hour · {pct(p.busyPct)} last 24 h · {p.models.join(', ')}
                </Typography>
              </Box>
              <Typography variant="caption" sx={{ color: C.textMuted, display: 'block', mb: 0.5 }}>
                {n > 0
                  ? `first token over ${n.toLocaleString()} streamed requests: queue ${secs(p.queueMsAvg)} avg + active ${secs(p.activeMsAvg)} avg · total p90 ${secs(Number(p.ttftMsP90))}, p99 ${secs(Number(p.ttftMsP99))} · active p90 ${secs(Number(p.activeMsP90))}, p99 ${secs(Number(p.activeMsP99))}`
                  : 'no streamed requests in the last 24 hours'}
              </Typography>
              <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap' }}>
                <MetricChart
                  title="Busy (share of each 15 min)"
                  series={[
                    { key: 'busy', color: seriesColor(0), values: p.busy.map((v) => v * 100) },
                    ...(m
                      ? [
                          { key: 'recent avg (3 h)', color: seriesColor(1), values: m.fast },
                          { key: 'typical avg (6.5 h)', color: seriesColor(2), values: m.slow },
                        ]
                      : []),
                  ]}
                  fmt={(v) => `${Math.round(v)}%`}
                  buckets={buckets}
                />
                {m && (
                  <MacdChart
                    title="Busy trend (MACD, percentage points)"
                    note="MACD = recent average (3 h) minus typical average (6.5 h) of busy %. Above 0: busier than typical; below 0: quieter. Bars are MACD minus its 2.25 h signal line: green means the gap is growing (accelerating), red means it is shrinking."
                    macd={m.macd}
                    signal={m.signal}
                    fmt={(v) => `${v.toFixed(1)} pp`}
                    buckets={buckets}
                  />
                )}
                <Box sx={{ flex: '1 1 380px', minWidth: 320 }}>
                  <StackedArea
                    title="Time to first token, mean per 15 min"
                    series={[
                      { key: 'queue', color: seriesColor(1), values: p.queueMs.map((v) => v / 1000) },
                      { key: 'cold load', color: seriesColor(2), values: p.loadMs.map((v) => v / 1000) },
                      { key: 'active', color: seriesColor(0), values: p.activeMs.map((v) => v / 1000) },
                    ]}
                    fmtTotal={(v) => `${v.toFixed(1)} s`}
                    buckets={buckets}
                  />
                </Box>
              </Box>
            </Box>
          )
        })
      )}
    </Panel>
  )
}
