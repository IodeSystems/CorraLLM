import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type MouseEvent, type ReactNode } from 'react'
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
import { graphql } from '@/gql'
import type { OverviewQuery, SelectionsQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { Panel, PageHeader, Row, Stat } from '@/Panel'
import { C } from '@/theme'
import { extractMessage, fmtInt, fmtTime } from '@/format'
import { useReadOnly } from '@/auth'
import { Loading } from '@/Loading'
import { PauseDialog, pauseCost, PauseCostNote } from '@/Pause'
import { useSetDisabled } from '@/Disable'
import { stateLabel } from '@/MachineData'
import { FilterField, matches } from '@/Paging'
import { EntryEditor, openEntry, type EntryEdit } from '@/EntryEditor'
import { ProviderDialog, toLimit, type ProviderInitial } from '@/ProviderDialog'
import { ProviderModelDialog } from '@/ProviderModelDialog'
import { AddModelDialog } from '@/AddModelDialog'
import { CatalogDialog } from '@/CatalogDialog'
import { OperatorNote } from '@/OperatorNote'
import { RefusingBanner } from '@/SetupBudgets'
import { byExtension, groupByBox, useSetupData, UNPLACED, type ProviderRow } from '@/SetupData'

const OverviewDoc = graphql(/* GraphQL */ `
  query Overview {
    corrallm {
      health {
        status
        version
      }
      overview {
        servers {
          server
          maxConcurrent
          devicePool
          agentStatus
          agentLastSeen
          pools {
            pool
            totalBytes
            reserveBytes
          }
        }
        models {
          name
          # Where it comes from: the page groups by these.
          provider
          extension
          persistent
          ttl
          idleUnload
          evictCost
          spawnable
          remote
          procKey
          # STILL LISTED, AND NOT SERVED. A model out of the setup stays in the
          # config and stays on this query, because this page is where the way
          # back has to be — and something that is silent everywhere else has to
          # be visible somewhere or the decision cannot be undone.
          disabled
          paused
          pauseScope
          pausedByExtension
          pauseReason
          pauseResumeMs
          placements {
            name
            server
            state
          }
          modalities {
            modality
            maxResolution
            formats
            maxTokens
          }
          capability
          type
          quality
          server
          target
          maxConcurrent
          maxTokens
          cmd
        }
        lanes {
          name
          members {
            model
            ttl
            idleUnload
            evictCost
          }
          ladder {
            model
            origin
            pool
            # Listed but never walked. The lane panel draws it so the decision
            # has somewhere to be seen and undone; pauseCost drops it so a list
            # is not credited with cover it does not have.
            disabled
          }
        }
        extensions {
          name
          cmd
          server
          provides
          notes
          state
          draining
          inFlight
          pinned
          paused
          pauseReason
          pauseResumeMs
        }
        groups {
          name
          weight
          shareCurrency
          interruptible
          acceptDegrade
          qualityFloor
          stages {
            type
            policy
          }
        }
        keys {
          key
          group
        }
        # WHAT CANNOT SERVE TODAY EITHER. A pause's fall-through is only real if
        # the rungs behind it work, and a backend answering 401/402/403 does not.
        # One extra field on a request this page already makes.
        refusing {
          backend
        }
      }
      residency {
        stopping
        servers {
          server
          pools {
            pool
            budget
            used
          }
        }
        gpus {
          available
          name
          uuid
          pool
          totalBytes
          usedBytes
          freeBytes
        }
        host {
          available
          name
          totalBytes
          usedBytes
          freeBytes
        }
        models {
          name
          modelName
          procKey
          remote
          server
          state
          refs
          nCtx
          nSlots
          hasUi
          footprintMiB
          usage {
            pool
            bytes
          }
        }
      }
    }
  }
`)


/**
 * One action whose meaning follows residency, replacing a Load/Unload pair that
 * were both always enabled — so half of every pair was a silent no-op.
 *
 *   absent/failed -> Load     ready -> Unload     loading/evicting -> Cancel
 *
 * Cancel unloads: mid-spawn there is nothing else useful to do, and leaving a
 * half-loaded backend occupying VRAM is the state people actually get stuck in.
 */
function ResidencyToggle(props: {
  state: string
  persistent: boolean
  paused: boolean
  stopping: boolean
  busy: boolean
  onLoad: () => void
  onUnload: () => void
}) {
  const { state, persistent, paused, stopping, onLoad, onUnload } = props
  // A viewer's load or unload would be refused; the buttons stay, inert.
  const readOnly = useReadOnly()
  const busy = props.busy || readOnly
  const inFlight = state === 'loading' || state === 'evicting'
  const resident = state === 'ready'

  // Mid-teardown: the process is gone from the residency ledger (its pools are
  // already freed) but has not actually exited, and a load aimed at it is
  // refused until it does. Offering Load here handed out a button whose only
  // outcome was an error message.
  if (stopping) {
    return (
      <Tooltip title="Still stopping — wait for it to exit before loading it again">
        <span>
          <Button size="small" variant="outlined" disabled>
            Stopping
          </Button>
        </span>
      </Tooltip>
    )
  }

  // A paused model refuses to load server-side, so offering Load would hand the
  // operator a button whose only outcome is an error message. Resume is the
  // action that exists here, and it is the adjacent button.
  if (paused && !resident && !inFlight) {
    return (
      <Tooltip title="Paused — resume it to load">
        <span>
          <Button size="small" variant="outlined" disabled>
            Load
          </Button>
        </span>
      </Tooltip>
    )
  }

  if (inFlight) {
    return (
      <Button size="small" variant="outlined" color="warning" disabled={busy || persistent} onClick={onUnload}>
        Cancel
      </Button>
    )
  }
  if (resident) {
    return (
      <Tooltip title={persistent ? 'pinned models cannot be unloaded' : ''}>
        <span>
          <Button size="small" variant="outlined" color="warning" disabled={busy || persistent} onClick={onUnload}>
            Unload
          </Button>
        </span>
      </Tooltip>
    )
  }
  return (
    <Button size="small" variant="outlined" disabled={busy} onClick={onLoad}>
      Load
    </Button>
  )
}

const ExtLoadDoc = graphql(/* GraphQL */ `
  mutation LoadExtension($extension: String!) {
    corrallm {
      loadExtension(body: { extension: $extension }) {
        ok
        message
      }
    }
  }
`)

const ExtUnloadDoc = graphql(/* GraphQL */ `
  mutation UnloadExtension($extension: String!) {
    corrallm {
      unloadExtension(body: { extension: $extension }) {
        ok
        message
        evicted
        draining
      }
    }
  }
`)

const ExtPauseDoc = graphql(/* GraphQL */ `
  mutation PauseExtension($extension: String!, $resumeAt: String, $reason: String) {
    corrallm {
      pauseExtension(body: { extension: $extension, resumeAt: $resumeAt, reason: $reason }) {
        ok
        message
        target
        affected
        evicted
        draining
      }
    }
  }
`)

const ExtUnpauseDoc = graphql(/* GraphQL */ `
  mutation UnpauseExtension($extension: String!) {
    corrallm {
      unpauseExtension(body: { extension: $extension }) {
        ok
        message
      }
    }
  }
`)

/**
 * ConfirmDialog is the guard on an action whose blast radius is wider than the
 * row it was clicked from.
 *
 * Unloading an extension-hosted model unloads the whole extension — every model
 * it provides goes with it. That has always been true and was never said
 * anywhere: the button sat on the oidio-tts row and silently took down stt,
 * stt-diarize and realtime-stt too.
 */
function ConfirmDialog(props: {
  open: boolean
  title: string
  body: string
  confirmLabel: string
  busy: boolean
  onClose: () => void
  onConfirm: () => void
}) {
  const { open, title, body, confirmLabel, busy, onClose, onConfirm } = props
  return (
    <Dialog open={open} onClose={onClose} maxWidth="xs" fullWidth>
      <DialogTitle>{title}</DialogTitle>
      <DialogContent>
        <Alert severity="warning">{body}</Alert>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button
          variant="contained"
          color="warning"
          disabled={busy}
          onClick={() => {
            onConfirm()
            onClose()
          }}
        >
          {confirmLabel}
        </Button>
      </DialogActions>
    </Dialog>
  )
}

function stateColor(state?: string): 'success' | 'info' | 'warning' | 'error' | 'default' {
  switch (state) {
    case 'ready':
      return 'success'
    case 'loading':
      return 'info'
    case 'evicting':
      return 'warning'
    case 'failed':
      return 'error'
    default:
      return 'default'
  }
}

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

// Blank templates for the two entries this page creates as YAML. They live with
// the page rather than in the editor, because the editor is a text box and this
// is the domain's opinion about what to type in it.
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

// THE OBJECT IS A FALLBACK LIST. The config file calls it a `lane` and so does
// the API, and OB-3 says that word is the gloss and never the label.
function blankLane(): EntryEdit {
  return {
    kind: 'lane',
    existing: false,
    name: '',
    yaml: `# A fallback list — a "lane" in this file — is an ORDERED list of
# models. Asking for its name allows a substitute from further down;
# asking for a model by name pins the request to that model.
members:
  - some-local-model      # best first
  - some-remote-model     # spilled to when the local one is full
`,
  }
}

type Ov = NonNullable<OverviewQuery['corrallm']['overview']>
type OvModel = Ov['models'][number]
type OvExtension = Ov['extensions'][number]
type Selection = NonNullable<SelectionsQuery['corrallm']['listSelections']>['selections'][number]

/**
 * Models: what can be called, grouped by the provider that serves it.
 *
 * This page and Setup's Providers and Models tabs were three views of one list.
 * "Add a model" lived on Setup, "what state is it in" lived here, and a model
 * chosen off a catalog appeared on a third panel that knew neither. One page
 * now, one panel per provider:
 *
 *   a LOCAL provider      models this box runs itself, by the box that runs
 *                         them; Edit and Delete act on the config entry
 *   an EXTENSION          one integration: its providers (with their accounts,
 *                         catalog and limits) and the models each one serves.
 *                         A hosted extension is one process, so load / unload /
 *                         pause sit on the extension, the unit they act on
 *   standalone            a model declared on its own under `models:`
 *
 * MODEL ROWS STILL CARRY NO RUNTIME CONTROLS. Load, unload, pause and logs act
 * on ONE process on ONE box, and a model can be served from more than one, so
 * those live on the model page, one set per placement. The buttons a row does
 * carry — Edit, Delete, Remove — act on the config entry, of which there is
 * exactly one however many boxes serve it.
 *
 * Grouped by provider rather than by capability: the question asked of this
 * page is "what do I get from groq" or "what runs here", and the type of each
 * model is a chip on its row.
 */
function ModelsPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const readOnly = useReadOnly()
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)
  const [q, setQ] = useState('')

  const ovq = useQuery({
    queryKey: ['overview'],
    queryFn: () => gqlClient.request(OverviewDoc),
    refetchInterval: 15000, // fallback; live via SSE (useLiveEvents)
  })
  const setup = useSetupData()
  const sel = useQuery({
    queryKey: ['selections'],
    queryFn: () => gqlClient.request(SelectionsDoc),
  })

  // Dialogs. Each "add" opens FROM the place it adds to, so the click is the
  // answer to "where"; the header buttons are for "I do not know yet".
  const [addProviderTo, setAddProviderTo] = useState<string | null>(null)
  const [editProvider, setEditProvider] = useState<ProviderInitial | null>(null)
  const [addModel, setAddModel] = useState<{ localProvider?: string } | null>(null)
  const [providerModel, setProviderModel] = useState<{
    provider: string
    extension?: string
    editId?: string
  } | null>(null)
  const [browse, setBrowse] = useState<{ provider: string; credential: string } | null>(null)
  const [editing, setEditing] = useState<EntryEdit | null>(null)
  // Deleting a model is not undoable from here, so it is confirmed by name.
  const [confirmDelete, setConfirmDelete] = useState<{ served: string; hasCmd: boolean } | null>(null)
  const [deleteErr, setDeleteErr] = useState('')

  const done = (text?: string | null, ok?: boolean | null) => {
    setMsg({ ok: !!ok, text: text ?? '' })
    void qc.invalidateQueries({ queryKey: ['overview'] })
  }
  const fail = (e: unknown) => setMsg({ ok: false, text: String(e) })

  const extLoad = useMutation({
    mutationFn: (extension: string) => gqlClient.request(ExtLoadDoc, { extension }),
    onSuccess: (d) => done(d.corrallm.loadExtension?.message, d.corrallm.loadExtension?.ok),
    onError: fail,
  })
  const extUnload = useMutation({
    mutationFn: (extension: string) => gqlClient.request(ExtUnloadDoc, { extension }),
    onSuccess: (d) => done(d.corrallm.unloadExtension?.message, d.corrallm.unloadExtension?.ok),
    onError: fail,
  })
  const extPause = useMutation({
    mutationFn: (v: { extension: string; resumeAt: string | null; reason: string }) =>
      gqlClient.request(ExtPauseDoc, { extension: v.extension, resumeAt: v.resumeAt, reason: v.reason }),
    onSuccess: (d) => done(d.corrallm.pauseExtension?.message, d.corrallm.pauseExtension?.ok),
    onError: fail,
  })
  const extUnpause = useMutation({
    mutationFn: (extension: string) => gqlClient.request(ExtUnpauseDoc, { extension }),
    onSuccess: (d) => done(d.corrallm.unpauseExtension?.message, d.corrallm.unpauseExtension?.ok),
    onError: fail,
  })
  const del = useMutation({
    mutationFn: (name: string) => gqlClient.request(DeleteModelDoc, { name }),
    onSuccess: (r) => {
      const res = r.corrallm?.deleteEntry
      // The server refuses to delete a model a fallback list still names, and
      // says which list. That is the useful half of the answer, so it goes on
      // screen rather than being collapsed into a generic failure.
      if (!res?.ok) {
        setDeleteErr(res?.message ?? 'delete refused')
        return
      }
      setConfirmDelete(null)
      setDeleteErr('')
      void qc.invalidateQueries({ queryKey: ['providers'] })
      void qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (e) => setDeleteErr(extractMessage(e)),
  })
  const unassign = useMutation({
    mutationFn: (body: { provider: string; credential: string; model: string }) =>
      gqlClient.request(UnassignDoc, { body }),
    onSuccess: (d) => {
      const r = d.corrallm?.unassignModel
      if (r && !r.ok) setMsg({ ok: false, text: r.message ?? 'remove refused' })
      void qc.invalidateQueries({ queryKey: ['selections'] })
      void qc.invalidateQueries({ queryKey: ['catalog'] })
      void qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: fail,
  })
  // THE WAY BACK. Taking a model out of the setup happens on the budgets
  // banner, where the evidence is; putting it back happens here, because here
  // is permanent and that banner disappears the moment the decision is made.
  const putBack = useSetDisabled({
    onDone: (text, ok) => {
      setMsg({ ok, text })
      void qc.invalidateQueries({ queryKey: ['overview'] })
      void qc.invalidateQueries({ queryKey: ['providers'] })
      void qc.invalidateQueries({ queryKey: ['quota'] })
    },
    onFail: (text) => setMsg({ ok: false, text }),
  })
  const busy =
    extLoad.isPending || extUnload.isPending || extPause.isPending || extUnpause.isPending || putBack.isPending

  // What the pause dialog is collecting a resume time for: an extension, which
  // takes every model it serves with it.
  const [pauseFor, setPauseFor] = useState<{ name: string; taking: string[]; warning: string } | null>(null)
  // A pending unload whose blast radius is wider than its row.
  const [confirmUnload, setConfirmUnload] = useState<{ title: string; body: string; run: () => void } | null>(null)

  if (ovq.isLoading || setup.isLoading) {
    return <Loading />
  }
  const err = ovq.error ?? setup.error
  if (err) {
    return (
      <Box sx={{ p: 3 }}>
        <Typography color="error">{String(err)}</Typography>
      </Box>
    )
  }

  // No non-null assertion. A query can settle with neither data nor error —
  // notably the moment a 401 handler calls clearToken() and reloads — and
  // `q.data!` then threw into the error boundary instead of showing the login
  // screen it was one tick away from.
  const c = ovq.data?.corrallm
  const list = setup.data?.corrallm?.listProviders
  if (!c || !list) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }
  const ov = c.overview

  // WHAT THIS BOX SERVES, AND WHAT IT KEEPS BUT DOES NOT SERVE — split before
  // anything else reads either. A model out of the setup is never resident and
  // answers nothing, so among the provider groups it would be the quietest
  // possible way to lose something. It gets a panel of its own instead.
  const allModels = ov?.models ?? []
  const models = allModels.filter((m) => !m.disabled)
  const outOfSetup = allModels.filter((m) => m.disabled)
  const lanes = ov?.lanes ?? []
  const secrets = list.secrets ?? []
  // Backends that already cannot serve, so a pause is not allowed to count them
  // as somewhere to fall through to.
  const alreadyOut = new Set<string>([
    ...models.filter((m) => m.paused).map((m) => m.name),
    ...(ov?.refusing ?? []).map((r) => r.backend),
  ])

  // Residency keyed by the BACKING PROCESS, with remote backends dropped: an
  // extension's models share one process, and a remote model has none, so
  // neither can be mistaken for a resident one.
  const stateByProc = new Map((c.residency?.models ?? []).filter((m) => !m.remote).map((m) => [m.procKey, m]))
  const stoppingProcs = new Set(c.residency?.stopping ?? [])
  const stateOf = (m: OvModel) => (m.remote ? undefined : stateByProc.get(m.procKey)?.state)
  const loadedCount = models.filter((m) => {
    const st = stateOf(m)
    return st === 'ready' || st === 'loading' || st === 'evicting'
  }).length

  const extState = new Map((ov?.extensions ?? []).map((e) => [e.name, e]))
  const localByServed = new Map(list.local.flatMap((p) => p.models.map((m) => [m.served, m] as const)))
  const selections = sel.data?.corrallm?.listSelections?.selections ?? []
  const selByModel = new Map(selections.map((s) => [`${s.provider}\u0000${s.model}`, s]))

  // The filter narrows models; a group whose own name matches keeps all of its
  // models, and a group left with none is dropped.
  const filtering = q.trim() !== ''
  const modelMatches = (m: OvModel) => matches(q, m.name, m.type, m.server, m.provider, m.extension)
  const pick = (ms: OvModel[], groupNames: string[]) =>
    !filtering || groupNames.some((n) => matches(q, n)) ? ms : ms.filter(modelMatches)

  // GROUPS, in the order a person thinks of them: what runs here, then what
  // comes from elsewhere, then whatever belongs to neither.
  const localGroups = list.local.map((p) => ({
    p,
    models: pick(
      models.filter((m) => !m.extension && m.provider === p.name),
      [p.name],
    ),
  }))
  const integrations = byExtension(ov?.extensions ?? [], list.pools ?? [], list.providers ?? []).map((g) => {
    const inExt = models.filter((m) => m.extension === g.name)
    const provs = g.provs.map((p) => ({
      p,
      models: pick(
        inExt.filter((m) => m.provider === p.name),
        [g.name, p.name, p.host],
      ),
      // Chosen off the catalog but not in the served registry: the provider or
      // its id may have gone away. Still listed, because Remove is here.
      stale: selections.filter((s) => s.provider === p.name && !s.serving),
    }))
    const named = new Set(g.provs.map((p) => p.name))
    // A hosted extension's models, and any whose provider is the extension's
    // implicit one, belong to the extension itself.
    const direct = pick(
      inExt.filter((m) => !named.has(m.provider)),
      [g.name],
    )
    return { g, provs, direct }
  })
  const standalone = pick(
    models.filter((m) => !m.extension && !list.local.some((p) => p.name === m.provider)),
    ['standalone'],
  )

  const shownLocal = localGroups.filter((x) => !filtering || x.models.length > 0)
  const shownIntegrations = integrations.filter(
    (x) =>
      !filtering ||
      x.direct.length > 0 ||
      x.provs.some((p) => p.models.length > 0) ||
      matches(q, x.g.name, ...x.g.provs.map((p) => p.name)),
  )

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
    limits: (p.limits ?? []).map(toLimit),
    credentials: (p.credentials ?? []).map((cr) => ({
      name: cr.name,
      secretRef: cr.secretRef ?? '',
      headerName: cr.headerName ?? '',
      allow: cr.allow ?? [],
      limits: (cr.limits ?? []).map(toLimit),
    })),
  })

  // One model, one row. The row navigates; the buttons on its right act on the
  // config entry and stop the click from navigating.
  const modelRow = (m: OvModel, opts: { box?: boolean } = {}) => {
    const st = stateOf(m)
    const places = m.placements ?? []
    const local = !m.extension ? localByServed.get(m.name) : undefined
    const s = selByModel.get(`${m.provider}\u0000${m.name}`)
    const stop = (f: () => void) => (e: MouseEvent) => {
      e.stopPropagation()
      f()
    }
    return (
      <Row key={m.name} onClick={() => navigate({ to: '/m/$name', params: { name: m.name } })}>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
          <Chip
            size="small"
            label={m.remote ? 'proxy' : stoppingProcs.has(m.procKey) ? 'stopping' : stateLabel(st ?? 'absent')}
            color={stateColor(m.remote ? 'proxy' : (st ?? 'absent'))}
            sx={{ minWidth: 68 }}
          />
          <Typography variant="subtitle2">{m.name}</Typography>
          {m.type && <Chip size="small" variant="outlined" label={m.type} />}
          {places.length > 1 && (
            <Tooltip title={`Served from ${places.length} boxes — open the model to act on one`}>
              <Chip size="small" variant="outlined" label={`${places.length} boxes`} />
            </Tooltip>
          )}
          {opts.box && places.length === 1 && places[0]?.server && (
            <Chip size="small" variant="outlined" label={places[0].server} />
          )}
          {(m.modalities ?? [])
            .map((x) => x?.modality)
            .filter((x): x is string => !!x && x !== 'text')
            .map((mod) => (
              <Chip key={mod} size="small" color="success" variant="outlined" label={mod} />
            ))}
          {m.persistent && <Chip size="small" variant="outlined" label="pinned" />}
          {m.paused && <Chip size="small" color="warning" variant="outlined" label="paused" />}
          {/* A catalog choice says which fallback list it joined and where;
              lower is tried first (the Fallback lists panel says so). */}
          {s?.lanes.map((l) => <Chip key={l.lane} size="small" label={`${l.lane} @ ${l.order}`} />)}
          <Box sx={{ flexGrow: 1 }} />
          {local && (
            <>
              <Button
                size="small"
                disabled={readOnly}
                onClick={stop(() => setProviderModel({ provider: m.provider, editId: local.id }))}
              >
                Edit
              </Button>
              <Button
                size="small"
                color="error"
                disabled={readOnly}
                onClick={stop(() => {
                  setDeleteErr('')
                  setConfirmDelete({ served: m.name, hasCmd: local.hasCmd })
                })}
              >
                Delete
              </Button>
            </>
          )}
          {s && (
            <Tooltip title="Takes it off the list chosen from this provider's catalog; requests for it are then refused. Anything mid-answer finishes.">
              <Button
                size="small"
                color="error"
                disabled={readOnly || unassign.isPending}
                onClick={stop(() => unassign.mutate({ provider: s.provider, credential: s.credential, model: s.model }))}
              >
                Remove
              </Button>
            </Tooltip>
          )}
        </Box>
      </Row>
    )
  }

  const staleRow = (s: Selection) => (
    <Row key={`${s.provider}/${s.credential}/${s.model}`}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
        <Tooltip title="On record, but not in the served registry — the provider or its id may have gone away.">
          <Chip size="small" color="warning" label="not serving" sx={{ minWidth: 68 }} />
        </Tooltip>
        <Typography variant="subtitle2">{s.model}</Typography>
        <Typography variant="caption" sx={{ color: C.textFaint }}>
          {s.credential}
          {s.upstream ? ` · ${s.upstream}` : ''}
        </Typography>
        <Box sx={{ flexGrow: 1 }} />
        <Button
          size="small"
          color="error"
          disabled={readOnly || unassign.isPending}
          onClick={() => unassign.mutate({ provider: s.provider, credential: s.credential, model: s.model })}
        >
          Remove
        </Button>
      </Box>
    </Row>
  )

  const empty = (text: ReactNode) => (
    <Typography variant="body2" sx={{ px: 2, py: 1.5, color: C.textFaint }}>
      {text}
    </Typography>
  )

  // An extension that runs a process loads, unloads and pauses as a unit: every
  // model it serves goes with it, so the controls sit on the extension.
  const extControls = (e: OvExtension) => {
    const stopping = stoppingProcs.has(`extension:${e.name}`)
    return (
      <>
        <Chip
          size="small"
          label={stopping ? 'stopping' : stateLabel(e.state)}
          color={stopping ? 'warning' : stateColor(e.state)}
          variant={e.state !== 'absent' || stopping ? 'filled' : 'outlined'}
        />
        {e.paused && (
          <Tooltip title={e.pauseReason || 'No reason given'}>
            <Chip
              size="small"
              color="warning"
              label={'paused' + (Number(e.pauseResumeMs) > 0 ? ` until ${fmtTime(e.pauseResumeMs)}` : '')}
            />
          </Tooltip>
        )}
        {e.pinned && <Chip size="small" variant="outlined" label="pinned" />}
        {Number(e.inFlight) > 0 && <Chip size="small" variant="outlined" label={`${fmtInt(e.inFlight)} in flight`} />}
        {e.draining && <Chip size="small" color="warning" variant="outlined" label="draining" />}
        <ResidencyToggle
          state={e.state}
          persistent={!!e.pinned}
          paused={!!e.paused}
          stopping={stopping}
          busy={busy}
          onLoad={() => extLoad.mutate(e.name)}
          onUnload={() =>
            setConfirmUnload({
              title: `Unload ${e.name}?`,
              body: `This stops the ${e.name} process, taking every model it serves down with it: ${e.provides.join(', ')}.`,
              run: () => extUnload.mutate(e.name),
            })
          }
        />
        {e.paused ? (
          <Button size="small" variant="outlined" color="success" disabled={busy} onClick={() => extUnpause.mutate(e.name)}>
            Resume
          </Button>
        ) : (
          <Button
            size="small"
            variant="outlined"
            color="warning"
            disabled={busy}
            onClick={() =>
              setPauseFor({
                name: e.name,
                taking: e.provides,
                warning: `Takes every model ${e.name} serves out of service: ${e.provides.join(', ')}.`,
              })
            }
          >
            Pause
          </Button>
        )}
      </>
    )
  }

  const nothing =
    shownLocal.length === 0 && shownIntegrations.length === 0 && standalone.length === 0 && filtering

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Models">
        <Button
          size="small"
          variant="contained"
          startIcon={<AddIcon />}
          disabled={readOnly}
          onClick={() => setAddProviderTo('')}
        >
          Add provider
        </Button>
        <Button size="small" variant="outlined" startIcon={<AddIcon />} disabled={readOnly} onClick={() => setAddModel({})}>
          Add model
        </Button>
        <Tooltip title="One integration — an upstream with several providers, or one local process serving several models — written as YAML.">
          <Button size="small" disabled={readOnly} onClick={() => setEditing(blankExtension())}>
            Add extension
          </Button>
        </Tooltip>
        <Chip size="small" variant="outlined" label={`${models.length} configured`} />
        <Chip size="small" variant="outlined" label={`${loadedCount} loaded`} />
        {/* ON ARRIVAL, NOT AFTER A SCROLL: a decision that is silent everywhere
            else has to be countable somewhere without hunting. "taken out", not
            "out of" — beside "11 configured" the shorter phrasing reads as a
            proportion. */}
        {outOfSetup.length > 0 && (
          <Chip size="small" variant="outlined" color="warning" label={`${outOfSetup.length} taken out of the setup`} />
        )}
        <Box sx={{ flexGrow: 1 }} />
        <FilterField value={q} onChange={setQ} placeholder="model, provider, type, host" />
      </PageHeader>

      {readOnly && (
        <Typography variant="body2" sx={{ opacity: 0.75, mt: -2 }}>
          Read-only: your role cannot change models or providers.
        </Typography>
      )}

      {msg && (
        <Alert severity={msg.ok ? 'success' : 'error'} onClose={() => setMsg(null)}>
          {msg.text}
        </Alert>
      )}

      {/* A backend refusing every request is a fault about a provider, so it is
          on the page the providers are on. */}
      <RefusingBanner />

      {nothing && <Alert severity="info">No model or provider matches “{q}”.</Alert>}

      {shownLocal.map(({ p, models: ms }) => (
        <Panel
          key={`local/${p.name}`}
          title={p.name}
          badge={<Chip size="small" variant="outlined" label="runs here" />}
          subtitle={`${ms.length} model${ms.length === 1 ? '' : 's'} this box runs itself`}
          help={
            <>
              Edit changes how a model is started. Delete removes it from the configuration; anything
              mid-answer on it ends there.{' '}
              {Number(p.barePrecedence) > 0
                ? `An unprefixed name that nothing else claims resolves here (precedence ${p.barePrecedence}).`
                : 'Only the prefixed name resolves (barePrecedence is 0).'}
            </>
          }
          actions={
            <Button size="small" startIcon={<AddIcon />} disabled={readOnly} onClick={() => setAddModel({ localProvider: p.name })}>
              Model
            </Button>
          }
          flush
        >
          {p.notes && (
            <Row>
              <OperatorNote text={p.notes} />
            </Row>
          )}
          {ms.length === 0 && empty('No models yet.')}
          {/* Grouped by BOX, because that is the question asked of this list:
              "what runs on box1". A model with no server is a pure proxy and
              groups under its own heading. */}
          {groupByBox(ms).map(([box, boxModels]) => (
            <Box key={box}>
              <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, px: 2, pt: 1 }}>
                <Typography variant="caption" sx={{ color: C.textMuted, fontWeight: 600, letterSpacing: 0.4 }}>
                  {box === UNPLACED ? 'no host (proxy)' : box}
                </Typography>
                <Chip size="small" variant="outlined" label={boxModels.length} />
              </Box>
              {boxModels.map((m) => modelRow(m))}
            </Box>
          ))}
        </Panel>
      ))}

      {shownIntegrations.map(({ g, provs, direct }) => {
        const e = extState.get(g.name)
        return (
          <Panel
            key={`ext/${g.name}`}
            title={g.name}
            badge={
              g.pool ? (
                <Tooltip title="A virtual extension: it has no endpoint of its own and pools its members' catalogues. Membership is re-derived on every refresh.">
                  <Chip size="small" color="info" variant="outlined" label="pool" />
                </Tooltip>
              ) : g.ext?.cmd ? (
                <Tooltip title="One local process serving several models. They load, unload and are accounted for together.">
                  <Chip size="small" variant="outlined" label={g.ext.server ? `runs on ${g.ext.server}` : 'managed'} />
                </Tooltip>
              ) : undefined
            }
            subtitle={
              g.provs.length > 0
                ? `${g.provs.length} provider${g.provs.length === 1 ? '' : 's'}`
                : `${direct.length} model${direct.length === 1 ? '' : 's'}`
            }
            actions={
              <>
                {e?.cmd && extControls(e)}
                {g.pool?.lanes?.map((l) => (
                  <Tooltip
                    key={l.lane}
                    title={`Every model in this pool joins the ${l.lane} fallback list at position ${l.order}.`}
                  >
                    <Chip size="small" variant="outlined" label={`${l.lane} · fallback list`} />
                  </Tooltip>
                ))}
                <Button size="small" startIcon={<AddIcon />} disabled={readOnly} onClick={() => setAddProviderTo(g.name)}>
                  Provider
                </Button>
                {g.ext && (
                  <Button size="small" disabled={readOnly} onClick={() => void openEntry('extension', g.name).then(setEditing)}>
                    Edit
                  </Button>
                )}
              </>
            }
            flush
          >
            {g.ext?.notes && (
              <Row>
                <OperatorNote text={g.ext.notes} />
              </Row>
            )}
            {direct.map((m) => modelRow(m, { box: true }))}
            {provs.map(({ p, models: ms, stale }) => {
              // A provider with no explicit credentials still has one — the
              // implicit "default" — and the catalogue is fetched per
              // credential, so that is what Browse uses.
              const creds = p.credentials ?? []
              const browseCred = creds[0]?.name ?? 'default'
              return (
                <Box key={`${p.extension}/${p.name}`}>
                  <Box
                    sx={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: 1,
                      flexWrap: 'wrap',
                      px: 2,
                      py: 1,
                      bgcolor: C.canvas,
                      borderTop: `1px solid ${C.border}`,
                    }}
                  >
                    <Typography variant="subtitle2">{p.name}</Typography>
                    <Typography variant="caption" sx={{ color: C.textMuted }}>
                      {p.host}:{p.port}
                      {p.basePath}
                    </Typography>
                    <Chip size="small" variant="outlined" label={`${ms.length} model${ms.length === 1 ? '' : 's'}`} />
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
                      <Chip key={i} size="small" variant="outlined" label={`provider ${l.usd ? `$${l.usd}` : l.req}/${l.per}`} />
                    ))}
                    {creds.length === 0 && (
                      <Tooltip title="No credentials block: the provider's own proxy headers are the one implicit account.">
                        <Chip size="small" variant="outlined" label="implicit account" />
                      </Tooltip>
                    )}
                    {creds.map((cr) => (
                      <Chip
                        key={cr.name}
                        size="small"
                        color={cr.hasSecret || !cr.secretRef ? 'success' : 'warning'}
                        variant="outlined"
                        label={
                          !cr.secretRef
                            ? `${cr.name} · no key`
                            : cr.hasSecret
                              ? `${cr.name} · ${cr.secretRef}`
                              : `${cr.name} · ${cr.secretRef} not stored`
                        }
                      />
                    ))}
                    <Box sx={{ flexGrow: 1 }} />
                    <Tooltip title="Choose models off this provider's catalog.">
                      <Button
                        size="small"
                        startIcon={<SearchIcon />}
                        onClick={() => setBrowse({ provider: p.name, credential: browseCred })}
                      >
                        Browse
                      </Button>
                    </Tooltip>
                    {/* For a provider whose catalogue cannot be browsed, or one
                        model pinned by name regardless of the catalogue. */}
                    <Tooltip title="Declare a model of this provider by hand, instead of choosing one off its catalog.">
                      <Button
                        size="small"
                        startIcon={<AddIcon />}
                        disabled={readOnly}
                        onClick={() => setProviderModel({ provider: p.name, extension: p.extension })}
                      >
                        Model
                      </Button>
                    </Tooltip>
                    <Button size="small" disabled={readOnly} onClick={() => setEditProvider(toInitial(p))}>
                      Edit
                    </Button>
                  </Box>
                  {ms.map((m) => modelRow(m, { box: true }))}
                  {stale.map(staleRow)}
                  {ms.length === 0 && stale.length === 0 && !filtering &&
                    empty(
                      <>
                        No models yet. <strong>Browse</strong> its catalog, or add one by hand with{' '}
                        <strong>+ Model</strong>.
                      </>,
                    )}
                </Box>
              )
            })}
          </Panel>
        )
      })}

      {standalone.length > 0 && (
        <Panel title="Standalone" subtitle="Declared on their own, not under a provider" flush>
          {standalone.map((m) => modelRow(m, { box: true }))}
        </Panel>
      )}

      {/* WHERE A DECISION THAT IS SILENT EVERYWHERE ELSE IS VISIBLE, AND
          REVERSIBLE. A model out of the setup raises no fault and is named on
          no other screen, which is right and is one step from being lost. Below
          the providers because it is not what anybody opened this page for; the
          header chip is what makes it findable on arrival. */}
      {outOfSetup.length > 0 && (
        <Panel
          title="Out of this setup"
          badge={<Chip size="small" color="warning" variant="outlined" label={outOfSetup.length} />}
          subtitle="Kept in the configuration, not served"
          help={
            <>
              Nothing resolves to these, nothing preloads them, and they are in no fallback list. A request
              for one by name gets a 404 with the reason, not a 429 retry. In the config file this is{' '}
              <code>disabled</code>. Put it back restores the model as it was left, including any pause.
            </>
          }
          flush
        >
          {outOfSetup.map((m) => (
            <Row key={m.name}>
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                <Typography variant="subtitle2">{m.name}</Typography>
                {m.type && <Chip size="small" variant="outlined" label={m.type} />}
                {(m.extension || m.provider) && (
                  <Chip size="small" variant="outlined" label={m.extension ? `${m.extension} / ${m.provider}` : m.provider} />
                )}
                <Box sx={{ flexGrow: 1 }} />
                <Button
                  size="small"
                  variant="outlined"
                  color="success"
                  disabled={busy || readOnly}
                  onClick={() => putBack.mutate({ model: m.name, disabled: false })}
                >
                  Put it back
                </Button>
              </Box>
            </Row>
          ))}
        </Panel>
      )}

      <Panel
        title={`Fallback lists (${lanes.length})`}
        subtitle="Models tried in order until one can serve"
        help={
          <>
            Asking for a list by name allows a substitute; asking for a model by name pins the request to
            that model. Outlined chips are written in the list; filled chips are added at runtime (a pool, a
            catalog choice or a selector). A struck-through chip is a model taken out of the setup; it is
            never tried. Click a list to edit it.
          </>
        }
        actions={
          <Button size="small" variant="outlined" disabled={readOnly} onClick={() => setEditing(blankLane())}>
            Add fallback list
          </Button>
        }
        flush
      >
        {lanes.length === 0 && empty('None declared.')}
        {lanes.map((l) => {
          // The LADDER, not the config's member list: a list gains rungs from
          // selectors, catalog choices and a pool, none of which are written in
          // `members`. Fall back to the declared list only for a list the
          // server did not resolve.
          const rungs =
            l.ladder.length > 0
              ? l.ladder
              : l.members.map((m) => ({ model: m.model, origin: 'declared', pool: '', disabled: false }))
          const sticky = new Map(l.members.map((m) => [m.model, m]))
          return (
            <Row
              key={l.name}
              onClick={readOnly ? undefined : () => void openEntry('lane', l.name).then(setEditing)}
            >
              <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                <Typography variant="subtitle2" sx={{ minWidth: 150 }}>
                  {l.name}
                </Typography>
                {rungs.map((r, i) => {
                  const mem = sticky.get(r.model)
                  const notes = [
                    mem?.ttl ? `keep for ${mem.ttl}` : null,
                    mem?.idleUnload ? `unload when idle ${mem.idleUnload}` : null,
                    mem?.evictCost ? `unload priority ${mem.evictCost}` : null,
                    r.origin === 'pool'
                      ? `from the ${r.pool || 'virtual'} pool — membership changes as providers add and withdraw models`
                      : null,
                    r.origin === 'selection' ? 'chosen off a provider catalog' : null,
                    r.origin === 'selector' ? 'matched by a selector, expanded here' : null,
                    r.disabled
                      ? 'written into this list and out of the setup — never tried, and not cover for the rungs above it'
                      : null,
                  ]
                    .filter(Boolean)
                    .join(' · ')
                  return (
                    <Box key={r.model} sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
                      {i > 0 && (
                        <Typography variant="body2" sx={{ color: C.textFaint }}>
                          →
                        </Typography>
                      )}
                      <Tooltip title={notes}>
                        <Chip
                          size="small"
                          variant={r.origin === 'declared' ? 'outlined' : 'filled'}
                          label={r.model}
                          sx={r.disabled ? { textDecoration: 'line-through', opacity: 0.55, borderStyle: 'dashed' } : undefined}
                        />
                      </Tooltip>
                    </Box>
                  )
                })}
                {rungs.length === 0 && (
                  <Typography variant="caption" sx={{ color: C.textFaint }}>
                    nothing resolves — this list serves no one
                  </Typography>
                )}
              </Box>
            </Row>
          )
        })}
      </Panel>

      {/* The credential store sits with the providers that reference it: a
          secretRef is only ever read in "which account does this provider use". */}
      <Panel
        title={`Credentials (${secrets.length})`}
        subtitle="Names only; no value is ever shown"
      >
        <Typography variant="body2" sx={{ opacity: 0.75, mb: 1 }}>
          Reference one from a provider as <code>${'{NAME}'}</code>.
        </Typography>
        <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: 'wrap' }}>
          {secrets.map((sname) => (
            <Chip key={sname} size="small" label={sname} />
          ))}
          {secrets.length === 0 && (
            <Typography variant="caption" sx={{ opacity: 0.7 }}>
              empty
            </Typography>
          )}
        </Stack>
      </Panel>

      {(ov?.servers ?? []).some((sv) => Number(sv.maxConcurrent) > 0) && (
        <Panel title="Host limits" flush>
          {(ov?.servers ?? [])
            .filter((sv) => Number(sv.maxConcurrent) > 0)
            .map((sv) => (
              <Row key={sv.server}>
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 3, flexWrap: 'wrap' }}>
                  <Typography variant="subtitle2" sx={{ minWidth: 150 }}>
                    {sv.server}
                  </Typography>
                  <Stat label="Max concurrent" value={fmtInt(sv.maxConcurrent)} />
                </Box>
              </Row>
            ))}
        </Panel>
      )}

      <PauseDialog
        target={pauseFor?.name ?? null}
        warning={pauseFor?.warning}
        // Counted, not asserted: which fallback lists survive this pause depends
        // on what is in them right now (OB-1).
        consequence={pauseFor ? <PauseCostNote cost={pauseCost(pauseFor.taking, lanes, alreadyOut)} /> : undefined}
        busy={busy}
        onClose={() => setPauseFor(null)}
        onConfirm={(resumeAt, reason) => {
          if (pauseFor) extPause.mutate({ extension: pauseFor.name, resumeAt, reason })
          setPauseFor(null)
        }}
      />
      <ConfirmDialog
        open={!!confirmUnload}
        title={confirmUnload?.title ?? ''}
        body={confirmUnload?.body ?? ''}
        confirmLabel="Unload"
        busy={busy}
        onClose={() => setConfirmUnload(null)}
        onConfirm={() => confirmUnload?.run()}
      />
      <ProviderDialog
        open={addProviderTo !== null}
        defaultExtension={addProviderTo || undefined}
        onClose={() => setAddProviderTo(null)}
        secrets={secrets}
      />
      <ProviderDialog
        open={editProvider != null}
        onClose={() => setEditProvider(null)}
        initial={editProvider ?? undefined}
        secrets={secrets}
      />
      <AddModelDialog
        open={addModel != null}
        onClose={() => setAddModel(null)}
        initialKind={addModel?.localProvider ? 'process' : undefined}
        localProvider={addModel?.localProvider}
      />
      {providerModel && (
        <ProviderModelDialog
          open
          onClose={() => setProviderModel(null)}
          provider={providerModel.provider}
          extension={providerModel.extension}
          editId={providerModel.editId}
        />
      )}
      {browse && (
        <CatalogDialog open onClose={() => setBrowse(null)} provider={browse.provider} credential={browse.credential} />
      )}
      <EntryEditor
        editing={editing}
        onChange={setEditing}
        onClose={() => setEditing(null)}
        invalidate={['providers', 'config', 'overview']}
      />
      <Dialog open={confirmDelete != null} onClose={() => setConfirmDelete(null)}>
        <DialogTitle>Delete {confirmDelete?.served}?</DialogTitle>
        <DialogContent>
          <Typography variant="body2">
            Removes it from the provider&apos;s config. Callers asking for it — by this name or its
            unprefixed one — start getting a 404.
          </Typography>
          {confirmDelete?.hasCmd && (
            <Typography variant="body2" sx={{ mt: 1.5, color: C.warn }}>
              If it is running right now, the process keeps running and keeps its memory: a config reload
              deliberately does not kill backends that are already up. Unload it from its model page first
              if you want the memory back before the next restart.
            </Typography>
          )}
          <Typography variant="caption" sx={{ display: 'block', mt: 1.5, color: C.textFaint }}>
            A model cannot be deleted while a fallback list still names it; the server will say which list.
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
    </Box>
  )
}

export const Route = createFileRoute('/models')({ component: ModelsPage })
