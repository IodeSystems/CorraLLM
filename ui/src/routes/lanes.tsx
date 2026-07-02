import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import {
  Box,
  Chip,
  CircularProgress,
  LinearProgress,
  Paper,
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
import { fmtInt } from '@/format'

const LanesDoc = graphql(/* GraphQL */ `
  query Lanes {
    corrallm {
      reservations {
        reservations {
          model
          lane
          slots
          expiresAt
        }
      }
      lanes {
        groups {
          name
          weight
          shareCurrency
          interruptible
          active
          waiting
        }
        backends {
          backend
          capacity
          active
          waiting
          groups {
            group
            active
            waiting
          }
        }
      }
    }
  }
`)

function capPct(active: string, capacity: string): number {
  const a = Number(active)
  const c = Number(capacity)
  if (!Number.isFinite(c) || c <= 0) return 0
  return Math.min(100, (a / c) * 100)
}

// fmtCountdown renders the time left on a lease as "4m 03s" / "42s" / "expired".
function fmtCountdown(expiresAt: string, nowMs: number): string {
  const secs = Math.round((new Date(expiresAt).getTime() - nowMs) / 1000)
  if (secs <= 0) return 'expired'
  const m = Math.floor(secs / 60)
  const s = secs % 60
  return m > 0 ? `${m}m ${String(s).padStart(2, '0')}s` : `${s}s`
}

function Lanes() {
  const q = useQuery({
    queryKey: ['lanes'],
    queryFn: () => gqlClient.request(LanesDoc),
    refetchInterval: 15000, // fallback; live updates arrive via SSE (useLiveEvents)
  })

  // Tick a local clock so reservation countdowns update between refetches.
  const [nowMs, setNowMs] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNowMs(Date.now()), 1000)
    return () => clearInterval(id)
  }, [])

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

  const lanes = q.data?.corrallm.lanes
  const groups = lanes?.groups ?? []
  const backends = lanes?.backends ?? []
  const reservations = q.data?.corrallm.reservations?.reservations ?? []

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <Box>
        <Typography variant="h6" gutterBottom>
          Priority Groups
        </Typography>
        <TableContainer component={Paper}>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Group</TableCell>
                <TableCell align="right">Weight</TableCell>
                <TableCell>Share currency</TableCell>
                <TableCell>Interruptible</TableCell>
                <TableCell align="right">Active</TableCell>
                <TableCell align="right">Waiting</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {groups.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={6}>
                    <Typography color="text.secondary">No groups configured.</Typography>
                  </TableCell>
                </TableRow>
              ) : (
                groups.map((g) => (
                  <TableRow key={g.name} hover>
                    <TableCell>{g.name}</TableCell>
                    <TableCell align="right">{fmtInt(g.weight)}</TableCell>
                    <TableCell>{g.shareCurrency}</TableCell>
                    <TableCell>{g.interruptible ? 'yes' : '—'}</TableCell>
                    <TableCell align="right">{fmtInt(g.active)}</TableCell>
                    <TableCell align="right">
                      {Number(g.waiting) > 0 ? (
                        <Chip size="small" color="warning" label={fmtInt(g.waiting)} />
                      ) : (
                        '0'
                      )}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Box>

      <Box>
        <Typography variant="h6" gutterBottom>
          Reservations
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          Slots held free for a lane so interactive work has headroom. Short-lived;
          renewed by heartbeat, auto-expiring.
        </Typography>
        <TableContainer component={Paper}>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Model</TableCell>
                <TableCell>Lane</TableCell>
                <TableCell align="right">Slots held</TableCell>
                <TableCell align="right">Expires in</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {reservations.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={4}>
                    <Typography color="text.secondary">No active reservations.</Typography>
                  </TableCell>
                </TableRow>
              ) : (
                reservations.map((r) => (
                  <TableRow key={`${r.model}/${r.lane}`} hover>
                    <TableCell>{r.model}</TableCell>
                    <TableCell>
                      <Chip size="small" color="info" label={r.lane} />
                    </TableCell>
                    <TableCell align="right">{fmtInt(r.slots)}</TableCell>
                    <TableCell align="right">{fmtCountdown(r.expiresAt, nowMs)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Box>

      <Box>
        <Typography variant="h6" gutterBottom>
          Backend Load
        </Typography>
        <TableContainer component={Paper}>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Backend</TableCell>
                <TableCell sx={{ width: 200 }}>Utilization</TableCell>
                <TableCell align="right">Active / Capacity</TableCell>
                <TableCell align="right">Waiting</TableCell>
                <TableCell>By group</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {backends.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5}>
                    <Typography color="text.secondary">No backends under load.</Typography>
                  </TableCell>
                </TableRow>
              ) : (
                backends.map((b) => (
                  <TableRow key={b.backend} hover>
                    <TableCell>{b.backend}</TableCell>
                    <TableCell>
                      <LinearProgress
                        variant="determinate"
                        value={capPct(b.active, b.capacity)}
                        sx={{ height: 8, borderRadius: 1 }}
                      />
                    </TableCell>
                    <TableCell align="right">
                      {fmtInt(b.active)} / {fmtInt(b.capacity)}
                    </TableCell>
                    <TableCell align="right">{fmtInt(b.waiting)}</TableCell>
                    <TableCell>
                      {b.groups.length === 0
                        ? '—'
                        : b.groups
                            .map(
                              (g) =>
                                `${g.group}: ${fmtInt(g.active)}${
                                  Number(g.waiting) > 0 ? ` (+${fmtInt(g.waiting)} q)` : ''
                                }`,
                            )
                            .join(', ')}
                    </TableCell>
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

export const Route = createFileRoute('/lanes')({ component: Lanes })
