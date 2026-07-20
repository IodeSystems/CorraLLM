import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Alert,
  AlertTitle,
  Box,
  Button,
  Checkbox,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  Link as MuiLink,
  Paper,
  Stack,
  Tab,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Tabs,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material'
import ExpandMoreIcon from '@mui/icons-material/ExpandMore'
import { graphql } from '@/gql'
import type { ModelBenchQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { capLabel, fmtDuration, fmtInt, fmtMiB, fmtUSD } from '@/format'
import { BenchProbeDetail } from '@/BenchProbeDetail'

// --- data ---------------------------------------------------------------

const ConsoleDoc = graphql(/* GraphQL */ `
  query ModelConsole {
    corrallm {
      overview {
        models {
          name
          modalities {
            modality
            maxResolution
            formats
            maxTokens
          }
          capability
          persistent
          ttl
          type
          quality
          server
          target
          cmd
          maxConcurrent
        }
      }
      residency {
        models {
          name
          modelName
          state
          hasUi
          nCtx
          nSlots
          footprintMiB
          baseMiB
          perSlotMiB
          peakMiB
          measuredSlots
          tunedSlots
          configSlots
        }
      }
    }
  }
`)

const ReplayDoc = graphql(/* GraphQL */ `
  query ConsoleReplay($id: Long!) {
    corrallm {
      activityDetail(id: $id) {
        record {
          served
          path
          reqBody
        }
      }
    }
  }
`)

const LogsDoc = graphql(/* GraphQL */ `
  query ConsoleLogs($backend: String!) {
    corrallm {
      modelLogs(backend: $backend) {
        lines
      }
    }
  }
`)

const UsageDoc = graphql(/* GraphQL */ `
  query ConsoleUsage {
    corrallm {
      usageRollup(windowHours: "24") {
        rows {
          served
          requests
          promptTokens
          completionTokens
          dwellMs
          costUsd
        }
      }
    }
  }
`)

type Manifest = {
  models_by_capability?: Record<string, string[]>
  endpoints?: Array<{
    path: string
    method: string
    capability: string
    description: string
    models?: string[]
    example?: { curl?: string; body?: unknown; note?: string; ws_url?: string; protocol?: string; flow?: string[] }
  }>
}

function useCapabilities() {
  return useQuery({
    queryKey: ['capabilities'],
    queryFn: async () => (await fetch('/v1/capabilities')).json() as Promise<Manifest>,
    staleTime: 60_000,
  })
}

function capabilityOf(man: Manifest | undefined, name: string): string {
  const m = man?.models_by_capability ?? {}
  for (const [cap, list] of Object.entries(m)) if (list.includes(name)) return cap
  return 'chat'
}

// modalityLabel renders a modality + its salient metadata as a compact chip
// label: "image ≤1024px jpeg,png", "text ≤4k tok", or just the name.
function modalityLabel(md: Modality): string {
  const parts = [md.modality]
  if (md.maxResolution) parts.push(`≤${md.maxResolution}px`)
  if (md.maxTokens) parts.push(`≤${fmtInt(md.maxTokens)} tok`)
  if (md.formats?.length) parts.push(md.formats.join(','))
  return parts.join(' ')
}

// --- console ------------------------------------------------------------

function ModelConsole() {
  const { name, replay, tab: tabParam } = Route.useSearch()
  const navigate = useNavigate()
  // Deep-linkable tabs: the Overview's Logs button now navigates HERE rather
  // than opening its own dialog, so the console is the single place a model's
  // detail lives instead of two half-views of it.
  const TAB_NAMES = ['info', 'test', 'logs', 'usage', 'bench'] as const
  const initialTab = tabParam
    ? Math.max(0, TAB_NAMES.indexOf(tabParam as (typeof TAB_NAMES)[number]))
    : replay
      ? 1
      : 0
  const [tab, setTab] = useState(initialTab)
  const ov = useQuery({ queryKey: ['console'], queryFn: () => gqlClient.request(ConsoleDoc), refetchInterval: 15000 })
  const caps = useCapabilities()

  const model = (ov.data?.corrallm.overview?.models ?? []).find((m) => m.name === name)
  const res = (ov.data?.corrallm.residency?.models ?? []).find((m) => m.modelName === name)
  // Capability comes from the model data itself (reliable) — NOT the async
  // /v1/capabilities fetch, which would briefly mis-dispatch (e.g. show a chat box
  // for an STT model) until it loaded.
  const capability = model?.capability ?? capabilityOf(caps.data, name)

  if (ov.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }
  if (!model) {
    return (
      <Box sx={{ p: 3 }}>
        <Typography color="error">Unknown model: {name}</Typography>
        <Button onClick={() => navigate({ to: "/" })} sx={{ mt: 1 }}>
          ← Overview
        </Button>
      </Box>
    )
  }

  return (
    <Box sx={{ p: 3 }}>
      <Stack direction="row" spacing={1} alignItems="center" sx={{ mb: 1, flexWrap: 'wrap' }}>
        <Button onClick={() => navigate({ to: "/" })} size="small">
          ← Overview
        </Button>
        <Typography variant="h6">{name}</Typography>
        <Chip size="small" label={res?.state ?? 'absent'} />
        <Chip size="small" color="info" variant="outlined" label={capLabel(capability)} />
        {(model.modalities ?? []).map((md) => (
          <Chip key={md.modality} size="small" variant="outlined" label={modalityLabel(md)} />
        ))}
        {model.persistent && <Chip size="small" variant="outlined" label="pinned" />}
      </Stack>

      <Tabs value={tab} onChange={(_, v) => setTab(v)} sx={{ mb: 2 }}>
        <Tab label="Info" />
        <Tab label="Test" />
        <Tab label="Logs" />
        <Tab label="Usage" />
        <Tab label="Bench" />
      </Tabs>

      {tab === 0 && <InfoTab model={model} res={res} caps={caps.data} name={name} />}
      {tab === 1 && (
        <TestTab
          capability={capability}
          model={name}
          ttsModels={caps.data?.models_by_capability?.['audio.tts'] ?? []}
          replayId={replay}
        />
      )}
      {tab === 2 && <LogsTab backend={res?.name ?? `${name}#0`} ready={!!res} />}
      {tab === 3 && <UsageTab name={name} />}
      {tab === 4 && <BenchTab name={name} />}
    </Box>
  )
}

// --- Info ---------------------------------------------------------------

type Modality = {
  modality: string
  maxResolution?: string | null
  formats?: string[] | null
  maxTokens?: string | null
}
type OvModel = {
  name: string
  modalities: Modality[]
  capability: string
  persistent: boolean
  type: string
  quality: string
  server: string
  target: string
  cmd: string
  maxConcurrent: string
}
type ResModel = {
  name: string
  modelName: string
  state: string
  hasUi: string
  nCtx: string
  nSlots: string
  footprintMiB: string
  baseMiB: string
  perSlotMiB: string
  peakMiB: string
  measuredSlots: string
  tunedSlots: string
  configSlots: string
}

function InfoTab({
  model,
  res,
  caps,
  name,
}: {
  model: OvModel
  res: ResModel | undefined
  caps: Manifest | undefined
  name: string
}) {
  // A measurement outlives residency: read it from benchPlan so the Memory
  // table below still has numbers when the model is evicted.
  const profQ = useQuery({
    queryKey: ['modelProfile'],
    queryFn: () => gqlClient.request(ModelProfileDoc),
    staleTime: 30000,
  })
  const prof = (profQ.data?.corrallm?.benchPlan?.models ?? []).find((m) => m.model === name)?.profile
  const examples = (caps?.endpoints ?? []).filter((e) => (e.models ?? []).includes(name))
  const noUI = res?.hasUi === 'no'
  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={1} sx={{ flexWrap: 'wrap' }}>
        {res && Number(res.nCtx) > 0 && <Chip size="small" variant="outlined" label={`ctx ${fmtInt(res.nCtx)}`} />}
        {res && Number(res.nSlots) > 0 && <Chip size="small" variant="outlined" label={`slots ${fmtInt(res.nSlots)}`} />}
        {noUI ? (
          <Chip size="small" variant="outlined" label="no web UI" />
        ) : (
          <Button size="small" component={MuiLink} href={`/upstream/${encodeURIComponent(name)}/`} target="_blank" rel="noreferrer">
            Open native UI
          </Button>
        )}
      </Stack>

      <Box>
        <Typography variant="subtitle2">Serving</Typography>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>type</TableCell>
              <TableCell>quality</TableCell>
              <TableCell>server</TableCell>
              <TableCell>target</TableCell>
              <TableCell>slots</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            <TableRow>
              <TableCell>{model.type}</TableCell>
              <TableCell>{model.quality}</TableCell>
              <TableCell>{model.server || '—'}</TableCell>
              <TableCell>{model.target || '—'}</TableCell>
              <TableCell>{model.maxConcurrent}</TableCell>
            </TableRow>
          </TableBody>
        </Table>
        {model.cmd && (
          <Box component="pre" sx={preSx}>
            {model.cmd}
          </Box>
        )}
      </Box>

      <Box>
        <Typography variant="subtitle2">Memory</Typography>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>live footprint</TableCell>
              <TableCell>base</TableCell>
              <TableCell>per-slot KV</TableCell>
              <TableCell>peak</TableCell>
              <TableCell>slots (tuned / config)</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            <TableRow>
              {/* Live footprint needs residency; everything else comes from the
                  persisted MEASUREMENT, so an evicted model still shows what it
                  was measured at instead of a row of dashes. */}
              <TableCell>{res ? fmtMiB(res.footprintMiB) : 'not resident'}</TableCell>
              <TableCell>{prof ? fmtMiB(prof.baseMiB) : res ? fmtMiB(res.baseMiB) : '—'}</TableCell>
              <TableCell>{prof ? fmtMiB(prof.perSlotMiB) : res ? fmtMiB(res.perSlotMiB) : '—'}</TableCell>
              <TableCell>{prof ? fmtMiB(prof.peakMiB) : res ? fmtMiB(res.peakMiB) : '—'}</TableCell>
              <TableCell>
                {res ? `${fmtInt(res.tunedSlots)} / ${fmtInt(res.configSlots)}` : prof ? `measured at ${prof.measuredSlots}` : '—'}
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </Box>

      <Box>
        <Typography variant="subtitle2">Example requests</Typography>
        {examples.length === 0 && <Typography color="text.secondary">No endpoint examples for this model.</Typography>}
        {examples.map((e) => (
          <Box key={e.path} sx={{ mb: 1 }}>
            <Typography variant="body2">
              <code>
                {e.method} {e.path}
              </code>{' '}
              — {e.description}
            </Typography>
            {e.example?.curl && <Box component="pre" sx={preSx}>{e.example.curl}</Box>}
            {e.example?.flow && (
              <Box component="pre" sx={preSx}>{['ws ' + (e.example.ws_url ?? ''), ...e.example.flow].join('\n')}</Box>
            )}
          </Box>
        ))}
      </Box>
    </Stack>
  )
}

