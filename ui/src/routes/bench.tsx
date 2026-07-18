import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import {
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
  DialogContentText,
  DialogTitle,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { BarChart, ScatterChart } from '@mui/x-charts'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtInt } from '@/format'

/**
 * Bench is the AGGREGATE view: how do the models compare?
 *
 * Per-model actions (probe this one, load/unload it) live on the model card and
 * the model console, where the model already is. This page exists for the
 * question you cannot answer from one card — which model is worth its VRAM.
 */
const BenchDoc = graphql(/* GraphQL */ `
  query BenchAggregate {
    corrallm {
      benchResults {
        results {
          runId
          model
          at
          score
          stages
          stagesPassed
          classes
          tokensProcessed
          tokensGenerated
          cachedTokens
          wallMs
          tokPerSec
          footprintMiB
        }
      }
      benchPlan {
        gpu
        newModels
        models {
          model
          new
          hasTuneProfile
          unverifiedModalities
          disagreements {
            modality
            runMode
          }
          probes {
            kind
            default
          }
        }
      }
      benchStatus {
        running
        startedAt
        args
        log
        done
        error
      }
      calibrationStatus {
        active
        reason
        remainingSeconds
      }
    }
  }
`)

const StartBenchDoc = graphql(/* GraphQL */ `
  mutation StartAggregateBench($body: corrallm_BenchRunInputBodyInput!) {
    corrallm {
      startBenchRun(body: $body) {
        ok
        message
        warning
      }
    }
  }
`)

const CancelBenchDoc = graphql(/* GraphQL */ `
  mutation CancelAggregateBench {
    corrallm {
      cancelBenchRun {
        running
        done
      }
    }
  }
`)

// Codegen maps GraphQL Long (int64) to string for a uniform id contract, so
// every count arrives as text. Coerce once at the boundary rather than sprinkling
// Number() through the chart props.
const n = (v: unknown): number => Number(v ?? 0) || 0


/** Seconds -> "3m 12s", so an elapsed clock reads at a glance. */
function elapsed(sinceUnix: number): string {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - sinceUnix))
  const m = Math.floor(s / 60)
  return m > 0 ? `${m}m ${s % 60}s` : `${s}s`
}

/**
 * The live view of a run in flight — the first thing on the page while anything
 * is running, because a bench takes minutes and evicts models while it does.
 *
 * It shows a run started ANYWHERE: a probe fired from a model card and an
 * aggregate run started here are the same single runner, so this is the one
 * place to watch either. Without that, clicking Probe on the Overview left you
 * with no way to see what it was doing.
 */
function ActiveRun(props: {
  args: readonly string[]
  log: readonly string[]
  startedAt: number
  leaseReason?: string | null
  leaseRemaining: number
  onCancel: () => void
}) {
  const { args, log, startedAt, leaseReason, leaseRemaining, onCancel } = props
  // Tail, not head: the interesting line during a run is the newest one.
  const tail = log.slice(-200)
  return (
    <Paper sx={{ p: 2, borderLeft: 4, borderColor: 'warning.main' }}>
      <Stack direction="row" spacing={2} alignItems="center" sx={{ mb: 1 }}>
        <CircularProgress size={20} />
        <Typography variant="subtitle1">Bench running</Typography>
        {startedAt > 0 && <Chip size="small" label={`elapsed ${elapsed(startedAt)}`} />}
        {leaseRemaining > 0 && (
          <Chip
            size="small"
            color="warning"
            label={`lease expires in ~${Math.ceil(leaseRemaining / 60)}m`}
            title="The lease self-expires, so a crashed run cannot lock the server permanently."
          />
        )}
        {leaseReason && <Chip size="small" variant="outlined" label={leaseReason} />}
        <Box sx={{ flexGrow: 1 }} />
        <Button size="small" variant="outlined" color="error" onClick={onCancel}>
          Cancel run
        </Button>
      </Stack>
      <Typography variant="caption" color="text.secondary">
        Models are being evicted for uncontended measurement; every other caller is receiving 429
        + Retry-After.
      </Typography>
      {args.length > 0 && (
        <Box component="pre" sx={{ m: 0, mt: 1, fontSize: 11, opacity: 0.75, overflowX: 'auto' }}>
          $ {args.join(' ')}
        </Box>
      )}
      <Box
        component="pre"
        sx={{
          m: 0,
          mt: 1,
          p: 1,
          fontSize: 12,
          lineHeight: 1.4,
          maxHeight: 320,
          overflow: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
          bgcolor: 'grey.900',
          color: 'grey.100',
          borderRadius: 1,
        }}
      >
        {tail.length ? tail.join('\n') : 'waiting for output…'}
      </Box>
    </Paper>
  )
}

const KINDS = ['measure', 'capability', 'quality'] as const
type Kind = (typeof KINDS)[number]

