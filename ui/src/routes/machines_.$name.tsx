import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { Alert, Box, Button, Chip, Stack, Tab, Tabs, Tooltip, Typography } from '@mui/material'
import { Panel, PageHeader, Row, Stat } from '@/Panel'
import { EntryEditor, openEntry, type EntryEdit } from '@/EntryEditor'
import { MemoryPanel } from '@/MemoryPanel'
import { ToolingPanel } from '@/ToolingPanel'
import { UtilizationBar } from '@/UtilizationBar'
import { C } from '@/theme'
import { fmtBytes, extractMessage } from '@/format'
import { Loading } from '@/Loading'
import {
  AGENT_STATUS_HINT,
  agentColor,
  derive,
  madeRoomNote,
  residentColor,
  useMachines,
  type Machine,
} from '@/MachineData'

/**
 * /machines/<name> — one machine, addressed by PATH.
 *
 * This is the page the report asked for: "going to the machine page should show
 * you tooling — I like being able to see an overview of machine health and quick
 * device utilization, and a quick list of model/plugin residency". All four of
 * those existed already; they were spread across five fleet-wide tables on
 * /machines with a machine column in each, so reading one box meant joining them
 * by eye.
 *
 * Three tabs, and the split is by how long you intend to look:
 *
 *   Overview  the glance. Is it up, how full is it, what is on it. Everything
 *             the report listed, above the fold, with nothing to join.
 *   Memory    the ledger. Accounted vs measured, per pool, segmented by model —
 *             the view you open when the glance looked wrong. It is deliberately
 *             NOT on Overview: two readings of every pool with a legend under
 *             each is the opposite of quick.
 *   Tooling   the programs that run the models here. It was a table at the
 *             bottom of a page about every machine; it is about one machine.
 *
 * The tab is in the URL (?tab=memory) so it is linkable and survives a reload —
 * the same pattern as /m/<name>.
 */

const TABS = ['overview', 'memory', 'tooling'] as const
type TabName = (typeof TABS)[number]

export const Route = createFileRoute('/machines_/$name')({
  validateSearch: (s: Record<string, unknown>): { tab?: string } => ({
    tab: s.tab ? String(s.tab) : undefined,
  }),
  component: MachinePage,
})

function MachinePage() {
  const { name } = Route.useParams()
  const { tab } = Route.useSearch()
  const nav = useNavigate()
  const q = useMachines()
  const [editing, setEditing] = useState<EntryEdit | null>(null)
  const [err, setErr] = useState('')

  // An unrecognised ?tab= falls back rather than rendering a Tabs with no
  // matching child, which MUI renders as an empty strip with nothing selected.
  const active: TabName = (TABS as readonly string[]).includes(tab ?? '')
    ? (tab as TabName)
    : 'overview'
  const setTab = (t: TabName) =>
    // replace: a tab is not a place you want the back button to walk through
    // one at a time on the way out of the page.
    nav({ to: '/machines/$name', params: { name }, search: { tab: t }, replace: true })

  const openEditor = async () => {
    setErr('')
    try {
      setEditing(await openEntry('server', name))
    } catch (e) {
      setErr(extractMessage(e))
    }
  }

  if (q.isLoading) return <Loading />
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Alert severity="error">{String(q.error)}</Alert>
      </Box>
    )
  }

  const d = derive(q.data)
  const m = d.machines.find((x) => x.server === name)

  if (!m) {
    return (
      <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 2 }}>
        <Alert severity="warning">
          No machine named <b>{name}</b> is configured. It may have been renamed or removed.
        </Alert>
        <Box>
          <Button size="small" variant="outlined" onClick={() => nav({ to: '/machines' })}>
            ← All machines
          </Button>
        </Box>
      </Box>
    )
  }

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 2 }}>
      {err && <Alert severity="error">{err}</Alert>}

      <Stack direction="row" spacing={1.5} useFlexGap sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
        <Button size="small" onClick={() => nav({ to: '/machines' })}>
          ← Machines
        </Button>
        <PageHeader title={m.server} />
        {m.agentStatus !== 'local' ? (
          <Tooltip title={AGENT_STATUS_HINT[m.agentStatus] ?? m.agentStatus}>
            <Chip size="small" label={m.agentStatus} color={agentColor(m.agentStatus)} />
          </Tooltip>
        ) : (
          <Tooltip title={AGENT_STATUS_HINT.local}>
            <Chip size="small" variant="outlined" label="this machine" />
          </Tooltip>
        )}
        <Box sx={{ flexGrow: 1 }} />
        <Tooltip title="Edit this machine's declared pools and reserve. Changes what it may admit NEXT; nothing already running is touched.">
          <Button size="small" variant="outlined" onClick={openEditor}>
            Edit host
          </Button>
        </Tooltip>
      </Stack>

      <Tabs value={active} onChange={(_, v: TabName) => setTab(v)} sx={{ mb: 1 }}>
        <Tab value="overview" label="Overview" />
        <Tab value="memory" label="Memory" />
        <Tab value="tooling" label="Tooling" />
      </Tabs>

      {active === 'overview' && <OverviewTab m={m} />}

      {active === 'memory' && (
        <MemoryPanel
          scoped
          pools={d.memPools.filter((p) => p.server === m.server)}
          models={d.memModels.filter((x) => x.server === m.server)}
          servers={d.memServers.filter((s) => s.server === m.server)}
          gpus={d.gpus}
          host={d.host}
          colorOf={d.colorOf}
        />
      )}

      {active === 'tooling' && <ToolingPanel host={m.server} />}

      <EntryEditor editing={editing} onChange={setEditing} onClose={() => setEditing(null)} />
    </Box>
  )
}