const preSx = {
  m: 0,
  mt: 0.5,
  p: 1,
  bgcolor: 'action.hover',
  borderRadius: 1,
  fontSize: '0.75rem',
  whiteSpace: 'pre-wrap',
  wordBreak: 'break-all',
  maxHeight: 200,
  overflow: 'auto',
} as const

// Standard OpenAI verbose_json segment, plus the additive `speaker` field a
// diarizing backend sets (oidio: a per-request stable speaker UUID).
type DiarSegment = { speaker?: string; start: number; end: number; text: string }
// An entry of the additive `speakers[]` array — stateless identity: a UUID, the
// voiceprint embedding, and cosine similarity to the other speakers.
type Speaker = { uuid: string; similarity?: Record<string, number> }

// Stable per-speaker colors keyed on the UUID (cycles for >8 speakers).
const SPEAKER_COLORS = ['#1565c0', '#c62828', '#2e7d32', '#6a1b9a', '#ef6c00', '#00838f', '#ad1457', '#4e342e']
const hashStr = (s: string) => {
  let h = 0
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0
  return Math.abs(h)
}
const speakerColor = (s: string) => SPEAKER_COLORS[hashStr(s) % SPEAKER_COLORS.length]
const shortId = (s: string) => s.slice(0, 4)
const fmtTime = (s: number) => `${Math.floor(s / 60)}:${String(Math.floor(s % 60)).padStart(2, '0')}`

// --- Logs ---------------------------------------------------------------

/**
 * The console is now the ONLY place logs are read — the Overview's dialog was
 * removed rather than left as a second half-view. This absorbs what that dialog
 * did better, so the consolidation is not a downgrade:
 *
 *   - 2s refresh (was 5s here): logs are watched while something is loading or
 *     failing, and 5s is a long time to stare at a stale tail.
 *   - a tall viewport (was a fixed 200-480px): a spawn failure's useful line is
 *     usually well above the last one.
 *   - terminal styling, so a wall of llama.cpp output reads as output.
 */
function LogsTab({ backend, ready }: { backend: string; ready: boolean }) {
  const q = useQuery({
    queryKey: ['consoleLogs', backend],
    queryFn: () => gqlClient.request(LogsDoc, { backend }),
    refetchInterval: 2000,
    // Fetch even when not ready: a backend that FAILED to load is exactly when
    // its logs matter most, and gating on `ready` hid the crash output.
    enabled: !!backend,
  })
  const lines = q.data?.corrallm.modelLogs?.lines ?? []
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      {!ready && (
        <Typography variant="caption" color="text.secondary">
          Backend is not resident — showing whatever output was captured from its last run.
        </Typography>
      )}
      <Box
        component="pre"
        sx={{
          m: 0,
          p: 1,
          fontSize: 12,
          lineHeight: 1.4,
          maxHeight: '65vh',
          overflow: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
          bgcolor: 'grey.900',
          color: 'grey.100',
          borderRadius: 1,
        }}
      >
        {lines.length ? lines.join('\n') : q.isLoading ? 'loading…' : '(no output captured)'}
      </Box>
    </Box>
  )
}

// --- Usage --------------------------------------------------------------

function UsageTab({ name }: { name: string }) {
  const q = useQuery({ queryKey: ['consoleUsage'], queryFn: () => gqlClient.request(UsageDoc) })
  const row = (q.data?.corrallm.usageRollup?.rows ?? []).find((r) => r.served === name)
  if (!row) return <Typography color="text.secondary">No usage in the last 24h.</Typography>
  return (
    <Table size="small">
      <TableBody>
        <TableRow>
          <TableCell>Requests (24h)</TableCell>
          <TableCell align="right">{fmtInt(row.requests)}</TableCell>
        </TableRow>
        <TableRow>
          <TableCell>Prompt → completion tokens</TableCell>
          <TableCell align="right">
            {fmtInt(row.promptTokens)} → {fmtInt(row.completionTokens)}
          </TableCell>
        </TableRow>
        <TableRow>
          <TableCell>Dwell</TableCell>
          <TableCell align="right">{fmtDuration(row.dwellMs)}</TableCell>
        </TableRow>
        <TableRow>
          <TableCell>Cost</TableCell>
          <TableCell align="right">{fmtUSD(row.costUsd)}</TableCell>
        </TableRow>
      </TableBody>
    </Table>
  )
}

// --- Test (playgrounds) -------------------------------------------------

function TestTab({
  capability,
  model,
  ttsModels,
  replayId,
}: {
  capability: string
  model: string
  ttsModels: string[]
  replayId?: string
}) {
  // The capability — derived from the backend cost type — already names the
  // delivery surface, so each one maps straight to its playground (no modes gate).
  if (capability === 'chat') return <ChatPlayground model={model} replayId={replayId} />
  if (capability === 'audio.stt') return <BatchStt model={model} ttsModels={ttsModels} />
  if (capability === 'audio.realtime') return <RealtimeStt model={model} ttsModels={ttsModels} />
  if (capability === 'audio.tts') return <TtsPlayground model={model} />
  return (
    <Typography color="text.secondary">
      A {capability} playground is coming. For now see the Info tab's example requests.
    </Typography>
  )
}

// SpeakBack: synthesize given text through a chosen TTS model and play it — the
// "→ TTS" half of the voice loop, shared by the batch + realtime STT views.
function SpeakBack({ text, ttsModels }: { text: string; ttsModels: string[] }) {
  const [ttsModel, setTtsModel] = useState(ttsModels[0] ?? '')
  const [err, setErr] = useState('')
  async function speak() {
    if (!ttsModel || !text) return
    setErr('')
    try {
      const r = await fetch('/v1/audio/speech', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model: ttsModel, input: text, voice: 'af_heart', response_format: 'mp3' }),
      })
      if (!r.ok) {
        setErr(`tts ${r.status}: ${await r.text()}`)
        return
      }
      await new Audio(URL.createObjectURL(await r.blob())).play()
    } catch (e) {
      setErr(String(e))
    }
  }
  if (!ttsModels.length) return null
  return (
    <Stack direction="row" spacing={1} alignItems="center">
      <Button variant="outlined" onClick={() => void speak()} disabled={!text}>
        🔊 Speak it back
      </Button>
      <TextField
        select
        size="small"
        sx={{ width: 220 }}
        label="TTS model"
        value={ttsModel}
        onChange={(e) => setTtsModel(e.target.value)}
        slotProps={{ select: { native: true } }}
      >
        {ttsModels.map((m) => (
          <option key={m} value={m}>
            {m}
          </option>
        ))}
      </TextField>
      {err && <Typography color="error" variant="caption">{err}</Typography>}
    </Stack>
  )
}

