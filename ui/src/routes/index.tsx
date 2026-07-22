import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Card,
  CardContent,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Link as MuiLink,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { capLabel, fmtBytes, fmtInt } from '@/format'

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
          evictCost
          spawnable
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
            evictCost
          }
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
        models {
          name
          modelName
          state
          refs
          nCtx
          nSlots
          hasUi
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
  busy: boolean
  onLoad: () => void
  onUnload: () => void
}) {
  const { state, persistent, busy, onLoad, onUnload } = props
  const inFlight = state === 'loading' || state === 'evicting'
  const resident = state === 'ready'

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
  const needsProbe = (name: string) => {
    const p = planByModel.get(name)
    if (!p) return false
    return !p.hasTuneProfile || !!p.unverifiedModalities?.length || !!p.disagreements?.length
  }

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
  const busy = load.isPending || unload.isPending

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

  const c = q.data!.corrallm
  const ov = c.overview
  const models = ov?.models ?? []
  const lanes = ov?.lanes ?? []
  const groups = ov?.groups ?? []
  const keys = ov?.keys ?? []
  const stateByModel = new Map((c.residency?.models ?? []).map((m) => [m.modelName, m]))

  // A group's effective policy for a capability = its onSaturated stage for that
  // model type, falling back to its `default` stage. Distinct values joined so
  // e.g. "queue/reject" when batch queues but realtime has no stage.
  const policyForTypes = (g: (typeof groups)[number], types: string[]) => {
    const def = g.stages.find((s) => s.type === 'default')?.policy ?? 'reject'
    const pols = types.map((t) => g.stages.find((s) => s.type === t)?.policy ?? def)
    return Array.from(new Set(pols)).join('/')
  }

  const groupStrip = (types: string[]) =>
    groups.length ? (
      <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap sx={{ mb: 1.5 }}>
        <Typography variant="caption" color="text.secondary" sx={{ alignSelf: 'center', mr: 0.5 }}>
          groups
        </Typography>
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
      </Stack>
    ) : null

  const modelCard = (m: (typeof models)[number]) => {
    const st = stateByModel.get(m.name)
    return (
      <Card key={m.name}>
        <CardContent>
          <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
            <Typography variant="subtitle1">{m.name}</Typography>
            {/* A pure-proxy backend (not spawnable — no cmd) has no local
                process, so it never has a residency state. Label it "proxy"
                (colored) rather than "absent", which reads as a failed local load. */}
            <Chip
              size="small"
              label={st?.state ?? (m.spawnable ? 'absent' : 'proxy')}
              color={!st?.state && !m.spawnable ? 'secondary' : stateColor(st?.state)}
            />
            <Chip size="small" color="info" variant="outlined" label={capLabel(m.capability)} />
            {m.persistent && <Chip size="small" variant="outlined" label="pinned" />}
            {m.ttl && <Chip size="small" variant="outlined" label={`ttl ${m.ttl}`} />}
            {st && Number(st.nCtx) > 0 && <Chip size="small" variant="outlined" label={`ctx ${fmtInt(st.nCtx)}`} />}
            {st && Number(st.nSlots) > 0 && <Chip size="small" variant="outlined" label={`slots ${fmtInt(st.nSlots)}`} />}
            <Box sx={{ flexGrow: 1 }} />
            {m.spawnable && (
              <>
                {/* ONE state-driven action, not two always-on buttons. Load and
                    Unload were both clickable regardless of residency, so half
                    of every pair was a no-op you could not tell apart from a
                    working one. While loading it becomes Cancel — the only
                    useful action mid-spawn. */}
                <ResidencyToggle
                  state={st?.state ?? 'absent'}
                  persistent={!!m.persistent}
                  busy={busy}
                  onLoad={() => load.mutate(m.name)}
                  onUnload={() => unload.mutate(m.name)}
                />
                {needsProbe(m.name) && (
                  <Tooltip title="This model has never been measured or verified">
                    <Button
                      size="small"
                      variant="outlined"
                      color="info"
                      disabled={busy}
                      onClick={() => setProbeFor(m.name)}
                    >
                      Probe
                    </Button>
                  </Tooltip>
                )}
                {st?.hasUi === 'no' ? (
                  <Tooltip title="This backend serves no web UI">
                    <span>
                      <Button size="small" disabled>
                        Open UI
                      </Button>
                    </span>
                  </Tooltip>
                ) : (
                  <Button
                    size="small"
                    component={MuiLink}
                    href={`/upstream/${encodeURIComponent(m.name)}/`}
                    target="_blank"
                    rel="noreferrer"
                  >
                    Open UI
                  </Button>
                )}
                {/* Logs live in the CONSOLE, not a second dialog here. Two
                    half-views of one model is how a detail page and a popup
                    drift apart; deep-link into the console instead. */}
                <Button
                  size="small"
                  disabled={!st}
                  onClick={() => navigate({ to: '/model', search: { name: m.name, tab: 'logs' } })}
                >
                  Logs
                </Button>
                <Button size="small" onClick={() => navigate({ to: '/model', search: { name: m.name } })}>
                  Console
                </Button>
              </>
            )}
          </Box>
          <TableContainer sx={{ mt: 1 }}>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Type</TableCell>
                  <TableCell align="right">Quality</TableCell>
                  <TableCell align="right">Slots</TableCell>
                  <TableCell align="right">Max tokens</TableCell>
                  <TableCell>cmd / target</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                <TableRow>
                  <TableCell>
                    <Chip size="small" variant="outlined" label={m.spawnable ? m.type : `${m.type} (proxy)`} />
                  </TableCell>
                  <TableCell align="right">{m.quality}</TableCell>
                  <TableCell align="right">{fmtInt(m.maxConcurrent)}</TableCell>
                  <TableCell align="right">{Number(m.maxTokens) > 0 ? fmtInt(m.maxTokens) : '—'}</TableCell>
                  <TableCell>
                    {m.cmd ? (
                      <Button size="small" onClick={() => setCmdView({ title: m.name, cmd: m.cmd })}>
                        View cmd
                      </Button>
                    ) : (
                      <Typography variant="caption" sx={{ wordBreak: 'break-all' }}>
                        {m.target || '—'}
                      </Typography>
                    )}
                  </TableCell>
                </TableRow>
              </TableBody>
            </Table>
          </TableContainer>
        </CardContent>
      </Card>
    )
  }

  // Assign each model to its capability section; collect leftovers into "Other".
  const seen = new Set<string>()
  const sections = CAP_SECTIONS.map((s) => {
    const ms = models.filter((m) => s.caps.includes(m.capability))
    ms.forEach((m) => seen.add(m.name))
    return { ...s, models: ms }
  }).filter((s) => s.models.length)
  const other = models.filter((m) => !seen.has(m.name))
  if (other.length) sections.push({ title: 'Other', blurb: '', caps: [], groupTypes: [], models: other })

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <Box sx={{ display: 'flex', gap: 2, alignItems: 'center' }}>
        <Typography variant="h6">Overview</Typography>
        <Chip size="small" color="success" label={`${c.health?.status} · ${c.health?.version}`} />
      </Box>

      {msg && (
        <Alert severity={msg.ok ? 'success' : 'error'} onClose={() => setMsg(null)}>
          {msg.text}
        </Alert>
      )}

      {/* A probe is DESTRUCTIVE: it evicts models and locks out other callers.
          Say so before the click, name exactly what this run will learn, and
          state that the lease self-expires — "will this wedge my server" is the
          first thing anyone sane wants to know. */}
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
                  <b>Cold/warm disagreement</b> — this modality worked in one residency state
                  and failed in the other. Re-running confirms whether it persists.
                </li>
              )}
            </ul>
            <Alert severity="warning" sx={{ mt: 1 }}>
              While it runs, models are <b>evicted</b> so measurements are uncontended, and
              every other caller receives <b>429 + Retry-After</b>. Clients that honor
              Retry-After pause and resume rather than fail. The lease self-expires, so a
              crashed run cannot lock the server permanently.
            </Alert>
          </DialogContent>
          <DialogActions>
            <Button onClick={() => setProbeFor(null)}>Cancel</Button>
            <Button
              variant="contained"
              color="warning"
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
                bgcolor: 'grey.900',
                color: 'grey.100',
                borderRadius: 1,
              }}
            >
              {cmdView.cmd}
            </Box>
          </DialogContent>
        </Dialog>
      )}

      {/* Capability sections: groups (filtered to this capability) over its models. */}
      {sections.map((s) => (
        <Box key={s.title}>
          <Typography variant="subtitle1">{s.title}</Typography>
          {s.blurb && (
            <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 1 }}>
              {s.blurb}
            </Typography>
          )}
          {groupStrip(s.groupTypes)}
          <Stack spacing={2}>{s.models.map(modelCard)}</Stack>
        </Box>
      ))}

      {/* Lanes: named ordered fallback lists over models. */}
      {lanes.length > 0 && (
        <Box>
          <Typography variant="subtitle1" gutterBottom>
            Lanes
          </Typography>
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 1 }}>
            Requestable as a model id; falls back across members in order.
          </Typography>
          <Stack spacing={1}>
            {lanes.map((l) => (
              <Card key={l.name}>
                <CardContent sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap', '&:last-child': { pb: 2 } }}>
                  <Typography variant="subtitle2" sx={{ mr: 1 }}>
                    {l.name}
                  </Typography>
                  {l.members.map((mem, i) => (
                    <Box key={mem.model} sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
                      {i > 0 && (
                        <Typography variant="body2" color="text.secondary">
                          →
                        </Typography>
                      )}
                      <Tooltip
                        title={[mem.ttl ? `ttl ${mem.ttl}` : null, mem.evictCost ? `evict ${mem.evictCost}` : null]
                          .filter(Boolean)
                          .join(' · ')}
                      >
                        <Chip size="small" variant="outlined" label={mem.model} />
                      </Tooltip>
                    </Box>
                  ))}
                </CardContent>
              </Card>
            ))}
          </Stack>
        </Box>
      )}

      {/* System capacity (orthogonal to capability). */}
      <Box>
        <Typography variant="subtitle1" gutterBottom>
          System Capacity
        </Typography>
        <Stack direction="row" spacing={2} flexWrap="wrap" useFlexGap>
          {(ov?.servers ?? []).map((s) => (
            <Card key={s.server} sx={{ minWidth: 260 }}>
              <CardContent>
                <Typography variant="subtitle2">
                  {s.server}
                  {Number(s.maxConcurrent) > 0 ? ` · max ${s.maxConcurrent}` : ''}
                </Typography>
                {s.pools.map((p) => (
                  <Typography key={p.pool} variant="body2" color="text.secondary">
                    {p.pool}: {fmtBytes(p.totalBytes)}
                    {Number(p.reserveBytes) > 0 ? ` (reserve ${fmtBytes(p.reserveBytes)})` : ''}
                  </Typography>
                ))}
              </CardContent>
            </Card>
          ))}
        </Stack>
      </Box>
    </Box>
  )
}

export const Route = createFileRoute('/')({ component: Home })
