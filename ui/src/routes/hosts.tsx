import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Stack,
  Tooltip,
  Typography,
} from '@mui/material'
import { Panel, PageHeader, Row, Stat } from '@/Panel'
import { EntryEditor, openEntry, type EntryEdit } from '@/EntryEditor'
import { ToolingPanel } from '@/ToolingPanel'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { fmtBytes, extractMessage } from '@/format'

/**
 * Config: what corrallm was TOLD, as opposed to what it is currently doing.
 *
 * The Overview answers "what is happening right now" — residency, memory, live
 * requests. This answers "what did I declare", which is the question you have
 * when something is missing or routed somewhere surprising, and the answer is
 * usually in a file rather than in the runtime.
 *
 * Editable, but only against the MANAGED config — the one corrallm writes and
 * owns. A hand-written config is still refused (the server checks for its
 * header), because rewriting it would silently drop the comments its author
 * put there.
 *
 * Models are edited through a form; everything else, and any model field the
 * form does not cover, through YAML. See the note on the save mutation for why
 * that split is now safe when it previously was not.
 */
const ConfigDoc = graphql(/* GraphQL */ `
  query Config {
    corrallm {
      overview {
        include
        servers {
          server
          maxConcurrent
          devicePool
          agentEndpoints
          agentStatus
          agentLastSeen
          noProcessMemory
          pools {
            pool
            totalBytes
            reserveBytes
          }
        }
        models {
          name
          notes
          server
          spawnable
          remote
          quality
          type
          capability
          cmd
          upstream
          target
          maxConcurrent
          persistent
          ttl
          idleUnload
          modalities {
            modality
          }
          contextPerRequest
        }
        lanes {
          name
          members {
            model
          }
        }
        extensions {
          name
          cmd
          server
          provides
          notes
        }
        groups {
          name
          weight
          interruptible
          acceptDegrade
          qualityFloor
        }
      }
    }
  }
`)

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