// STT playground: batch (record a clip → upload) or realtime (live ws streaming),
// either feeding the optional speak-it-back TTS loop. Mic needs a secure context.
// Batch STT: record a clip with MediaRecorder, POST it to /v1/audio/transcriptions.
function BatchStt({ model, ttsModels }: { model: string; ttsModels: string[] }) {
  const [recording, setRecording] = useState(false)
  const [busy, setBusy] = useState(false)
  const [transcript, setTranscript] = useState('')
  const [segments, setSegments] = useState<DiarSegment[]>([])
  const [speakers, setSpeakers] = useState<Speaker[]>([])
  const [err, setErr] = useState('')
  const [key, setKey] = useState('')
  const recRef = useRef<MediaRecorder | null>(null)
  const chunksRef = useRef<Blob[]>([])

  async function start() {
    setErr('')
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
      const rec = new MediaRecorder(stream)
      chunksRef.current = []
      rec.ondataavailable = (e) => e.data.size && chunksRef.current.push(e.data)
      rec.onstop = () => {
        stream.getTracks().forEach((t) => t.stop())
        const mime = rec.mimeType
        const ext = mime.includes('ogg') ? 'ogg' : mime.includes('mp4') ? 'mp4' : 'webm'
        void transcribe(new Blob(chunksRef.current, { type: mime }), `recording.${ext}`)
      }
      recRef.current = rec
      rec.start()
      setRecording(true)
    } catch (e) {
      setErr(`mic error: ${String(e)}`)
    }
  }
  function stop() {
    recRef.current?.stop()
    setRecording(false)
  }

  async function transcribe(blob: Blob, filename: string) {
    setBusy(true)
    setErr('')
    try {
      const fd = new FormData()
      fd.append('model', model)
      fd.append('file', blob, filename)
      // standard OpenAI: verbose_json gives segments[]; a diarizing backend adds
      // speaker UUIDs on each segment + a speakers[] array (both additive).
      fd.append('response_format', 'verbose_json')
      const headers: Record<string, string> = {}
      if (key) headers.Authorization = `Bearer ${key}`
      const r = await fetch('/v1/audio/transcriptions', { method: 'POST', headers, body: fd })
      if (!r.ok) {
        setErr(`${r.status}: ${await r.text()}`)
        return
      }
      const j = await r.json()
      setTranscript(String(j.text ?? JSON.stringify(j)))
      setSegments(Array.isArray(j.segments) ? (j.segments as DiarSegment[]) : [])
      setSpeakers(Array.isArray(j.speakers) ? (j.speakers as Speaker[]) : [])
    } catch (e) {
      setErr(String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={1} alignItems="center">
        {recording ? (
          <Button variant="contained" color="error" onClick={stop}>
            ■ Stop
          </Button>
        ) : (
          <Button variant="contained" onClick={() => void start()} disabled={busy}>
            ● Record
          </Button>
        )}
        <Button variant="outlined" component="label" disabled={busy || recording}>
          ⬆ Upload
          <input
            type="file"
            accept="audio/*,.wav,.mp3,.m4a,.ogg,.webm,.flac"
            hidden
            onChange={(e) => {
              const f = e.target.files?.[0]
              if (f) void transcribe(f, f.name)
              e.target.value = ''
            }}
          />
        </Button>
        {busy && <CircularProgress size={20} />}
        <TextField size="small" sx={{ width: 160 }} placeholder="group key (opt)" value={key} onChange={(e) => setKey(e.target.value)} />
      </Stack>
      <Box>
        <Typography variant="subtitle2">
          Transcript{speakers.length > 0 && ` · ${speakers.length} speakers`}
        </Typography>
        {speakers.length > 0 && (
          <Stack direction="row" spacing={0.5} sx={{ mt: 0.5, flexWrap: 'wrap', gap: 0.5 }}>
            {speakers.map((sp) => (
              <Chip
                key={sp.uuid}
                size="small"
                label={shortId(sp.uuid)}
                title={sp.uuid}
                sx={{ bgcolor: speakerColor(sp.uuid), color: '#fff', fontFamily: 'monospace' }}
              />
            ))}
          </Stack>
        )}
        {segments.some((s) => s.speaker) ? (
          <Stack spacing={0.5} sx={{ mt: 1 }}>
            {segments.map((s, i) => (
              <Box key={i} sx={{ display: 'flex', gap: 1, alignItems: 'baseline' }}>
                <Chip
                  size="small"
                  label={s.speaker ? shortId(s.speaker) : '—'}
                  sx={{ bgcolor: s.speaker ? speakerColor(s.speaker) : 'grey.500', color: '#fff', fontFamily: 'monospace', minWidth: 44 }}
                />
                <Typography variant="caption" color="text.secondary" sx={{ fontFamily: 'monospace', minWidth: 56 }}>
                  {fmtTime(s.start)}
                </Typography>
                <Typography variant="body2">{s.text}</Typography>
              </Box>
            ))}
          </Stack>
        ) : (
          <Box component="pre" sx={preSx}>{transcript || (busy ? 'transcribing…' : '—')}</Box>
        )}
      </Box>
      <SpeakBack text={transcript} ttsModels={ttsModels} />
      {err && <Typography color="error" variant="body2">{err}</Typography>}
    </Stack>
  )
}

// Realtime STT: stream mic audio (PCM16 @ 24 kHz, base64) over the OpenAI Realtime
// transcription ws to /v1/realtime, appending each finalized transcript segment.
// (Works only against a realtime-capable backend, e.g. Speaches — not batch-only
// parakeet, which will refuse the upgrade.) ws can't set headers → default group.
function RealtimeStt({ model, ttsModels }: { model: string; ttsModels: string[] }) {
  const [live, setLive] = useState(false)
  const [transcript, setTranscript] = useState('')
  const [partial, setPartial] = useState('')
  const [err, setErr] = useState('')
  const wsRef = useRef<WebSocket | null>(null)
  const ctxRef = useRef<AudioContext | null>(null)
  const streamRef = useRef<MediaStream | null>(null)
  const procRef = useRef<ScriptProcessorNode | null>(null)

  function teardown() {
    procRef.current?.disconnect()
    procRef.current = null
    void ctxRef.current?.close()
    streamRef.current?.getTracks().forEach((t) => t.stop())
  }
  function stop() {
    // Stop capturing, flush the last utterance (commit), then close the ws shortly
    // after so the final "completed" has time to arrive.
    teardown()
    const ws = wsRef.current
    try {
      ws?.send(JSON.stringify({ type: 'input_audio_buffer.commit' }))
    } catch {
      /* already closed */
    }
    setLive(false)
    setTimeout(() => ws?.close(), 800)
  }
  useEffect(() => teardown, [])

  async function start() {
    setErr('')
    setTranscript('')
    setPartial('')
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
      streamRef.current = stream
      const ctx = new AudioContext({ sampleRate: 24000 })
      ctxRef.current = ctx
      const proto = location.protocol === 'https:' ? 'wss' : 'ws'
      const ws = new WebSocket(`${proto}://${location.host}/v1/realtime?model=${encodeURIComponent(model)}&intent=transcription`)
      wsRef.current = ws
      ws.onopen = () => {
        ws.send(
          JSON.stringify({
            type: 'session.update',
            session: { input_audio_transcription: { model }, turn_detection: { type: 'server_vad' } },
          }),
        )
        const src = ctx.createMediaStreamSource(stream)
        const proc = ctx.createScriptProcessor(4096, 1, 1)
        procRef.current = proc
        proc.onaudioprocess = (e) => {
          if (ws.readyState !== WebSocket.OPEN) return
          const f32 = e.inputBuffer.getChannelData(0)
          const i16 = new Int16Array(f32.length)
          for (let i = 0; i < f32.length; i++) {
            const s = Math.max(-1, Math.min(1, f32[i]))
            i16[i] = s < 0 ? s * 0x8000 : s * 0x7fff
          }
          const bytes = new Uint8Array(i16.buffer)
          let bin = ''
          for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i])
          ws.send(JSON.stringify({ type: 'input_audio_buffer.append', audio: btoa(bin) }))
        }
        src.connect(proc)
        proc.connect(ctx.destination)
      }
      ws.onmessage = (ev) => {
        try {
          const m = JSON.parse(ev.data)
          if (m.type === 'conversation.item.input_audio_transcription.delta') {
            setPartial((p) => p + (m.delta ?? '')) // live partial as it streams
          } else if (m.type === 'conversation.item.input_audio_transcription.completed') {
            setTranscript((t) => (t + ' ' + (m.transcript ?? '')).trim())
            setPartial('')
          } else if (m.type === 'error') {
            setErr(JSON.stringify(m.error ?? m))
          }
        } catch {
          /* ignore */
        }
      }
      ws.onerror = () => setErr('websocket error — does this model support realtime? (only the realtime-stt model does)')
      ws.onclose = () => setLive(false)
      setLive(true)
    } catch (e) {
      setErr(String(e))
      stop()
    }
  }

  // Live capture needs the mic, which browsers gate behind a secure context.
  if (typeof window !== 'undefined' && !window.isSecureContext) {
    return (
      <Typography color="warning.main" variant="body2">
        The microphone needs a <b>secure context</b> — open the dashboard over <b>https</b> (e.g.
        https://llm.iodesystems.com). Plain http blocks <code>getUserMedia</code>.
      </Typography>
    )
  }

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={1} alignItems="center">
        {live ? (
          <Button variant="contained" color="error" onClick={stop}>
            ■ Stop
          </Button>
        ) : (
          <Button variant="contained" onClick={() => void start()}>
            ● Go live
          </Button>
        )}
        {live && <CircularProgress size={20} />}
      </Stack>
      <Box>
        <Typography variant="subtitle2">Live transcript</Typography>
        <Box component="pre" sx={preSx}>
          {transcript || (live && !partial ? 'listening…' : '—')}
          {partial && <span style={{ opacity: 0.55 }}> {partial}</span>}
        </Box>
      </Box>
      <SpeakBack text={transcript} ttsModels={ttsModels} />
      {err && <Typography color="error" variant="body2">{err}</Typography>}
    </Stack>
  )
}

