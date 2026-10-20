import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { Box, Button, Tab, Tabs, Typography } from '@mui/material'
import AddIcon from '@mui/icons-material/Add'
import { PageHeader } from '@/Panel'
import { ProvidersTab } from '@/SetupProviders'
import { ModelsTab } from '@/SetupModels'
import { ProviderBudgets, RefusingBanner } from '@/SetupBudgets'
import { HistoryPanels } from '@/SetupHistory'
import { WarningSettings } from '@/WarningControls'
import { AddModelDialog } from '@/AddModelDialog'

/**
 * Setup: five questions, five tabs, one page.
 *
 * It was 1020 lines of stacked panels and no tabs — every integration, every
 * local model, every catalogue selection, every lane, the credential store, the
 * upstream budget ledger and the whole configuration history, in one scroll, in
 * the order the code happened to declare them. "Way too long to find what a
 * user wants."
 *
 * The grouping is by the question being asked, not by the shape of the data:
 *
 *   Providers  where models come from, and what we authenticate as. The
 *              integrations tree and the credential store it references — a
 *              secretRef is only ever read in the sentence "which account does
 *              this provider use", so the store sits under the providers that
 *              name it instead of at the bottom of the page.
 *   Models     what this box serves and under what name: local models, the ones
 *              chosen off somebody's catalogue, and the lanes a caller can ask
 *              for instead of either. All three are lists OF models; they were
 *              separated by a screen of provider configuration.
 *   Budgets    what the upstreams have left. Read, not edited — and the only
 *              4-second poll on the page, which now runs when someone is
 *              looking at it rather than always.
 *   Warnings   what Traffic's Warnings panel holds back: thresholds and hides.
 *              The panel itself shows a setting only while it is holding a
 *              warning back, so this is the one place they are all listed.
 *   History    what changed, and the undo.
 *
 * History used to be rendered FIRST on the scroll, because "is this something I
 * did" is why people open this page when something is wrong (OB-11, Lenny run
 * 4). Providers is the default tab now and that finding still holds: the point
 * of OB-11 was that the answer sat below a screen and a half of settings, and a
 * labelled tab at the top of the page is not below anything. /history still
 * lands on it directly.
 *
 * THE TAB IS IN THE URL (?tab=models), which is what makes one linkable, back-
 * buttonable and able to survive a reload — the same reason /m/$name puts its
 * tab there. /history and /quota, which were pages before they were panels,
 * redirect to their tab rather than to the top of the page.
 *
 * What did NOT go behind a tab: a backend refusing every request. That is a
 * fault, and a fault inside a tab is a fault nobody sees, so it is a banner
 * above the tab bar on every tab.
 */
const TABS = [
  { key: 'providers', label: 'Providers' },
  { key: 'models', label: 'Models' },
  { key: 'budgets', label: 'Budgets' },
  { key: 'warnings', label: 'Warnings' },
  { key: 'history', label: 'History' },
] as const

type TabKey = (typeof TABS)[number]['key']

function isTab(v: string | undefined): v is TabKey {
  return TABS.some((t) => t.key === v)
}

function SetupPage() {
  const navigate = useNavigate()
  const { tab: tabParam } = Route.useSearch()
  const tab: TabKey = isTab(tabParam) ? tabParam : 'providers'
  // Replaces rather than pushes: a tab is where you ARE on this page, not a
  // place you went, and eight tab clicks should not be eight presses of Back to
  // leave the page.
  const goTab = (t: TabKey) => navigate({ to: '/setup', search: { tab: t }, replace: true })

  // The one piece of tab state the page owns: the header's "Add provider" and
  // an extension row's "+ Provider" open the same dialog, and the header button
  // has to work from whichever tab you are on.
  const [addTo, setAddTo] = useState<string | null>(null)
  const [addModel, setAddModel] = useState(false)

  return (
    <Box sx={{ p: 2, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <PageHeader title="Setup">
        <Button
          size="small"
          variant="contained"
          startIcon={<AddIcon />}
          onClick={() => {
            setAddTo('')
            goTab('providers')
          }}
        >
          Add provider
        </Button>
        <Button size="small" variant="outlined" startIcon={<AddIcon />} onClick={() => setAddModel(true)}>
          Add model
        </Button>
        <Typography variant="body2" sx={{ opacity: 0.75 }}>
          Changes apply to the next request.
        </Typography>
      </PageHeader>
      <AddModelDialog open={addModel} onClose={() => setAddModel(false)} />

      {/* Above the tabs on purpose — see the file comment. */}
      <RefusingBanner />

      <Tabs value={tab} onChange={(_, v: TabKey) => goTab(v)} sx={{ mb: 1 }}>
        {TABS.map((t) => (
          <Tab key={t.key} value={t.key} label={t.label} />
        ))}
      </Tabs>

      {/* Mounted one at a time, not hidden with CSS: an unmounted tab stops
          polling, and Budgets polls every four seconds. */}
      {tab === 'providers' && <ProvidersTab addTo={addTo} onAddTo={setAddTo} />}
      {tab === 'models' && <ModelsTab />}
      {tab === 'budgets' && <ProviderBudgets />}
      {tab === 'warnings' && <WarningSettings />}
      {tab === 'history' && <HistoryPanels />}
    </Box>
  )
}

export const Route = createFileRoute('/setup')({
  validateSearch: (s: Record<string, unknown>): { tab?: string } => ({
    tab: s.tab ? String(s.tab) : undefined,
  }),
  component: SetupPage,
})
