import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /setup → /models or /settings.
 *
 * Setup had a Providers tab and a Models tab beside a page called Models, so
 * "where do I add a model" had two answers and "what is this model's state" had
 * a third. Providers and models are one page now (/models, grouped by
 * provider); what Setup had left — access, budgets, warnings, history — is
 * /settings.
 *
 * Kept as a redirect because /setup is bookmarked and linked. A tab that moved
 * goes to where it went, not to the top of a page that no longer has it.
 */
export const Route = createFileRoute('/setup')({
  validateSearch: (s: Record<string, unknown>): { tab?: string } => ({
    tab: s.tab ? String(s.tab) : undefined,
  }),
  beforeLoad: ({ search }) => {
    if (!search.tab || search.tab === 'providers' || search.tab === 'models') {
      throw redirect({ to: '/models' })
    }
    throw redirect({ to: '/settings', search: { tab: search.tab } })
  },
})
