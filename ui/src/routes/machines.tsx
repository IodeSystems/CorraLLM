import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { Alert, Box, Button, Chip, Stack, Tooltip, Typography } from '@mui/material'
import { Panel, PageHeader, Row } from '@/Panel'
import { EntryEditor, type EntryEdit } from '@/EntryEditor'
import { UtilizationBar } from '@/UtilizationBar'
import { FleetSection } from '@/FleetPanel'
import { GpuUtilizationPanel } from '@/GpuUtilization'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { fmtBytes, extractMessage } from '@/format'
import {
  AGENT_STATUS_HINT,
  agentColor,
  derive,
  madeRoomNote,
  useMachines,
  type Machine,
} from '@/MachineData'

/**
 * /machines — the LIST. One row per machine, and nothing else.
 *
 * It used to be six stacked panels: declared capacity, the live memory ledger,
 * every resident model on every box, the agents, and the fleet's whole toolchain
 * table. Every one of those is per-machine, so the page rendered each of them
 * once with a machine column, and answering anything about ONE box meant
 * reading five tables and joining them by eye. The report was blunt about it:
 * "too many things, not organized well, and obvious tab/subrouting solutions".
 *
 * So the machine became the subject and got its own address. This page is the
 * index: what boxes exist, whether each is reachable, and how full it is. The
 * rest — memory attribution, residency, tooling — is at /machines/<name>, where
 * it is about one machine and can be read without a join.
 *
 * What stays here is what belongs to no single machine: adding a host, attaching
 * an agent, and a GPU no pool has claimed.
 */
const MintTokenDoc = graphql(/* GraphQL */ `
  mutation MintEnrollmentToken($body: corrallm_MintEnrollmentTokenInputBodyInput!) {
    corrallm {
      mintEnrollmentToken(body: $body) {
        token
        command
        expires
      }
    }
  }
`)

/**
 * One machine, at a glance: is it reachable, how full is it, what is on it.
 *
 * The utilisation bars are the row's body rather than a number in a column
 * because "how full" is the question that decides whether you click in, and a
 * percentage in a table cell cannot be compared across two boxes with different
 * pool sizes the way two bars can.
 */
function MachineRow({ m, onOpen }: { m: Machine; onOpen: () => void }) {
  const resident = m.resident.length
  const busy = m.resident.reduce((n, r) => n + r.refs, 0)
  const note = madeRoomNote(m)
  return (
    <Row onClick={onOpen}>
      <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
        <Typography variant="subtitle2">{m.server}</Typography>
        {/* 'local' is this machine and needs no chip — it would be on every row
            of a single-box install, saying nothing. A configured agent gets one
            because whether it is heartbeating decides if anything can start
            there. */}
        {m.agentStatus !== 'local' && (
          <Tooltip title={AGENT_STATUS_HINT[m.agentStatus] ?? m.agentStatus}>
            <Chip size="small" label={m.agentStatus} color={agentColor(m.agentStatus)} />
          </Tooltip>
        )}
        {m.maxConcurrent > 0 && (
          <Chip size="small" variant="outlined" label={`max ${m.maxConcurrent}`} />
        )}
        <Box sx={{ flexGrow: 1 }} />
        <Typography variant="caption" sx={{ color: C.textMuted }}>
          {resident === 0
            ? 'nothing loaded'
            : `${resident} loaded${busy > 0 ? ` · ${busy} requests in flight` : ''}`}
        </Typography>
      </Box>

      <Box
        sx={{
          display: 'grid',
          gridTemplateColumns: { xs: '1fr', md: 'repeat(auto-fit, minmax(240px, 1fr))' },
          gap: 1.5,
          mt: 1,
        }}
      >
        {m.pools.map((p) => (
          <UtilizationBar
            key={p.pool}
            label={p.pool}
            role={p.role}
            used={p.used}
            capacity={p.budget}
            reserve={p.reserve}
            note={note}
            lines={[
              ...(p.device?.available
                ? [`${p.device.name} reports ${fmtBytes(p.device.usedBytes)} actually in use`]
                : []),
              resident > 0
                ? `Holding it: ${m.resident.map((r) => r.modelName).join(', ')}`
                : 'Nothing is loaded here right now.',
            ]}
          />
        ))}
        {m.pools.length === 0 && (
          <Typography variant="caption" sx={{ color: C.textFaint }}>
            No pools declared, so the scheduler will not admit anything here.
          </Typography>
        )}
      </Box>

      {/* Said out loud, not only in a tooltip: it changes what an operator has
          to write for every model that runs here. */}
      {m.noProcessMemory && (
        <Typography variant="caption" sx={{ display: 'block', color: C.textMuted, mt: 0.75 }}>
          This machine cannot measure how much memory a model is using, so every model configured
          to run here has to state its own size.
        </Typography>
      )}
    </Row>
  )
}

