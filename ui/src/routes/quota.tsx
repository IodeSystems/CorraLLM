import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  Alert,
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
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtInt } from '@/format'

/**
 * Free-tier quota ledger (P16): each remote backend's remaining rate-limit
 * budget, learned live from the provider's X-Ratelimit-* headers. The selector
 * routes around a backend that shows unavailable here before it 429s.
 */
const QuotaDoc = graphql(/* GraphQL */ `
  query Quota {
    corrallm {
      quotaLedger {
        backends {
          backend
          available
          coolingInSec
          observedAgoSec
          seen
          windows {
            label
            limit
            used
            resetsIn
          }
          requests {
            limit
            remaining
            cap
            effRemaining
            resetsIn
            stale
          }
          tokens {
            limit
            remaining
            cap
            effRemaining
            resetsIn
            stale
          }
        }
      }
    }
  }
`)

type Bucket = {
  limit: string | number
  remaining: string | number
  cap?: string | number | null
  effRemaining?: string | number | null
  resetsIn?: string | null
  stale?: boolean | null
}

const n = (v: unknown): number => Number(v ?? 0) || 0

/** A budget bar: effective (capped) remaining over the ceiling that governs it. */
function BucketCell({ b }: { b: Bucket }) {
  const capped = n(b.cap) > 0 && n(b.cap) < n(b.limit)
  // The number availability is judged on: effective remaining when capped, else
  // the provider's own remaining.
  const rem = capped ? n(b.effRemaining) : n(b.remaining)
  const ceil = capped ? n(b.cap) : n(b.limit)
  const pct = ceil > 0 ? Math.max(0, Math.min(100, (rem / ceil) * 100)) : 0
  const color = pct > 40 ? 'success' : pct > 10 ? 'warning' : 'error'
  return (
    <Box sx={{ minWidth: 160 }}>
      <Box sx={{ display: 'flex', justifyContent: 'space-between', mb: 0.3 }}>
        <Typography variant="caption">
          {fmtInt(rem)} / {fmtInt(ceil)}
          {capped && (
            <Tooltip title={`Self-capped at ${fmtInt(n(b.cap))} of the provider's ${fmtInt(n(b.limit))}`}>
              <Chip size="small" label="cap" sx={{ ml: 0.5, height: 16 }} />
            </Tooltip>
          )}
        </Typography>
        <Typography variant="caption" color="text.secondary">
          {b.stale ? 'window rolled' : b.resetsIn ? `resets ${b.resetsIn}` : ''}
        </Typography>
      </Box>
      <LinearProgress variant="determinate" value={pct} color={color} />
    </Box>
  )
}

function QuotaPage() {
  const q = useQuery({
    queryKey: ['quota'],
    queryFn: () => gqlClient.request(QuotaDoc),
    refetchInterval: 4000,
  })
  if (q.isLoading) return <Box sx={{ p: 3 }}><CircularProgress /></Box>
  const backends = q.data?.corrallm?.quotaLedger?.backends ?? []

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Typography variant="h5">Free-tier quota</Typography>
      <Typography variant="body2" color="text.secondary">
        Per-backend budget learned live from each provider's rate-limit headers. A backend goes{' '}
        <b>unavailable</b> when exhausted or cooling from a 429, and the free lane routes around it.
        Counts are a snapshot from the last call — <i>observed N ago</i> — not a live tick.
      </Typography>

      {backends.length === 0 ? (
        <Alert severity="info">
          No remote free backend has been called yet. The ledger learns a backend's budget from its
          first response, so it stays empty until traffic flows.
        </Alert>
      ) : (
        <TableContainer component={Paper}>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Backend</TableCell>
                <TableCell>Status</TableCell>
                <TableCell>Requests</TableCell>
                <TableCell>Tokens</TableCell>
                <TableCell align="right">Seen</TableCell>
                <TableCell align="right">Observed</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {backends.map((be) => (
                <TableRow key={be.backend} hover>
                  <TableCell sx={{ fontFamily: 'monospace' }}>{be.backend}</TableCell>
                  <TableCell>
                    {be.available ? (
                      <Chip size="small" color="success" label="available" />
                    ) : n(be.coolingInSec) > 0 ? (
                      <Tooltip title="Cooling down after a 429">
                        <Chip size="small" color="error" label={`cooling ${be.coolingInSec}s`} />
                      </Tooltip>
                    ) : (
                      <Chip size="small" color="warning" label="exhausted" />
                    )}
                  </TableCell>
                  <TableCell>
                    {(be.windows ?? []).length > 0 ? (
                      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.3 }}>
                        {(be.windows ?? []).map((w) => (
                          <Typography key={w.label} variant="caption">
                            {w.used}/{w.limit} /{w.label}
                            {w.resetsIn ? ` · resets ${w.resetsIn}` : ''}
                          </Typography>
                        ))}
                        <Typography variant="caption" color="text.secondary">counter-mode</Typography>
                      </Box>
                    ) : (
                      <BucketCell b={be.requests} />
                    )}
                  </TableCell>
                  <TableCell>
                    {(be.windows ?? []).length > 0 ? (
                      <Typography variant="caption" color="text.secondary">—</Typography>
                    ) : (
                      <BucketCell b={be.tokens} />
                    )}
                  </TableCell>
                  <TableCell align="right">{fmtInt(n(be.seen))}</TableCell>
                  <TableCell align="right">
                    <Typography variant="caption" color="text.secondary">
                      {n(be.observedAgoSec)}s ago
                    </Typography>
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

export const Route = createFileRoute('/quota')({ component: QuotaPage })
