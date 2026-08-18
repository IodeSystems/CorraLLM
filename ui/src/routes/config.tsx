import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /config → /hosts.
 *
 * The page was named after a FILE, and it accumulated everything that lived in
 * that file: hosts, agents, models, lanes, extensions, priority groups. Those
 * have gone to the pages they belong to; what is left is what a machine is, so
 * the route says so.
 *
 * Kept as a redirect rather than deleted because /config is what is bookmarked,
 * linked from notes, and typed from memory — and because the file it was named
 * after is on its way out, so this name would be a lie twice over.
 */
export const Route = createFileRoute('/config')({
  beforeLoad: () => {
    throw redirect({ to: '/hosts' })
  },
})