// Text-to-speech: type text → synthesize → play through the speaker.
function TtsPlayground({ model }: { model: string }) {
  const [text, setText] = useState('Hello from corrallm.')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [key, setKey] = useState('')

  async function speak() {
    if (!text.trim()) return
    setBusy(true)
    setErr('')
    try {
      const headers: Record<string, string> = { 'Content-Type': 'application/json' }
      if (key) headers.Authorization = `Bearer ${key}`
      const r = await fetch('/v1/audio/speech', {
        method: 'POST',
        headers,
        body: JSON.stringify({ model, input: text, voice: 'af_heart', response_format: 'mp3' }),
      })
      if (!r.ok) {
        setErr(`${r.status}: ${await r.text()}`)
        return
      }
      await new Audio(URL.createObjectURL(await r.blob())).play()
    } catch (e) {
      setErr(String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Stack spacing={2}>
      <TextField multiline minRows={3} fullWidth value={text} onChange={(e) => setText(e.target.value)} placeholder="Text to speak…" />
      <Stack direction="row" spacing={1} alignItems="center">
        <Button variant="contained" onClick={() => void speak()} disabled={busy}>
          🔊 Speak
        </Button>
        {busy && <CircularProgress size={20} />}
        <TextField size="small" sx={{ width: 160 }} placeholder="group key (opt)" value={key} onChange={(e) => setKey(e.target.value)} />
      </Stack>
      {err && <Typography color="error" variant="body2">{err}</Typography>}
    </Stack>
  )
}

type ToolCall = { id: string; name: string; args: string }
// ReplayTool: an OpenAI function-tool def captured on the replayed request, kept so
// continuations can re-offer the same tools to the model.
type ReplayTool = { name: string; description: string; parameters: unknown }
type Msg = {
  role: 'system' | 'user' | 'assistant' | 'tool'
  content: string
  image?: string
  file?: { name: string; data: string }
  toolCalls?: ToolCall[] // on assistant turns
  toolCallId?: string // on tool turns
  toolName?: string // on tool turns, matched back from the assistant tool_call
}

// extractText pulls the text out of a message content that may be a string or the
// multimodal content-parts array (used when replaying a captured request).
function extractText(content: unknown): string {
  if (typeof content === 'string') return content
  if (Array.isArray(content)) {
    return content
      .map((p) => (p && typeof p === 'object' && 'text' in p ? String((p as { text: unknown }).text) : ''))
      .join(' ')
      .trim()
  }
  return ''
}

// parseUserContent splits an OpenAI user `content` (string or content-parts array)
// into the text + any attached image / file — WITHOUT flattening media away.
function parseUserContent(content: unknown): Pick<Msg, 'content' | 'image' | 'file'> {
  if (typeof content === 'string') return { content }
  if (!Array.isArray(content)) return { content: extractText(content) }
  let text = ''
  let image: string | undefined
  let file: Msg['file']
  for (const p of content) {
    if (!p || typeof p !== 'object') continue
    const part = p as Record<string, any>
    if (part.type === 'text') text += (text ? ' ' : '') + String(part.text ?? '')
    else if (part.type === 'image_url') image = String(part.image_url?.url ?? '')
    else if (part.type === 'file') file = { name: String(part.file?.filename ?? 'file'), data: String(part.file?.file_data ?? '') }
  }
  return { content: text, image, file }
}

// prettyArgs pretty-prints a tool_call arguments JSON string; falls back to raw.
function prettyArgs(args: string): string {
  try {
    return JSON.stringify(JSON.parse(args), null, 2)
  } catch {
    return args
  }
}

// toApiMsg renders a Msg back into OpenAI wire shape, preserving assistant
// tool_calls and tool-role results so a replayed exchange carries into follow-ups.
function toApiMsg(m: Msg): Record<string, unknown> {
  if (m.role === 'assistant') {
    const msg: Record<string, unknown> = { role: 'assistant', content: m.content || '' }
    if (m.toolCalls?.length) {
      msg.tool_calls = m.toolCalls.map((tc) => ({ id: tc.id, type: 'function', function: { name: tc.name, arguments: tc.args } }))
    }
    return msg
  }
  if (m.role === 'tool') return { role: 'tool', tool_call_id: m.toolCallId, content: m.content }
  if (m.role === 'system') return { role: 'system', content: m.content }
  return { role: 'user', content: apiContent(m) }
}

// apiContent renders a message in OpenAI shape: a plain string, or — when an image
// or a file is attached — the multimodal content-parts array. A PDF goes as a
// `file` part; corrallm extracts its text server-side so even a text model reads it.
function apiContent(m: Msg): unknown {
  if (!m.image && !m.file) return m.content
  const parts: unknown[] = [{ type: 'text', text: m.content }]
  if (m.image) parts.push({ type: 'image_url', image_url: { url: m.image } })
  if (m.file) parts.push({ type: 'file', file: { filename: m.file.name, file_data: m.file.data } })
  return parts
}

// toApiTool renders a captured ReplayTool back into an OpenAI function-tool def so a
// continuation re-offers it to the model.
function toApiTool(t: ReplayTool): Record<string, unknown> {
  return { type: 'function', function: { name: t.name, description: t.description, parameters: t.parameters } }
}

function ChatPlayground({ model, replayId }: { model: string; replayId?: string }) {
  const [key, setKey] = useState('')
  const [input, setInput] = useState('')
  const [image, setImage] = useState<string | null>(null)
  const [file, setFile] = useState<{ name: string; data: string } | null>(null)
  const [msgs, setMsgs] = useState<Msg[]>([])
  const [busy, setBusy] = useState(false)
  // tools captured on the replayed request; re-offered on every continuation
  const [replayTools, setReplayTools] = useState<ReplayTool[]>([])
  // tool_calls the model just made that await a user-pasted result (console can't
  // execute tools); keyed manual-entry text lives in `results`
  const [pending, setPending] = useState<ToolCall[] | null>(null)
  const [results, setResults] = useState<Record<string, string>>({})
  const msgsRef = useRef<Msg[]>([])
  const fileRef = useRef<HTMLInputElement>(null)
  msgsRef.current = msgs

  // Replay (P11e): load a logged request's captured payload into the chat — prior
  // turns become history, the last user turn lands in the input to re-run/tweak.
  useEffect(() => {
    if (!replayId) return
    let cancelled = false
    void gqlClient.request(ReplayDoc, { id: replayId }).then((d) => {
      if (cancelled) return
      const raw = d.corrallm.activityDetail?.record?.reqBody
      if (!raw) return
      try {
        const body = JSON.parse(raw)
        const rawMsgs: any[] = body.messages ?? []
        // map tool_call_id → name so tool results can name the call they answer
        const toolNames = new Map<string, string>()
        for (const m of rawMsgs) {
          if (m?.role === 'assistant' && Array.isArray(m.tool_calls)) {
            for (const tc of m.tool_calls) if (tc?.id) toolNames.set(String(tc.id), String(tc.function?.name ?? ''))
          }
        }
        const parsed: Msg[] = rawMsgs.map((m) => {
          if (m?.role === 'assistant') {
            const toolCalls: ToolCall[] = Array.isArray(m.tool_calls)
              ? m.tool_calls.map((tc: any) => ({ id: String(tc?.id ?? ''), name: String(tc?.function?.name ?? ''), args: String(tc?.function?.arguments ?? '') }))
              : []
            return {
              role: 'assistant',
              content: typeof m.content === 'string' ? m.content : extractText(m.content),
              toolCalls: toolCalls.length ? toolCalls : undefined,
            }
          }
          if (m?.role === 'tool') {
            const id = m.tool_call_id != null ? String(m.tool_call_id) : undefined
            return {
              role: 'tool',
              content: typeof m.content === 'string' ? m.content : extractText(m.content),
              toolCallId: id,
              toolName: id ? toolNames.get(id) : undefined,
            }
          }
          if (m?.role === 'system') return { role: 'system', content: typeof m.content === 'string' ? m.content : extractText(m.content) }
          return { role: 'user', ...parseUserContent(m?.content) }
        })
        // capture the function-tool defs the model had available so continuations
        // re-offer them (purely informational in the Tools panel too).
        const rawTools: any[] = Array.isArray(body.tools) ? body.tools : []
        const tools: ReplayTool[] = rawTools
          .filter((t) => t?.type === 'function' && t.function)
          .map((t) => ({
            name: String(t.function.name ?? ''),
            description: String(t.function.description ?? ''),
            parameters: t.function.parameters,
          }))
        setReplayTools(tools)
        // pull the LAST user turn into the input to re-run/tweak; keep every other
        // turn (incl. any trailing assistant/tool turns) in history as-is.
        const lastUserIdx = parsed.map((m) => m.role).lastIndexOf('user')
        if (lastUserIdx >= 0) {
          setInput(parsed[lastUserIdx].content)
          setMsgs([...parsed.slice(0, lastUserIdx), ...parsed.slice(lastUserIdx + 1)])
        } else {
          setMsgs(parsed)
        }
      } catch {
        setInput(raw.slice(0, 4000))
      }
    })
    return () => {
      cancelled = true
    }
  }, [replayId])

  function attach(e: React.ChangeEvent<HTMLInputElement>) {
    const f = e.target.files?.[0]
    if (!f) return
    const isPdf = f.type === 'application/pdf' || f.name.toLowerCase().endsWith('.pdf')
    const reader = new FileReader()
    reader.onload = () => {
      const data = String(reader.result)
      if (isPdf) setFile({ name: f.name, data })
      else setImage(data)
    }
    reader.readAsDataURL(f)
    e.target.value = ''
  }

  // runCompletion streams one assistant turn over `history` (already includes any
  // trailing user/tool turn). Re-offers replayTools; accumulates streamed tool_calls
  // and, if the model calls tools, parks them for manual result entry instead of
  // auto-continuing.
  async function runCompletion(history: Msg[]) {
    const base: Msg[] = [...history, { role: 'assistant', content: '' }]
    msgsRef.current = base
    setMsgs(base)
    setBusy(true)
    try {
      const headers: Record<string, string> = { 'Content-Type': 'application/json' }
      if (key) headers.Authorization = `Bearer ${key}`
      const resp = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers,
        body: JSON.stringify({
          model,
          stream: true,
          messages: history.map(toApiMsg),
          ...(replayTools.length ? { tools: replayTools.map(toApiTool) } : {}),
        }),
      })
      if (!resp.ok || !resp.body) {
        appendToLast(`\n[error ${resp.status}] ${await resp.text()}`)
        return
      }
      const reader = resp.body.getReader()
      const dec = new TextDecoder()
      let buf = ''
      // accumulate streamed tool_calls by their delta index; id + name arrive whole,
      // arguments stream as fragments to be concatenated.
      const toolAcc: ToolCall[] = []
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buf += dec.decode(value, { stream: true })
        const parts = buf.split('\n\n')
        buf = parts.pop() ?? ''
        for (const p of parts) {
          const line = p.split('\n').find((l) => l.startsWith('data:'))
          if (!line) continue
          const data = line.slice(5).trim()
          if (data === '[DONE]') continue
          try {
            const delta = JSON.parse(data)?.choices?.[0]?.delta
            if (delta?.content) appendToLast(delta.content)
            if (Array.isArray(delta?.tool_calls)) {
              for (const tc of delta.tool_calls) {
                const idx = typeof tc?.index === 'number' ? tc.index : 0
                if (!toolAcc[idx]) toolAcc[idx] = { id: '', name: '', args: '' }
                if (tc?.id) toolAcc[idx].id = String(tc.id)
                if (tc?.function?.name) toolAcc[idx].name = String(tc.function.name)
                if (tc?.function?.arguments) toolAcc[idx].args += String(tc.function.arguments)
              }
            }
          } catch {
            /* ignore keepalive/non-json */
          }
        }
      }
      const calls = toolAcc.filter(Boolean)
      if (calls.length) {
        // attach the calls to the assistant turn and park them for manual results —
        // don't auto-continue (the console can't execute tools).
        setMsgs((cur) => {
          const next = cur.slice()
          next[next.length - 1] = { ...next[next.length - 1], toolCalls: calls }
          return next
        })
        setPending(calls)
      }
    } catch (e) {
      appendToLast(`\n[error] ${String(e)}`)
    } finally {
      setBusy(false)
    }
  }

  function send() {
    const text = input.trim()
    if ((!text && !image && !file) || busy) return
    setInput('')
    const userMsg: Msg = { role: 'user', content: text, image: image ?? undefined, file: file ?? undefined }
    setImage(null)
    setFile(null)
    setPending(null)
    setResults({})
    void runCompletion([...msgsRef.current, userMsg])
  }

  // submitResults turns the user-pasted text for each parked tool_call into tool
  // messages, then fires a continuation so the model resumes with the results.
  function submitResults() {
    if (!pending || busy) return
    const toolMsgs: Msg[] = pending.map((tc) => ({
      role: 'tool',
      content: results[tc.id] ?? '',
      toolCallId: tc.id,
      toolName: tc.name,
    }))
    setPending(null)
    setResults({})
    void runCompletion([...msgsRef.current, ...toolMsgs])
  }

  function appendToLast(s: string) {
    setMsgs((cur) => {
      const next = cur.slice()
      next[next.length - 1] = { ...next[next.length - 1], content: next[next.length - 1].content + s }
      return next
    })
  }

  return (
    <Stack spacing={1} sx={{ height: '60vh' }}>
      {replayTools.length > 0 && (
        <Accordion disableGutters elevation={0} variant="outlined" sx={{ '&:before': { display: 'none' } }}>
          <AccordionSummary expandIcon={<ExpandMoreIcon fontSize="small" />} sx={{ minHeight: 0, '& .MuiAccordionSummary-content': { my: 0.5 } }}>
            <Typography variant="caption" color="text.secondary">
              Tools ({replayTools.length})
            </Typography>
          </AccordionSummary>
          <AccordionDetails sx={{ py: 0.5 }}>
            <Stack spacing={1}>
              {replayTools.map((t, i) => (
                <Box key={i}>
                  <Typography variant="body2" sx={{ fontFamily: 'monospace', fontWeight: 600 }}>
                    {t.name}
                  </Typography>
                  {t.description && (
                    <Typography variant="caption" color="text.secondary" sx={{ whiteSpace: 'pre-wrap' }}>
                      {t.description}
                    </Typography>
                  )}
                  {t.parameters != null && (
                    <Accordion disableGutters elevation={0} sx={{ bgcolor: 'transparent', '&:before': { display: 'none' } }}>
                      <AccordionSummary expandIcon={<ExpandMoreIcon fontSize="small" />} sx={{ minHeight: 0, px: 0, '& .MuiAccordionSummary-content': { my: 0.25 } }}>
                        <Typography variant="caption" color="text.disabled">
                          parameters ▸
                        </Typography>
                      </AccordionSummary>
                      <AccordionDetails sx={{ px: 0, py: 0.5 }}>
                        <Box component="pre" sx={{ m: 0, fontFamily: 'monospace', fontSize: 12, whiteSpace: 'pre-wrap', overflowX: 'auto' }}>
                          {JSON.stringify(t.parameters, null, 2)}
                        </Box>
                      </AccordionDetails>
                    </Accordion>
                  )}
                </Box>
              ))}
            </Stack>
          </AccordionDetails>
        </Accordion>
      )}
      {/* column-reverse: newest pins to the bottom, no scroll management */}
      <Paper variant="outlined" sx={{ flex: 1, p: 1, display: 'flex', flexDirection: 'column-reverse', overflow: 'auto' }}>
        <Box>
          {msgs.map((m, i) => {
            if (m.role === 'system') {
              return (
                <Accordion
                  key={i}
                  disableGutters
                  elevation={0}
                  sx={{ my: 0.5, bgcolor: 'transparent', '&:before': { display: 'none' } }}
                >
                  <AccordionSummary
                    expandIcon={<ExpandMoreIcon fontSize="small" />}
                    sx={{ minHeight: 0, px: 0, '& .MuiAccordionSummary-content': { my: 0.5 } }}
                  >
                    <Typography variant="caption" color="text.secondary">
                      system ▸
                    </Typography>
                  </AccordionSummary>
                  <AccordionDetails sx={{ px: 0, py: 0.5 }}>
                    <Typography variant="body2" color="text.secondary" sx={{ whiteSpace: 'pre-wrap' }}>
                      {m.content}
                    </Typography>
                  </AccordionDetails>
                </Accordion>
              )
            }
            if (m.role === 'tool') {
              const preview = m.content.replace(/\s+/g, ' ').trim().slice(0, 80)
              return (
                <Accordion key={i} disableGutters elevation={0} variant="outlined" sx={{ my: 0.5, '&:before': { display: 'none' } }}>
                  <AccordionSummary expandIcon={<ExpandMoreIcon fontSize="small" />} sx={{ minHeight: 0, '& .MuiAccordionSummary-content': { my: 0.5, alignItems: 'baseline' } }}>
                    <Typography variant="caption" color="text.secondary" sx={{ fontFamily: 'monospace' }}>
                      → {m.toolName || 'tool'} result
                    </Typography>
                    <Typography
                      variant="caption"
                      color="text.disabled"
                      sx={{ ml: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                    >
                      {preview}
                    </Typography>
                  </AccordionSummary>
                  <AccordionDetails>
                    <Box
                      component="pre"
                      sx={{ m: 0, fontFamily: 'monospace', fontSize: 12, whiteSpace: 'pre-wrap', overflowX: 'auto' }}
                    >
                      {m.content}
                    </Box>
                  </AccordionDetails>
                </Accordion>
              )
            }
            return (
              <Box key={i} sx={{ mb: 1 }}>
                <Typography variant="caption" color="text.secondary">
                  {m.role}
                </Typography>
                {m.image && (
                  <Box
                    component="img"
                    src={m.image}
                    sx={{ display: 'block', maxWidth: 200, maxHeight: 160, borderRadius: 1, my: 0.5 }}
                  />
                )}
                {m.file && <Chip size="small" label={`📄 ${m.file.name}`} sx={{ display: 'flex', width: 'fit-content', my: 0.5 }} />}
                <Typography variant="body2" sx={{ whiteSpace: 'pre-wrap' }}>
                  {m.content || (busy && i === msgs.length - 1 ? '…' : '')}
                </Typography>
                {m.toolCalls?.map((tc, j) => (
                  <Box
                    key={j}
                    sx={{
                      border: 1,
                      borderColor: 'divider',
                      borderRadius: 1,
                      p: 1,
                      my: 0.5,
                      bgcolor: 'action.hover',
                      fontFamily: 'monospace',
                      fontSize: 12,
                      overflowX: 'auto',
                    }}
                  >
                    <Typography variant="caption" component="div" sx={{ fontFamily: 'monospace', fontWeight: 600 }}>
                      🔧 {tc.name}
                    </Typography>
                    <Box component="pre" sx={{ m: 0, mt: 0.5, whiteSpace: 'pre-wrap' }}>
                      {prettyArgs(tc.args)}
                    </Box>
                  </Box>
                ))}
              </Box>
            )
          })}
        </Box>
      </Paper>
      {image && (
        <Stack direction="row" spacing={1} alignItems="center">
          <Box component="img" src={image} sx={{ maxHeight: 48, borderRadius: 1 }} />
          <Button size="small" onClick={() => setImage(null)}>
            remove image
          </Button>
        </Stack>
      )}
      {file && (
        <Stack direction="row" spacing={1} alignItems="center">
          <Chip size="small" label={`📄 ${file.name}`} />
          <Button size="small" onClick={() => setFile(null)}>
            remove file
          </Button>
        </Stack>
      )}
      {pending && pending.length > 0 && !busy && (
        <Paper variant="outlined" sx={{ p: 1 }}>
          <Typography variant="caption" color="text.secondary">
            paste a result for each tool call, then submit to continue
          </Typography>
          <Stack spacing={1} sx={{ mt: 1 }}>
            {pending.map((tc, i) => (
              <TextField
                key={tc.id || i}
                size="small"
                fullWidth
                multiline
                minRows={2}
                label={`result for 🔧 ${tc.name}(${tc.args})`}
                value={results[tc.id] ?? ''}
                onChange={(e) => setResults((r) => ({ ...r, [tc.id]: e.target.value }))}
              />
            ))}
            <Button variant="contained" size="small" sx={{ alignSelf: 'flex-start' }} onClick={submitResults}>
              submit results
            </Button>
          </Stack>
        </Paper>
      )}
      {replayTools.length > 0 && !(pending && pending.length > 0) && (
        <Typography variant="caption" color="text.secondary">
          tools are re-offered; when the model calls one, paste a result to continue
        </Typography>
      )}
      <Stack direction="row" spacing={1}>
        <input ref={fileRef} type="file" accept="image/*,application/pdf,.pdf" hidden onChange={attach} />
        <Button variant="outlined" onClick={() => fileRef.current?.click()} title="Attach an image (vision) or a PDF (auto-converted to text)">
          📎
        </Button>
        <TextField
          size="small"
          fullWidth
          placeholder="Message…"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault()
              void send()
            }
          }}
        />
        <TextField
          size="small"
          sx={{ width: 160 }}
          placeholder="group key (opt)"
          value={key}
          onChange={(e) => setKey(e.target.value)}
        />
        <Button variant="contained" onClick={() => void send()} disabled={busy}>
          Send
        </Button>
      </Stack>
    </Stack>
  )
}

