import { useState } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Box, Button, Chip, Typography } from '@mui/material'
import { PageHeader } from '@/Panel'
import { ActivityLog } from '@/ActivityLog'
import { ActiveRequests } from '@/ActiveRequests'
import { ModelStats } from '@/ModelStats'
import { ServiceProfiles } from '@/ServiceProfiles'
import { TrafficWarnings } from '@/TrafficWarnings'
import { TurnedAway } from '@/TurnedAway'
import { CallerSpend, MoreCharts, SpeedCacheHeadline, TrafficHeadline } from '@/routes/usage'
import { TimeWindowPicker, windowPhrase, type TimeWindow } from '@/TimeWindow'
import { C } from '@/theme'

/**
 * TRAFFIC — what this box has been doing, over a span you choose.
 *
 * The merge of `/activity` and `/usage` (P30 phase B), reordered around what a
 * person opens it for (P30 wave 1). It had become eight panels in the order the
 * data happened to exist: six unbounded lists, each fetched separately, with the
 * charts and the live sessions buried under two tables of rejections and a
 * hundred log rows. "This page looks like it's way too long to find what a user
 * wants."
 *
 * The page now has a HEADER and a BODY, and the split is the point.
 *
 *   Header — what you need without scrolling. Anything wrong, the window's five
 *   totals, two charts, and who is being served right now. Same rule the home
 *   screen already follows: faults first, then what it is getting done.
 *
 *   Body — one panel per question, in the order the questions get asked. What
 *   each model did and how fast it did it; the requests themselves, paged; who
 *   we turned away; who spent the money.
 *
 * Under it, collapsed, the queueing diagnostics — the per-caller service
 * distributions and six charts about the scheduler's own behaviour. Not deleted:
 * they explain the waits above, and somebody investigating a bad hour needs
 * them. They are just not what the page is for, and they are the two heaviest
 * aggregations on it, so they are not fetched until asked for.
 *
 * WINDOW IN THE URL, so it can be sent to somebody. "Look at nine this morning"
 * is a link, not an instruction to click four things. State that only exists in
 * a component cannot be shared or repeated.
 */
function Traffic() {
  const { key, from, to, minutes } = Route.useSearch()
  const navigate = useNavigate()
  const [detail, setDetail] = useState(false)

  const window: TimeWindow =
    from && to ? { kind: 'absolute', fromMS: from, toMS: to } : { kind: 'relative', minutes: minutes ?? 60 }
  const setWindow = (w: TimeWindow) =>
    navigate({
      to: '/traffic',
      search: {
        ...(key ? { key } : {}),
        ...(w.kind === 'absolute' ? { from: w.fromMS, to: w.toMS } : { minutes: w.minutes }),
      },
    })

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Traffic">
        <TimeWindowPicker value={window} onChange={setWindow} />
      </PageHeader>

      {key && (
        <Box>
          <Chip
            size="small"
            label={`caller: ${key}`}
            onDelete={() => navigate({ to: '/traffic', search: {} })}
          />
        </Box>
      )}

      {/* ── Header ───────────────────────────────────────────────────────── */}

      {/* Anything wrong comes first, with the sentence and the button, not the
          table. Each line is lifted from the panel that measured it — the panels
          are still below for whoever wants the rows. */}
      <TrafficWarnings window={window} filterKey={key} />

      {/* What it did and what it cost: five totals and two charts. */}
      <TrafficHeadline window={window} />

      {/* How fast and how warm: per-request speed and cache distributions, global
          and — when more than one model served — per model. */}
      <SpeedCacheHeadline window={window} />

      {/* Who is being served RIGHT NOW. It is also on the home screen, which is
          the page that answers "right now" — but a person watching a bad hour on
          this page should not have to leave it to see whether anything is still
          moving. One component, so the two cannot drift; compact here, because
          on this page it is a header and not the subject. */}
      <ActiveRequests compact />

      {/* ── Body ─────────────────────────────────────────────────────────── */}

      {/* One row per model: requests, tp/s and tg/s, tokens processed, generated
          and cached, cost, and what reaching it cost callers. */}
      <ModelStats window={window} />

      {/* The requests themselves, a page at a time. */}
      <ActivityLog
        title="Recent requests"
        window={window}
        filterKey={key}
        subtitle={
          key
            ? `Everything ${key} ran ${windowPhrase(window)} — click a row for payloads`
            : `Completed requests ${windowPhrase(window)}, newest first — click a row for payloads`
        }
      />

      {/* The ones we sent away — traffic the completed-request log cannot show,
          because they never started. One panel, two readings: by caller, and
          promise by promise. */}
      <TurnedAway window={window} filterKey={key} />

      <CallerSpend window={window} />

      {/* ── Diagnostics, on request ──────────────────────────────────────── */}

      {/* A LABEL, not a panel. What it holds is panels, and a card inside a card
          is how a dense dashboard turns to mud (Panel.tsx). Same overline the
          chart groups already use. */}
      <Box>
        <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1.5, mb: 1, flexWrap: 'wrap' }}>
          <Typography variant="overline" sx={{ color: C.textMuted }}>
            Queueing detail
          </Typography>
          <Button size="small" onClick={() => setDetail((d) => !d)} sx={{ textTransform: 'none' }}>
            {detail ? 'hide' : 'show'}
          </Button>
          <Typography variant="caption" sx={{ color: C.textFaint }}>
            Why the waits above are what they are — per-caller service distributions, and the
            scheduler&apos;s own behaviour over time. The two heaviest queries on the page, so
            neither runs until you ask.
          </Typography>
        </Box>
        {/* Mounted only when open, which is also when either query is issued.
            Paying for both on every load was a real part of "this page takes
            forever to load". */}
        {detail && (
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
            <ServiceProfiles window={window} />
            <MoreCharts window={window} />
          </Box>
        )}
      </Box>
    </Box>
  )
}

export const Route = createFileRoute('/traffic')({
  validateSearch: (
    s: Record<string, unknown>,
  ): { key?: string; from?: number; to?: number; minutes?: number } => {
    const num = (v: unknown) => {
      const n = Number(v)
      return Number.isFinite(n) && n > 0 ? n : undefined
    }
    return {
      key: typeof s.key === 'string' && s.key ? s.key : undefined,
      from: num(s.from),
      to: num(s.to),
      minutes: num(s.minutes),
    }
  },
  component: Traffic,
})
