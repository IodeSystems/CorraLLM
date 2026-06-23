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
import { fmtBytes, fmtInt, fmtTime } from '@/format'

const UsageDoc = graphql(/* GraphQL */ `
  query Usage {
    corrallm {
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
    refetchInterval: 2000,
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

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
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
