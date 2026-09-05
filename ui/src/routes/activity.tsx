import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Box, Button, Chip } from '@mui/material'
import { ActivityLog } from '@/ActivityLog'
import { RetryPromises } from '@/RetryPromises'
import { Journeys } from '@/Journeys'
import { Utilization } from '@/Utilization'
import { ServiceProfiles } from '@/ServiceProfiles'
import { PageHeader } from '@/Panel'

/**
 * The whole box's request log, optionally narrowed to one caller.
 *
 * The `key` search param is what a per-key page links to, so a filtered view is
 * a URL you can share rather than UI state you have to reproduce by clicking.
 * The table itself lives in ActivityLog — the key detail page renders the same
 * one.
 */
type Search = { key?: string }

function Activity() {
  const { key } = Route.useSearch()
  const navigate = useNavigate()

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Activity" />
      {/* The summary line for everything below it: which models are under
          pressure, and what that pressure has cost callers. */}
      <Utilization />
      {/* IN-FLIGHT LIVES ON THE HOME SCREEN, and only there (P30 phase A). The
          same live table used to render on both pages, so two screens showed the
          same requests and neither was the authority — and a person who found one
          had no way to know the other existed. Everything below this line is
          traffic that has stopped moving: requests we sent away, and requests
          that finished. */}
      {/* The ones we sent away — traffic the completed-request log cannot show,
          because they never started. */}
      <RetryPromises filterKey={key} />
      {/* The same rejections, grouped by who took them: one caller refused four
          times reads very differently from four callers refused once. */}
      <Journeys />
      {/* Why the estimates above run short: one dwell EWMA per backend, averaging
          over callers whose work differs several-fold in cost and variability. */}
      <ServiceProfiles />
      <ActivityLog
        filterKey={key}
        subtitle={
          key
            ? `Completed requests from ${key}, newest first — click a row for payloads`
            : 'Completed requests, newest first — click a row for payloads'
        }
        action={
          key ? (
            <>
              <Chip
                size="small"
                color="primary"
                label={`key: ${key}`}
                onDelete={() => navigate({ to: '/activity', search: {} })}
              />
              <Button size="small" onClick={() => navigate({ to: '/keys/$key', params: { key } })}>
                Key detail
              </Button>
            </>
          ) : undefined
        }
      />
    </Box>
  )
}

export const Route = createFileRoute('/activity')({
  component: Activity,
  validateSearch: (s: Record<string, unknown>): Search => ({
    key: typeof s.key === 'string' && s.key ? s.key : undefined,
  }),
})
