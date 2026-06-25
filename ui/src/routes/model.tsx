import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useRef, useState } from 'react'
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
import { fmtDuration, fmtInt, fmtUSD } from '@/format'

// --- data ---------------------------------------------------------------

const ConsoleDoc = graphql(/* GraphQL */ `
  query ModelConsole {
    corrallm {
      overview {
        models {
          name
          modality
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
  const { name } = Route.useSearch()
  const navigate = useNavigate()
  const [tab, setTab] = useState(0)
  const ov = useQuery({ queryKey: ['console'], queryFn: () => gqlClient.request(ConsoleDoc), refetchInterval: 15000 })
  const caps = useCapabilities()

  const model = (ov.data?.corrallm.overview?.models ?? []).find((m) => m.name === name)
  const res = (ov.data?.corrallm.residency?.models ?? []).find((m) => m.modelName === name)
  const capability = capabilityOf(caps.data, name)

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
        <Chip size="small" color="info" variant="outlined" label={capability} />
        {model.modality === 'audio' && <Chip size="small" color="info" variant="outlined" label="audio" />}
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
        />
      )}
      {tab === 2 && <LogsTab backend={res?.name ?? `${name}#0`} ready={!!res} />}
      {tab === 3 && <UsageTab name={name} />}
    </Box>
  )
}

// --- Info ---------------------------------------------------------------

type Backend = { index: string; type: string; quality: string; target: string; cmd: string; maxConcurrent: string }
type OvModel = { name: string; modality: string; persistent: boolean; backends: Backend[] }
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

function TestTab({ capability, model, ttsModels }: { capability: string; model: string; ttsModels: string[] }) {
  if (capability === 'chat') return <ChatPlayground model={model} />
  if (capability === 'audio.stt') return <SttPlayground model={model} ttsModels={ttsModels} />
  if (capability === 'audio.tts') return <TtsPlayground model={model} />
  return (
    <Typography color="text.secondary">
      A {capability} playground is coming. For now see the Info tab's example requests.
    </Typography>
  )
}

// Voice: mic → STT (this model), then optionally speak the transcript back via a
// TTS model — a full browser voice loop over the Web Audio + MediaRecorder APIs.
function SttPlayground({ model, ttsModels }: { model: string; ttsModels: string[] }) {
  const [recording, setRecording] = useState(false)
  const [busy, setBusy] = useState(false)
  const [transcript, setTranscript] = useState('')
  const [err, setErr] = useState('')
  const [ttsModel, setTtsModel] = useState(ttsModels[0] ?? '')
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
        void transcribe(new Blob(chunksRef.current, { type: rec.mimeType }))
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

  async function transcribe(blob: Blob) {
    setBusy(true)
    setErr('')
    try {
      const fd = new FormData()
      fd.append('model', model)
      fd.append('file', blob, 'recording.webm')
      const headers: Record<string, string> = {}
      if (key) headers.Authorization = `Bearer ${key}`
      const r = await fetch('/v1/audio/transcriptions', { method: 'POST', headers, body: fd })
      if (!r.ok) {
        setErr(`${r.status}: ${await r.text()}`)
        return
      }
      const j = await r.json()
      setTranscript(String(j.text ?? JSON.stringify(j)))
    } catch (e) {
      setErr(String(e))
    } finally {
      setBusy(false)
    }
  }

  async function speak() {
    if (!ttsModel || !transcript) return
    setErr('')
    try {
      const headers: Record<string, string> = { 'Content-Type': 'application/json' }
      if (key) headers.Authorization = `Bearer ${key}`
      const r = await fetch('/v1/audio/speech', {
        method: 'POST',
        headers,
        body: JSON.stringify({ model: ttsModel, input: transcript, voice: 'af_heart', response_format: 'mp3' }),
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
        {busy && <CircularProgress size={20} />}
        <TextField size="small" sx={{ width: 160 }} placeholder="lane key (opt)" value={key} onChange={(e) => setKey(e.target.value)} />
      </Stack>
      <Box>
        <Typography variant="subtitle2">Transcript</Typography>
        <Box component="pre" sx={preSx}>{transcript || (busy ? 'transcribing…' : '—')}</Box>
      </Box>
      {ttsModels.length > 0 && (
        <Stack direction="row" spacing={1} alignItems="center">
          <Button variant="outlined" onClick={() => void speak()} disabled={!transcript}>
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
        </Stack>
      )}
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

type Msg = { role: 'user' | 'assistant'; content: string }

function ChatPlayground({ model }: { model: string }) {
  const [key, setKey] = useState('')
  const [input, setInput] = useState('')
  const [msgs, setMsgs] = useState<Msg[]>([])
  const [busy, setBusy] = useState(false)
  const msgsRef = useRef<Msg[]>([])
  msgsRef.current = msgs

  async function send() {
    const text = input.trim()
    if (!text || busy) return
    setInput('')
    const base: Msg[] = [...msgsRef.current, { role: 'user', content: text }, { role: 'assistant', content: '' }]
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
          messages: base.slice(0, -1).map((m) => ({ role: m.role, content: m.content })),
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
              <Typography variant="body2" sx={{ whiteSpace: 'pre-wrap' }}>
                {m.content || (busy && i === msgs.length - 1 ? '…' : '')}
              </Typography>
            </Box>
          ))}
        </Box>
      </Paper>
      <Stack direction="row" spacing={1}>
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
  validateSearch: (s: Record<string, unknown>): { name: string; tab?: string } => ({
    name: String(s.name ?? ''),
    tab: s.tab ? String(s.tab) : undefined,
  }),
  component: ModelConsole,
})
