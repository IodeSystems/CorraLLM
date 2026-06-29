import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import {
  Box,
  Button,
  Chip,
  CircularProgress,
  Link as MuiLink,
  Paper,
  Stack,
  Tab,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Tabs,
  TextField,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { capLabel, fmtDuration, fmtInt, fmtUSD } from '@/format'

// --- data ---------------------------------------------------------------

const ConsoleDoc = graphql(/* GraphQL */ `
  query ModelConsole {
    corrallm {
      overview {
        models {
          name
          modality
          capability
          persistent
          ttl
          backends {
            index
            type
            quality
            target
            cmd
            maxConcurrent
          }
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

// --- console ------------------------------------------------------------

function ModelConsole() {
  const { name, replay } = Route.useSearch()
  const navigate = useNavigate()
  const [tab, setTab] = useState(replay ? 1 : 0)
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
        {model.persistent && <Chip size="small" variant="outlined" label="pinned" />}
      </Stack>

      <Tabs value={tab} onChange={(_, v) => setTab(v)} sx={{ mb: 2 }}>
        <Tab label="Info" />
        <Tab label="Test" />
        <Tab label="Logs" />
        <Tab label="Usage" />
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
    </Box>
  )
}

// --- Info ---------------------------------------------------------------

type Backend = { index: string; type: string; quality: string; target: string; cmd: string; maxConcurrent: string }
type OvModel = { name: string; modality: string; capability: string; persistent: boolean; backends: Backend[] }
type ResModel = { name: string; modelName: string; state: string; hasUi: string; nCtx: string; nSlots: string }

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
        <Typography variant="subtitle2">Backends</Typography>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>#</TableCell>
              <TableCell>type</TableCell>
              <TableCell>quality</TableCell>
              <TableCell>target</TableCell>
              <TableCell>slots</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {model.backends.map((b) => (
              <TableRow key={b.index}>
                <TableCell>{b.index}</TableCell>
                <TableCell>{b.type}</TableCell>
                <TableCell>{b.quality}</TableCell>
                <TableCell>{b.target || '—'}</TableCell>
                <TableCell>{b.maxConcurrent}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {model.backends.find((b) => b.cmd) && (
          <Box component="pre" sx={preSx}>
            {model.backends.filter((b) => b.cmd).map((b) => b.cmd).join('\n\n')}
          </Box>
        )}
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

function LogsTab({ backend, ready }: { backend: string; ready: boolean }) {
  const q = useQuery({
    queryKey: ['consoleLogs', backend],
    queryFn: () => gqlClient.request(LogsDoc, { backend }),
    refetchInterval: 5000,
    enabled: ready,
  })
  const lines = q.data?.corrallm.modelLogs?.lines ?? []
  if (!ready) return <Typography color="text.secondary">Backend not loaded — no logs yet.</Typography>
  return (
    <Box component="pre" sx={{ ...preSx, maxHeight: 480 }}>
      {lines.length ? lines.join('\n') : 'No logs.'}
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
        <TextField size="small" sx={{ width: 160 }} placeholder="lane key (opt)" value={key} onChange={(e) => setKey(e.target.value)} />
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
// parakeet, which will refuse the upgrade.) ws can't set headers → default lane.
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
        <TextField size="small" sx={{ width: 160 }} placeholder="lane key (opt)" value={key} onChange={(e) => setKey(e.target.value)} />
      </Stack>
      {err && <Typography color="error" variant="body2">{err}</Typography>}
    </Stack>
  )
}

type Msg = { role: 'user' | 'assistant'; content: string; image?: string; file?: { name: string; data: string } }

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

function ChatPlayground({ model, replayId }: { model: string; replayId?: string }) {
  const [key, setKey] = useState('')
  const [input, setInput] = useState('')
  const [image, setImage] = useState<string | null>(null)
  const [file, setFile] = useState<{ name: string; data: string } | null>(null)
  const [msgs, setMsgs] = useState<Msg[]>([])
  const [busy, setBusy] = useState(false)
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
        const parsed: Msg[] = (body.messages ?? [])
          .map((m: { role: string; content: unknown }) => ({
            role: m.role === 'assistant' ? 'assistant' : 'user',
            content: typeof m.content === 'string' ? m.content : extractText(m.content),
          }))
          .filter((m: Msg) => m.role === 'user' || m.role === 'assistant')
        const lastUser = [...parsed].reverse().find((m) => m.role === 'user')
        if (lastUser) {
          const idx = parsed.lastIndexOf(lastUser)
          setMsgs(parsed.slice(0, idx))
          setInput(lastUser.content)
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

  async function send() {
    const text = input.trim()
    if ((!text && !image && !file) || busy) return
    setInput('')
    const userMsg: Msg = { role: 'user', content: text, image: image ?? undefined, file: file ?? undefined }
    setImage(null)
    setFile(null)
    const base: Msg[] = [...msgsRef.current, userMsg, { role: 'assistant', content: '' }]
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
          messages: base.slice(0, -1).map((m) => ({ role: m.role, content: apiContent(m) })),
        }),
      })
      if (!resp.ok || !resp.body) {
        appendToLast(`\n[error ${resp.status}] ${await resp.text()}`)
        return
      }
      const reader = resp.body.getReader()
      const dec = new TextDecoder()
      let buf = ''
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
            const delta = JSON.parse(data)?.choices?.[0]?.delta?.content
            if (delta) appendToLast(delta)
          } catch {
            /* ignore keepalive/non-json */
          }
        }
      }
    } catch (e) {
      appendToLast(`\n[error] ${String(e)}`)
    } finally {
      setBusy(false)
    }
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
      {/* column-reverse: newest pins to the bottom, no scroll management */}
      <Paper variant="outlined" sx={{ flex: 1, p: 1, display: 'flex', flexDirection: 'column-reverse', overflow: 'auto' }}>
        <Box>
          {msgs.map((m, i) => (
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
            </Box>
          ))}
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
          placeholder="lane key (opt)"
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
