import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  Box,
  Card,
  CardContent,
  Chip,
  CircularProgress,
  LinearProgress,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtBytes, fmtDuration, fmtInt, fmtTime, fmtUSD } from '@/format'

const KEY_COLORS = ['#1976d2', '#9c27b0', '#2e7d32', '#ed6c02', '#0288d1', '#d32f2f']

type ChartSeries = { key: string; color: string; values: number[] }

// StackedArea draws bands summed bottom-to-top — for priority-group throughput
// over time, so a shrinking high-priority band signals starvation.
function StackedArea({
  title,
  series,
  fmtTotal,
}: {
  title: string
  series: ChartSeries[]
  fmtTotal: (n: number) => string
}) {
  const W = 600
  const H = 160
  const pad = 4
  const n = series[0]?.values.length ?? 0
  if (n === 0) return null
  const totals = Array.from({ length: n }, (_, i) => series.reduce((s, ser) => s + (ser.values[i] || 0), 0))
  const max = Math.max(0, ...totals)
  const x = (i: number) => (n <= 1 ? pad : (i / (n - 1)) * (W - 2 * pad) + pad)
  const y = (v: number) => (max <= 0 ? H - pad : H - pad - (v / max) * (H - 2 * pad))

  const cum = new Array<number>(n).fill(0)
  const bands = series.map((ser) => {
    const bottom = cum.slice()
    const top = cum.map((c, i) => c + (ser.values[i] || 0))
    for (let i = 0; i < n; i++) cum[i] = top[i]
    const pts = [
      ...top.map((v, i) => `${x(i)},${y(v)}`),
      ...bottom.map((v, i) => `${x(i)},${y(v)}`).reverse(),
    ].join(' ')
    return { key: ser.key, color: ser.color, pts }
  })

  return (
    <Card>
      <CardContent>
        <Box sx={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' }}>
          <Typography variant="subtitle2">{title}</Typography>
          <Typography variant="caption" color="text.secondary">
            peak {fmtTotal(max)}
          </Typography>
        </Box>
        <Box
          component="svg"
          viewBox={`0 0 ${W} ${H}`}
          preserveAspectRatio="none"
          sx={{ width: '100%', height: 180, display: 'block', mt: 1 }}
        >
          {bands.map((b) => (
            <polygon
              key={b.key}
              points={b.pts}
              fill={b.color}
              fillOpacity={0.55}
              stroke={b.color}
              strokeWidth={1}
              vectorEffect="non-scaling-stroke"
            />
          ))}
        </Box>
        <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1.5, mt: 1 }}>
          {series.map((s) => (
            <Box key={s.key} sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
              <Box sx={{ width: 10, height: 10, bgcolor: s.color, borderRadius: 0.3 }} />
              <Typography variant="caption">{s.key}</Typography>
            </Box>
          ))}
        </Box>
      </CardContent>
    </Card>
  )
}

// MetricChart draws one metric over time, one line per key (dependency-free SVG).
function MetricChart({
  title,
  series,
  fmt,
}: {
  title: string
  series: ChartSeries[]
  fmt: (n: number) => string
}) {
  const W = 600
  const H = 120
  const pad = 4
  const n = series[0]?.values.length ?? 0
  const max = Math.max(0, ...series.flatMap((s) => s.values))
  const x = (i: number) => (n <= 1 ? pad : (i / (n - 1)) * (W - 2 * pad) + pad)
  const y = (v: number) => (max <= 0 ? H - pad : H - pad - (v / max) * (H - 2 * pad))
  return (
    <Card sx={{ flex: '1 1 380px', minWidth: 320 }}>
      <CardContent>
        <Box sx={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' }}>
          <Typography variant="subtitle2">{title}</Typography>
          <Typography variant="caption" color="text.secondary">
            peak {fmt(max)}
          </Typography>
        </Box>
        <Box
          component="svg"
          viewBox={`0 0 ${W} ${H}`}
          preserveAspectRatio="none"
          sx={{ width: '100%', height: 130, display: 'block', mt: 1 }}
        >
          {series.map((s) => (
            <polyline
              key={s.key}
              fill="none"
              stroke={s.color}
              strokeWidth={1.5}
              vectorEffect="non-scaling-stroke"
              points={s.values.map((v, i) => `${x(i)},${y(v)}`).join(' ')}
            />
          ))}
        </Box>
        <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1.5, mt: 1 }}>
          {series.map((s) => (
            <Box key={s.key} sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
              <Box sx={{ width: 10, height: 10, bgcolor: s.color, borderRadius: '50%' }} />
              <Typography variant="caption">{s.key || '(unkeyed)'}</Typography>
            </Box>
          ))}
        </Box>
      </CardContent>
    </Card>
  )
}

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

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <Card sx={{ minWidth: 160, flex: '1 1 160px' }}>
      <CardContent>
        <Typography variant="overline" color="text.secondary">
          {label}
        </Typography>
        <Typography variant="h5">{value}</Typography>
      </CardContent>
    </Card>
  )
}