/** Health, utilisation, residency — the three things the glance is for. */
function OverviewTab({ m }: { m: Machine }) {
  const note = madeRoomNote(m)
  const busy = m.resident.reduce((n, r) => n + r.refs, 0)

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
      <Panel
        title="Health"
        subtitle="Whether anything can start here, and what it has cost to fit things in today."
      >
        <Box sx={{ display: 'flex', gap: 3, flexWrap: 'wrap' }}>
          <Stat
            label="reachable"
            value={m.agentStatus === 'local' ? 'this machine' : m.agentStatus}
            title={AGENT_STATUS_HINT[m.agentStatus] ?? m.agentStatus}
          />
          {m.agentStatus !== 'local' && (
            <Stat
              label="last heard from"
              value={
                m.agentLastSeen > 0 ? new Date(m.agentLastSeen).toLocaleTimeString() : 'never'
              }
              title="The last heartbeat. A machine that has never reported in is configured but unproven."
            />
          )}
          <Stat
            label="concurrency cap"
            value={m.maxConcurrent > 0 ? String(m.maxConcurrent) : 'none'}
            title="How many requests this host may serve at once, across every model on it. 'none' means the models' own caps are the only limit."
          />
          <Stat
            label="loaded"
            value={`${m.resident.length} of ${m.declaredModels}`}
            title="Backends resident right now, out of the models configured to run here."
          />
          <Stat
            label="in flight"
            value={String(busy)}
            title="Requests holding a backend on this machine right now."
          />
          <Stat
            label="made room"
            value={m.madeRoomCount > 0 ? `${m.madeRoomCount} today` : 'not today'}
            title="Loads in the last 24h that had to unload something first. This — not a percentage — is what says whether being full here costs anything."
          />
        </Box>

        {/* The two states worth a sentence rather than a stat, because each
            changes what an operator has to DO. */}
        {m.madeRoomCount > 0 && (
          <Typography variant="body2" sx={{ color: C.textMuted, mt: 1.5 }}>
            {note}
          </Typography>
        )}
        {m.noProcessMemory && (
          <Typography variant="body2" sx={{ color: C.textMuted, mt: 1.5 }}>
            This machine cannot attribute memory to a single process, so a model here MUST declare
            its <code>ramUsage</code> — nothing can measure it, and a declared size is the only size
            there is. A model with no size declared will not start here. Nothing to do with whether
            the machine is reachable.
          </Typography>
        )}
        {m.agentEndpoints.length > 0 && (
          <Box sx={{ mt: 1.5 }}>
            <Typography variant="caption" sx={{ color: C.textFaint, display: 'block' }}>
              agent reached at — several is normal; a LAN, a VPN and an external address can all be
              valid at once
            </Typography>
            {m.agentEndpoints.map((e) => (
              <Typography key={e} variant="body2" sx={{ fontFamily: 'monospace', fontSize: 12 }}>
                {e}
              </Typography>
            ))}
          </Box>
        )}
        {m.notes && (
          <Typography variant="body2" sx={{ color: C.textMuted, mt: 1.5, whiteSpace: 'pre-wrap' }}>
            {m.notes}
          </Typography>
        )}
      </Panel>

      <Panel
        title="Utilisation"
        subtitle="One bar per pool: the thin rule is what the scheduler may fill, the thick bar is what it has. Hover for the measured reading and whether being full has cost anything."
      >
        {m.pools.length === 0 ? (
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            No pools declared on this machine, so the scheduler will not admit anything here. Edit
            host to give it a memory budget.
          </Typography>
        ) : (
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
            {m.pools.map((p) => {
              const holders = m.resident
                .map((r) => ({ r, bytes: r.usage.find((u) => u.pool === p.pool)?.bytes ?? 0 }))
                .filter((x) => x.bytes > 0)
                .sort((a, b) => b.bytes - a.bytes)
              return (
                <UtilizationBar
                  key={p.pool}
                  label={p.pool}
                  role={p.role}
                  used={p.used}
                  capacity={p.budget}
                  reserve={p.reserve}
                  note={note}
                  lines={[
                    // ACCOUNTED AND MEASURED ARE NOT THE SAME NUMBER and are
                    // never averaged into one. The bar draws the accounted
                    // figure — the one admission and eviction are actually
                    // decided on — and the device's own reading rides in the
                    // tooltip beside it. A gap between them is the interesting
                    // case; the Memory tab is where it is broken down.
                    ...(p.device
                      ? p.device.available
                        ? [
                            `${p.device.name} measures ${fmtBytes(p.device.usedBytes)} of ${fmtBytes(p.device.totalBytes)} really in use — see the Memory tab for who.`,
                          ]
                        : [
                            `${p.device.name || 'The device'} could not be probed, so there is no measured reading to compare — not a reading of zero.`,
                          ]
                      : []),
                    holders.length > 0
                      ? `Holding it: ${holders.map((h) => `${h.r.modelName} ${fmtBytes(h.bytes)}`).join(', ')}`
                      : 'Nothing is holding this pool right now.',
                    `Declared total ${fmtBytes(p.total)}.`,
                  ]}
                />
              )
            })}
          </Box>
        )}
      </Panel>

      <Panel
        title="Loaded here"
        subtitle="What is on this machine right now. Nothing here is a cost — it is what the memory above is being spent on."
        badge={
          <Chip
            size="small"
            variant="outlined"
            label={`${m.resident.length + m.extensions.length}`}
          />
        }
        flush
      >
        {m.resident.length === 0 && m.extensions.length === 0 && (
          <Row>
            <Typography variant="body2" sx={{ color: C.textMuted }}>
              Nothing is loaded. corrallm starts a model when a request asks for one, so an idle box
              holds nothing — this is the normal state between requests, not a fault.
            </Typography>
          </Row>
        )}

        {m.resident.map((r) => (
          <Row key={r.name}>
            <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
              <Typography variant="subtitle2">{r.modelName}</Typography>
              <Chip size="small" label={r.state} color={residentColor(r.state)} />
              {r.persistent && <Chip size="small" variant="outlined" label="kept loaded" />}
              {r.refs > 0 && <Chip size="small" variant="outlined" label={`${r.refs} in flight`} />}
            </Box>
            <Box sx={{ display: 'flex', gap: 2, mt: 0.5, flexWrap: 'wrap' }}>
              <Typography variant="caption" sx={{ color: C.textMuted }}>
                holding {r.usage.map((u) => `${fmtBytes(u.bytes)} of ${u.pool}`).join(', ') || '—'}
              </Typography>
              <Typography variant="caption" sx={{ color: C.textFaint }}>
                last used{' '}
                {r.lastUsedMs > 0
                  ? new Date(r.lastUsedMs).toLocaleString()
                  : 'not since this daemon started'}
              </Typography>
            </Box>
          </Row>
        ))}

        {/* Plugins, in the same list as the models, because the report asked for
            "a quick list of model/plugin residency" as ONE thing — and it is:
            an extension is a process on this box holding memory, serving several
            models from it. A separate panel would be the same split that made
            this page unreadable. */}
        {m.extensions.map((e) => (
          <Row key={`ext:${e.name}`}>
            <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
              <Typography variant="subtitle2">{e.name}</Typography>
              <Chip size="small" variant="outlined" label="plugin" />
              {/* A remote integration has no local process, so a residency state
                  would claim a load that never happened. */}
              {e.local ? (
                <Chip size="small" label={e.state} color={residentColor(e.state)} />
              ) : (
                <Tooltip title="A remote integration: corrallm forwards to it and runs no process here, so it holds no memory on this machine.">
                  <Chip size="small" color="secondary" label="remote" />
                </Tooltip>
              )}
              {e.pinned && <Chip size="small" variant="outlined" label="kept loaded" />}
              {e.draining && <Chip size="small" color="warning" variant="outlined" label="draining" />}
              {e.inFlight > 0 && (
                <Chip size="small" variant="outlined" label={`${e.inFlight} in flight`} />
              )}
              {e.paused && (
                <Tooltip title={e.pauseReason || 'Out of service by operator order; every model it provides is refused.'}>
                  <Chip size="small" color="warning" label="paused" />
                </Tooltip>
              )}
            </Box>
            <Typography variant="caption" sx={{ color: C.textMuted, display: 'block', mt: 0.5 }}>
              {e.provides.length > 0
                ? `serves ${e.provides.join(', ')}`
                : 'serves no models — nothing will route to it'}
            </Typography>
          </Row>
        ))}
      </Panel>
    </Box>
  )
}
