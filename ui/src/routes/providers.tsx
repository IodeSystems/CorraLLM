import { createFileRoute, useNavigate } from '@tanstack/react-router'
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
  Tooltip,
  Typography,
} from '@mui/material'
import AddIcon from '@mui/icons-material/Add'
import SearchIcon from '@mui/icons-material/Search'
import { Panel, PageHeader, Row } from '@/Panel'
import { EntryEditor, openEntry, type EntryEdit } from '@/EntryEditor'
import { ProviderDialog } from '@/ProviderDialog'
import type { ProviderInitial } from '@/ProviderDialog'
import { CatalogDialog } from '@/CatalogDialog'
import { ProviderModelDialog } from '@/ProviderModelDialog'
import { graphql } from '@/gql'
import type { ProvidersQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { extractMessage } from '@/format'
import { C } from '@/theme'

/**
 * Providers (P21): an upstream endpoint and the accounts held against it.
 *
 * The page is a LIST plus two dialogs, not a form. Adding a provider asks for
 * things nobody knows by heart — host, port, the path prefix in front of /v1 —
 * so that lives in a modal with a typeahead over known endpoints; and choosing
 * which of a provider's three hundred models to serve is a search problem, so
 * that lives in a modal of its own, opened FROM the provider that holds the
 * catalogue. Neither belongs inline under a heading.
 *
 * Secrets are NEVER shown. A credential carries a secretRef — the name of an
 * entry in ~/.corrallm/credentials, referenced from config as ${NAME} — and the
 * value is written through a separate write-only endpoint. That is what keeps
 * the config document safe to read, back up and share.
 */
// A local model with no `server` is a pure proxy — corrallm forwards to it and
// never starts it — so it has no box. It gets its own heading rather than being
// dropped or filed under an arbitrary host, because "which machine runs this"
// having no answer is itself the answer.
// Blank templates for the two entry kinds this page now owns. They live with
// the panels that create them rather than in the editor, because the editor is
// a text box and these are the domain's opinion about what to type in it.
function blankLane(): EntryEdit {
  return {
    kind: 'lane',
    existing: false,
    name: '',
    yaml: `# A lane is an ORDERED fallback list. Requesting the lane allows
# substitution across its members; requesting a model pins that model.
members:
  - some-local-model      # best first
  - some-remote-model     # spilled to when the local one is full
`,
  }
}

function blankExtension(): EntryEdit {
  return {
    kind: 'extension',
    existing: false,
    name: '',
    yaml: `# One process serving SEVERAL models. They load, unload and are
# accounted for together, because they are the same bytes.
# cmd: "exec my-server --addr :5806"
# server: box1
# ramUsage: { system: 3GB }     # counted ONCE, not per provided model
proxy: 5806
provides:
  - name: my-model
    type: chat
`,
  }
}

const UNPLACED = '\u0000unplaced'

function groupByBox<T extends { server?: string | null }>(models: readonly T[]): [string, T[]][] {
  const by = new Map<string, T[]>()
  for (const m of models) {
    const k = m.server || UNPLACED
    by.set(k, [...(by.get(k) ?? []), m])
  }
  // Named boxes first, alphabetically; the unplaced group last.
  return [...by.entries()].sort(([a], [b]) =>
    a === UNPLACED ? 1 : b === UNPLACED ? -1 : a.localeCompare(b),
  )
}

const ProvidersDoc = graphql(/* GraphQL */ `
  query Providers {
    corrallm {
      overview {
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
      }
      listProviders {
        secrets
        local {
          name
          barePrecedence
          notes
          models {
            id
            served
            bare
            type
            server
            hasCmd
          }
        }
        pools {
          extension
          sources
          freeOnly
          minContext
          limit
          models
          lanes {
            lane
            order
          }
        }
        providers {
          extension
          name
          host
          port
          basePath
          manual
          provides
          directory {
            freeOnly
            minContext
          }
          limits {
            req
            usd
            sec
            per
          }
          credentials {
            name
            secretRef
            headerName
            allow
            hasSecret
            limits {
              req
              usd
              sec
              per
            }
          }
        }
      }
    }
  }
`)

type ProviderRow = NonNullable<
  NonNullable<ProvidersQuery['corrallm']>['listProviders']
>['providers'][number]

const SelectionsDoc = graphql(/* GraphQL */ `
  query Selections {
    corrallm {
      listSelections {
        selections {
          provider
          credential
          model
          upstream
          quality
          serving
          lanes {
            lane
            order
          }
        }
      }
    }
  }
`)

const DeleteModelDoc = graphql(/* GraphQL */ `
  mutation DeleteLocalModel($name: String!) {
    corrallm {
      deleteEntry(kind: "model", name: $name) {
        ok
        message
      }
    }
  }
`)

const UnassignDoc = graphql(/* GraphQL */ `
  mutation UnassignFromList($body: corrallm_UnassignModelInputBodyInput!) {
    corrallm {
      unassignModel(body: $body) {
        ok
        message
      }
    }
  }
`)

/**
 * Everything chosen off a directory, in one list.
 *
 * The per-provider dialog is where you CHOOSE; this is where you see what you
 * chose across all of them and where each one landed. Both write the same rows
 * — there is no separate approvals page any more, because there is nothing to
 * approve: a model is selected or it is not.
 */
function AssignedModels() {
  const qc = useQueryClient()
  const { data } = useQuery({
    queryKey: ['selections'],
    queryFn: () => gqlClient.request(SelectionsDoc),
  })
  const unassign = useMutation({
    mutationFn: (body: { provider: string; credential: string; model: string }) =>
      gqlClient.request(UnassignDoc, { body }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['selections'] })
      qc.invalidateQueries({ queryKey: ['catalog'] })
    },
  })
  const rows = data?.corrallm?.listSelections?.selections ?? []

  return (
    <Panel title={`Assigned models (${rows.length})`} flush>
      {rows.length === 0 ? (
        <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
          None yet. <strong>Browse</strong> a provider above and add the models you want — with a
          lane and priority if they should join one.
        </Typography>
      ) : (
        rows.map((r) => (
          <Row key={`${r.provider}/${r.credential}/${r.model}`}>
            <Stack direction="row" spacing={1.5} alignItems="center" flexWrap="wrap" useFlexGap>
              <Box sx={{ minWidth: 220, flex: 1 }}>
                <Typography variant="body2" sx={{ fontFamily: 'monospace', fontSize: 12.5 }}>
                  {r.model}
                </Typography>
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {r.provider} / {r.credential}
                  {r.upstream ? ` · ${r.upstream}` : ' · placement only'}
                </Typography>
              </Box>
              <Stack direction="row" spacing={0.75} flexWrap="wrap" useFlexGap>
                {r.lanes.map((l) => (
                  <Chip key={l.lane} size="small" label={`${l.lane} @ ${l.order}`} />
                ))}
                {r.lanes.length === 0 && (
                  <Tooltip title="Servable by name, but in no lane — nothing routes to it automatically.">
                    <Chip size="small" variant="outlined" label="no lane" />
                  </Tooltip>
                )}
                {Number(r.quality ?? 0) > 0 && (
                  <Chip size="small" variant="outlined" label={`q${r.quality}`} />
                )}
                {!r.serving && (
                  <Tooltip title="On record, but not in the served registry — the provider or its id may have gone away.">
                    <Chip size="small" color="warning" label="not serving" />
                  </Tooltip>
                )}
              </Stack>
              <Button
                size="small"
                color="error"
                disabled={unassign.isPending}
                onClick={() =>
                  unassign.mutate({
                    provider: r.provider,
                    credential: r.credential,
                    model: r.model,
                  })
                }
              >
                Remove
              </Button>
            </Stack>
          </Row>
        ))
      )}
    </Panel>
  )
}

