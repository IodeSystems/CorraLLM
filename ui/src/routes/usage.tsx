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