function HostsPage() {
  const q = useQuery({
    queryKey: ['config'],
    queryFn: () => gqlClient.request(ConfigDoc),
    refetchInterval: 30000,
  })

  // Hooks BEFORE the early returns: React counts them per render, and a hook
  // that only runs on the success path changes that count between renders —
  // "rendered more hooks than during the previous render" (React #310).
  const nav = useNavigate()
  const [editing, setEditing] = useState<EntryEdit | null>(null)
  const [err, setErr] = useState('')
  const [minted, setMinted] = useState<{ command: string; expires: string } | null>(null)

  // openEditor loads an entry's stored YAML and opens the shared editor.
  const openEditor = async (kind: 'server' | 'lane' | 'group' | 'extension', name: string) => {
    setErr('')
    try {
      setEditing(await openEntry(kind, name))
    } catch (e) {
      setErr(extractMessage(e))
    }
  }

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

  if (q.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress size={20} />
      </Box>
    )
  }
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Alert severity="error">{String(q.error)}</Alert>
      </Box>
    )
  }

  const ov = q.data?.corrallm.overview
  const servers = ov?.servers ?? []
  const models = ov?.models ?? []
  const includes = ov?.include ?? []

  // A server with endpoints is an attached machine; one without is this box.
  const agents = servers.filter((s) => (s.agentEndpoints ?? []).length > 0)

  // What the form needs to keep an operator from writing an unspellable
  // footprint: each server's OWN pool names, and whether it can measure itself.

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      {err && <Alert severity="error">{err}</Alert>}

      <PageHeader title="Hosts">
        <Chip size="small" variant="outlined" label={`${models.length} models`} />
        <Chip size="small" variant="outlined" label={`${servers.length} servers`} />
      </PageHeader>

      {/* First run. Every panel below renders empty on a fresh install, which
          looks like a page that failed to load rather than one with nothing to
          show yet — and says nothing about which of the two "Add" buttons to
          press first. A proxy model needs no host, so it is the shortest path
          to a working instance. */}
      {models.length === 0 && servers.length === 0 && (
        <Panel title="Nothing configured yet">
          <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
            This instance is running but serves no models. Declare a host here first — its
            memory budget is what the scheduler admits against — then add models on{' '}
            <b>Providers</b>, which is where a model gets the provider that owns it. A{' '}
            <code>proxy:</code> model (an upstream API such as Groq or OpenRouter) needs no host
            at all, so that is the shortest path to a working instance.
          </Typography>
          <Stack direction="row" spacing={1}>
            <Button size="small" variant="contained" onClick={() => setEditing(blankServer())}>
              Add host
            </Button>
            <Button size="small" variant="outlined" onClick={() => nav({ to: '/providers' })}>
              Go to Providers
            </Button>
          </Stack>
        </Panel>
      )}

      <Panel
        title="Hosts"
        subtitle="Declared capacity. A budget the scheduler admits against — not a probe."
        actions={
          <Button size="small" variant="outlined" onClick={() => setEditing(blankServer())}>
            Add host
          </Button>
        }
        flush
      >
        {servers.map((s) => (
          <Row key={s.server} onClick={() => openEditor('server', s.server)}>
            <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
              <Typography variant="subtitle2">{s.server}</Typography>
              {/* No "local" chip: every server IS local until a server can be
                  bound to an agent, so it would carry no information now and be
                  wrong the moment one can. The Agents panel says where that
                  distinction will appear. */}
              {Number(s.maxConcurrent) > 0 && (
                <Chip size="small" variant="outlined" label={`max ${s.maxConcurrent}`} />
              )}
            </Box>
            <Box sx={{ display: 'flex', gap: 3, mt: 0.75, flexWrap: 'wrap' }}>
              {s.pools.map((p) => (
                <Stat
                  key={p.pool}
                  label={
                    p.pool === s.devicePool ? `${p.pool} · device` : p.pool
                  }
                  value={
                    Number(p.reserveBytes) > 0
                      ? `${fmtBytes(Number(p.totalBytes))} − ${fmtBytes(Number(p.reserveBytes))} reserved`
                      : fmtBytes(Number(p.totalBytes))
                  }
                  title={
                    p.pool === s.devicePool
                      ? 'A measured footprint is charged against this pool. Unified-memory hosts point it at their single system pool.'
                      : undefined
                  }
                />
              ))}
            </Box>
          </Row>
        ))}
      </Panel>

      {/* Agents: enrolled machines, and the one command that attaches another. */}
      <Panel
        title="Agents"
        subtitle="Other machines this daemon spawns and evicts on"
        badge={<Chip size="small" variant="outlined" label={`${agents.length}`} />}
        actions={
          <Button size="small" variant="outlined" disabled={mint.isPending} onClick={() => mint.mutate()}>
            Attach a machine
          </Button>
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
                Single use, expires {new Date(Number(minted.expires)).toLocaleTimeString()}. The token is
                shown once — mint another if you lose it.
              </Typography>
              <Box sx={{ flexGrow: 1 }} />
              <Button size="small" onClick={() => setMinted(null)}>
                Dismiss
              </Button>
            </Box>
          </Row>
        )}

        {agents.length === 0 && !minted && (
          <Row>
            <Typography variant="body2" sx={{ color: C.textMuted }}>
              No machines attached yet. <b>Attach a machine</b> mints a single-use token and shows the
              one command to run there — it installs the agent, registers the machine, and sizes it
              from its own memory probe.
            </Typography>
          </Row>
        )}

        {agents.map((a) => (
          <Row key={a.server}>
            <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
              <Typography variant="subtitle2">{a.server}</Typography>
              <Tooltip title={AGENT_STATUS_HINT[a.agentStatus] ?? a.agentStatus}>
                <Chip
                  size="small"
                  label={a.agentStatus}
                  color={
                    a.agentStatus === 'up'
                      ? 'success'
                      : a.agentStatus === 'down'
                        ? 'error'
                        : 'warning'
                  }
                />
              </Tooltip>
              {a.noProcessMemory && (
                <Tooltip title="This host cannot attribute memory to a single process (macOS has no nvidia-smi equivalent). A model here MUST declare ramUsage — nothing can measure it, so a declared size is the only size there is.">
                  <Chip size="small" variant="outlined" color="warning" label="ramUsage required" />
                </Tooltip>
              )}
              {Number(a.agentLastSeen) > 0 && (
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  last seen {new Date(Number(a.agentLastSeen)).toLocaleTimeString()}
                </Typography>
              )}
            </Box>
            <Box sx={{ mt: 0.5 }}>
              {(a.agentEndpoints ?? []).map((e) => (
                <Typography key={e} variant="body2" sx={{ fontFamily: 'monospace', fontSize: 12 }}>
                  {e}
                </Typography>
              ))}
            </Box>
          </Row>
        ))}
      </Panel>

      {/* Per-machine, like the two panels above it — what each host can RUN
          the models with. Its own query, because a survey asks every host and
          would otherwise make capacity wait on a sleeping laptop. */}
      <ToolingPanel />

      {/* The per-box model list moved to Providers. A model belongs to the
          provider that owns it — that is what decides its served prefix and
          what it falls back to — and having a second list here meant two places
          to add one thing, which disagreed about which fields mattered. Boxes
          are still the grouping there; this page keeps what a box IS, not what
          runs on it. */}

      {/* Lanes, Extensions and Priority groups moved off this page. A lane is
          a list of models and an extension serves models, so both belong with
          the models on Providers; priority groups belong beside the live group
          view on Groups. What is left is what a MACHINE is: its declared
          capacity, the agent that reaches it, and the tools it can run models
          with. The editor they shared is now a component, which is what made
          moving them possible at all. */}
      <EntryEditor editing={editing} onChange={setEditing} onClose={() => setEditing(null)} />

      <Panel
        title="Included files"
        subtitle="Merged into the top-level config, weakest first; the hand-written file always wins"
        flush
      >
        {includes.length === 0 ? (
          <Row>
            <Typography variant="body2" sx={{ color: C.textFaint }}>
              None. Everything is declared in the one config file.
            </Typography>
          </Row>
        ) : (
          includes.map((f) => (
            <Row key={f}>
              <Typography variant="body2" sx={{ fontFamily: 'monospace' }}>
                {f}
              </Typography>
            </Row>
          ))
        )}
      </Panel>
    </Box>
  )
}

export const Route = createFileRoute('/hosts')({ component: HostsPage })

// specFromGql fills the gaps a nullable schema leaves, so the form always has a
// value to render. A null quality is 0, not "unset" — the form has to put
// SOMETHING in the box, and 0 is what the config means by absent.

// What each agent state means for whether anything can run there.
const AGENT_STATUS_HINT: Record<string, string> = {
  up: 'heartbeating; models can be spawned here',
  down: 'stopped reporting in — new spawns are refused here, but its config and any running backends are left alone',
  unknown: 'configured but has never reported in; a spawn will be attempted and will say why if it fails',
  local: 'this machine',
}

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