const UsageDoc = graphql(/* GraphQL */ `
  query Usage {
    corrallm {
      usageRollup(windowHours: "24") {
        windowHours
        rows {
          served
          requests
          promptTokens
          completionTokens
          dwellMs
          costUsd
        }
        total {
          requests
          promptTokens
          completionTokens
          dwellMs
          costUsd
        }
      }
      queueDepth(windowHours: "24", bucketMinutes: "60") {
        lanes {
          group
          points {
            avgWaiting
            maxWaiting
          }
        }
      }
      usageSeriesByGroup(windowHours: "24", bucketMinutes: "60") {
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
      usageSeries(windowHours: "24", bucketMinutes: "60") {
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
      usageByKey(windowHours: "24") {
        rows {
          key
          requests
          promptTokens
          completionTokens
          dwellMs
          costUsd
          energyKwh
        }
      }
      residency {
        servers {
          server
          pools {
            pool
            budget
            used
          }
        }
        models {
          name
          modelName
          server
          state
          refs
          persistent
          lastUsedMs
          usage {
            pool
            bytes
          }
        }
      }
    }
  }
`)

function pct(used: string, budget: string): number {
  const u = Number(used)
  const b = Number(budget)
  if (!Number.isFinite(b) || b <= 0) return 0
  return Math.min(100, (u / b) * 100)
}

function stateColor(state: string): 'success' | 'info' | 'warning' | 'error' | 'default' {
  switch (state) {
    case 'ready':
      return 'success'
    case 'loading':
      return 'info'
    case 'evicting':
      return 'warning'
    case 'failed':
      return 'error'
    default:
      return 'default'
  }
}