export const Route = createFileRoute('/model')({
  validateSearch: (s: Record<string, unknown>): { name: string; tab?: string; replay?: string } => ({
    name: String(s.name ?? ''),
    tab: s.tab ? String(s.tab) : undefined,
    replay: s.replay ? String(s.replay) : undefined,
  }),
  component: ModelConsole,
})

// --- Bench --------------------------------------------------------------

const ModelBenchDoc = graphql(/* GraphQL */ `
  query ModelBench($model: String!) {
    corrallm {
      benchResults(model: $model) {
        results {
          runId
          at
          score
          stages
          stagesPassed
          classes
          tokensProcessed
          tokensGenerated
          cachedTokens
          tokPerSec
          footprintMiB
        }
      }
      benchPlan {
        models {
          model
          new
          hasTuneProfile
          unverifiedModalities
          disagreements {
            modality
            runMode
            detail
          }
          probes {
            kind
            default
            reason
          }
        }
      }
      benchStatus {
        running
        args
        log
        done
        error
      }
      benchProbes(model: $model) {
        runId
        at
        capabilities {
          capability
          stages
          stagesPassed
          score
          skippedProbes
          probes {
            probe
            class
            score
            stages
            stagesPassed
            pass
            skipped
            skipReason
            note
            disagreement
            arms {
              label
              toolset
              toolFormat
              runMode
              isBaseline
              stages
              stagesPassed
              score
              scoreDelta
              checksPassed
              checksTotal
              pass
              wallMs
              newPromptTokens
              completionTokens
              note
              skipped
              skipReason
            }
          }
        }
      }
    }
  }
`)

