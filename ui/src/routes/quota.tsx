import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /quota → Settings' Budgets tab.
 *
 * "Quota" is the word a person uses for "am I about to run out", and three
 * Lenny runs went looking under it for exactly that and found a ledger of
 * remote providers' rate limits instead. The page was honest; its NAME answered
 * a different question. It is called Budgets now, and it is a tab of Setup
 * beside the providers it describes. The component lives in `@/SetupBudgets`.
 *
 * The old address is kept and points at the tab rather than the top of Setup.
 */
export const Route = createFileRoute('/quota')({
  beforeLoad: () => {
    throw redirect({ to: '/settings', search: { tab: 'budgets' } })
  },
})
