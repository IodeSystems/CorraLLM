import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /keys is /callers now (P30 phase C).
 *
 * The page was named after the object it listed; a person arrives asking about
 * the PEOPLE calling this box, not about key strings. It absorbed /groups with
 * the rename — a key maps to exactly one group, and the weight that decides who
 * wins under contention lives there, so "who is calling and what are they
 * allowed" used to be two pages with the answer on neither.
 */
export const Route = createFileRoute('/keys')({
  beforeLoad: () => {
    throw redirect({ to: '/callers' })
  },
})
