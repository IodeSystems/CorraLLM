import { createFileRoute, redirect } from '@tanstack/react-router'

/** One caller's page moved with /keys → /callers (P30 phase C). */
export const Route = createFileRoute('/keys_/$key')({
  beforeLoad: ({ params }) => {
    throw redirect({ to: '/callers/$key', params: { key: params.key } })
  },
})
