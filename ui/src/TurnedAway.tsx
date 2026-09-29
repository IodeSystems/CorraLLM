import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { DEFAULT_WINDOW, windowKey, windowPhrase, windowVars, type TimeWindow } from '@/TimeWindow'
import {
  Box,
  Chip,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  ToggleButton,
  ToggleButtonGroup,
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
 * EVERYONE WE TURNED AWAY, on one surface instead of two.
 *
 * "Come back later" and "Journeys" were two panels, one under the other, over
 * the same events: a 429 is a rejection AND an appointment, and a journey is
 * what one caller's run of them looks like from the caller's side. Stacked, they
 * were up to seventy-five rows saying the same thing twice — "right now it's too
 * many of each records" — and the reader had to hold one table in their head to
 * read the other.
 *
 * So: one panel, two readings of it, and BY CALLER is the default because it is
 * the one that answers the question. A rejection count cannot tell one caller
 * refused eight times from eight callers refused once, and those are very
 * different boxes to be a customer of. The per-promise list is still the only
 * place that shows what time we actually named and whether they honoured it, so
 * it is a toggle away rather than deleted.
 *
 * The link in the by-caller view is the ticket: the caller's own request id, or
 * one the proxy minted on the first rejection for the retry to echo. A caller
 * that never echoes it appears as a run of one-attempt journeys — itself worth
 * seeing, because it means the retries are invisible to the scheduler too, so
 * patience is buying that caller nothing.
 */

const OUTCOME: Record<string, { color: 'default' | 'info' | 'success' | 'warning'; hint: string }> =
  {
    waiting: { color: 'info', hint: 'due back in the future — still owed a slot' },
    honored: { color: 'success', hint: 'came back at or after the time we gave them' },
    early: { color: 'warning', hint: 'came back BEFORE the time we gave them — ignoring Retry-After' },
    gone: { color: 'default', hint: 'the time we gave them has passed and they never returned' },
  }

// ONE ROUND TRIP, because one panel. These were two queries under two panels
// that were always rendered together over the same window; merging the panels
// without merging the fetches would have kept the second request and lost the
// reason for it.
const TurnedAwayDoc = graphql(/* GraphQL */ `
  query TurnedAway($limit: Long!, $journeyLimit: Long!, $minutes: Long!, $key: String, $from: Long, $to: Long) {
    corrallm {
      retryPromises(limit: $limit, minutes: $minutes, key: $key, from: $from, to: $to) {
        unanswered {
          served
          toldToReturn
          refused
          endedEarly
        }
        slowSpell {
          multiple
          count
          thresholdMs
          meanMs
          firstMs
          lastMs
          worstMs
          worstAtMs
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
      journeys(limit: $journeyLimit, minutes: $minutes, from: $from, to: $to) {
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

/**
 * The one fetch, exported so the page header can read the same numbers.
 *
 * React Query dedupes on the key, so the warnings strip at the top of Traffic
 * and this panel share a single request rather than each asking for the refusal
 * counts. Two components reading the same window must not be able to disagree
 * about it, which is the whole reason the page has one window in the first place.
 */
export function useTurnedAway({
  window = DEFAULT_WINDOW,
  filterKey,
  limit = 25,
  journeyLimit = 25,
}: {
  window?: TimeWindow
  filterKey?: string
  limit?: number
  journeyLimit?: number
}) {
  const vars = windowVars(window)
  return useQuery({
    // 'activity' prefix so the SSE listener's invalidation reaches it — a
    // promise is made on the same event that writes an activity row.
    queryKey: ['activity', 'turnedAway', filterKey ?? '', limit, journeyLimit, windowKey(window)],
    queryFn: () =>
      gqlClient.request(TurnedAwayDoc, {
        limit: String(limit),
        journeyLimit: String(journeyLimit),
        minutes: vars.minutes,
        from: vars.from,
        to: vars.to,
        key: filterKey || undefined,
      }),
    refetchInterval: window.kind === 'absolute' ? false : 15000,
  })
}

/** Every way a request ended with nothing, summed over the window. */
export function unansweredTotals(
  data: ReturnType<typeof useTurnedAway>['data'],
): { refused: number; endedEarly: number } {
  const rows = data?.corrallm.retryPromises?.unanswered ?? []
  return {
    refused: rows.reduce((n, u) => n + Number(u.refused), 0),
    endedEarly: rows.reduce((n, u) => n + Number(u.endedEarly), 0),
  }
}

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
export function Unanswered({ refused, endedEarly }: { refused: number; endedEarly: number }) {
  if (refused === 0 && endedEarly === 0) return null
  const parts = [
    refused > 0
      ? `${refused.toLocaleString()} request${refused === 1 ? ' was' : 's were'} refused outright — nothing could serve ${refused === 1 ? 'it' : 'them'}, and no time to come back was offered`
      : '',
    endedEarly > 0
      ? `${endedEarly.toLocaleString()} answer${endedEarly === 1 ? '' : 's'} stopped part-way because the caller's connection went away`
      : '',
  ].filter(Boolean)
  return (
    <Typography variant="body2" sx={{ color: C.warn }}>
      {parts.join('. ')}. {refused > 0 ? 'A refusal is not a promise: nobody was told when to return.' : ''}
    </Typography>
  )
}

