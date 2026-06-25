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
          modality
          capability
          backends {
            index
            type
            quality
            spawnable
            server
            target
            maxConcurrent
            maxTokens
            cmd
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

const LogsDoc = graphql(/* GraphQL */ `
  query ModelLogs($backend: String!) {
    corrallm {
      modelLogs(backend: $backend) {
        backend
        lines
      }
    }
  }
`)

function LogsDialog({ backend, onClose }: { backend: string; onClose: () => void }) {
  const q = useQuery({
    queryKey: ['logs', backend],
    queryFn: () => gqlClient.request(LogsDoc, { backend }),
    refetchInterval: 2000,
  })
  const lines = q.data?.corrallm.modelLogs?.lines ?? []
  return (
    <Dialog open onClose={onClose} maxWidth="lg" fullWidth>
      <DialogTitle>Logs · {backend}</DialogTitle>
      <DialogContent dividers>
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
      </DialogContent>
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

function Home() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)
  const [logsFor, setLogsFor] = useState<string | null>(null)
  const [cmdView, setCmdView] = useState<{ title: string; cmd: string } | null>(null)

  const q = useQuery({
    queryKey: ['overview'],
    queryFn: () => gqlClient.request(OverviewDoc),
    refetchInterval: 15000, // fallback; live via SSE (useLiveEvents)
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
  const stateByModel = new Map((c.residency?.models ?? []).map((m) => [m.modelName, m]))

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

      {logsFor && <LogsDialog backend={logsFor} onClose={() => setLogsFor(null)} />}

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

      {/* Models + lane definitions, with load/unload + open-UI for spawnable backends. */}
      <Box>
        <Typography variant="subtitle1" gutterBottom>
          Models
        </Typography>
        <Stack spacing={2}>
          {(ov?.models ?? []).map((m) => {
            const st = stateByModel.get(m.name)
            return (
              <Card key={m.name}>
                <CardContent>
                  <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                    <Typography variant="subtitle1">{m.name}</Typography>
                    <Chip size="small" label={st?.state ?? 'absent'} color={stateColor(st?.state)} />
                    <Chip size="small" color="info" variant="outlined" label={capLabel(m.capability)} />
                    {m.persistent && <Chip size="small" variant="outlined" label="pinned" />}
                    {m.ttl && <Chip size="small" variant="outlined" label={`ttl ${m.ttl}`} />}
                    {st && Number(st.nCtx) > 0 && (
                      <Chip size="small" variant="outlined" label={`ctx ${fmtInt(st.nCtx)}`} />
                    )}
                    {st && Number(st.nSlots) > 0 && (
                      <Chip size="small" variant="outlined" label={`slots ${fmtInt(st.nSlots)}`} />
                    )}
                    <Box sx={{ flexGrow: 1 }} />
                    {m.spawnable && (
                      <>
                        <Button
                          size="small"
                          variant="outlined"
                          disabled={busy}
                          onClick={() => load.mutate(m.name)}
                        >
                          Load
                        </Button>
                        <Tooltip title={m.persistent ? 'pinned models cannot be unloaded' : ''}>
                          <span>
                            <Button
                              size="small"
                              variant="outlined"
                              color="warning"
                              disabled={busy || m.persistent}
                              onClick={() => unload.mutate(m.name)}
                            >
                              Unload
                            </Button>
                          </span>
                        </Tooltip>
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
                        <Button
                          size="small"
                          disabled={!st}
                          onClick={() => st && setLogsFor(st.name)}
                        >
                          Logs
                        </Button>
                        <Button
                          size="small"
                          onClick={() => navigate({ to: '/model', search: { name: m.name } })}
                        >
                          Console
                        </Button>
                      </>
                    )}
                  </Box>
                  <TableContainer sx={{ mt: 1 }}>
                    <Table size="small">
                      <TableHead>
                        <TableRow>
                          <TableCell>#</TableCell>
                          <TableCell>Type</TableCell>
                          <TableCell align="right">Quality</TableCell>
                          <TableCell align="right">Slots</TableCell>
                          <TableCell align="right">Max tokens</TableCell>
                          <TableCell>cmd / target</TableCell>
                        </TableRow>
                      </TableHead>
                      <TableBody>
                        {m.backends.map((b) => (
                          <TableRow key={b.index}>
                            <TableCell>{b.index}</TableCell>
                            <TableCell>
                              <Chip
                                size="small"
                                variant="outlined"
                                label={b.spawnable ? b.type : `${b.type} (proxy)`}
                              />
                            </TableCell>
                            <TableCell align="right">{b.quality}</TableCell>
                            <TableCell align="right">{fmtInt(b.maxConcurrent)}</TableCell>
                            <TableCell align="right">{Number(b.maxTokens) > 0 ? fmtInt(b.maxTokens) : '—'}</TableCell>
                            <TableCell>
                              {b.cmd ? (
                                <Button size="small" onClick={() => setCmdView({ title: `${m.name} #${b.index}`, cmd: b.cmd })}>
                                  View cmd
                                </Button>
                              ) : (
                                <Typography variant="caption" sx={{ wordBreak: 'break-all' }}>
                                  {b.target || '—'}
                                </Typography>
                              )}
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </TableContainer>
                </CardContent>
              </Card>
            )
          })}
        </Stack>
      </Box>

      {/* System capacity. */}
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

      {/* Lanes / priority groups. */}
      <Box>
        <Typography variant="subtitle1" gutterBottom>
          Lanes
        </Typography>
        <TableContainer component={Card}>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Group</TableCell>
                <TableCell align="right">Weight</TableCell>
                <TableCell>Currency</TableCell>
                <TableCell>Interruptible</TableCell>
                <TableCell>Degrade</TableCell>
                <TableCell>onSaturated</TableCell>
                <TableCell>Keys</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {(ov?.groups ?? []).map((g) => (
                <TableRow key={g.name}>
                  <TableCell>{g.name}</TableCell>
                  <TableCell align="right">{fmtInt(g.weight)}</TableCell>
                  <TableCell>{g.shareCurrency}</TableCell>
                  <TableCell>{g.interruptible ? 'yes' : '—'}</TableCell>
                  <TableCell>
                    {g.acceptDegrade ? `≥ ${g.qualityFloor}` : 'top only'}
                  </TableCell>
                  <TableCell>
                    {g.stages.map((s) => `${s.type}: ${s.policy}`).join('; ') || '—'}
                  </TableCell>
                  <TableCell>
                    {(ov?.keys ?? [])
                      .filter((k) => k.group === g.name)
                      .map((k) => k.key)
                      .join(', ') || '—'}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      </Box>
    </Box>
  )
}

export const Route = createFileRoute('/')({ component: Home })
