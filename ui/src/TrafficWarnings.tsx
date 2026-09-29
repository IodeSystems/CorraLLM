import type React from 'react'
import { Box, Chip, Stack, Typography } from '@mui/material'
import { DEFAULT_WINDOW, windowPhrase, type TimeWindow } from '@/TimeWindow'
import { Panel, Row } from '@/Panel'
import { C } from '@/theme'
import { fmtInt } from '@/format'
import { QueueDepthClash, useModelStats } from '@/ModelStats'
import { SlowStretch, Unanswered, unansweredTotals, useTurnedAway } from '@/TurnedAway'
import { HiddenWarnings, SuppressButton, ThresholdButton, useWarningPrefs } from '@/WarningControls'

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
 * depends on what the box is for, which the reader knows and this does not —
 * so every line can be hidden for a while, and every count can be given a
 * threshold it must pass. Both change only this panel; the tables below it
 * still show every row.
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
  const prefs = useWarningPrefs()

  const util = models.data?.corrallm.utilization?.rows ?? []
  const clashing = util.filter(
    (r) => r.depthUnreachable && !prefs.suppressed('queue-clash', r.served),
  )
  const turnedAway = util.reduce((n, r) => n + Number(r.turnedAway), 0)
  const notHonored = util.reduce((n, r) => n + Number(r.notHonored), 0)

  const promises = away.data?.corrallm.retryPromises
  const { refused, endedEarly } = unansweredTotals(away.data)
  const spell = promises?.slowSpell
  const slow = Number(spell?.count ?? 0)
  const open = Number(away.data?.corrallm.journeys?.open ?? 0)

  // A count threshold of N means "warn only above N"; 0 is the default, which
  // warns on the first one. The slow-stretch threshold is a multiple of the
  // mean and is applied server-side, so `slow` already honours it.
  const shows = (kind: 'turned-away' | 'unanswered' | 'open-callers', n: number) =>
    n > prefs.threshold(kind) && !prefs.suppressed(kind)
  const showTurned = shows('turned-away', turnedAway)
  const showUnanswered = shows('unanswered', refused + endedEarly)
  const showOpen = shows('open-callers', open)
  const showSlow = slow > 0 && !prefs.suppressed('slow-stretch')

  const count =
    (clashing.length > 0 ? 1 : 0) +
    (showTurned ? 1 : 0) +
    (showUnanswered ? 1 : 0) +
    (showSlow ? 1 : 0) +
    (showOpen ? 1 : 0)

  // Queries still in flight: say nothing rather than "all clear", which would
  // be a verdict delivered before the evidence arrived.
  const settled = !models.isLoading && !away.isLoading && !prefs.isLoading

  if (settled && count === 0) {
    return (
      <Panel title="Warnings" badge={<Chip size="small" variant="outlined" label="clear" />} dense>
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          {prefs.prefs.length === 0
            ? `Nothing to flag ${windowPhrase(window)} — nobody was turned away or refused, no caller is still waiting on an answer, and no run of requests came in much slower than the rest.`
            : `Nothing to flag ${windowPhrase(window)} past the thresholds and hidden warnings set here.`}
        </Typography>
        <HiddenWarnings prefs={prefs.prefs} />
      </Panel>
    )
  }

  const countThreshold = (
    kind: 'turned-away' | 'unanswered' | 'open-callers',
    what: string,
  ) => (
    <ThresholdButton
      kind={kind}
      current={prefs.threshold(kind)}
      isDefault={prefs.threshold(kind) === 0}
      label={`Warn only above this many ${what}`}
    />
  )
  const controls = (children: React.ReactNode) => (
    <Box sx={{ display: 'flex', gap: 1, alignItems: 'center', mt: 0.5, flexWrap: 'wrap' }}>
      {children}
    </Box>
  )

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

      {showTurned && (
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
          {controls(
            <>
              {countThreshold('turned-away', 'requests')}
              <SuppressButton kind="turned-away" />
            </>,
          )}
        </Row>
      )}

      {showUnanswered && (
        <Row>
          <Unanswered refused={refused} endedEarly={endedEarly} />
          {controls(
            <>
              {countThreshold('unanswered', 'refused or cut-short requests')}
              <SuppressButton kind="unanswered" />
            </>,
          )}
        </Row>
      )}

      {showSlow && (
        <Row>
          <SlowStretch spell={spell} window={window} />
          {controls(
            <>
              <ThresholdButton
                kind="slow-stretch"
                current={Number(spell?.multiple ?? prefs.slowStretchDefault)}
                isDefault={prefs.threshold('slow-stretch') === 0}
                label="Times the average that counts as slow"
                min={1}
                minExclusive
                step={0.5}
              />
              <SuppressButton kind="slow-stretch" />
            </>,
          )}
        </Row>
      )}

      {showOpen && (
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
          {controls(
            <>
              {countThreshold('open-callers', 'callers')}
              <SuppressButton kind="open-callers" />
            </>,
          )}
        </Row>
      )}

      {prefs.prefs.length > 0 && (
        <Row>
          <HiddenWarnings prefs={prefs.prefs} />
        </Row>
      )}
    </Panel>
  )
}
