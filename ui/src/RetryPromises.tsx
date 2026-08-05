import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  Box,
  Chip,
  CircularProgress,
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
import { C } from '@/theme'
import { fmtDuration, fmtTime } from '@/format'

/**
 * Who we told to come back, when they are due, and what they did about it.
 *
 * A 429 is not just a rejection — it is an appointment. The caller was handed a
 * Retry-After and, if they honor it, will return at a time we chose. Until this
 * view the promise went out on the wire and was forgotten: the log could say
 * "we refused this key at 14:03" but never "…and told them 4s", so nobody could
 * see who was already scheduled to come back, or whether the number we gave
 * them bore any relation to reality.
 *
 * The outcome column is the second half of that. `early` in bulk means callers
 * are ignoring Retry-After and hammering; `gone` in bulk means we drove them
 * off. Neither is visible from a 429 count.
 */

const OUTCOME: Record<string, { color: 'default' | 'info' | 'success' | 'warning'; hint: string }> =
  {
    waiting: { color: 'info', hint: 'due back in the future — still owed a slot' },
    honored: { color: 'success', hint: 'came back at or after the time we gave them' },
    early: { color: 'warning', hint: 'came back BEFORE the time we gave them — ignoring Retry-After' },
    gone: { color: 'default', hint: 'the time we gave them has passed and they never returned' },
  }

const PromisesDoc = graphql(/* GraphQL */ `
  query RetryPromises($limit: Long!, $minutes: Long!, $key: String) {
    corrallm {
      retryPromises(limit: $limit, minutes: $minutes, key: $key) {
        waiting
        promises {
          id
          ts
          key
          sourceIp
          served
          reason
          retryAfterMs
          dueMs
          returnedMs
          waitedMs
          state
        }
      }
    }
  }
`)

export function RetryPromises({
  filterKey,
  limit = 50,
  minutes = 60,
}: {
  filterKey?: string
  limit?: number
  minutes?: number
}) {
  const navigate = useNavigate()
  // Keyed under 'activity' so the SSE listener's invalidation reaches it — a
  // promise is made on the same event that writes an activity row.
  const q = useQuery({
    queryKey: ['activity', 'promises', filterKey ?? '', limit, minutes],
    queryFn: () =>
      gqlClient.request(PromisesDoc, {
        limit: String(limit),
        minutes: String(minutes),
        key: filterKey || undefined,
      }),
    refetchInterval: 15000,
  })

  const data = q.data?.corrallm.retryPromises
  const rows = data?.promises ?? []
  const waiting = Number(data?.waiting ?? 0)

  const body = q.isLoading ? (
    <Box sx={{ p: 2 }}>
      <CircularProgress />
    </Box>
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : rows.length === 0 ? (
    <Box sx={{ p: 2 }}>
      <Typography color="text.secondary">
        Nobody has been turned away in the last {minutes} minutes.
      </Typography>
    </Box>
  ) : (
    <TableContainer>
      <Table size="small" stickyHeader>
        <TableHead>
          <TableRow>
            <TableCell>Told at</TableCell>
            <TableCell>Key</TableCell>
            <TableCell>Source</TableCell>
            <TableCell>Served</TableCell>
            <TableCell>Why</TableCell>
            <TableCell align="right">Told to wait</TableCell>
            <TableCell>Due back</TableCell>
            <TableCell align="right">Actually waited</TableCell>
            <TableCell>Outcome</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((p) => {
            const o = OUTCOME[p.state] ?? { color: 'default' as const, hint: p.state }
            return (
              <TableRow
                key={p.id}
                hover
                sx={{ cursor: p.key ? 'pointer' : 'default' }}
                onClick={() =>
                  p.key && navigate({ to: '/activity', search: { key: p.key } })
                }
              >
                <TableCell>{fmtTime(p.ts)}</TableCell>
                <TableCell>{p.key || '—'}</TableCell>
                <TableCell sx={{ fontFamily: 'monospace' }}>{p.sourceIp || '—'}</TableCell>
                <TableCell>{p.served}</TableCell>
                <TableCell>
                  <Chip size="small" variant="outlined" label={p.reason} />
                </TableCell>
                <TableCell align="right">{fmtDuration(p.retryAfterMs)}</TableCell>
                <TableCell>{fmtTime(p.dueMs)}</TableCell>
                <TableCell align="right">
                  {Number(p.waitedMs) > 0 ? (
                    fmtDuration(p.waitedMs)
                  ) : (
                    <span style={{ color: C.textFaint }}>—</span>
                  )}
                </TableCell>
                <TableCell>
                  <Tooltip title={o.hint}>
                    <Chip size="small" color={o.color} label={p.state} />
                  </Tooltip>
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </TableContainer>
  )

  return (
    <Panel
      title="Come back later"
      subtitle={`Callers we turned away in the last ${minutes} minutes, and when we told them to return`}
      badge={
        <Tooltip title="Promises still outstanding: due in the future and not back yet. These are arrivals the queue depth cannot see.">
          <Chip
            size="small"
            variant={waiting > 0 ? 'filled' : 'outlined'}
            color={waiting > 0 ? 'info' : 'default'}
            label={`${waiting} due back`}
          />
        </Tooltip>
      }
      flush
    >
      {body}
    </Panel>
  )
}
