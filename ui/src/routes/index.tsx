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
  Link as MuiLink,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { ActiveRequests } from '@/ActiveRequests'
import { MemoryPanel } from '@/MemoryPanel'
import { Panel, PageHeader, Row, Stat } from '@/Panel'
import { C, seriesColor } from '@/theme'
import { capLabel, fmtInt } from '@/format'

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
          evictCost
          spawnable
          remote
          procKey
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
        servers {
          server
          pools {
            pool
            budget
            used
          }
        }
        gpu {
          available
          name
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

  // One model = one ROW inside its panel, not a card of its own. A page of
  // twenty cards, each with its own border and its own five-column table for
  // four numbers, is the "everything runs together" problem: every model looks
  // structurally identical to its section heading. Here the panel is the object
  // and the model is a line in it.
  const modelRow = (m: (typeof models)[number]) => {
    const st = stateOf(m)
    return (
      <Row key={m.name}>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
          {/* State first and always with its word — the colored dot alone would
              encode state in color only. A remote backend has no local process
              and so no residency at all: it reads "proxy" permanently, never
              "absent" (which looks like a failed load) and never "ready" (which
              claimed a load that never happened). */}
          <Chip
            size="small"
            label={m.remote ? 'proxy' : (st?.state ?? 'absent')}
            color={m.remote ? 'secondary' : stateColor(st?.state)}
            variant={st?.state ? 'filled' : 'outlined'}
            sx={{ minWidth: 68 }}
          />
          <Typography variant="subtitle2" sx={{ minWidth: 150 }}>
            {m.name}
          </Typography>
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
        {/* Four numbers do not need a table — a table costs a header row, a
            border box, and a scroll container to say "quality 100". */}
        <Box sx={{ display: 'flex', gap: 3, mt: 1, alignItems: 'flex-end', flexWrap: 'wrap' }}>
          <Stat label="Type" value={m.spawnable ? m.type : `${m.type} · proxy`} />
          <Stat label="Quality" value={m.quality} />
          <Stat label="Slots" value={fmtInt(m.maxConcurrent)} />
          <Stat
            label="Max tokens"
            value={Number(m.maxTokens) > 0 ? fmtInt(m.maxTokens) : '—'}
            title="max_tokens clamp applied when a request degrades onto this model"
          />
          <Box sx={{ minWidth: 0 }}>
            {m.cmd ? (
              <Button size="small" onClick={() => setCmdView({ title: m.name, cmd: m.cmd })}>
                View cmd
              </Button>
            ) : (
              <Typography variant="caption" sx={{ wordBreak: 'break-all', color: C.textMuted }}>
                {m.target || '—'}
              </Typography>
            )}
          </Box>
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
        gpu={dev(res?.gpu)}
        host={dev(res?.host)}
        colorOf={colorOf}
      />

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

      {/* Lanes: named ordered fallback lists over models. */}
      {lanes.length > 0 && (
        <Panel
          title="Lanes"
          subtitle="Requestable as a model id; falls back across members in order"
          flush
        >
          {lanes.map((l) => (
            <Row key={l.name}>
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                <Typography variant="subtitle2" sx={{ minWidth: 150 }}>
                  {l.name}
                </Typography>
                {l.members.map((mem, i) => (
                  <Box key={mem.model} sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
                    {i > 0 && (
                      <Typography variant="body2" sx={{ color: C.textFaint }}>
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
              </Box>
            </Row>
          ))}
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