export type SpellShape = {
  multiple?: number
  count: number | string
  thresholdMs: number | string
  meanMs: number | string
  firstMs: number | string
  lastMs: number | string
  worstMs: number | string
  worstAtMs: number | string
} | null | undefined

/**
 * What the verdict above it leaves out (OB-13).
 *
 * "Everybody got an answer — nobody was told to come back, nothing was refused,
 * and no answer was cut short" was true of an hour in which sixteen requests ran
 * past eleven seconds and the worst took forty. Nothing in that sentence is
 * wrong; it is just not the whole of what happened, and it is the sentence a
 * person reads before they stop reading.
 *
 * It reports and does not judge — this many, this much slower, between these
 * times. Whether that is bad depends on what the box is for, which the reader
 * knows and this does not.
 */
export function SlowStretch({ spell, window }: { spell: SpellShape; window: TimeWindow }) {
  if (!spell || Number(spell.count) < 1) return null
  const count = Number(spell.count)
  const first = Number(spell.firstMs)
  const last = Number(spell.lastMs)
  const worstAt = Number(spell.worstAtMs)
  const clock = (ms: number) =>
    new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  return (
    <Box>
      <Typography variant="body2" sx={{ color: C.warn }}>
        {count === 1 ? 'One request was' : `${count} requests were`} much slower than the rest{' '}
        {windowPhrase(window)}: over {fmtDuration(Number(spell.thresholdMs))}, against an average
        of {fmtDuration(Number(spell.meanMs))}.
        {first > 0 && last > first ? ` Between ${clock(first)} and ${clock(last)}.` : ''}
      </Typography>
      <Typography variant="body2" sx={{ color: C.textMuted, mt: 0.5 }}>
        The slowest took {fmtDuration(Number(spell.worstMs))}
        {worstAt > 0 ? `, at ${clock(worstAt)}` : ''}. Everyone still got an answer — this is
        how long some of them waited for it.
      </Typography>
    </Box>
  )
}

type View = 'callers' | 'promises'

