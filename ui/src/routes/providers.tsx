import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /providers is /setup now (P30 phase C).
 *
 * The page already held more than providers — extensions, credentials, the
 * models chosen off each catalog, the fallback lanes — and it has absorbed the
 * two other pages about configuring this box: what its upstreams are allowed
 * (the old /quota) and what changed (the old /history). One page for "what is
 * set up here, and what did I change".
 */
export const Route = createFileRoute('/providers')({
  beforeLoad: () => {
    throw redirect({ to: '/setup' })
  },
})
