import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Box, Button, Chip } from '@mui/material'
import { ActiveRequests } from '@/ActiveRequests'
import { ActivityLog } from '@/ActivityLog'
import { RetryPromises } from '@/RetryPromises'
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
      {/* In-flight first: the table below only ever holds FINISHED requests. */}
      <ActiveRequests />
      {/* Then the ones we sent away. Both are traffic the completed-request log
          cannot show: one hasn't finished, the other never started. */}
      <RetryPromises filterKey={key} />
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