export function TurnedAway({
  window = DEFAULT_WINDOW,
  filterKey,
}: {
  window?: TimeWindow
  filterKey?: string
}) {
  const navigate = useNavigate()
  const [view, setView] = useState<View>('callers')
  const q = useTurnedAway({ window, filterKey })

  const promiseData = q.data?.corrallm.retryPromises
  const journeyData = q.data?.corrallm.journeys
  const promises = promiseData?.promises ?? []
  const waiting = Number(promiseData?.waiting ?? 0)
  const open = Number(journeyData?.open ?? 0)

  // journeys() takes no key argument — it groups by ticket, and a ticket is not
  // a key. Narrowing here is therefore a filter over the window's WORST N
  // journeys, not over all of them, so a quiet caller can be absent from a list
  // that is not empty. The subtitle says so rather than letting the reader
  // conclude this caller never struggled.
  const allJourneys = journeyData?.journeys ?? []
  const journeys = filterKey ? allJourneys.filter((j) => j.key === filterKey) : allJourneys

  const nothing = promises.length === 0 && allJourneys.length === 0

  const callerTable = (
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
          {journeys.map((j) => {
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

  const promiseTable = (
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
          {promises.map((p) => {
            const o = OUTCOME[p.state] ?? { color: 'default' as const, hint: p.state }
            return (
              <TableRow
                key={p.id}
                hover
                sx={{ cursor: p.key ? 'pointer' : 'default' }}
                onClick={() => p.key && navigate({ to: '/traffic', search: { key: p.key } })}
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

  const empty = (text: string) => (
    <Box sx={{ p: 2 }}>
      <Typography variant="body2" sx={{ color: 'text.secondary' }}>
        {text}
      </Typography>
    </Box>
  )

  // Refusals, dropped connections and the slow stretch are NOT rendered here.
  // They are facts about the window rather than about a rejection, and they are
  // the first thing a person needs to see, so they are lifted to the warnings
  // panel at the top of the page — which reads them from this same query.
  const totals = unansweredTotals(q.data)

  const body = q.isLoading ? (
    <Loading size={20} minHeight={120} />
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : nothing ? (
    empty(
      totals.refused === 0 && totals.endedEarly === 0
        ? `Everybody got an answer ${windowPhrase(window)} — nobody was told to come back, nothing was refused, and no answer was cut short.`
        : `Nobody was told to come back ${windowPhrase(window)}.`,
    )
  ) : view === 'callers' ? (
    journeys.length === 0 ? (
      empty(
        filterKey
          ? `${filterKey} is not among the hardest-working callers ${windowPhrase(window)} — switch to "every promise", which does narrow to one key.`
          : `Nobody had to work for an answer ${windowPhrase(window)}.`,
      )
    ) : (
      callerTable
    )
  ) : promises.length === 0 ? (
    empty(`Nobody was told to come back ${windowPhrase(window)}.`)
  ) : (
    promiseTable
  )

  return (
    <Panel
      title="Turned away"
      subtitle={
        view === 'callers'
          ? filterKey
            ? `The window's hardest-working callers, narrowed to ${filterKey} — a quiet caller may not be among them`
            : "One caller's attempts, grouped by ticket — how many rejections it took, and whether they ever got in"
          : `Each rejection we put a time on ${windowPhrase(window)}, and what the caller did about it`
      }
      badge={
        <Stack direction="row" spacing={0.5}>
          <Tooltip title="Callers still without an answer: their last attempt was a rejection and they have not got in since.">
            <Chip
              size="small"
              label={`${open} unanswered`}
              color={open > 0 ? 'warning' : 'default'}
              variant={open > 0 ? 'filled' : 'outlined'}
            />
          </Tooltip>
          <Tooltip title="Promises still outstanding: due in the future and not back yet. These are arrivals the queue depth cannot see.">
            <Chip
              size="small"
              variant={waiting > 0 ? 'filled' : 'outlined'}
              color={waiting > 0 ? 'info' : 'default'}
              label={`${waiting} due back`}
            />
          </Tooltip>
        </Stack>
      }
      actions={
        <ToggleButtonGroup
          size="small"
          exclusive
          value={view}
          onChange={(_, v: View | null) => v && setView(v)}
        >
          <ToggleButton value="callers">by caller</ToggleButton>
          <ToggleButton value="promises">every promise</ToggleButton>
        </ToggleButtonGroup>
      }
      flush
    >
      {body}
    </Panel>
  )
}
