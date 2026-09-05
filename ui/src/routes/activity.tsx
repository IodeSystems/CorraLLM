import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /activity moved into /traffic (P30 phase B).
 *
 * It was six panels about traffic, beside a /usage page of seven more about the
 * same traffic, each measuring its own window. They are one page now, under one
 * time control. The address is kept — links live in notes and chat history, and
 * a 404 for a page that moved is the worst possible answer.
 *
 * The `?key=` filter survives the redirect: it is how the Keys pages hand a
 * caller over, and dropping it would land somebody on everyone's traffic while
 * they were looking at one caller's.
 */
export const Route = createFileRoute('/activity')({
  validateSearch: (s: Record<string, unknown>): { key?: string } => ({
    key: typeof s.key === 'string' && s.key ? s.key : undefined,
  }),
  beforeLoad: ({ search }) => {
    throw redirect({ to: '/traffic', search: search.key ? { key: search.key } : {} })
  },
})
