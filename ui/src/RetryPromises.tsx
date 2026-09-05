import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { DEFAULT_WINDOW, windowKey, windowPhrase, windowVars, type TimeWindow } from '@/TimeWindow'
import {
  Box,
  Chip,
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
import { fmtDuration, fmtInt, fmtTime } from '@/format'
import { Loading } from '@/Loading'

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
  query RetryPromises($limit: Long!, $minutes: Long!, $key: String, $from: Long, $to: Long) {
    corrallm {
      retryPromises(limit: $limit, minutes: $minutes, key: $key, from: $from, to: $to) {
        unanswered {
          served
          toldToReturn
          refused
          endedEarly
        }
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

/**
 * The two endings a "come back later" list cannot show, said in words.
 *
 * A promise is a request we turned away AND gave a time to. These are the ones
 * we gave nothing: a refusal, where nothing could serve it and no time was
 * offered, and an answer that stopped part-way because the caller's connection
 * went away. Neither is a promise, so neither appeared here — and the panel's
 * empty state cheerfully said nobody was turned away while eight refusals sat in
 * the same window (OB-5: the reader must not be left believing something false).
 */
function Unanswered({ refused, endedEarly }: { refused: number; endedEarly: number }) {
  if (refused === 0 && endedEarly === 0) return null
  const parts = [
    refused > 0
      ? `${fmtInt(refused)} request${refused === 1 ? ' was' : 's were'} refused outright — nothing could serve ${refused === 1 ? 'it' : 'them'}, and no time to come back was offered`
      : '',
    endedEarly > 0
      ? `${fmtInt(endedEarly)} answer${endedEarly === 1 ? '' : 's'} stopped part-way because the caller's connection went away`
      : '',
  ].filter(Boolean)
  return (
    <Box sx={{ px: 2, pb: 2, pt: refused || endedEarly ? 1 : 0 }}>
      <Typography variant="body2" sx={{ color: C.warn }}>
        {parts.join('. ')}. {refused > 0 ? 'A refusal is not a promise: nobody was told when to return.' : ''}
      </Typography>
    </Box>
  )
}

export function RetryPromises({
  filterKey,
  limit = 50,
  window = DEFAULT_WINDOW,
}: {
  filterKey?: string
  limit?: number
  window?: TimeWindow
}) {
  const vars = windowVars(window)
  const navigate = useNavigate()
  // Keyed under 'activity' so the SSE listener's invalidation reaches it — a
  // promise is made on the same event that writes an activity row.
  const q = useQuery({
    queryKey: ['activity', 'promises', filterKey ?? '', limit, windowKey(window)],
    queryFn: () =>
      gqlClient.request(PromisesDoc, {
        limit: String(limit),
        minutes: vars.minutes,
        from: vars.from,
        to: vars.to,
        key: filterKey || undefined,
      }),
    refetchInterval: window.kind === 'absolute' ? false : 15000,
  })

  const data = q.data?.corrallm.retryPromises
  const rows = data?.promises ?? []
  const waiting = Number(data?.waiting ?? 0)

  // EVERY WAY A REQUEST ENDS WITH NOTHING, because the panel used to claim one.
  // "Nobody was turned away" was said on the 429 count alone, so a window holding
  // eight `503 no backend available` reported nobody turned away while eight
  // callers got nothing. A caller scored that sentence 0 — believing something
  // false — and he was right.
  const unanswered = data?.unanswered ?? []
  const refused = unanswered.reduce((n, u) => n + Number(u.refused), 0)
  const endedEarly = unanswered.reduce((n, u) => n + Number(u.endedEarly), 0)

  const body = q.isLoading ? (
    <Loading size={20} minHeight={120} />
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : rows.length === 0 ? (
    <Box sx={{ p: 2 }}>
      <Typography sx={{
        color: "text.secondary"
      }}>
        {refused === 0 && endedEarly === 0
          ? `Everybody got an answer ${windowPhrase(window)} — nobody was told to come back, nothing was refused, and no answer was cut short.`
          : `Nobody was told to come back ${windowPhrase(window)}.`}
      </Typography>
      <Unanswered refused={refused} endedEarly={endedEarly} />
    </Box>
  ) : (
    <Box>
      <Unanswered refused={refused} endedEarly={endedEarly} />
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
                  p.key && navigate({ to: '/traffic', search: { key: p.key } })
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
    </Box>
  )

  return (
    <Panel
      title="Come back later"
      subtitle={`Callers we turned away ${windowPhrase(window)}, and when we told them to return`}
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
