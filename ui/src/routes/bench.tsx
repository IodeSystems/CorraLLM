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
        args
        log
        done
        error
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
        <Alert severity="warning">
          <AlertTitle>Bench running — exclusive mode</AlertTitle>
          Models are being evicted and every other caller is receiving 429 + Retry-After.
        </Alert>
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

      {(status?.log?.length ?? 0) > 0 && (
        <Paper sx={{ p: 2, maxHeight: 320, overflow: 'auto' }}>
          <Typography variant="subtitle2">Run output</Typography>
          <Box component="pre" sx={{ m: 0, fontSize: 12 }}>
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