function ProvidersPage() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [addOpen, setAddOpen] = useState(false)
  const [edit, setEdit] = useState<ProviderInitial | null>(null)
  const [browse, setBrowse] = useState<{ provider: string; credential: string } | null>(null)
  const [addLocal, setAddLocal] = useState<{
    provider: string
    extension?: string
    editId?: string
  } | null>(null)
  // Deleting a model is not undoable from here, so it is confirmed by name.
  const [confirmDelete, setConfirmDelete] = useState<{ served: string; hasCmd: boolean } | null>(
    null,
  )
  const [deleteErr, setDeleteErr] = useState('')
  const [editing, setEditing] = useState<EntryEdit | null>(null)

  const del = useMutation({
    mutationFn: (name: string) => gqlClient.request(DeleteModelDoc, { name }),
    onSuccess: (r) => {
      const res = r.corrallm?.deleteEntry
      // The server refuses to delete a lane member and says which lane. That is
      // the useful half of the answer, so it goes on screen rather than being
      // collapsed into a generic failure.
      if (!res?.ok) {
        setDeleteErr(res?.message ?? 'delete refused')
        return
      }
      setConfirmDelete(null)
      setDeleteErr('')
      qc.invalidateQueries({ queryKey: ['providers'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (e) => setDeleteErr(extractMessage(e)),
  })

  const { data, isLoading, error } = useQuery({
    queryKey: ['providers'],
    queryFn: () => gqlClient.request(ProvidersDoc),
  })

  if (isLoading) return <CircularProgress sx={{ m: 4 }} />
  if (error)
    return (
      <Alert severity="error" sx={{ m: 2 }}>
        {String(error)}
      </Alert>
    )

  const list = data?.corrallm?.listProviders
  const providers = list?.providers ?? []
  const pools = list?.pools ?? []
  const local = list?.local ?? []
  const ov = data?.corrallm?.overview
  // Lanes and extensions moved here from the Config page. Both are about
  // MODELS — a lane is an ordered list of them, an extension is one process
  // serving several — so they belong with the models rather than on a page
  // named after a file.
  const lanes = ov?.lanes ?? []
  const extensions = ov?.extensions ?? []

  // Providers live INSIDE extensions — `extensions.free.providers.groq` — and
  // the page used to contradict that by listing extensions, their providers and
  // their pools as three flat panels, so `free` appeared three times and its
  // three providers appeared unattached. One tree instead: the extension is the
  // container, its providers are its members, and a pool is a property of the
  // extension rather than a separate object.
  const byExtension = (() => {
    const groups = new Map<
      string,
      {
        name: string
        ext?: (typeof extensions)[number]
        pool?: (typeof pools)[number]
        provs: typeof providers
      }
    >()
    const get = (name: string) => {
      let g = groups.get(name)
      if (!g) {
        g = {
          name,
          ext: extensions.find((x) => x.name === name),
          pool: pools.find((x) => x.extension === name),
          provs: [],
        }
        groups.set(name, g)
      }
      return g
    }
    // Every declared extension gets a group even with no providers: claude and
    // oidio provide models directly, and omitting them would hide half of what
    // is configured.
    for (const x of extensions) get(x.name)
    for (const pr of providers) get(pr.extension).provs.push(pr)
    return [...groups.values()].sort((a, b) => b.provs.length - a.provs.length || a.name.localeCompare(b.name))
  })()
  const secrets = list?.secrets ?? []

  const toInitial = (p: ProviderRow): ProviderInitial => ({
    extension: p.extension,
    name: p.name,
    host: p.host,
    port: String(p.port ?? '443'),
    basePath: p.basePath ?? '',
    manual: p.manual ?? false,
    directory: p.directory
      ? {
          freeOnly: p.directory.freeOnly ?? false,
          minContext: String(p.directory.minContext ?? '0'),
        }
      : null,
  })

  return (
    <Box sx={{ p: 2, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <PageHeader title="Providers">
        <Button
          size="small"
          variant="contained"
          startIcon={<AddIcon />}
          onClick={() => setAddOpen(true)}
        >
          Add provider
        </Button>
        <Typography variant="body2" sx={{ opacity: 0.75 }}>
          An upstream endpoint and the accounts held against it. Secrets live in the credential
          store and are referenced as <code>${'{NAME}'}</code> — this page can set one, never show
          one.
        </Typography>
      </PageHeader>

      <Panel
        title={`Integrations (${byExtension.length})`}
        subtitle="An extension is the container: one integration, its providers, and the models it serves."
        actions={
          <Button size="small" variant="outlined" onClick={() => setEditing(blankExtension())}>
            Add extension
          </Button>
        }
        flush
      >
        {byExtension.length === 0 && (
          <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
            None yet. <strong>Add provider</strong> starts from a table of known OpenAI-compatible
            endpoints, or takes a custom one.
          </Typography>
        )}
        {byExtension.map((g) => (
          <Box key={g.name}>
            {/* The extension itself. Clicking it opens the same YAML editor the
                flat Extensions panel used to own. */}
            <Row
              onClick={() => {
                if (g.ext) void openEntry('extension', g.name).then(setEditing)
              }}
            >
              <Stack direction="row" spacing={1.25} alignItems="baseline" flexWrap="wrap" useFlexGap>
                <Typography variant="subtitle2" sx={{ minWidth: 120 }}>
                  {g.name}
                </Typography>
                {g.pool && (
                  <Tooltip title="A virtual extension: it has no endpoint of its own and pools its members' catalogues. Membership is re-derived on every refresh, so a provider withdrawing a model does not take the pool down.">
                    <Chip size="small" color="info" variant="outlined" label="pool" />
                  </Tooltip>
                )}
                {g.ext?.cmd && (
                  <Tooltip title="One local process serving several models. They load, unload and are accounted for together, because they are the same bytes.">
                    <Chip size="small" variant="outlined" label="spawned" />
                  </Tooltip>
                )}
                {g.ext?.server && <Chip size="small" variant="outlined" label={g.ext.server} />}
                {g.provs.length > 0 && (
                  <Chip
                    size="small"
                    variant="outlined"
                    label={`${g.provs.length} provider${g.provs.length === 1 ? '' : 's'}`}
                  />
                )}
                {(g.ext?.provides ?? []).length > 0 && (
                  <Typography variant="caption" sx={{ color: C.textFaint }}>
                    serves {(g.ext?.provides ?? []).join(', ')}
                  </Typography>
                )}
                {g.pool?.lanes?.map((l) => (
                  <Tooltip
                    key={l.lane}
                    title="Every model in the pool joins this lane at this priority. Ask for the lane to get the pool."
                  >
                    <Chip size="small" variant="outlined" label={`lane ${l.lane}`} />
                  </Tooltip>
                ))}
              </Stack>
              {g.ext?.notes && (
                <Tooltip title={<span style={{ whiteSpace: 'pre-wrap' }}>{g.ext.notes}</span>}>
                  <Typography
                    variant="caption"
                    sx={{
                      color: C.textMuted,
                      whiteSpace: 'pre-wrap',
                      display: '-webkit-box',
                      WebkitLineClamp: 3,
                      WebkitBoxOrient: 'vertical',
                      overflow: 'hidden',
                      mt: 0.5,
                    }}
                  >
                    {g.ext.notes}
                  </Typography>
                </Tooltip>
              )}
            </Row>

            {/* Its providers, indented to say so. */}
            <Box sx={{ pl: 3, borderLeft: `2px solid ${C.border}`, ml: 2 }}>
        {g.provs.map((p) => {
          // A provider with no explicit credentials still has one — the
          // implicit "default" carrying the provider's own headers — and the
          // catalogue is fetched per credential, so that is what Browse uses.
          const creds = p.credentials ?? []
          const browseCred = creds[0]?.name ?? 'default'
          return (
            <Row key={`${p.extension}/${p.name}`}>
              <Stack direction="row" spacing={1.5} alignItems="center" flexWrap="wrap" useFlexGap>
                <Box sx={{ minWidth: 200 }}>
                  {/* No extension label: the row above and the indent say it. */}
                  <Typography variant="subtitle2">{p.name}</Typography>
                  <Typography variant="caption" sx={{ color: C.textMuted }}>
                    {p.host}:{p.port}
                    {p.basePath}
                  </Typography>
                </Box>

                <Stack direction="row" spacing={0.75} flexWrap="wrap" useFlexGap sx={{ flex: 1 }}>
                  {/* Where this provider's models come from. A directory filter
                      is NOT one of them — it only changes what Browse opens
                      pre-filtered to. */}
                  {Number(p.provides ?? 0) > 0 && (
                    <Tooltip title="Models written into the extension YAML by hand. Edit them there, not here.">
                      <Chip size="small" variant="outlined" label={`${p.provides} declared`} />
                    </Tooltip>
                  )}
                  {p.manual && (
                    <Tooltip title="Models are chosen one at a time off this provider's directory.">
                      <Chip size="small" variant="outlined" label="hand-picked" />
                    </Tooltip>
                  )}
                  {p.directory?.freeOnly && (
                    <Tooltip title="Browse opens pre-filtered to free models. A browsing default only — it imports nothing.">
                      <Chip size="small" variant="outlined" label="browse: free" />
                    </Tooltip>
                  )}
                  {(p.limits ?? []).map((l, i) => (
                    <Chip
                      key={i}
                      size="small"
                      variant="outlined"
                      label={`provider ${l.usd ? `$${l.usd}` : l.req}/${l.per}`}
                    />
                  ))}
                  {creds.length === 0 && (
                    <Tooltip title="No credentials block: the provider's own proxy headers are the one implicit account.">
                      <Chip size="small" variant="outlined" label="implicit account" />
                    </Tooltip>
                  )}
                  {creds.map((c) => (
                    <Chip
                      key={c.name}
                      size="small"
                      color={c.hasSecret ? 'success' : 'warning'}
                      variant="outlined"
                      label={`${c.name}${
                        c.hasSecret ? '' : ` · ${c.secretRef || 'no secret'} missing`
                      }`}
                    />
                  ))}
                </Stack>

                <Stack direction="row" spacing={1}>
                  <Button
                    size="small"
                    startIcon={<SearchIcon />}
                    onClick={() => setBrowse({ provider: p.name, credential: browseCred })}
                  >
                    Browse
                  </Button>
                  {/* Declaring a model by hand, for a provider whose catalogue
                      cannot be browsed — Groq and Cerebras publish no pricing or
                      context, so their directories cannot be filtered — or for
                      one model you want pinned by name regardless of what the
                      catalogue says today. */}
                  <Tooltip title="Declare a model of this provider by hand, instead of choosing one off its directory.">
                    <Button
                      size="small"
                      startIcon={<AddIcon />}
                      onClick={() =>
                        setAddLocal({ provider: p.name, extension: p.extension })
                      }
                    >
                      Model
                    </Button>
                  </Tooltip>
                  <Button size="small" onClick={() => setEdit(toInitial(p))}>
                    Edit
                  </Button>
                </Stack>
              </Stack>
            </Row>
          )
        })}
            </Box>
          </Box>
        ))}
      </Panel>

      {local.length > 0 && (
        <Panel title={`Local (${local.reduce((n, p) => n + p.models.length, 0)} models)`} flush>
          <Typography variant="body2" sx={{ px: 2, pt: 1.5, color: C.textMuted }}>
            Models that run on this box. Each owns its process, its GPU budget and its port —
            which is why they are declared rather than browsed: the catalogue is whatever you put
            on the disk. Served as <code>&lt;provider&gt;-&lt;id&gt;</code> like any remote.
          </Typography>
          {local.map((p) => (
            <Row key={p.name}>
              <Stack direction="row" spacing={1.5} alignItems="baseline" flexWrap="wrap" useFlexGap>
                <Box sx={{ minWidth: 140 }}>
                  <Typography variant="subtitle2">{p.name}</Typography>
                  <Typography variant="caption" sx={{ color: C.textFaint }}>
                    {p.models.length} model{p.models.length === 1 ? '' : 's'}
                  </Typography>
                </Box>
                {/* The provider's own notes. A real config field, so unlike a
                    YAML comment it survives the file being rewritten — which is
                    where an explanation about this provider belongs. */}
                {p.notes && (
                  <Tooltip title={<span style={{ whiteSpace: 'pre-wrap' }}>{p.notes}</span>}>
                    <Typography
                      variant="caption"
                      sx={{
                        color: C.textMuted,
                        flexBasis: '100%',
                        whiteSpace: 'pre-wrap',
                        display: '-webkit-box',
                        WebkitLineClamp: 3,
                        WebkitBoxOrient: 'vertical',
                        overflow: 'hidden',
                        mt: 0.5,
                      }}
                    >
                      {p.notes}
                    </Typography>
                  </Tooltip>
                )}
                <Stack direction="row" spacing={0.75} flexWrap="wrap" useFlexGap sx={{ flex: 1 }}>
                  {Number(p.barePrecedence) > 0 ? (
                    <Tooltip
                      title={`An unprefixed name that nothing else claims resolves here (precedence ${p.barePrecedence}). This is what keeps callers working after the prefix rename.`}
                    >
                      <Chip size="small" variant="outlined" label="bare names → here" />
                    </Tooltip>
                  ) : (
                    <Tooltip title="Only the prefixed name resolves; barePrecedence is 0.">
                      <Chip size="small" variant="outlined" label="prefixed only" />
                    </Tooltip>
                  )}
                </Stack>
                <Button
                  size="small"
                  startIcon={<AddIcon />}
                  onClick={() => setAddLocal({ provider: p.name })}
                >
                  Add model
                </Button>
              </Stack>
              {p.models.length === 0 && (
                <Typography variant="caption" sx={{ color: C.textFaint, pl: 0.5 }}>
                  no models yet
                </Typography>
              )}
              {/* Grouped by BOX, because that is the question actually asked of
                  this list: "what runs on box1". A per-row server chip said the
                  same thing scattered across twenty rows, which reads as a flat
                  list of processes with no machine behind them. A model with no
                  server is a pure proxy and groups under its own heading rather
                  than being hidden or faked onto a host. */}
              {groupByBox(p.models).map(([box, boxModels]) => (
                <Box key={box} sx={{ mt: 1 }}>
                  <Stack
                    direction="row"
                    spacing={1}
                    alignItems="baseline"
                    sx={{ pl: 2, pt: 0.5 }}
                  >
                    <Typography
                      variant="caption"
                      sx={{ color: C.textMuted, fontWeight: 600, letterSpacing: 0.4 }}
                    >
                      {box === UNPLACED ? 'no host (proxy)' : box}
                    </Typography>
                    <Chip size="small" variant="outlined" label={boxModels.length} />
                  </Stack>
                  {boxModels.map((m) => (
                <Stack
                  key={m.id}
                  direction="row"
                  spacing={1.5}
                  alignItems="center"
                  flexWrap="wrap"
                  useFlexGap
                  sx={{ pl: 2, py: 0.75, borderTop: `1px solid ${C.border}`, mt: 0.75 }}
                >
                  <Box sx={{ minWidth: 220 }}>
                    <Typography variant="body2" sx={{ fontFamily: 'monospace', fontSize: 12.5 }}>
                      {m.served}
                    </Typography>
                    <Typography variant="caption" sx={{ color: C.textFaint }}>
                      {m.bare ? `also answers to ${m.id}` : 'prefixed name only'}
                    </Typography>
                  </Box>
                  <Stack direction="row" spacing={0.75} flexWrap="wrap" useFlexGap sx={{ flex: 1 }}>
                    {m.type && <Chip size="small" variant="outlined" label={m.type} />}
                    {/* No server chip: the group heading above already says
                        which box, and repeating it on every row was the flat
                        list's way of answering a question the grouping now
                        answers once. */}
                    <Tooltip
                      title={
                        m.hasCmd
                          ? 'corrallm starts and stops this process'
                          : 'A pure proxy — corrallm forwards to it and never starts or stops it'
                      }
                    >
                      <Chip
                        size="small"
                        variant="outlined"
                        label={m.hasCmd ? 'spawned' : 'proxy'}
                      />
                    </Tooltip>
                  </Stack>
                  <Button
                    size="small"
                    onClick={() => setAddLocal({ provider: p.name, editId: m.id })}
                  >
                    Edit
                  </Button>
                  <Button
                    size="small"
                    onClick={() => navigate({ to: '/m/$name', params: { name: m.served } })}
                  >
                    Inspect
                  </Button>
                  <Button
                    size="small"
                    color="error"
                    onClick={() => {
                      setDeleteErr('')
                      setConfirmDelete({ served: m.served, hasCmd: m.hasCmd })
                    }}
                  >
                    Delete
                  </Button>
                </Stack>
              ))}
                </Box>
              ))}
            </Row>
          ))}
        </Panel>
      )}

      <AssignedModels />

      {/* The Pools and Extensions panels are gone: both described objects that
          are now rows in Integrations above. `free` used to appear three times
          — as an extension, as a pool, and implicitly as the parent of three
          unattached providers — which is three places to look for one thing. */}

      <Panel
        title={`Lanes (${lanes.length})`}
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
            <Row
              key={l.name}
              onClick={() => {
                void openEntry('lane', l.name).then(setEditing)
              }}
            >
              <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                <Typography variant="subtitle2">{l.name}</Typography>
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {l.members.map((mem) => mem.model).join('  \u2192  ')}
                </Typography>
              </Box>
            </Row>
          ))
        )}
      </Panel>

      <Panel title={`Credential store (${secrets.length})`}>
        <Typography variant="body2" sx={{ opacity: 0.75, mb: 1 }}>
          Names only. No endpoint returns a value — that is what keeps the config document safe to
          read and share.
        </Typography>
        <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
          {secrets.map((s) => (
            <Chip key={s} size="small" label={s} />
          ))}
          {secrets.length === 0 && (
            <Typography variant="caption" sx={{ opacity: 0.7 }}>
              empty
            </Typography>
          )}
        </Stack>
      </Panel>

      <ProviderDialog open={addOpen} onClose={() => setAddOpen(false)} secrets={secrets} />
      <ProviderDialog
        open={edit != null}
        onClose={() => setEdit(null)}
        initial={edit ?? undefined}
        secrets={secrets}
      />
      {addLocal && (
        <ProviderModelDialog
          open
          onClose={() => setAddLocal(null)}
          provider={addLocal.provider}
          extension={addLocal.extension}
          editId={addLocal.editId}
        />
      )}
      <Dialog open={confirmDelete != null} onClose={() => setConfirmDelete(null)}>
        <DialogTitle>Delete {confirmDelete?.served}?</DialogTitle>
        <DialogContent>
          <Typography variant="body2">
            Removes it from the provider&apos;s config. Callers asking for it — by this name or its
            unprefixed one — start getting a 404.
          </Typography>
          {confirmDelete?.hasCmd && (
            <Typography variant="body2" sx={{ mt: 1.5, color: C.warn }}>
              If it is running right now, the process keeps running and keeps its memory: a config
              reload deliberately does not kill backends that are already up. Unload it from its
              model page first if you want the memory back before the next restart.
            </Typography>
          )}
          <Typography variant="caption" sx={{ display: 'block', mt: 1.5, color: C.textFaint }}>
            A lane member cannot be deleted until it is removed from the lane; the server will say
            which lane.
          </Typography>
          {deleteErr && (
            <Alert severity="error" sx={{ mt: 2 }}>
              {deleteErr}
            </Alert>
          )}
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirmDelete(null)}>Cancel</Button>
          <Button
            color="error"
            variant="contained"
            disabled={del.isPending}
            onClick={() => confirmDelete && del.mutate(confirmDelete.served)}
          >
            {del.isPending ? 'Deleting…' : 'Delete'}
          </Button>
        </DialogActions>
      </Dialog>

      {browse && (
        <CatalogDialog
          open
          onClose={() => setBrowse(null)}
          provider={browse.provider}
          credential={browse.credential}
        />
      )}
      <EntryEditor
        editing={editing}
        onChange={setEditing}
        onClose={() => setEditing(null)}
        invalidate={['providers', 'config']}
      />
    </Box>
  )
}

export const Route = createFileRoute('/providers')({ component: ProvidersPage })