// The measured profile, queried on the INFO tab so the Memory table has numbers
// even when the model is not resident.
const ModelProfileDoc = graphql(/* GraphQL */ `
  query ModelProfile {
    corrallm {
      benchPlan {
        models {
          model
          profile {
            baseMiB
            perSlotMiB
            peakMiB
            measuredSlots
            ctx
            source
            measuredAt
          }
        }
      }
    }
  }
`)

const RunModelBenchDoc = graphql(/* GraphQL */ `
  mutation RunModelBench($body: corrallm_BenchRunInputBodyInput!) {
    corrallm {
      startBenchRun(body: $body) {
        ok
        message
        warning
      }
    }
  }
`)

const CancelModelBenchDoc = graphql(/* GraphQL */ `
  mutation CancelModelBench {
    corrallm {
      cancelBenchRun {
        running
      }
    }
  }
`)

const BENCH_KINDS = ['measure', 'capability', 'quality'] as const
type BenchKind = (typeof BENCH_KINDS)[number]

// Codegen maps int64 -> string; coerce once rather than per-use.
const bn = (v: unknown): number => Number(v ?? 0) || 0

/**
 * The last run, broken out by the capability each probe required.
 *
 * The single run-wide pass rate in History is NOT comparable across models and
 * reading it as if it were is the bug this section fixes: a probe a model cannot
 * serve is skipped rather than failed, so an STT model is scored on its four
 * audio probes, passes them, and shows 100% — ranking above a chat model that
 * ran twenty mixed probes and scored 90%. Split by capability, "stt: 4/4, chat:
 * not applicable" says what actually happened.
 *
 * Skipped capabilities are shown, not hidden. "No chat probes ran because this
 * model has no chat surface" and "no chat data yet" are different facts, and
 * omitting the row makes them look identical.
 */
