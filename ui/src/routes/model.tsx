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
      {tab === 1 && <TestTab capability={capability} model={name} />}
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

function TestTab({ capability, model }: { capability: string; model: string }) {
  if (capability === 'chat') return <ChatPlayground model={model} />
  return (
    <Typography color="text.secondary">
      A {capability} playground is coming. For now see the Info tab's example requests.
    </Typography>
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
