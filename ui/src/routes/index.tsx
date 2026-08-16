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
  TextField,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { ActiveRequests } from '@/ActiveRequests'
import { MemoryPanel } from '@/MemoryPanel'
import { Panel, PageHeader, Row, Stat } from '@/Panel'
import { C, seriesColor } from '@/theme'
import { fmtInt, fmtTime } from '@/format'

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
          pools {
            pool
            totalBytes
            reserveBytes
          }
        }
        models {
          name
          persistent
          ttl
          idleUnload
          evictCost
          spawnable
          remote
          procKey
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
  const { state, persistent, paused, stopping, busy, onLoad, onUnload } = props
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

const ProbePlanDoc = graphql(/* GraphQL */ `
  query ProbePlanOverview {
    corrallm {
      benchPlan {
        models {
          model
          new
          hasTuneProfile
          unverifiedModalities
          disagreements {
            modality
            runMode
          }
        }
      }
    }
  }
`)

const ProbeRunDoc = graphql(/* GraphQL */ `
  mutation ProbeRun($body: corrallm_BenchRunInputBodyInput!) {
    corrallm {
      startBenchRun(body: $body) {
        ok
        message
        warning
      }
    }
  }
`)

const LoadDoc = graphql(/* GraphQL */ `
  mutation LoadModel($model: String!) {
    corrallm {
      loadModel(body: { model: $model }) {
        ok
        message
        backend
      }
    }
  }
`)

const UnloadDoc = graphql(/* GraphQL */ `
  mutation UnloadModel($model: String!) {
    corrallm {
      unloadModel(body: { model: $model }) {
        ok
        message
        evicted
      }
    }
  }
`)

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

const PauseDoc = graphql(/* GraphQL */ `
  mutation PauseModel($model: String!, $resumeAt: String, $reason: String) {
    corrallm {
      pauseModel(body: { model: $model, resumeAt: $resumeAt, reason: $reason }) {
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

const UnpauseDoc = graphql(/* GraphQL */ `
  mutation UnpauseModel($model: String!) {
    corrallm {
      unpauseModel(body: { model: $model }) {
        ok
        message
      }
    }
  }
`)

/**
 * PauseDialog collects the optional resume time and reason for a pause.
 *
 * The input is `datetime-local`, whose value is a LOCAL wall-clock string with
 * no zone ("2026-08-02T09:00"). The server takes RFC3339 and enforces
 * "in the future", so the conversion has to happen here, through a Date — which
 * is exactly right: the operator means 9am where they are, not 9am UTC.
 *
 * `min` is now, not midnight tonight: "after today" in the sense that matters is
 * "not in the past", and forbidding a pause that lifts in two hours because it
 * is still today would be the wrong reading.
 *
 * `warning` carries the blast radius when it is wider than the thing named —
 * pausing one model of an extension pauses all of them, and that must be on the
 * screen BEFORE the click, not in the result message after it.
 */
function PauseDialog(props: {
  target: string | null
  warning?: string
  busy: boolean
  onClose: () => void
  onConfirm: (resumeAt: string | null, reason: string) => void
}) {
  const { target, warning, busy, onClose, onConfirm } = props
  const [until, setUntil] = useState('')
  const [reason, setReason] = useState('')

  // Local-time "now" in the format the input wants, sliced to minutes.
  const nowLocal = () => {
    const d = new Date()
    d.setMinutes(d.getMinutes() - d.getTimezoneOffset())
    return d.toISOString().slice(0, 16)
  }

  const close = () => {
    setUntil('')
    setReason('')
    onClose()
  }

  const parsed = until ? new Date(until) : null
  const invalid = !!parsed && !(parsed.getTime() > Date.now())

  return (
    <Dialog open={!!target} onClose={close} maxWidth="xs" fullWidth>
      <DialogTitle>Pause {target}</DialogTitle>
      <DialogContent>
        {warning && (
          <Alert severity="warning" sx={{ mb: 2 }}>
            {warning}
          </Alert>
        )}
        <Typography variant="body2" sx={{ mb: 2, color: C.textMuted }}>
          Unloads it and keeps it unloaded — no request, lane fall-through or preload will
          start it again until it is resumed. Requests naming an affected model fall through
          to the rest of their lane, or fail with 503.
        </Typography>
        <TextField
          fullWidth
          size="small"
          type="datetime-local"
          label="Resume automatically at"
          value={until}
          onChange={(e) => setUntil(e.target.value)}
          slotProps={{ inputLabel: { shrink: true }, htmlInput: { min: nowLocal() } }}
          error={invalid}
          helperText={invalid ? 'Must be in the future' : 'Leave empty to pause until resumed by hand'}
          sx={{ mb: 2 }}
        />
        <TextField
          fullWidth
          size="small"
          label="Reason (optional)"
          placeholder="e.g. freeing the GPU for the 70B eval"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
      </DialogContent>
      <DialogActions>
        <Button onClick={close}>Cancel</Button>
        <Button
          variant="contained"
          color="warning"
          disabled={busy || invalid}
          onClick={() => {
            onConfirm(parsed ? parsed.toISOString() : null, reason.trim())
            close()
          }}
        >
          Pause
        </Button>
      </DialogActions>
    </Dialog>
  )
}

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

// Task-oriented capability sections — "I want to chat / transcribe / synthesize /
// embed / …". A model lands in the first section whose caps include its
// capability; groupTypes maps the section to the model cost type(s) whose group
// policy is relevant. Sections with no models are hidden; anything unmatched falls
// into "Other" so nothing disappears.
const CAP_SECTIONS: { title: string; blurb: string; caps: string[]; groupTypes: string[] }[] = [
  { title: 'Chat', blurb: 'Conversational + instruct models', caps: ['chat'], groupTypes: ['chat'] },
  { title: 'Image understanding', blurb: 'Vision / multimodal', caps: ['vision', 'image'], groupTypes: ['chat'] },
  { title: 'Embeddings', blurb: 'Vector embeddings', caps: ['embeddings'], groupTypes: ['embed'] },
  {
    title: 'Speech-to-text',
    blurb: 'Transcription — batch (upload) + realtime (ws / webrtc)',
    caps: ['audio.stt', 'audio.realtime'],
    groupTypes: ['stt', 'realtime'],
  },
  { title: 'Text-to-speech', blurb: 'Speech synthesis', caps: ['audio.tts'], groupTypes: ['tts'] },
  { title: 'Rerank', blurb: 'Document reranking', caps: ['rerank'], groupTypes: ['rerank'] },
]

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

function Home() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)
  const [cmdView, setCmdView] = useState<{ title: string; cmd: string } | null>(null)

  const q = useQuery({
    queryKey: ['overview'],
    queryFn: () => gqlClient.request(OverviewDoc),
    refetchInterval: 15000, // fallback; live via SSE (useLiveEvents)
  })

  const [probeFor, setProbeFor] = useState<string | null>(null)

  // What is missing per model — drives whether a Probe button appears at all.
  const probePlan = useQuery({
    queryKey: ['probePlanOverview'],
    queryFn: () => gqlClient.request(ProbePlanDoc),
    refetchInterval: 30000,
  })
  const planByModel = new Map(
    (probePlan.data?.corrallm?.benchPlan?.models ?? []).map((m) => [m.model, m]),
  )
  // Offer a probe only when there is something to learn: no VRAM profile, a
  // declared modality nothing has exercised, or a cold/warm disagreement. A
  // fully-covered model gets no button — an always-present Probe would train
  // people to ignore it.

  const probe = useMutation({
    mutationFn: (model: string) =>
      gqlClient.request(ProbeRunDoc, {
        body: { models: [model], classes: ['capability'], reason: `probe ${model}` },
      }),
    onSuccess: (d) => {
      const r = d.corrallm?.startBenchRun
      setMsg({ ok: !!r?.ok, text: r?.message ?? '' })
      void qc.invalidateQueries({ queryKey: ['probePlanOverview'] })
    },
    onError: (e) => setMsg({ ok: false, text: String(e) }),
  })

  const load = useMutation({
    mutationFn: (model: string) => gqlClient.request(LoadDoc, { model }),
    onSuccess: (d) => {
      const r = d.corrallm.loadModel
      setMsg({ ok: !!r?.ok, text: r?.message ?? '' })
      void qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (e) => setMsg({ ok: false, text: String(e) }),
  })
  const unload = useMutation({
    mutationFn: (model: string) => gqlClient.request(UnloadDoc, { model }),
    onSuccess: (d) => {
      const r = d.corrallm.unloadModel
      setMsg({ ok: !!r?.ok, text: r?.message ?? '' })
      void qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (e) => setMsg({ ok: false, text: String(e) }),
  })
  // What the pause dialog is collecting a resume time for. `kind` decides which
  // mutation fires; `warning` is the blast radius shown before the click.
  const [pauseFor, setPauseFor] = useState<{ kind: 'model' | 'extension'; name: string; warning?: string } | null>(
    null,
  )
  // A pending unload whose blast radius is wider than its row (a hosted model).
  const [confirmUnload, setConfirmUnload] = useState<{ title: string; body: string; run: () => void } | null>(null)

  const done = (text?: string | null, ok?: boolean | null) => {
    setMsg({ ok: !!ok, text: text ?? '' })
    void qc.invalidateQueries({ queryKey: ['overview'] })
  }
  const fail = (e: unknown) => setMsg({ ok: false, text: String(e) })

  const pause = useMutation({
    mutationFn: (v: { model: string; resumeAt: string | null; reason: string }) =>
      gqlClient.request(PauseDoc, { model: v.model, resumeAt: v.resumeAt, reason: v.reason }),
    onSuccess: (d) => done(d.corrallm.pauseModel?.message, d.corrallm.pauseModel?.ok),
    onError: fail,
  })
  const unpause = useMutation({
    mutationFn: (model: string) => gqlClient.request(UnpauseDoc, { model }),
    onSuccess: (d) => done(d.corrallm.unpauseModel?.message, d.corrallm.unpauseModel?.ok),
    onError: fail,
  })
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
  const busy =
    load.isPending ||
    unload.isPending ||
    pause.isPending ||
    unpause.isPending ||
    extLoad.isPending ||
    extUnload.isPending ||
    extPause.isPending ||
    extUnpause.isPending

  if (q.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Typography color="error">{String(q.error)}</Typography>
      </Box>
    )
  }

  // No non-null assertion. The guards above cover isLoading and error, but a
  // query can settle with neither — notably the moment a 401 handler calls
  // clearToken() and reloads, which re-renders once with the cache already
  // dropped. `q.data!` then threw "Cannot read properties of undefined
  // (reading 'corrallm')" straight into the error boundary, replacing the whole
  // dashboard with "Something went wrong!" instead of the login screen it was
  // one tick away from showing.
  const c = q.data?.corrallm
  if (!c) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }
  const ov = c.overview
  const models = ov?.models ?? []
  const lanes = ov?.lanes ?? []
  const groups = ov?.groups ?? []
  const keys = ov?.keys ?? []
  // Residency keyed by the BACKING PROCESS, and remote backends dropped before
  // the map is built. Two bugs fixed at once:
  //   - an extension's models share one process, so keying by modelName made
  //     whichever sibling spawned it read "ready" and the other three "absent"
  //     off that same live process;
  //   - a remote model (groq, cerebras, anthropic) has no process at all, yet
  //     latched to "ready" on its first request and sat in "Loaded" forever.
  // Nothing downstream can now mistake a proxied model for a resident one.
  const stateByProc = new Map(
    (c.residency?.models ?? []).filter((m) => !m.remote).map((m) => [m.procKey, m]),
  )
  const stateOf = (m: { procKey: string; remote: boolean }) =>
    m.remote ? undefined : stateByProc.get(m.procKey)

  // Keyed by procKey like stateByProc, so one lookup covers a model and the
  // extension that hosts it — they are the same process.
  const stoppingProcs = new Set(c.residency?.stopping ?? [])

  // A group's effective policy for a capability = its onSaturated stage for that
  // model type, falling back to its `default` stage. Distinct values joined so
  // e.g. "queue/reject" when batch queues but realtime has no stage.
  const policyForTypes = (g: (typeof groups)[number], types: string[]) => {
    const def = g.stages.find((s) => s.type === 'default')?.policy ?? 'reject'
    const pols = types.map((t) => g.stages.find((s) => s.type === t)?.policy ?? def)
    return Array.from(new Set(pols)).join('/')
  }

  // The group strip rides in the panel HEADER, not above it — it qualifies the
  // panel's contents (which lanes may use these models, under what policy), so
  // it belongs inside the panel's boundary.
  const groupStrip = (types: string[]) =>
    groups.length ? (
      <>
        {groups.map((g) => {
          const ks = keys.filter((k) => k.group === g.name).map((k) => k.key)
          const detail = [
            `weight ${g.weight}`,
            g.shareCurrency,
            g.interruptible ? 'interruptible' : null,
            g.acceptDegrade ? `degrade ≥ ${g.qualityFloor}` : 'top-quality only',
            ks.length ? `keys: ${ks.join(', ')}` : 'no keys',
          ]
            .filter(Boolean)
            .join(' · ')
          return (
            <Tooltip key={g.name} title={detail}>
              <Chip
                size="small"
                variant="outlined"
                label={`${g.name} · w${g.weight} · ${policyForTypes(g, types)}`}
              />
            </Tooltip>
          )
        })}
      </>
    ) : null

  const extensions = ov?.extensions ?? []

  const modelRow = (m: (typeof models)[number]) => {
    const st = stateOf(m)
    const places = m.placements ?? []
    return (
      // The whole row navigates, and carries no controls.
      //
      // Every control that used to live here — load, unload, pause, probe, the
      // backend's UI, its logs — acts on ONE process on ONE box. A model can be
      // served from more than one, so a per-model button had to pick a box
      // silently and hope it was the one you meant. They live on the model page
      // now, one set per placement.
      <Row key={m.name} onClick={() => navigate({ to: '/m/$name', params: { name: m.name } })}>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
          <Chip
            size="small"
            label={m.remote ? 'proxy' : stoppingProcs.has(m.procKey) ? 'stopping' : (st?.state ?? 'absent')}
            color={stateColor(m.remote ? 'proxy' : (st?.state ?? 'absent'))}
          />
          <Typography variant="subtitle2">{m.name}</Typography>
          {places.length > 1 && (
            <Tooltip title={`Served from ${places.length} boxes — open the model to act on one`}>
              <Chip size="small" variant="outlined" label={`${places.length} placements`} />
            </Tooltip>
          )}
          {places.length === 1 && places[0]?.server && (
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
        </Box>
      </Row>
    )
  }

  // Resident models lead the page: what is occupying VRAM right now is the first
  // thing anyone opens this dashboard to check. They are pulled OUT of their
  // capability sections below so no card renders twice — each card already
  // carries its capability chip, so nothing is lost by the move.
  const LOAD_RANK: Record<string, number> = { ready: 0, loading: 1, evicting: 2 }
  // Remote models are excluded by stateOf, so "Loaded" now means what it says:
  // something is resident on this box holding memory.
  const loadRank = (m: (typeof models)[number]) => LOAD_RANK[stateOf(m)?.state ?? '']
  const loaded = models
    .filter((m) => loadRank(m) !== undefined)
    .sort((a, b) => loadRank(a)! - loadRank(b)! || a.name.localeCompare(b.name))
  const unloaded = models.filter((m) => loadRank(m) === undefined)

  // Assign each remaining model to its capability section; leftovers → "Other".
  const seen = new Set<string>()
  const sections = CAP_SECTIONS.map((s) => {
    const ms = unloaded.filter((m) => s.caps.includes(m.capability))
    ms.forEach((m) => seen.add(m.name))
    return { ...s, models: ms }
  }).filter((s) => s.models.length)
  const other = unloaded.filter((m) => !seen.has(m.name))
  if (other.length) sections.push({ title: 'Other', blurb: '', caps: [], groupTypes: [], models: other })

  // Memory attribution colors follow the MODEL, assigned over the full sorted
  // model list — never over the subset in one bar. Color must not change when a
  // model loads or unloads, or every bar repaints and the eye reads a change
  // that did not happen.
  const colorIndex = new Map(models.map((m) => m.name).sort().map((n, i) => [n, i]))
  const colorOf = (name: string) => seriesColor(colorIndex.get(name) ?? 0)
  const res = c.residency
  // Declared reserve lives on the config view, live budget/used on residency —
  // join them so one bar can say both what is spoken for and what is being held
  // back, instead of a second near-duplicate "capacity" panel saying half of it.
  const reserveByPool = new Map(
    (ov?.servers ?? []).flatMap((s) => s.pools.map((p) => [`${s.server}/${p.pool}`, Number(p.reserveBytes)])),
  )
  const memPools = (res?.servers ?? []).flatMap((s) =>
    s.pools.map((p) => ({
      server: s.server,
      pool: p.pool,
      budget: Number(p.budget),
      used: Number(p.used),
      reserve: reserveByPool.get(`${s.server}/${p.pool}`) ?? 0,
    })),
  )
  const memModels = (res?.models ?? []).map((m) => ({
    model: m.modelName,
    server: m.server,
    pools: m.usage.map((u) => ({ pool: u.pool, bytes: Number(u.bytes) })),
    measuredBytes: Number(m.footprintMiB) * 1024 * 1024,
  }))
  const memServers = (ov?.servers ?? []).map((s) => ({
    server: s.server,
    devicePool: s.devicePool,
  }))
  const dev = (d?: { available: boolean; name: string; totalBytes: string; usedBytes: string; freeBytes: string }) => ({
    available: !!d?.available,
    name: d?.name ?? '',
    totalBytes: Number(d?.totalBytes ?? 0),
    usedBytes: Number(d?.usedBytes ?? 0),
    freeBytes: Number(d?.freeBytes ?? 0),
  })

  // Device readings carry the pool they back, which is what pairs each card
  // with the right ledger row. A card no pool claims keeps pool='' and is shown
  // as unclaimed rather than dropped — a freshly installed GPU nothing budgets
  // is precisely the state worth seeing.
  const devs = (
    list?: readonly {
      available: boolean
      name: string
      uuid?: string | null
      pool?: string | null
      totalBytes: string
      usedBytes: string
      freeBytes: string
    }[],
  ) => (list ?? []).map((d) => ({ ...dev(d), uuid: d.uuid ?? '', pool: d.pool ?? '' }))

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Overview">
        <Chip size="small" color="success" label={`${c.health?.status} · ${c.health?.version}`} />
      </PageHeader>

      {msg && (
        <Alert severity={msg.ok ? 'success' : 'error'} onClose={() => setMsg(null)}>
          {msg.text}
        </Alert>
      )}

      {/* What the box is doing right now, above what it merely could do. */}
      <ActiveRequests />

      {/* …and what it is HOLDING right now, with attribution. */}
      <MemoryPanel
        pools={memPools}
        models={memModels}
        servers={memServers}
        gpus={devs(res?.gpus)}
        host={dev(res?.host)}
        colorOf={colorOf}
      />

      <PauseDialog
        target={pauseFor?.name ?? null}
        warning={pauseFor?.warning}
        busy={busy}
        onClose={() => setPauseFor(null)}
        onConfirm={(resumeAt, reason) => {
          if (pauseFor?.kind === 'extension') {
            extPause.mutate({ extension: pauseFor.name, resumeAt, reason })
          } else if (pauseFor) {
            pause.mutate({ model: pauseFor.name, resumeAt, reason })
          }
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

      {/* A probe is no longer destructive — it evicts nothing and locks nobody
          out — but it does consume GPU time on a box other people are using.
          Name exactly what the run will learn before the click, and be honest
          about the cost that remains. */}
      {probeFor && (
        <Dialog open onClose={() => setProbeFor(null)} maxWidth="sm" fullWidth>
          <DialogTitle>Probe {probeFor}?</DialogTitle>
          <DialogContent dividers>
            <Typography variant="body2" gutterBottom>
              This runs llm-bench against <b>{probeFor}</b> to learn what corrallm cannot
              observe on its own:
            </Typography>
            <ul>
              {!planByModel.get(probeFor)?.hasTuneProfile && (
                <li>
                  <b>VRAM footprint</b> — today corrallm schedules this model on its
                  declared <code>ramUsage</code>, which nothing has verified.
                </li>
              )}
              {!!planByModel.get(probeFor)?.unverifiedModalities?.length && (
                <li>
                  <b>Declared modalities</b> (
                  {planByModel.get(probeFor)?.unverifiedModalities?.join(', ')}) — advertised
                  but never exercised against the live backend.
                </li>
              )}
              {!!planByModel.get(probeFor)?.disagreements?.length && (
                <li>
                  <b>Cold/warm disagreement</b> — recorded when cold probing still existed:
                  this modality worked in one residency state and failed in the other. A
                  re-run can no longer confirm it, since arranging a cold model meant
                  evicting one.
                </li>
              )}
            </ul>
            <Alert severity="info" sx={{ mt: 1 }}>
              The run shares the box: nothing is evicted and no caller is turned away. It
              queues for slots like any other client and waits out <b>429 + Retry-After</b>
              backpressure, subtracting that wait from its timings. It will still use GPU
              time, so expect added latency while it runs.
            </Alert>
          </DialogContent>
          <DialogActions>
            <Button onClick={() => setProbeFor(null)}>Cancel</Button>
            <Button
              variant="contained"
              onClick={() => {
                probe.mutate(probeFor)
                setProbeFor(null)
              }}
            >
              Evict and probe
            </Button>
          </DialogActions>
        </Dialog>
      )}

      {cmdView && (
        <Dialog open onClose={() => setCmdView(null)} maxWidth="lg" fullWidth>
          <DialogTitle>Command · {cmdView.title}</DialogTitle>
          <DialogContent dividers>
            <Box
              component="pre"
              sx={{
                m: 0,
                p: 1,
                fontSize: 13,
                lineHeight: 1.5,
                maxHeight: '65vh',
                overflow: 'auto',
                whiteSpace: 'pre-wrap',
                wordBreak: 'break-all',
                bgcolor: C.canvas,
                color: C.text,
                border: `1px solid ${C.border}`,
                borderRadius: 1,
                fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
              }}
            >
              {cmdView.cmd}
            </Box>
          </DialogContent>
        </Dialog>
      )}

      {/* Resident models first — see the split above. */}
      {loaded.length > 0 && (
        <Panel
          title="Loaded"
          badge={<Chip size="small" color="success" label={loaded.length} />}
          subtitle="Resident or coming up — holding capacity right now"
          flush
        >
          {loaded.map(modelRow)}
        </Panel>
      )}

      {/* Capability sections: groups (filtered to this capability) over its models. */}
      {sections.map((s) => (
        <Panel key={s.title} title={s.title} subtitle={s.blurb} actions={groupStrip(s.groupTypes)} flush>
          {s.models.map(modelRow)}
        </Panel>
      ))}

      {/* Extensions: the PROCESS behind a group of models. It had no surface at
          all before — you could see oidio-stt and oidio-tts but not the single
          oidio process serving both, which is the thing that actually loads,
          unloads and pauses. Controls live here because this is the unit they
          really act on. */}
      {extensions.length > 0 && (
        <Panel
          title="Extensions"
          subtitle="One process serving several models — it loads, unloads and pauses as a unit"
          flush
        >
          {extensions.map((e) => {
            const spawnable = !!e.cmd
            const stopping = stoppingProcs.has(`extension:${e.name}`)
            return (
              <Row key={e.name}>
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                  <Chip
                    size="small"
                    label={!spawnable ? 'proxy' : stopping ? 'stopping' : e.state}
                    color={!spawnable ? 'secondary' : stopping ? 'warning' : stateColor(e.state)}
                    variant={e.state !== 'absent' || !spawnable || stopping ? 'filled' : 'outlined'}
                    sx={{ minWidth: 68 }}
                  />
                  <Typography variant="subtitle2" sx={{ minWidth: 150 }}>
                    {e.name}
                  </Typography>
                  {e.paused && (
                    <Tooltip title={e.pauseReason || 'No reason given'}>
                      <Chip
                        size="small"
                        color="warning"
                        label={
                          'paused' + (Number(e.pauseResumeMs) > 0 ? ` until ${fmtTime(e.pauseResumeMs)}` : '')
                        }
                      />
                    </Tooltip>
                  )}
                  {e.pinned && <Chip size="small" variant="outlined" label="pinned" />}
                  {Number(e.inFlight) > 0 && (
                    <Chip size="small" variant="outlined" label={`${fmtInt(e.inFlight)} in flight`} />
                  )}
                  {e.draining && <Chip size="small" color="warning" variant="outlined" label="draining" />}
                  <Tooltip title={`Serves: ${e.provides.join(', ')}`}>
                    <Chip size="small" variant="outlined" label={`${e.provides.length} models`} />
                  </Tooltip>
                  <Box sx={{ flexGrow: 1 }} />
                  {spawnable && (
                    <>
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
                        <Button
                          size="small"
                          variant="outlined"
                          color="success"
                          disabled={busy}
                          onClick={() => extUnpause.mutate(e.name)}
                        >
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
                              kind: 'extension',
                              name: e.name,
                              warning: `Takes every model ${e.name} serves out of service: ${e.provides.join(', ')}.`,
                            })
                          }
                        >
                          Pause
                        </Button>
                      )}
                    </>
                  )}
                </Box>
              </Row>
            )
          })}
        </Panel>
      )}

      {/* Lanes: named ordered fallback lists over models. */}
      {lanes.length > 0 && (
        <Panel
          title="Lanes"
          subtitle="Requestable as a model id; tried left to right. Filled chips are contributed at runtime — a pool, a directory choice, or a selector — and are not in your config's member list."
          flush
        >
          {lanes.map((l) => {
            // The LADDER, not the config's member list. A lane gains rungs from
            // selectors, from models chosen off a directory, and from a virtual
            // extension's pool — none of which are written in `members`, so
            // rendering `members` showed `free` as two entries while it actually
            // resolved to twelve. Fall back to the declared list only for a lane
            // the server did not resolve.
            const rungs =
              l.ladder.length > 0
                ? l.ladder
                : l.members.map((m) => ({ model: m.model, origin: 'declared', pool: '' }))
            const sticky = new Map(l.members.map((m) => [m.model, m]))
            return (
              <Row key={l.name}>
                <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                  <Typography variant="subtitle2" sx={{ minWidth: 150 }}>
                    {l.name}
                  </Typography>
                  {rungs.map((r, i) => {
                    const mem = sticky.get(r.model)
                    const notes = [
                      mem?.ttl ? `ttl ${mem.ttl}` : null,
                      mem?.idleUnload ? `idle-unload ${mem.idleUnload}` : null,
                      mem?.evictCost ? `evict ${mem.evictCost}` : null,
                      r.origin === 'pool'
                        ? `from the ${r.pool || 'virtual'} pool — membership changes as providers add and withdraw models`
                        : null,
                      r.origin === 'selection' ? 'chosen off a provider directory' : null,
                      r.origin === 'selector' ? 'matched by a selector, expanded here' : null,
                    ]
                      .filter(Boolean)
                      .join(' · ')
                    return (
                      <Box
                        key={r.model}
                        sx={{ display: 'flex', alignItems: 'center', gap: 1 }}
                      >
                        {i > 0 && (
                          <Typography variant="body2" sx={{ color: C.textFaint }}>
                            →
                          </Typography>
                        )}
                        <Tooltip title={notes}>
                          {/* Declared rungs are outlined; everything the runtime
                              contributed is filled, so "what did I write down"
                              stays readable at a glance against "what showed up". */}
                          <Chip
                            size="small"
                            variant={r.origin === 'declared' ? 'outlined' : 'filled'}
                            label={r.model}
                          />
                        </Tooltip>
                      </Box>
                    )
                  })}
                  {rungs.length === 0 && (
                    <Typography variant="caption" sx={{ color: C.textFaint }}>
                      nothing resolves — this lane serves no one
                    </Typography>
                  )}
                </Box>
              </Row>
            )
          })}
        </Panel>
      )}

      {/* Host concurrency caps — the one declared fact the Memory panel above
          does not already show live. Pools moved there; a second "capacity"
          panel restating budget/reserve would just be the live one, staler. */}
      {(ov?.servers ?? []).some((s) => Number(s.maxConcurrent) > 0) && (
        <Panel title="Host limits" flush>
          {(ov?.servers ?? [])
            .filter((s) => Number(s.maxConcurrent) > 0)
            .map((s) => (
              <Row key={s.server}>
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 3, flexWrap: 'wrap' }}>
                  <Typography variant="subtitle2" sx={{ minWidth: 150 }}>
                    {s.server}
                  </Typography>
                  <Stat label="Max concurrent" value={fmtInt(s.maxConcurrent)} />
                </Box>
              </Row>
            ))}
        </Panel>
      )}
    </Box>
  )
}

export const Route = createFileRoute('/')({ component: Home })