function CapabilityBreakdown({
  data,
  model,
}: {
  data: ModelBenchQuery['corrallm']['benchProbes']
  model: string
}) {
  const caps = data?.capabilities ?? []
  const runId = data?.runId ?? ''
  if (caps.length === 0) return null
  return (
    <Paper sx={{ p: 2 }}>
      <Stack direction="row" spacing={1} alignItems="baseline" sx={{ mb: 1 }}>
        <Typography variant="subtitle1">Last run by capability</Typography>
        <Typography variant="caption" color="text.secondary">
          run {data?.runId}
        </Typography>
      </Stack>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        Each capability is scored only on the probes written for it. Scores across different
        capabilities are not comparable — a speech model passing every speech probe says nothing
        about whether it can hold a conversation.
      </Typography>
      {caps.map((c) => {
        const ran = (c.probes ?? []).filter((p) => !p.skipped)
        const skipped = (c.probes ?? []).filter((p) => p.skipped)
        const notApplicable = ran.length === 0 && skipped.length > 0
        return (
          <Accordion key={c.capability} disableGutters>
            <AccordionSummary expandIcon={<ExpandMoreIcon />}>
              <Stack direction="row" spacing={1.5} alignItems="center" sx={{ width: '100%' }}>
                <Chip size="small" label={capLabel(c.capability)} />
                {notApplicable ? (
                  <Typography variant="body2" color="text.secondary">
                    not applicable — {skipped.length} probe{skipped.length === 1 ? '' : 's'} skipped
                  </Typography>
                ) : (
                  <Typography variant="body2">
                    <b>{(bn(c.score) * 100).toFixed(0)}%</b> ({c.stagesPassed}/{c.stages} stages,{' '}
                    {ran.length} probe{ran.length === 1 ? '' : 's'})
                  </Typography>
                )}
                <Box sx={{ flexGrow: 1 }} />
                {!notApplicable && skipped.length > 0 && (
                  <Typography variant="caption" color="text.secondary">
                    {skipped.length} skipped
                  </Typography>
                )}
              </Stack>
            </AccordionSummary>
            <AccordionDetails sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
              {(c.probes ?? []).map((p) => (
                <ProbeRow key={p.probe} runId={runId} model={model} probe={p} />
              ))}
            </AccordionDetails>
          </Accordion>
        )
      })}
    </Paper>
  )
}

type ProbeArm = NonNullable<
  NonNullable<
    NonNullable<ModelBenchQuery['corrallm']['benchProbes']>['capabilities'][number]['probes']
  >[number]['arms']
>[number]

type ProbeEntry = NonNullable<
  NonNullable<ModelBenchQuery['corrallm']['benchProbes']>['capabilities'][number]['probes']
>[number]

/** Signed percentage delta, e.g. "+2%" / "-5%". */
function deltaLabel(d: number): string {
  const pct = d * 100
  return `${pct > 0 ? '+' : ''}${pct.toFixed(0)}%`
}

/**
 * One probe: its baseline score, every A/B arm beside it, and the evidence.
 *
 * Arms are rows rather than a merged number because that is the whole point of
 * running them — the comparison is the result. The headline comes from the
 * baseline arm alone so it means the same thing run to run; the others show as
 * deltas against it.
 */
