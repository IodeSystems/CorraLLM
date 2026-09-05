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
import { fmtDuration, fmtTime } from '@/format'
import { Loading } from '@/Loading'

/**
 * One caller's ATTEMPT to get served, across every rejection it took.
 *
 * A rejection count cannot say whether it was one caller refused eight times or
 * eight callers refused once, and those are very different boxes to be a
 * customer of. The panel above lists rejections; this one lists the people
 * behind them.
 *
 * The link is the ticket: the caller's own request id, or one the proxy minted
 * on the first rejection and handed back for the retry to echo. A caller that
 * never echoes it appears here as a run of one-attempt journeys, which is
 * itself worth seeing — it means the retries are invisible to the scheduler
 * too, so patience is buying that caller nothing.
 */

const JourneysDoc = graphql(/* GraphQL */ `
  query Journeys($limit: Long!, $minutes: Long!, $from: Long, $to: Long) {
    corrallm {
      journeys(limit: $limit, minutes: $minutes, from: $from, to: $to) {
        open
        journeys {
          ticket
          key
          served
          firstMs
          lastMs
          attempts
          rejections
          succeeded
          waitedMs
        }
      }
    }
  }
`)

export function Journeys({ window = DEFAULT_WINDOW, limit = 25 }: { window?: TimeWindow; limit?: number }) {
  const vars = windowVars(window)
  const q = useQuery({
    // 'activity' prefix so the SSE listener's invalidation reaches it.
    queryKey: ['activity', 'journeys', windowKey(window), limit],
    queryFn: () =>
      gqlClient.request(JourneysDoc, {
        minutes: vars.minutes,
        from: vars.from,
        to: vars.to,
        limit: String(limit),
      }),
    refetchInterval: window.kind === 'absolute' ? false : 15000,
  })

  const data = q.data?.corrallm?.journeys
  const rows = data?.journeys ?? []
  const open = Number(data?.open ?? 0)

  const body = q.isLoading ? (
    <Loading size={20} minHeight={120} />
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : rows.length === 0 ? (
    <Box sx={{ p: 2 }}>
      <Typography variant="body2" sx={{
        color: "text.secondary"
      }}>
        Nobody had to work for an answer {windowPhrase(window)}.
      </Typography>
    </Box>
  ) : (
    <TableContainer>
      <Table size="small" stickyHeader>
        <TableHead>
          <TableRow>
            <TableCell>Caller</TableCell>
            <TableCell>Asked for</TableCell>
            <TableCell align="right">
              <Tooltip title="Requests made on this ticket, and how many of them we turned away. 4 attempts / 3 rejections is a caller who got in on the fourth try.">
                <span>Attempts</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Wall clock from the first attempt to the last — what the caller actually spent getting served, backoff included. Much larger than any single request's queue wait.">
                <span>Spent</span>
              </Tooltip>
            </TableCell>
            <TableCell>Started</TableCell>
            <TableCell>Outcome</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((j) => {
            const attempts = Number(j.attempts)
            const rejections = Number(j.rejections)
            return (
              <TableRow key={j.ticket} hover>
                <TableCell>
                  {j.key || <span style={{ color: C.textFaint }}>unkeyed</span>}
                  <Tooltip title={`ticket ${j.ticket}`}>
                    <Box
                      component="span"
                      sx={{ ml: 1, fontFamily: 'monospace', fontSize: 11, color: C.textFaint }}
                    >
                      {String(j.ticket).slice(0, 12)}
                    </Box>
                  </Tooltip>
                </TableCell>
                <TableCell>{j.served}</TableCell>
                <TableCell align="right">
                  {attempts}
                  <span style={{ color: rejections > 0 ? C.warn : C.textFaint }}>
                    {' / '}
                    {rejections}
                  </span>
                </TableCell>
                <TableCell align="right">{fmtDuration(Number(j.waitedMs))}</TableCell>
                <TableCell>{fmtTime(Number(j.firstMs))}</TableCell>
                <TableCell>
                  {j.succeeded ? (
                    <Chip size="small" color="success" variant="outlined" label="served" />
                  ) : (
                    // Not "failed": the caller may still be backing off and due
                    // to return. What is certain is that we have not answered.
                    <Chip size="small" color="warning" variant="outlined" label="no answer yet" />
                  )}
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
      title="Journeys"
      badge={<Chip size="small" label={`${open} unanswered`} color={open > 0 ? 'warning' : 'default'} />}
      subtitle="One caller's attempts, grouped by ticket — how many rejections it took, and whether they ever got in"
      flush
    >
      {body}
    </Panel>
  )
}