function BenchPage() {
  const qc = useQueryClient()
  const [selected, setSelected] = useState<Record<string, Set<Kind>> | null>(null)
  const [confirming, setConfirming] = useState(false)

  const { data, isLoading } = useQuery({
    queryKey: ['benchAggregate'],
    queryFn: () => gqlClient.request(BenchDoc),
    refetchInterval: (q) => (q.state.data?.corrallm?.benchStatus?.running ? 2000 : false),
  })

  const results = data?.corrallm?.benchResults?.results ?? []
  const plan = data?.corrallm?.benchPlan
  const status = data?.corrallm?.benchStatus
  const lease = data?.corrallm?.calibrationStatus
  const running = !!status?.running

  const checks = useMemo(() => {
    if (selected) return selected
    const init: Record<string, Set<Kind>> = {}
    for (const m of plan?.models ?? []) {
      const on = new Set<Kind>()
      for (const p of m.probes ?? []) if (p.default) on.add(p.kind as Kind)
      init[m.model] = on
    }
    return init
  }, [plan, selected])

  const toggle = (model: string, kind: Kind) => {
    const next: Record<string, Set<Kind>> = {}
    for (const [k, v] of Object.entries(checks)) next[k] = new Set(v)
    if (!next[model]) next[model] = new Set()
    if (next[model].has(kind)) next[model].delete(kind)
    else next[model].add(kind)
    setSelected(next)
  }

  const request = useMemo(() => {
    const models: string[] = []
    const classes = new Set<string>()
    for (const [model, kinds] of Object.entries(checks)) {
      if (kinds.size === 0) continue
      models.push(model)
      if (kinds.has('capability')) classes.add('capability')
      if (kinds.has('quality')) {
        classes.add('coding')
        classes.add('tooluse')
        classes.add('adversarial')
      }
    }
    return { models, classes: [...classes] }
  }, [checks])

  const start = useMutation({
    mutationFn: () =>
      gqlClient.request(StartBenchDoc, {
        body: { models: request.models, classes: request.classes, reason: 'aggregate bench' },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['benchAggregate'] }),
  })
  const cancel = useMutation({
    mutationFn: () => gqlClient.request(CancelBenchDoc),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['benchAggregate'] }),
  })

  if (isLoading)
    return (
      <Box sx={{ p: 4 }}>
        <CircularProgress />
      </Box>
    )

  const ranked = [...results].sort((a, b) => n(b.score) - n(a.score))
  const labels = ranked.map((r) => r.model)
  const nothingSelected = request.models.length === 0

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <Box>
        <Typography variant="h5">Bench</Typography>
        <Typography variant="body2" color="text.secondary">
          Cross-model comparison. GPU: {plan?.gpu || 'unknown'}
        </Typography>
      </Box>

      {running && (
        <ActiveRun
          args={status?.args ?? []}
          log={status?.log ?? []}
          startedAt={n(status?.startedAt)}
          leaseReason={lease?.reason}
          leaseRemaining={n(lease?.remainingSeconds)}
          onCancel={() => cancel.mutate()}
        />
      )}

      {results.length === 0 ? (
        <Alert severity="info">
          <AlertTitle>No benchmark results yet</AlertTitle>
          Nothing has been benched on this box. Select probes below and run one — or probe a
          single model from its card on the Overview page.
        </Alert>
      ) : (
        <>
          <Paper sx={{ p: 2 }}>
            <Typography variant="subtitle1" gutterBottom>
              Score — stage pass rate
            </Typography>
            <BarChart
              height={260}
              xAxis={[{ scaleType: 'band', data: labels }]}
              yAxis={[{ min: 0, max: 1 }]}
              series={[{ data: ranked.map((r) => Number(n(r.score).toFixed(3))), label: 'pass rate' }]}
            />
          </Paper>

          <Paper sx={{ p: 2 }}>
            <Typography variant="subtitle1">Score vs resident VRAM</Typography>
            <Typography variant="caption" color="text.secondary">
              The tradeoff that actually decides what to run: quality against what it costs to
              keep resident. Up and to the left is better.
            </Typography>
            <ScatterChart
              height={300}
              xAxis={[{ label: 'resident MiB' }]}
              yAxis={[{ label: 'pass rate', min: 0, max: 1 }]}
              series={ranked
                .filter((r) => n(r.footprintMiB) > 0)
                .map((r) => ({
                  label: r.model,
                  data: [{ x: n(r.footprintMiB), y: Number(n(r.score).toFixed(3)), id: r.model }],
                }))}
            />
          </Paper>

          <Paper sx={{ p: 2 }}>
            <Typography variant="subtitle1">Tokens — processed vs generated</Typography>
            <Typography variant="caption" color="text.secondary">
              Cache hits are excluded from &ldquo;processed&rdquo;: a model that ran second over the
              same fixtures gets cheap prompt tokens through no merit of its own, so counting
              them would reward the running order rather than the model.
            </Typography>
            <BarChart
              height={280}
              xAxis={[{ scaleType: 'band', data: labels }]}
              series={[
                { data: ranked.map((r) => n(r.tokensProcessed)), label: 'processed (uncached)', stack: 'a' },
                { data: ranked.map((r) => n(r.tokensGenerated)), label: 'generated', stack: 'a' },
              ]}
            />
          </Paper>

          <TableContainer component={Paper}>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Model</TableCell>
                  <TableCell align="right">Score</TableCell>
                  <TableCell align="right">Stages</TableCell>
                  <TableCell align="right">Processed</TableCell>
                  <TableCell align="right">Generated</TableCell>
                  <TableCell align="right">Cached</TableCell>
                  <TableCell align="right">tok/s</TableCell>
                  <TableCell align="right">VRAM MiB</TableCell>
                  <TableCell>Classes</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {ranked.map((r) => (
                  <TableRow key={r.model} hover>
                    <TableCell>{r.model}</TableCell>
                    <TableCell align="right">{(n(r.score) * 100).toFixed(0)}%</TableCell>
                    <TableCell align="right">
                      {r.stagesPassed}/{r.stages}
                    </TableCell>
                    <TableCell align="right">{fmtInt(n(r.tokensProcessed))}</TableCell>
                    <TableCell align="right">{fmtInt(n(r.tokensGenerated))}</TableCell>
                    <TableCell align="right">{fmtInt(n(r.cachedTokens))}</TableCell>
                    <TableCell align="right">{n(r.tokPerSec).toFixed(0)}</TableCell>
                    <TableCell align="right">{fmtInt(n(r.footprintMiB))}</TableCell>
                    <TableCell>
                      <Typography variant="caption">{r.classes || '—'}</Typography>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableContainer>
        </>
      )}

      <Paper sx={{ p: 2 }}>
        <Typography variant="subtitle1" gutterBottom>
          Run a benchmark
        </Typography>
        {!!plan?.newModels && (
          <Alert severity="info" sx={{ mb: 2 }}>
            {plan.newModels} model(s) have never been benched — corrallm is scheduling them on
            unverified <code>ramUsage</code>.
          </Alert>
        )}
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Model</TableCell>
                {KINDS.map((k) => (
                  <TableCell key={k} align="center">
                    {k}
                  </TableCell>
                ))}
              </TableRow>
            </TableHead>
            <TableBody>
              {(plan?.models ?? []).map((m) => {
                const offered = new Set((m.probes ?? []).map((p) => p.kind))
                return (
                  <TableRow key={m.model} hover>
                    <TableCell>
                      {m.model}{' '}
                      {m.new && <Chip size="small" color="info" label="never benched" />}
                      {!!m.disagreements?.length && (
                        <Chip size="small" color="error" label="cold/warm disagreement" />
                      )}
                    </TableCell>
                    {KINDS.map((k) => (
                      <TableCell key={k} align="center">
                        {offered.has(k) ? (
                          <Checkbox
                            size="small"
                            disabled={running}
                            checked={checks[m.model]?.has(k) ?? false}
                            onChange={() => toggle(m.model, k)}
                          />
                        ) : (
                          <Typography variant="caption" color="text.disabled">
                            n/a
                          </Typography>
                        )}
                      </TableCell>
                    ))}
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </TableContainer>
        <Box sx={{ display: 'flex', gap: 2, mt: 2, alignItems: 'center' }}>
          <Button
            variant="contained"
            color="warning"
            disabled={running || nothingSelected}
            onClick={() => setConfirming(true)}
          >
            Run bench
          </Button>
          {running && (
            <Button variant="outlined" color="error" onClick={() => cancel.mutate()}>
              Cancel run
            </Button>
          )}
        </Box>
      </Paper>

      {/* Finished runs keep their output available; a running one is shown in
          the live panel at the top instead of twice on one page. */}
      {!running && (status?.log?.length ?? 0) > 0 && (
        <Paper sx={{ p: 2, maxHeight: 320, overflow: 'auto' }}>
          <Typography variant="subtitle2">Last run output</Typography>
          <Box component="pre" sx={{ m: 0, fontSize: 12, whiteSpace: 'pre-wrap' }}>
            {status?.log?.join('\n')}
          </Box>
        </Paper>
      )}
      {!!status?.error && <Alert severity="error">{status.error}</Alert>}

      <Dialog open={confirming} onClose={() => setConfirming(false)}>
        <DialogTitle>Run bench in exclusive mode?</DialogTitle>
        <DialogContent>
          <DialogContentText component="div">
            This will <b>evict resident models</b> so measurements are uncontended, and turn
            away every other caller with <b>429 + Retry-After</b> until it finishes. Clients
            that honor Retry-After pause and resume rather than fail.
            <br />
            <br />
            Models: <b>{request.models.join(', ') || 'none'}</b>
            <br />
            Classes: <b>{request.classes.join(', ') || 'measurement only'}</b>
            <br />
            <br />
            The lease self-expires, so a crashed run cannot lock the server permanently.
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirming(false)}>Cancel</Button>
          <Button
            color="warning"
            variant="contained"
            onClick={() => {
              setConfirming(false)
              start.mutate()
            }}
          >
            Evict and run
          </Button>
        </DialogActions>
      </Dialog>

      {start.data?.corrallm?.startBenchRun?.ok === false && (
        <Alert severity="error">{start.data.corrallm.startBenchRun.message}</Alert>
      )}
    </Box>
  )
}

export const Route = createFileRoute('/bench')({ component: BenchPage })
