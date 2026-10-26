import { createFileRoute, redirect } from '@tanstack/react-router'

/** /requests is /approvals now: "requests" also means API calls on this dashboard. */
export const Route = createFileRoute('/requests')({
  beforeLoad: () => {
    throw redirect({ to: '/approvals' })
  },
})
