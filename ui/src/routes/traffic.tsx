import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Box, Chip } from '@mui/material'
import { PageHeader } from '@/Panel'
import { ActivityLog } from '@/ActivityLog'
import { RetryPromises } from '@/RetryPromises'
import { Journeys } from '@/Journeys'
import { ServiceProfiles } from '@/ServiceProfiles'
import { Utilization } from '@/Utilization'
import { UsagePanels } from '@/routes/usage'
import { TimeWindowPicker, windowPhrase, type TimeWindow } from '@/TimeWindow'

/**
 * TRAFFIC — what this box has been doing, over a span you choose.
 *
 * The merge of `/activity` and `/usage` (P30 phase B). They were one subject on
 * two pages: per-key cost, per-model dwell and backend pressure were rendered on
 * both, each over its OWN window — 60 minutes here, 24 hours there, a 6h/24h/7d
 * toggle on the charts, the newest hundred rows in the log. So the two pages
 * disagreed about the same caller, nothing said why, and neither could answer the
 * question a person actually arrives with: "was it slow at nine this morning, and
 * is it still?"
 *
 * One window, at the top, obeyed by every panel. The order is the order the
 * questions get asked: what is under pressure, who we turned away, who had to
 * work for an answer, how long each caller holds a slot, what it cost, and then
 * the log itself.
 *
 * IN-FLIGHT IS NOT HERE. It lives on the home screen, which is the page that
 * answers "right now" (P30 phase A). Everything on this page has stopped moving.
 */
function Traffic() {
  const { key, from, to, minutes } = Route.useSearch()
  const navigate = useNavigate()

  // THE WINDOW LIVES IN THE URL, so it can be sent to somebody. "Look at nine
  // this morning" is a link, not an instruction to click four things — and it is
  // the same reason the walk that measures this page can photograph a fixed span
  // at all. State that only exists in a component cannot be shared or repeated.
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

      {/* What is under pressure, and what that pressure cost callers. */}
      <Utilization window={window} />
      {/* The ones we sent away — traffic the completed-request log cannot show,
          because they never started. */}
      <RetryPromises filterKey={key} window={window} />
      {/* The same rejections, grouped by who took them: one caller refused four
          times reads very differently from four callers refused once. */}
      <Journeys window={window} />
      {/* Why the estimates above run short: one dwell EWMA per backend, averaging
          over callers whose work differs several-fold in cost and variability. */}
      <ServiceProfiles window={window} />

      {/* What it cost, and who spent it — the old /usage page, same window. */}
      <UsagePanels window={window} />

      <ActivityLog
        filterKey={key}
        window={window}
        subtitle={
          key
            ? `Everything ${key} ran ${windowPhrase(window)} — click a row for payloads`
            : `Completed requests ${windowPhrase(window)}, newest first — click a row for payloads`
        }
      />
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
