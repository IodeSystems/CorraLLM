import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material'
import { Panel, PageHeader, Row, Stat } from '@/Panel'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { fmtBytes } from '@/format'

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
 * put there. Entries are edited as YAML rather than through a form: see the
 * note on the mutation below.
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

const EntryYamlDoc = graphql(/* GraphQL */ `
  query EntryYaml($kind: String!, $name: String!) {
    corrallm {
      entryYaml(kind: $kind, name: $name) {
        kind
        name
        yaml
      }
    }
  }
`)

const PutYamlDoc = graphql(/* GraphQL */ `
  mutation PutEntryYaml($kind: String!, $name: String!, $body: corrallm_PutEntryYAMLInputBodyInput!) {
    corrallm {
      putEntryYaml(kind: $kind, name: $name, body: $body) {
        ok
        message
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

const DeleteEntryDoc = graphql(/* GraphQL */ `
  mutation DeleteEntry($kind: String!, $name: String!) {
    corrallm {
      deleteEntry(kind: $kind, name: $name) {
        ok
        message
      }
    }
  }
`)

// A model's home: the server it draws capacity from, else the fact that it is
// somebody else's machine. Grouping by this is the question the page answers —
// "what runs where" — and it is also the grouping the Agents section will slot
// into once a server can be bound to a remote agent.
const REMOTE = '(remote — not ours to run)'
const UNBOUND = '(no server)'

function ConfigPage() {
  const q = useQuery({
    queryKey: ['config'],
    queryFn: () => gqlClient.request(ConfigDoc),
    refetchInterval: 30000,
  })

  // Hooks BEFORE the early returns: React counts them per render, and a hook
  // that only runs on the success path changes that count between renders —
  // "rendered more hooks than during the previous render" (React #310).
  const qc = useQueryClient()
  const [editing, setEditing] = useState<Edit | null>(null)
  const [err, setErr] = useState('')
  const [minted, setMinted] = useState<{ command: string; expires: string } | null>(null)

  // Editing YAML rather than a form: a model carries far more than fits a form
  // (ramUsage, sticky, contextPerRequest, modalities, convert, swap, freeTier),
  // and every field the form omits is one the dashboard cannot set. YAML is the
  // schema, so the editor is complete the day a field is added.
  const save = useMutation({
    mutationFn: (f: Edit) =>
      gqlClient.request(PutYamlDoc, { kind: f.kind, name: f.name, body: { yaml: f.yaml } }),
    onSuccess: () => {
      setEditing(null)
      setErr('')
      qc.invalidateQueries({ queryKey: ['config'] })
    },
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  // Fetch the stored YAML rather than re-rendering it from the read view: the
  // read view is lossy (a resolved target cannot be turned back into the port
  // that was written), and round-tripping through it would rewrite fields the
  // operator never touched.
  const openEditor = async (kind: EditKind, name: string) => {
    setErr('')
    try {
      const d = await gqlClient.request(EntryYamlDoc, { kind, name })
      setEditing({ kind, existing: true, name, yaml: d.corrallm.entryYaml?.yaml ?? '' })
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

  const del = useMutation({
    mutationFn: (e: Edit) => gqlClient.request(DeleteEntryDoc, { kind: e.kind, name: e.name }),
    onSuccess: () => {
      setEditing(null)
      setErr('')
      qc.invalidateQueries({ queryKey: ['config'] })
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
  const lanes = ov?.lanes ?? []
  const extensions = ov?.extensions ?? []
  const groups = ov?.groups ?? []
  const includes = ov?.include ?? []

  // A server with endpoints is an attached machine; one without is this box.
  const agents = servers.filter((s) => (s.agentEndpoints ?? []).length > 0)

  const homeOf = (m: (typeof models)[number]) =>
    m.remote ? REMOTE : m.server ? m.server : UNBOUND

  const byHome = new Map<string, typeof models>()
  for (const m of models) {
    const k = homeOf(m)
    byHome.set(k, [...(byHome.get(k) ?? []), m])
  }
  // Declared servers first (in order), then remote, then anything unbound —
  // roughly most-ours to least-ours.
  const homes = [
    ...servers.map((s) => s.server).filter((s) => byHome.has(s)),
    ...[REMOTE, UNBOUND].filter((k) => byHome.has(k)),
  ]

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Config">
        <Chip size="small" variant="outlined" label={`${models.length} models`} />
        <Chip size="small" variant="outlined" label={`${servers.length} servers`} />
        <Box sx={{ flexGrow: 1 }} />
        <Button size="small" variant="outlined" onClick={() => setEditing(blankModel())}>
          Add model
        </Button>
      </PageHeader>

      {/* First run. Every panel below renders empty on a fresh install, which
          looks like a page that failed to load rather than one with nothing to
          show yet — and says nothing about which of the two "Add" buttons to
          press first. A proxy model needs no host, so it is the shortest path
          to a working instance. */}
      {models.length === 0 && servers.length === 0 && (
        <Panel title="Nothing configured yet">
          <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
            This instance is running but serves no models. The quickest start is{' '}
            <b>Add model</b> with a <code>proxy:</code> target — an upstream API such as Groq or
            OpenRouter — which needs no host declared. To run models on this machine instead,{' '}
            <b>Add host</b> first to declare its memory budget, then add models that name it.
          </Typography>
          <Stack direction="row" spacing={1}>
            <Button size="small" variant="contained" onClick={() => setEditing(blankModel())}>
              Add model
            </Button>
            <Button size="small" variant="outlined" onClick={() => setEditing(blankServer())}>
              Add host
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

      {homes.map((home) => {
        const ms = byHome.get(home) ?? []
        return (
          <Panel
            key={home}
            title={home}
            subtitle={
              home === REMOTE
                ? 'Forwarded to a host we do not run. No process, no residency.'
                : home === UNBOUND
                  ? 'Declared without a server.'
                  : 'Spawned and evicted here'
            }
            badge={<Chip size="small" variant="outlined" label={`${ms.length}`} />}
            flush
          >
            {ms
              .slice()
              .sort((a, b) => Number(b.quality) - Number(a.quality) || a.name.localeCompare(b.name))
              .map((m) => (
                <Row key={m.name} onClick={() => openEditor('model', m.name)}>
                  <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                    <Typography variant="subtitle2">{m.name}</Typography>
                    {/* The alias. corrallm routes on the served name; the backend
                        knows it by this one. */}
                    {m.upstream && m.upstream !== m.name && (
                      <Tooltip title="The id the BACKEND knows this model by. corrallm routes on the served name and rewrites it on the way out.">
                        <Chip size="small" variant="outlined" label={`→ ${m.upstream}`} />
                      </Tooltip>
                    )}
                    <Chip size="small" color="info" variant="outlined" label={m.capability} />
                    {m.persistent && <Chip size="small" variant="outlined" label="pinned" />}
                    {m.ttl && <Chip size="small" variant="outlined" label={`ttl ${m.ttl}`} />}
                  </Box>
                  <Box sx={{ display: 'flex', gap: 3, mt: 0.75, flexWrap: 'wrap' }}>
                    <Stat label="Quality" value={m.quality} />
                    <Stat label="Type" value={m.type} />
                    <Stat label="Slots" value={m.maxConcurrent} />
                    <Stat label="Target" value={m.target || '—'} />
                  </Box>
                  {m.notes && (
                    <Typography
                      variant="caption"
                      sx={{ display: 'block', mt: 0.75, color: C.textMuted, whiteSpace: 'pre-wrap' }}
                    >
                      {m.notes.length > 240 ? m.notes.slice(0, 240) + '…' : m.notes}
                    </Typography>
                  )}
                  {m.cmd && (
                    <Box
                      component="pre"
                      sx={{
                        mt: 0.75,
                        mb: 0,
                        p: 1,
                        bgcolor: C.raised,
                        border: `1px solid ${C.border}`,
                        borderRadius: 1,
                        fontSize: 11,
                        color: C.textMuted,
                        overflowX: 'auto',
                        whiteSpace: 'pre',
                      }}
                    >
                      {m.cmd}
                    </Box>
                  )}
                </Row>
              ))}
          </Panel>
        )
      })}

      <Panel
        title="Lanes"
        subtitle="Named fallback lists. Requesting a lane allows substitution; requesting a model pins it."
        actions={
          <Button size="small" variant="outlined" onClick={() => setEditing(blankLane())}>
            Add lane
          </Button>
        }
        flush
      >
        {lanes.length === 0 ? (
          <Row>
            <Typography variant="body2" sx={{ color: C.textFaint }}>
              No lanes declared.
            </Typography>
          </Row>
        ) : (
          lanes.map((l) => (
            <Row key={l.name} onClick={() => openEditor('lane', l.name)}>
              <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                <Typography variant="subtitle2">{l.name}</Typography>
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {l.members.map((mem) => mem.model).join('  →  ')}
                </Typography>
              </Box>
            </Row>
          ))
        )}
      </Panel>

      {editing && (
        <Dialog open onClose={() => setEditing(null)} maxWidth="md" fullWidth>
          <DialogTitle>
            {editing.existing ? `Edit ${editing.kind} ${editing.name}` : `Add a ${editing.kind}`}
          </DialogTitle>
          <DialogContent>
            {err && (
              <Alert severity="error" sx={{ mb: 2, whiteSpace: 'pre-wrap', fontFamily: 'monospace', fontSize: 12 }}>
                {err}
              </Alert>
            )}
            <Stack spacing={2} sx={{ mt: 1 }}>
              <TextField
                label="Name"
                size="small"
                value={editing.name}
                disabled={editing.existing}
                onChange={(e) => setEditing({ ...editing, name: e.target.value })}
                helperText={
                  editing.kind === 'model'
                    ? 'The name callers request, and the config key. Renaming means add + delete.'
                    : 'The config key. Renaming means add + delete.'
                }
              />
              <TextField
                label="Configuration (YAML)"
                value={editing.yaml}
                onChange={(e) => setEditing({ ...editing, yaml: e.target.value })}
                multiline
                minRows={18}
                slotProps={{ input: { sx: { fontFamily: 'monospace', fontSize: 12.5 } } }}
                helperText={`The ${editing.kind} exactly as it appears in the config file. Checked twice on save: unknown keys are rejected, then the whole config is validated — an unknown server, or a lane this would break, is caught here rather than at the next restart.`}
              />
            </Stack>
          </DialogContent>
          <DialogActions>
            {editing.existing && (
              <Button
                color="error"
                disabled={del.isPending}
                onClick={() => del.mutate(editing)}
                sx={{ mr: 'auto' }}
              >
                Delete
              </Button>
            )}
            <Button onClick={() => setEditing(null)}>Cancel</Button>
            <Button variant="contained" disabled={save.isPending} onClick={() => save.mutate(editing)}>
              Save
            </Button>
          </DialogActions>
        </Dialog>
      )}

      <Panel
        title="Extensions"
        subtitle="One process serving several models — they load, unload and are accounted for together"
        actions={
          <Button size="small" variant="outlined" onClick={() => setEditing(blankExtension())}>
            Add extension
          </Button>
        }
        flush
      >
        {extensions.length === 0 ? (
          <Row>
            <Typography variant="body2" sx={{ color: C.textFaint }}>
              None declared.
            </Typography>
          </Row>
        ) : (
          extensions.map((e) => (
            <Row key={e.name} onClick={() => openEditor('extension', e.name)}>
              <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                <Typography variant="subtitle2">{e.name}</Typography>
                <Chip size="small" variant="outlined" label={e.cmd ? 'hosted' : 'remote'} />
                {e.server && <Chip size="small" variant="outlined" label={e.server} />}
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {(e.provides ?? []).join(', ')}
                </Typography>
              </Box>
              {e.notes && (
                <Typography
                  variant="caption"
                  sx={{ display: 'block', mt: 0.5, color: C.textMuted, whiteSpace: 'pre-wrap' }}
                >
                  {e.notes.length > 200 ? e.notes.slice(0, 200) + '…' : e.notes}
                </Typography>
              )}
            </Row>
          ))
        )}
      </Panel>

      <Panel
        title="Priority groups"
        subtitle="Who gets served first under load, and what they accept when degraded"
        actions={
          <Button size="small" variant="outlined" onClick={() => setEditing(blankGroup())}>
            Add group
          </Button>
        }
        flush
      >
        {groups.map((g) => (
          <Row key={g.name} onClick={() => openEditor('group', g.name)}>
            <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
              <Typography variant="subtitle2">{g.name}</Typography>
              <Chip size="small" variant="outlined" label={`weight ${g.weight}`} />
              {g.interruptible && (
                <Tooltip title="Requests from this group may be preempted by a strictly higher-weight one. Equal weights never preempt each other, so there is no kick loop.">
                  <Chip size="small" variant="outlined" label="interruptible" />
                </Tooltip>
              )}
              {g.acceptDegrade && (
                <Chip size="small" variant="outlined" label={`degrades to ${g.qualityFloor}`} />
              )}
            </Box>
          </Row>
        ))}
      </Panel>

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

export const Route = createFileRoute('/config')({ component: ConfigPage })

// Edit is what the dialog holds: which kind of entry, its config key, and the
// YAML for it. One dialog for models, servers and lanes — they differ only in
// the schema behind the text, and the server validates that either way.
type EditKind = 'model' | 'server' | 'lane' | 'group' | 'extension'
type Edit = { kind: EditKind; existing: boolean; name: string; yaml: string }

// blankModel seeds a new entry with the fields every model needs, so the first
// thing an operator sees is a shape to fill in rather than an empty box.
function blankModel(): Edit {
  return {
    kind: 'model',
    existing: false,
    name: '',
    yaml: `# A model is exactly ONE serving path: a spawned cmd, or a proxy target.
# Everything the config schema accepts works here.

# cmd: "exec llama-server --port 5800 ..."   # spawned locally; needs a server
# server: box1
proxy: 5800            # port, host:port, or {host, port, headers}
type: chat             # chat | embed | stt | tts
quality: 1             # fractional is fine — 1.5 sits between two tiers
maxConcurrent: 1
# ramUsage: { gpu0: 16GB }   # required on a host that cannot measure itself
# sticky: { ttl: 300s, evictCost: high }
# notes: |
#   Why this model is configured the way it is.
`,
  }
}

// extractMessage digs the server's actual complaint out of a GraphQL error.
// The useful text — "lane chat member 0: unknown model" — is nested, and the
// wrapper alone says nothing actionable.
function extractMessage(e: unknown): string {
  const any = e as { response?: { errors?: { message?: string }[] }; message?: string }
  const first = any?.response?.errors?.[0]?.message
  return first || any?.message || String(e)
}

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
function blankServer(): Edit {
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

// blankLane seeds a fallback list. Order is the whole point, so the template
// says so rather than leaving an empty array.
function blankLane(): Edit {
  return {
    kind: 'lane',
    existing: false,
    name: '',
    yaml: `# Members are walked best-quality-first. Requesting the LANE name allows
# substitution across them; requesting a model name pins exactly that model.
members:
  - model: some-model
  # - model: a-fallback
  #   sticky: { ttl: 120s }   # unloads sooner when loaded on the lane's behalf

# notes: |
#   Why these members, in this order.
`,
  }
}

// blankGroup seeds a priority group. The saturation stages are the part that
// decides behaviour under load, so the template spells them out rather than
// leaving an empty object.
function blankGroup(): Edit {
  return {
    kind: 'group',
    existing: false,
    name: '',
    yaml: `weight: 5              # relative share; higher is served first
interruptible: false   # may a strictly HIGHER weight take this group's slot?
onSaturated:
  chat: { queue: true }
  default: reject
# acceptDegrade: true  # allow a lower-quality model when the top tier is busy
# qualityFloor: 1      # ...but not below this
`,
  }
}

// blankExtension seeds an integration. The two shapes are genuinely different —
// a local process versus a remote endpoint — so both are shown.
function blankExtension(): Edit {
  return {
    kind: 'extension',
    existing: false,
    name: '',
    yaml: `# A HOSTED extension: one local process serving several models. They load,
# unload and are accounted for together, because they are the same bytes.
# cmd: "exec my-server --addr :5806"
# server: box1
# ramUsage: { system: 3GB }     # counted ONCE, not per provided model
proxy: 5806
provides:
  something: { type: chat }

# A REMOTE integration has no cmd and no residency — just an endpoint and
# credentials:
# proxy: { host: api.example.com, port: 443, headers: { authorization: "Bearer \${MY_KEY}" } }
`,
  }
}