function Usage() {
  const q = useQuery({
    queryKey: ['usage'],
    queryFn: () => gqlClient.request(UsageDoc),
    refetchInterval: 15000, // fallback; live updates arrive via SSE (useLiveEvents)
  })

  if (q.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Typography color="error">{String(q.error)}</Typography>
      </Box>
    )
  }

  const res = q.data?.corrallm.residency
  const servers = res?.servers ?? []
  const models = res?.models ?? []
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

  const lanes = q.data?.corrallm.usageSeriesByGroup?.groups ?? []
  const laneColor = (i: number) => KEY_COLORS[i % KEY_COLORS.length]
  const groupSeries: ChartSeries[] = lanes.map((g, i) => ({
    key: g.group,
    color: laneColor(i),
    values: g.points.map((p) => Number(p.requests)),
  }))
  const rejectSeries: ChartSeries[] = lanes.map((g, i) => ({
    key: g.group,
    color: laneColor(i),
    values: g.points.map((p) => Number(p.rejected)),
  }))
  // Avg queue wait per request, per lane, per bucket (ms).
  const waitSeries: ChartSeries[] = lanes.map((g, i) => ({
    key: g.group,
    color: laneColor(i),
    values: g.points.map((p) => {
      const reqs = Number(p.requests)
      return reqs > 0 ? Number(p.queuedMs) / reqs : 0
    }),
  }))
  const anyRejections = rejectSeries.some((s) => s.values.some((v) => v > 0))

  const depthLanes = q.data?.corrallm.queueDepth?.lanes ?? []
  const depthSeries: ChartSeries[] = depthLanes.map((l, i) => ({
    key: l.group,
    color: laneColor(i),
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
      color: KEY_COLORS[i % KEY_COLORS.length],
      values: k.points.map(sel),
    }))

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <Box>
        <Typography variant="h6" gutterBottom>
          Usage — last 24h
        </Typography>
        <Stack direction="row" spacing={2} sx={{ mb: 2 }} flexWrap="wrap" useFlexGap>
          <Stat label="Requests" value={fmtInt(total?.requests ?? 0)} />
          <Stat label="Prompt tokens" value={fmtInt(total?.promptTokens ?? 0)} />
          <Stat label="Completion tokens" value={fmtInt(total?.completionTokens ?? 0)} />
          <Stat label="Cost" value={fmtUSD(total?.costUsd ?? 0)} />
        </Stack>
        <TableContainer component={Paper}>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Model</TableCell>
                <TableCell align="right">Requests</TableCell>
                <TableCell align="right">Prompt</TableCell>
                <TableCell align="right">Completion</TableCell>
                <TableCell align="right">Dwell</TableCell>
                <TableCell align="right">Cost</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {rollupRows.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={6}>
                    <Typography color="text.secondary">No usage in window.</Typography>
                  </TableCell>
                </TableRow>
              ) : (
                rollupRows.map((r) => (
                  <TableRow key={r.served} hover>
                    <TableCell>{r.served}</TableCell>
                    <TableCell align="right">{fmtInt(r.requests)}</TableCell>
                    <TableCell align="right">{fmtInt(r.promptTokens)}</TableCell>
                    <TableCell align="right">{fmtInt(r.completionTokens)}</TableCell>
                    <TableCell align="right">{fmtDuration(r.dwellMs)}</TableCell>
                    <TableCell align="right">{fmtUSD(r.costUsd)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Box>

      <Box>
        <Typography variant="h6" gutterBottom>
          Priority lanes — last 24h
        </Typography>
        {groupSeries.length === 0 ? (
          <Typography color="text.secondary">No usage in window.</Typography>
        ) : (
          <Stack spacing={2}>
            <StackedArea title="Throughput — requests/bucket (stacked)" series={groupSeries} fmtTotal={fmtInt} />
            {anyRejections ? (
              <StackedArea
                title="Queue pressure — 429s/bucket by lane"
                series={rejectSeries}
                fmtTotal={fmtInt}
              />
            ) : (
              <Typography variant="caption" color="text.secondary">
                No rejections in window — no lane is being starved.
              </Typography>
            )}
            <MetricChart title="Avg queue wait / request" series={waitSeries} fmt={(n) => fmtDuration(n)} />
            {anyDepth ? (
              <StackedArea
                title="Queue depth — avg waiting by lane (sampled)"
                series={depthSeries}
                fmtTotal={(n) => n.toFixed(1)}
              />
            ) : (
              <Typography variant="caption" color="text.secondary">
                Queue depth: no lane has queued requests in the sampled window.
              </Typography>
            )}
          </Stack>
        )}
      </Box>

      <Box>
        <Typography variant="h6" gutterBottom>
          By Key over time — last 24h
        </Typography>
        {seriesKeys.length === 0 ? (
          <Typography color="text.secondary">No usage in window.</Typography>
        ) : (
          <Stack direction="row" spacing={2} flexWrap="wrap" useFlexGap>
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

      <Box>
        <Typography variant="h6" gutterBottom>
          By Key — last 24h
        </Typography>
        <TableContainer component={Paper}>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Key</TableCell>
                <TableCell align="right">Cost</TableCell>
                <TableCell align="right">Requests</TableCell>
                <TableCell align="right">Energy</TableCell>
                <TableCell align="right">Time</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {byKey.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5}>
                    <Typography color="text.secondary">No keyed usage in window.</Typography>
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
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Box>

      <Box>
        <Typography variant="h6" gutterBottom>
          Server Pools
        </Typography>
        <Stack direction="row" spacing={2} flexWrap="wrap" useFlexGap>
          {servers.length === 0 ? (
            <Typography color="text.secondary">No servers configured.</Typography>
          ) : (
            servers.map((s) => (
              <Card key={s.server} sx={{ minWidth: 280, flex: '1 1 280px' }}>
                <CardContent>
                  <Typography variant="subtitle1" gutterBottom>
                    {s.server}
                  </Typography>
                  <Stack spacing={1.5}>
                    {s.pools.map((p) => (
                      <Box key={p.pool}>
                        <Box sx={{ display: 'flex', justifyContent: 'space-between' }}>
                          <Typography variant="body2">{p.pool}</Typography>
                          <Typography variant="body2" color="text.secondary">
                            {fmtBytes(p.used)} / {fmtBytes(p.budget)}
                          </Typography>
                        </Box>
                        <LinearProgress
                          variant="determinate"
                          value={pct(p.used, p.budget)}
                          sx={{ height: 8, borderRadius: 1 }}
                        />
                      </Box>
                    ))}
                  </Stack>
                </CardContent>
              </Card>
            ))
          )}
        </Stack>
      </Box>

      <Box>
        <Typography variant="h6" gutterBottom>
          Resident Models
        </Typography>
        <TableContainer component={Paper}>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Backend</TableCell>
                <TableCell>Model</TableCell>
                <TableCell>Server</TableCell>
                <TableCell>State</TableCell>
                <TableCell align="right">Refs</TableCell>
                <TableCell>Pinned</TableCell>
                <TableCell>Reserved</TableCell>
                <TableCell>Last used</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {models.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={8}>
                    <Typography color="text.secondary">Nothing warm.</Typography>
                  </TableCell>
                </TableRow>
              ) : (
                models.map((m) => (
                  <TableRow key={m.name} hover>
                    <TableCell>{m.name}</TableCell>
                    <TableCell>{m.modelName}</TableCell>
                    <TableCell>{m.server || '—'}</TableCell>
                    <TableCell>
                      <Chip size="small" label={m.state} color={stateColor(m.state)} />
                    </TableCell>
                    <TableCell align="right">{fmtInt(m.refs)}</TableCell>
                    <TableCell>{m.persistent ? 'yes' : '—'}</TableCell>
                    <TableCell>
                      {m.usage.length === 0
                        ? '—'
                        : m.usage.map((u) => `${u.pool}:${fmtBytes(u.bytes)}`).join(', ')}
                    </TableCell>
                    <TableCell>{fmtTime(m.lastUsedMs)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Box>
    </Box>
  )
}

export const Route = createFileRoute('/usage')({ component: Usage })
