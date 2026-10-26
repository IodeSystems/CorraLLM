import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Box, Tab, Tabs, Typography } from '@mui/material'
import { PageHeader } from '@/Panel'
import { ProviderBudgets, RefusingBanner } from '@/SetupBudgets'
import { HistoryPanels } from '@/SetupHistory'
import { WarningSettings } from '@/WarningControls'
import { AccessTab } from '@/SetupAccess'
import { RequestRulesTab } from '@/RequestRules'
import { useReadOnly } from '@/auth'

/**
 * Settings: what this box allows, and what changed.
 *
 * It was Setup, which also held Providers and Models tabs — beside a page
 * called Models. Those two are /models now, one page grouped by provider, and
 * /setup redirects there. What stayed here is everything that is not a model:
 *
 *   Access     who may sign in, with what role, through which provider, and
 *              whether the admin token is still accepted. Admin only, so it is
 *              not offered to a viewer.
 *   Budgets    what the upstreams have left. Read, not edited — and the only
 *              4-second poll on the page, which runs only while it is open.
 *   Warnings   what Traffic's Warnings panel holds back: thresholds and hides.
 *              The panel itself shows a setting only while it is holding a
 *              warning back, so this is the one place they are all listed.
 *   History    what changed, and the undo.
 *
 * THE TAB IS IN THE URL (?tab=history), which is what makes one linkable, back-
 * buttonable and able to survive a reload. /history and /quota, which were
 * pages before they were tabs, redirect to their tab.
 *
 * What did NOT go behind a tab: a backend refusing every request. That is a
 * fault, and a fault inside a tab is a fault nobody sees, so it is a banner
 * above the tab bar on every tab (and on /models).
 */
const TABS = [
  { key: 'access', label: 'Access' },
  { key: 'budgets', label: 'Budgets' },
  { key: 'requests', label: 'Approvals' },
  { key: 'warnings', label: 'Warnings' },
  { key: 'history', label: 'History' },
] as const

type TabKey = (typeof TABS)[number]['key']

function isTab(v: string | undefined): v is TabKey {
  return TABS.some((t) => t.key === v)
}

function SettingsPage() {
  const navigate = useNavigate()
  const { tab: tabParam } = Route.useSearch()
  const readOnly = useReadOnly()
  const tabs = TABS.filter((t) => !(readOnly && (t.key === 'access' || t.key === 'requests')))
  const tab: TabKey = isTab(tabParam) && tabs.some((t) => t.key === tabParam) ? tabParam : tabs[0].key
  // Replaces rather than pushes: a tab is where you ARE on this page, not a
  // place you went, and eight tab clicks should not be eight presses of Back to
  // leave the page.
  const goTab = (t: TabKey) => navigate({ to: '/settings', search: { tab: t }, replace: true })

  return (
    <Box sx={{ p: 2, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <PageHeader title="Settings">
        <Typography variant="body2" sx={{ opacity: 0.75 }}>
          {readOnly ? 'Read-only: your role cannot change settings.' : 'Changes apply to the next request.'}
        </Typography>
      </PageHeader>

      {/* Above the tabs on purpose — see the file comment. */}
      <RefusingBanner />

      <Tabs value={tab} onChange={(_, v: TabKey) => goTab(v)} sx={{ mb: 1 }}>
        {tabs.map((t) => (
          <Tab key={t.key} value={t.key} label={t.label} />
        ))}
      </Tabs>

      {/* Mounted one at a time, not hidden with CSS: an unmounted tab stops
          polling, and Budgets polls every four seconds. */}
      {tab === 'access' && <AccessTab />}
      {tab === 'budgets' && <ProviderBudgets />}
      {tab === 'requests' && <RequestRulesTab />}
      {tab === 'warnings' && <WarningSettings />}
      {tab === 'history' && <HistoryPanels />}
    </Box>
  )
}

export const Route = createFileRoute('/settings')({
  validateSearch: (s: Record<string, unknown>): { tab?: string } => ({
    tab: s.tab ? String(s.tab) : undefined,
  }),
  component: SettingsPage,
})