function MachinesPage() {
  const q = useMachines()

  // Hooks BEFORE the early returns: React counts them per render, and a hook
  // that only runs on the success path changes that count between renders —
  // "rendered more hooks than during the previous render" (React #310).
  const nav = useNavigate()
  const [editing, setEditing] = useState<EntryEdit | null>(null)
  const [err, setErr] = useState('')
  const [minted, setMinted] = useState<{ command: string; expires: string } | null>(null)

  const mint = useMutation({
    mutationFn: () =>
      gqlClient.request(MintTokenDoc, {
        body: {
          server: '',
          note: 'from the dashboard',
          ttlMinutes: '60',
          // Whatever address this page was loaded from is, by definition, an
          // address that reaches the daemon — right scheme, right port, through
          // whatever proxy is in front. Better than anything the server can
          // infer about itself.
          base: window.location.origin,
        },
      }),
    onSuccess: (d) => {
      const t = d.corrallm.mintEnrollmentToken
      if (t) setMinted({ command: t.command, expires: String(t.expires) })
    },
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  if (q.error) {
    return (
      <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
        <PageHeader title="Machines" />
        <Alert severity="error">{String(q.error)}</Alert>
      </Box>
    )
  }

  // THE PAGE RENDERS BEFORE THE ANSWER DOES. It used to return `<Loading />`
  // here, which on a busy box is a nav rail and an empty frame for as long as
  // the query takes — and this is the second screen an owner opens when
  // something feels wrong (OB-5). The header, the panel and its note are known
  // without asking anything, so they are drawn straight away and the body says
  // what it is waiting for.
  //
  // `pending` is carried rather than duplicating the panel in a second branch:
  // two copies of a header drift, and this one holds the page's only statement
  // of where to go next (OB-8).
  const d = q.data ? derive(q.data) : null
  const pending = d == null
  const machines = d?.machines ?? []
  const unclaimed = d?.unclaimed ?? []
  const modelCount = d?.modelCount ?? 0

  // IS EVERYTHING HEALTHY — the question a bare /machines still has to answer
  // now that the detail lives a click away. Only two things here are a fault:
  // a configured agent that stopped reporting in, and a pool holding more than
  // its own budget. Fullness is not one of them (see UtilizationBar), so it is
  // deliberately absent from this count.
  const unreachable = machines.filter((m) => m.agentStatus === 'down')
  const overBudget = machines.filter((m) => m.pools.some((p) => p.budget > 0 && p.used > p.budget))
  const resident = machines.reduce((n, m) => n + m.resident.length, 0)

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      {err && <Alert severity="error">{err}</Alert>}

      <PageHeader title="Machines">
        {/* NOT three chips reading 0, 0 of 0 and a green "all reachable" — the
            last of those is a verdict, and it would be the first thing read on a
            page that has not asked yet. */}
        {pending ? (
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            Asking this box what hardware it has — nothing here is a reading yet.
          </Typography>
        ) : (
          <>
            <Chip size="small" variant="outlined" label={`${machines.length} machines`} />
            <Chip size="small" variant="outlined" label={`${resident} of ${modelCount} loaded`} />
            {unreachable.length === 0 && overBudget.length === 0 ? (
              <Tooltip title="Every configured machine is reporting in, and no pool is holding more than its budget. How FULL a box is is not a fault — see a bar's tooltip.">
                <Chip size="small" color="success" variant="outlined" label="all reachable" />
              </Tooltip>
            ) : (
              <>
                {unreachable.length > 0 && (
                  <Tooltip title={`Stopped reporting in: ${unreachable.map((m) => m.server).join(', ')}. Nothing new will start there; whatever is already running is left alone.`}>
                    <Chip
                      size="small"
                      color="error"
                      variant="outlined"
                      label={`${unreachable.length} unreachable`}
                    />
                  </Tooltip>
                )}
                {overBudget.length > 0 && (
                  <Tooltip title={`Holding more than the declared budget: ${overBudget.map((m) => m.server).join(', ')}. The ledger and the machine disagree — open the machine's Memory tab for which pool.`}>
                    <Chip
                      size="small"
                      color="error"
                      variant="outlined"
                      label={`${overBudget.length} over budget`}
                    />
                  </Tooltip>
                )}
              </>
            )}
          </>
        )}
      </PageHeader>

      {/* First run. Every panel below renders empty on a fresh install, which
          looks like a page that failed to load rather than one with nothing to
          show yet — and says nothing about which of the two "Add" buttons to
          press first. A proxy model needs no host, so it is the shortest path
          to a working instance. */}
      {!pending && machines.length === 0 && modelCount === 0 && (
        <Panel title="Nothing configured yet">
          <Typography variant="body2" sx={{ color: 'text.secondary', mb: 2 }}>
            No models are served yet. Add a host here, then add models on <b>Models</b>. A remote API
            model (such as Groq or OpenRouter) needs no host.
          </Typography>
          <Stack direction="row" spacing={1}>
            <Button size="small" variant="contained" onClick={() => setEditing(blankServer())}>
              Add host
            </Button>
            <Button size="small" variant="outlined" onClick={() => nav({ to: '/models' })}>
              Go to Models
            </Button>
          </Stack>
        </Panel>
      )}

      <Panel
        title="Machines"
        subtitle="Hardware, reachability and capacity in use"
        // The second sentence is the page's only statement of where to go next
        // (OB-8), and the header was cutting it off before it named anything.
        note="Open a machine for its memory, loaded models and tools."
        actions={
          <>
            <Button size="small" variant="outlined" onClick={() => setEditing(blankServer())}>
              Add host
            </Button>
            <Tooltip title="Issues a one-time code for a NEW machine. Changes nothing about the ones already here.">
              <Button
                size="small"
                variant="outlined"
                disabled={mint.isPending}
                onClick={() => mint.mutate()}
              >
                Attach a machine
              </Button>
            </Tooltip>
          </>
        }
        flush
      >
        {minted && (
          <Row>
            <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
              Run this on the machine you want to attach
            </Typography>
            <Box
              component="pre"
              sx={{
                m: 0,
                p: 1.25,
                bgcolor: C.raised,
                border: `1px solid ${C.border}`,
                borderRadius: 1,
                fontSize: 12,
                overflowX: 'auto',
                whiteSpace: 'pre-wrap',
                wordBreak: 'break-all',
              }}
            >
              {minted.command}
            </Box>
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, mt: 1 }}>
              <Button size="small" onClick={() => navigator.clipboard?.writeText(minted.command)}>
                Copy
              </Button>
              <Typography variant="caption" sx={{ color: C.textFaint }}>
                {/* Both properties are load-bearing and easy to be surprised by. */}
                Single use, expires {new Date(Number(minted.expires)).toLocaleTimeString()}. The
                token is shown once — mint another if you lose it.
              </Typography>
              <Box sx={{ flexGrow: 1 }} />
              <Button size="small" onClick={() => setMinted(null)}>
                Dismiss
              </Button>
            </Box>
          </Row>
        )}

        {/* "No machines declared" is a finding about a box that answered. Until
            it has, the list is not empty — it is unasked. */}
        {pending && (
          <Row>
            <Typography variant="body2" sx={{ color: C.textFaint }}>
              Waiting for the list — no machine has been counted yet.
            </Typography>
          </Row>
        )}

        {!pending && machines.length === 0 && !minted && (
          <Row>
            <Typography variant="body2" sx={{ color: C.textMuted }}>
              No machines declared. <b>Add host</b> writes one by hand; <b>Attach a machine</b>{' '}
              mints a single-use token and shows the one command to run on another box — it
              installs the agent, registers the machine, and sizes it from its own memory probe.
            </Typography>
          </Row>
        )}

        {machines.map((m) => (
          <MachineRow
            key={m.server}
            m={m}
            onOpen={() => nav({ to: '/machines/$name', params: { name: m.server } })}
          />
        ))}
      </Panel>

      {/* Where the models sit and how busy each card is — moved here from the
          home screen when it became the dashboard (P56). Both are about the
          hardware, which is this page's subject. */}
      <FleetSection />
      <GpuUtilizationPanel />

      {/* Belongs to no machine row, so if it is not here it is nowhere — and a
          GPU the scheduler cannot see looks exactly like a GPU that is not
          installed. */}
      {unclaimed.length > 0 && (
        <Panel title="Unclaimed GPUs" dense>
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            Present in this machine, but no pool budgets{' '}
            {unclaimed.length === 1 ? 'it' : 'them'}, so the scheduler cannot use{' '}
            {unclaimed.length === 1 ? 'it' : 'them'}:{' '}
            {unclaimed.map((g) => `${g.name} (${fmtBytes(g.totalBytes)})`).join(', ')}. Declare a
            pool and a <code>devices:</code> entry on the machine that holds{' '}
            {unclaimed.length === 1 ? 'it' : 'them'}.
          </Typography>
        </Panel>
      )}

      <EntryEditor editing={editing} onChange={setEditing} onClose={() => setEditing(null)} />
    </Box>
  )
}

export const Route = createFileRoute('/machines')({ component: MachinesPage })

// blankServer seeds a host. Pools are the part people get wrong, so the
// template shows both shapes: a discrete card, and unified memory where one
// pool IS the device.
function blankServer(): EntryEdit {
  return {
    kind: 'server',
    existing: false,
    name: '',
    yaml: `pools:
  gpu0: 30GB           # a discrete card
  system: 120GB
reserve:
  system: 16GB         # headroom kept free for the OS and everything else
devicePool: gpu0       # the pool a MEASURED footprint is charged against

# A unified-memory host (Apple silicon) has ONE pool that is both:
# pools: { system: 64GB }
# devicePool: system

# notes: |
#   What this machine is.
`,
  }
}
