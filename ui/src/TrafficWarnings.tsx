import { Chip, Stack, Typography } from '@mui/material'
import { DEFAULT_WINDOW, windowPhrase, type TimeWindow } from '@/TimeWindow'
import { Panel, Row } from '@/Panel'
import { C } from '@/theme'
import { fmtInt } from '@/format'
import { QueueDepthClash, useModelStats } from '@/ModelStats'
import { SlowStretch, Unanswered, unansweredTotals, useTurnedAway } from '@/TurnedAway'

/**
 * WHAT WENT WRONG IN THIS WINDOW, at the top, before anything it went wrong to.
 *
 * Every one of these lines existed already — inside the panel it came from, four
 * or five screens down, next to the table that produced it. That is the failure
 * the report describes: the page "shows everything it knows, in the order the
 * data happens to exist", so a settings clash that is costing callers timeouts
 * sat below a hundred log rows, and the run of forty-second requests sat under a
 * table of who was told to come back.
 *
 * Nothing here is a new measurement. It is the same numbers, lifted to where a
 * person reads first, each one saying whose problem it is and what to do — and
 * each one still has its table further down for whoever wants the rows.
 *
 * It reports and does not judge. Whether four rejections in an hour is bad
 * depends on what the box is for, which the reader knows and this does not.
 */
export function TrafficWarnings({
  window = DEFAULT_WINDOW,
  filterKey,
}: {
  window?: TimeWindow
  filterKey?: string
}) {
  const models = useModelStats(window)
  const away = useTurnedAway({ window, filterKey })

  const util = models.data?.corrallm.utilization?.rows ?? []
  const clashing = util.filter((r) => r.depthUnreachable)
  const turnedAway = util.reduce((n, r) => n + Number(r.turnedAway), 0)
  const notHonored = util.reduce((n, r) => n + Number(r.notHonored), 0)

  const promises = away.data?.corrallm.retryPromises
  const { refused, endedEarly } = unansweredTotals(away.data)
  const spell = promises?.slowSpell
  const slow = Number(spell?.count ?? 0)
  const open = Number(away.data?.corrallm.journeys?.open ?? 0)

  const count =
    (clashing.length > 0 ? 1 : 0) +
    (turnedAway > 0 ? 1 : 0) +
    (refused > 0 || endedEarly > 0 ? 1 : 0) +
    (slow > 0 ? 1 : 0) +
    (open > 0 ? 1 : 0)

  // Both queries still in flight: say nothing rather than "all clear", which
  // would be a verdict delivered before the evidence arrived.
  const settled = !models.isLoading && !away.isLoading

  if (settled && count === 0) {
    return (
      <Panel title="Warnings" badge={<Chip size="small" variant="outlined" label="clear" />} dense>
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          Nothing to flag {windowPhrase(window)} — nobody was turned away or refused, no caller is
          still waiting on an answer, and no run of requests came in much slower than the rest.
        </Typography>
      </Panel>
    )
  }

  return (
    <Panel
      title="Warnings"
      badge={
        <Chip
          size="small"
          color={count > 0 ? 'warning' : 'default'}
          variant={count > 0 ? 'filled' : 'outlined'}
          label={settled ? count : '…'}
        />
      }
      subtitle={`Everything below is drawn from ${windowPhrase(window)} — the tables that produced these lines are further down`}
      flush
    >
      {/* The only one of these with a control, so it goes first: the rest are
          things to know, this is a thing to do. */}
      {clashing.length > 0 && (
        <Row>
          <QueueDepthClash rows={clashing} />
        </Row>
      )}

      {turnedAway > 0 && (
        <Row>
          <Typography variant="body2" sx={{ color: C.warn }}>
            {fmtInt(turnedAway)} request{turnedAway === 1 ? ' was' : 's were'} told to come back
            later — demand that did not fit.
            {notHonored > 0 && (
              <>
                {' '}
                {fmtInt(notHonored)} of those callers never came back, which may mean we drove them
                off rather than delayed them.
              </>
            )}
          </Typography>
          <Typography variant="body2" sx={{ color: C.textMuted, mt: 0.25 }}>
            Who, and how many tries it took them, is in <b>Turned away</b> below.
          </Typography>
        </Row>
      )}

      {(refused > 0 || endedEarly > 0) && (
        <Row>
          <Unanswered refused={refused} endedEarly={endedEarly} />
        </Row>
      )}

      {slow > 0 && (
        <Row>
          <SlowStretch spell={spell} window={window} />
        </Row>
      )}

      {open > 0 && (
        <Row>
          <Stack direction="row" spacing={1} sx={{ alignItems: 'baseline', flexWrap: 'wrap' }}>
            <Typography variant="body2" sx={{ color: C.warn }}>
              {open} caller{open === 1 ? ' is' : 's are'} still without an answer.
            </Typography>
            <Typography variant="body2" sx={{ color: C.textMuted }}>
              Their last attempt was a rejection and they have not got in since — some may still be
              backing off and due to return.
            </Typography>
          </Stack>
        </Row>
      )}
    </Panel>
  )
}
