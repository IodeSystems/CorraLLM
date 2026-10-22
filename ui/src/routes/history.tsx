import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /history → Settings' History tab.
 *
 * Configuration history was a top-level page, then a panel at the top of Setup,
 * and is now Setup's fourth tab — a revision spans everything on that page, so
 * it belongs beside them rather than under any one of them. The component lives
 * in `@/SetupHistory`; this file is the old address, kept because /history is
 * what is bookmarked and typed from memory.
 *
 * It redirects to the TAB, not to the top of Setup: sending someone who asked
 * for the history to a page where they have to find it again is barely better
 * than a 404.
 */
export const Route = createFileRoute('/history')({
  beforeLoad: () => {
    throw redirect({ to: '/settings', search: { tab: 'history' } })
  },
})