function ProbeRow({
  runId,
  model,
  probe,
}: {
  runId: string
  model: string
  probe: ProbeEntry
}) {
  const arms = (probe.arms ?? []) as ProbeArm[]
  const ranArms = arms.filter((a) => !a.skipped)
  const baseline = ranArms.find((a) => a.isBaseline)
  return (
    <Accordion variant="outlined" disableGutters>
      <AccordionSummary expandIcon={<ExpandMoreIcon />}>
        <Stack direction="row" spacing={1.5} alignItems="center" sx={{ width: '100%' }}>
          <Typography variant="body2" sx={{ fontFamily: 'monospace' }}>
            {probe.probe}
          </Typography>
          <Chip size="small" variant="outlined" label={probe.class || 'probe'} />
          {probe.skipped ? (
            <Tooltip title={probe.skipReason ?? ''}>
              <Chip size="small" variant="outlined" label="skipped" />
            </Tooltip>
          ) : (
            <>
              <Typography variant="body2">
                <b>{(bn(probe.score) * 100).toFixed(0)}%</b> ({probe.stagesPassed}/{probe.stages})
              </Typography>
              <Chip
                size="small"
                color={probe.pass ? 'success' : 'error'}
                label={probe.pass ? 'pass' : 'fail'}
              />
            </>
          )}
          {probe.disagreement && (
            <Tooltip title="Arms of this probe reached different verdicts. That disagreement is the finding — a pooled score would hide it.">
              <Chip size="small" color="warning" label="arms disagree" />
            </Tooltip>
          )}
          <Box sx={{ flexGrow: 1 }} />
          {ranArms.length > 1 && (
            <Typography variant="caption" color="text.secondary">
              {ranArms.length} arms
            </Typography>
          )}
        </Stack>
      </AccordionSummary>
      <AccordionDetails sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        {!!probe.note && (
          <Typography variant="body2" color="error">
            {probe.note}
          </Typography>
        )}
        {ranArms.length > 1 && (
          <Box>
            <Typography variant="subtitle2">A/B arms</Typography>
            <Typography variant="caption" color="text.secondary">
              Delta is against the baseline arm. Token counts are prompt tokens actually
              evaluated, so a cached prefix re-sent every turn is not charged to an arm twice.
            </Typography>
            <TableContainer sx={{ mt: 1 }}>
              <Table size="small">
                <TableHead>
                  <TableRow>
                    <TableCell>Arm</TableCell>
                    <TableCell align="right">Score</TableCell>
                    <TableCell align="right">Δ</TableCell>
                    <TableCell align="right">Stages</TableCell>
                    <TableCell align="right">Checks</TableCell>
                    <TableCell align="right">In</TableCell>
                    <TableCell align="right">Out</TableCell>
                    <TableCell align="right">Wall</TableCell>
                    <TableCell>Result</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {ranArms.map((a) => (
                    <TableRow key={a.label} hover>
                      <TableCell>
                        <Stack direction="row" spacing={0.5} alignItems="center">
                          <Typography variant="caption">{a.label}</Typography>
                          {a.isBaseline && <Chip size="small" label="baseline" />}
                        </Stack>
                      </TableCell>
                      <TableCell align="right">{(bn(a.score) * 100).toFixed(0)}%</TableCell>
                      <TableCell align="right">
                        {a.isBaseline ? (
                          '—'
                        ) : (
                          <Typography
                            variant="body2"
                            color={
                              bn(a.scoreDelta) > 0
                                ? 'success.main'
                                : bn(a.scoreDelta) < 0
                                  ? 'error.main'
                                  : 'text.secondary'
                            }
                          >
                            {deltaLabel(bn(a.scoreDelta))}
                          </Typography>
                        )}
                      </TableCell>
                      <TableCell align="right">
                        {a.stagesPassed}/{a.stages}
                      </TableCell>
                      <TableCell align="right">
                        {a.checksPassed}/{a.checksTotal}
                      </TableCell>
                      <TableCell align="right">{fmtInt(bn(a.newPromptTokens))}</TableCell>
                      <TableCell align="right">{fmtInt(bn(a.completionTokens))}</TableCell>
                      <TableCell align="right">{fmtDuration(bn(a.wallMs))}</TableCell>
                      <TableCell>
                        <Tooltip title={a.note ?? ''}>
                          <Chip
                            size="small"
                            color={a.pass ? 'success' : 'error'}
                            label={a.pass ? 'pass' : 'fail'}
                          />
                        </Tooltip>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          </Box>
        )}
        {probe.skipped ? (
          <Alert severity="info">{probe.skipReason || 'This probe was not run.'}</Alert>
        ) : (
          <BenchProbeDetail
            runId={runId}
            model={model}
            probe={probe.probe}
            toolset={baseline?.toolset || undefined}
            runMode={baseline?.runMode || undefined}
            armLabel={baseline?.label || undefined}
          />
        )}
      </AccordionDetails>
    </Accordion>
  )
}

/**
 * This model's measurement history plus the controls to take a new one.
 *
 * Scoped to ONE model deliberately: the cross-model comparison lives on /bench.
 * Here the question is "what do we know about this model, and what is missing?"
 */
function BenchTab({ name }: { name: string }) {
  const qc = useQueryClient()
  const [kinds, setKinds] = useState<Set<BenchKind> | null>(null)
  const [confirming, setConfirming] = useState(false)

  const q = useQuery({
    queryKey: ['modelBench', name],
    queryFn: () => gqlClient.request(ModelBenchDoc, { model: name }),
    refetchInterval: (r) => (r.state.data?.corrallm?.benchStatus?.running ? 2000 : false),
  })

  const results = q.data?.corrallm?.benchResults?.results ?? []
  const plan = (q.data?.corrallm?.benchPlan?.models ?? []).find((m) => m.model === name)
  const status = q.data?.corrallm?.benchStatus
  const running = !!status?.running

  // Seeded from what is MISSING, then the user owns it — same rule as the
  // aggregate page, so a probe defaults on exactly where it would teach us
  // something.
  const checked = kinds ?? new Set<BenchKind>(
    (plan?.probes ?? []).filter((p) => p.default).map((p) => p.kind as BenchKind),
  )
  const toggle = (k: BenchKind) => {
    const next = new Set(checked)
    if (next.has(k)) next.delete(k)
    else next.add(k)
    setKinds(next)
  }

  const classes = [...checked].flatMap((k) =>
    k === 'capability' ? ['capability'] : k === 'quality' ? ['coding', 'tooluse', 'adversarial'] : [],
  )

  const run = useMutation({
    mutationFn: () =>
      gqlClient.request(RunModelBenchDoc, {
        body: { models: [name], classes, reason: `console bench ${name}` },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['modelBench', name] }),
  })
  const cancel = useMutation({
    mutationFn: () => gqlClient.request(CancelModelBenchDoc),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['modelBench', name] }),
  })

  const offered = new Set((plan?.probes ?? []).map((p) => p.kind))

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      {!!plan?.disagreements?.length && (
        <Alert severity="error">
          <AlertTitle>Cold/warm disagreement</AlertTitle>
          A declared modality worked in one residency state and failed in the other. This is the
          failure a warm-only check cannot see:{' '}
          {plan.disagreements.map((d) => `${d.modality} (${d.runMode || 'any'})`).join(', ')}
        </Alert>
      )}
      {!plan?.hasTuneProfile && (
        <Alert severity="info">
          No measured VRAM profile — corrallm is scheduling this model on its declared{' '}
          <code>ramUsage</code>, which nothing has verified.
        </Alert>
      )}

      <Paper sx={{ p: 2 }}>
        <Typography variant="subtitle1" gutterBottom>
          Run a benchmark on {name}
        </Typography>
        <Stack direction="row" spacing={2} alignItems="center" flexWrap="wrap">
          {BENCH_KINDS.map((k) => {
            const p = (plan?.probes ?? []).find((x) => x.kind === k)
            return (
              <Tooltip key={k} title={p?.reason ?? 'not applicable to this model'}>
                <span>
                  <FormControlLabel
                    control={
                      <Checkbox
                        size="small"
                        disabled={running || !offered.has(k)}
                        checked={checked.has(k)}
                        onChange={() => toggle(k)}
                      />
                    }
                    label={k}
                  />
                </span>
              </Tooltip>
            )
          })}
          <Box sx={{ flexGrow: 1 }} />
          <Button
            variant="contained"
            color="warning"
            disabled={running || checked.size === 0}
            onClick={() => setConfirming(true)}
          >
            Run
          </Button>
          {running && (
            <Button variant="outlined" color="error" onClick={() => cancel.mutate()}>
              Cancel
            </Button>
          )}
        </Stack>
      </Paper>

      {running && (
        <Alert severity="warning">
          <AlertTitle>Bench running — exclusive mode</AlertTitle>
          Models are being evicted and all other callers are receiving 429 + Retry-After.
        </Alert>
      )}

      {(status?.log?.length ?? 0) > 0 && (
        <Paper sx={{ p: 2, maxHeight: 280, overflow: 'auto' }}>
          <Typography variant="subtitle2">Run output</Typography>
          <Box component="pre" sx={{ m: 0, fontSize: 12 }}>
            {status?.log?.join('\n')}
          </Box>
        </Paper>
      )}
      {!!status?.error && <Alert severity="error">{status.error}</Alert>}

      <CapabilityBreakdown data={q.data?.corrallm?.benchProbes} model={name} />

      <Paper sx={{ p: 2 }}>
        <Typography variant="subtitle1" gutterBottom>
          History
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          One pass rate per run, across every probe that ran. Use it to track a model against
          itself over time — not to rank models against each other, since two models rarely run
          the same probe set. The capability breakdown above is the comparable view.
        </Typography>
        {results.length === 0 ? (
          <Typography variant="body2" color="text.secondary">
            No benchmark has been run against this model.
          </Typography>
        ) : (
          <TableContainer>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Run</TableCell>
                  <TableCell align="right">Score</TableCell>
                  <TableCell align="right">Processed</TableCell>
                  <TableCell align="right">Generated</TableCell>
                  <TableCell align="right">Cached</TableCell>
                  <TableCell align="right">tok/s</TableCell>
                  <TableCell align="right">VRAM MiB</TableCell>
                  <TableCell>Classes</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {results.map((r) => (
                  <TableRow key={r.runId} hover>
                    <TableCell>{r.runId}</TableCell>
                    <TableCell align="right">
                      {(bn(r.score) * 100).toFixed(0)}% ({r.stagesPassed}/{r.stages})
                    </TableCell>
                    <TableCell align="right">{fmtInt(bn(r.tokensProcessed))}</TableCell>
                    <TableCell align="right">{fmtInt(bn(r.tokensGenerated))}</TableCell>
                    <TableCell align="right">{fmtInt(bn(r.cachedTokens))}</TableCell>
                    <TableCell align="right">{bn(r.tokPerSec).toFixed(0)}</TableCell>
                    <TableCell align="right">{fmtInt(bn(r.footprintMiB))}</TableCell>
                    <TableCell>
                      <Typography variant="caption">{r.classes || '—'}</Typography>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableContainer>
        )}
      </Paper>

      <Dialog open={confirming} onClose={() => setConfirming(false)}>
        <DialogTitle>Bench {name} in exclusive mode?</DialogTitle>
        <DialogContent dividers>
          <Typography variant="body2" component="div">
            Selected: <b>{[...checked].join(', ') || 'nothing'}</b>
            <Alert severity="warning" sx={{ mt: 2 }}>
              Models are <b>evicted</b> so measurements are uncontended, and every other caller
              receives <b>429 + Retry-After</b> until the run finishes. The lease self-expires, so
              a crashed run cannot lock the server permanently.
            </Alert>
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirming(false)}>Cancel</Button>
          <Button
            variant="contained"
            color="warning"
            onClick={() => {
              setConfirming(false)
              run.mutate()
            }}
          >
            Evict and run
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  )
}
