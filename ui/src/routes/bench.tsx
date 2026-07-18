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
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'

const BenchPlanDoc = graphql(/* GraphQL */ `
  query BenchPlan {
    corrallm {
      benchPlan {
        gpu
        newModels
        models {
          model
          new
          hasTuneProfile
          hasCapabilityData
          declaredModalities
          unverifiedModalities
          disagreements {
            modality
            runMode
            verified
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
        startedAt
        args
        log
        done
        error
      }
    }
  }
`)

const StartBenchDoc = graphql(/* GraphQL */ `
  mutation StartBenchRun($body: corrallm_BenchRunInputBodyInput!) {
    corrallm {
      startBenchRun(body: $body) {
        ok
        message
        warning
        status {
          running
          args
        }
      }
    }
  }
`)

const CancelBenchDoc = graphql(/* GraphQL */ `
  mutation CancelBenchRun {
    corrallm {
      cancelBenchRun {
        running
        done
      }
    }
  }
`)

/** The probe kinds a run can select, in the order they are offered. */
const KINDS = ['measure', 'capability', 'quality'] as const
type Kind = (typeof KINDS)[number]

function BenchPage() {
  const qc = useQueryClient()
  const [selected, setSelected] = useState<Record<string, Set<Kind>> | null>(null)
  const [confirming, setConfirming] = useState(false)

  const { data, isLoading } = useQuery({
    queryKey: ['benchPlan'],
    queryFn: () => gqlClient.request(BenchPlanDoc),
    // Poll while a run is in flight so the log tails; the interval is set from
    // the previous response rather than a constant, so an idle page is quiet.
    refetchInterval: (q) =>
      q.state.data?.corrallm?.benchStatus?.running ? 2000 : false,
  })

  const plan = data?.corrallm?.benchPlan
  const status = data?.corrallm?.benchStatus
  const running = !!status?.running

  // Seed the checkboxes from the server's suggestion exactly once, then let the
  // user own them. Re-seeding on every poll would fight anyone mid-edit.
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

  // A probe KIND maps onto llm-bench's --classes; "measure" is not a probe
  // class but a by-product of any run, so it contributes no class of its own.
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
        body: { models: request.models, classes: request.classes, reason: 'dashboard bench run' },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['benchPlan'] }),
  })
  const cancel = useMutation({
    mutationFn: () => gqlClient.request(CancelBenchDoc),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['benchPlan'] }),
  })

  if (isLoading) return <Box sx={{ p: 4 }}><CircularProgress /></Box>

  const nothingSelected = request.models.length === 0

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Typography variant="h5">Bench</Typography>
      <Typography variant="body2" color="text.secondary">
        corrallm cannot learn a model&apos;s real VRAM footprint or verify its declared
        modalities on its own — both come from a bench run. GPU: {plan?.gpu || 'unknown'}
      </Typography>

      {!!plan?.newModels && !running && (
        <Alert severity="info">
          <AlertTitle>{plan.newModels} model(s) have never been benched</AlertTitle>
          Until they are, corrallm schedules them on their declared <code>ramUsage</code>
          (unverified) and advertises modalities nothing has exercised.
        </Alert>
      )}

      {running && (
        <Alert severity="warning">
          <AlertTitle>Bench running — exclusive mode</AlertTitle>
          Every caller except this run is receiving 429 + Retry-After, and models are
          being evicted to take cold measurements.
        </Alert>
      )}

      <TableContainer component={Paper}>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Model</TableCell>
              <TableCell>Coverage</TableCell>
              {KINDS.map((k) => (
                <TableCell key={k} align="center">{k}</TableCell>
              ))}
            </TableRow>
          </TableHead>
          <TableBody>
            {(plan?.models ?? []).map((m) => {
              const offered = new Set((m.probes ?? []).map((p) => p.kind))
              return (
                <TableRow key={m.model} hover>
                  <TableCell>
                    <Typography variant="body2">{m.model}</Typography>
                    {m.new && <Chip size="small" color="info" label="never benched" sx={{ mr: 0.5 }} />}
                    {!!m.disagreements?.length && (
                      <Chip
                        size="small"
                        color="error"
                        label="cold/warm disagreement"
                        title="A modality verified in one residency state and failed in the other."
                      />
                    )}
                  </TableCell>
                  <TableCell>
                    <Typography variant="caption" display="block">
                      VRAM profile: {m.hasTuneProfile ? 'measured' : '— none —'}
                    </Typography>
                    <Typography variant="caption" display="block">
                      {m.unverifiedModalities?.length
                        ? `unverified: ${m.unverifiedModalities.join(', ')}`
                        : 'declared modalities verified'}
                    </Typography>
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
                        <Typography variant="caption" color="text.disabled">n/a</Typography>
                      )}
                    </TableCell>
                  ))}
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </TableContainer>

      <Box sx={{ display: 'flex', gap: 2, alignItems: 'center' }}>
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
        {nothingSelected && !running && (
          <Typography variant="caption" color="text.secondary">
            Nothing selected — every model already has the data a default run would produce.
          </Typography>
        )}
      </Box>

      {(status?.args?.length ?? 0) > 0 && (
        <Paper sx={{ p: 2 }}>
          <Typography variant="subtitle2">Invocation</Typography>
          {/* Shown so a run started here is reproducible from a shell — llm-bench
              is a first-class CLI, not an implementation detail of this page. */}
          <Box component="pre" sx={{ m: 0, fontSize: 12, overflowX: 'auto' }}>
            {status?.args?.join(' ')}
          </Box>
        </Paper>
      )}

      {!!status?.error && (
        <Alert severity="error">
          <AlertTitle>Run failed</AlertTitle>
          {status.error}
        </Alert>
      )}

      {(status?.log?.length ?? 0) > 0 && (
        <Paper sx={{ p: 2, maxHeight: 360, overflow: 'auto' }}>
          <Typography variant="subtitle2">Output</Typography>
          <Box component="pre" sx={{ m: 0, fontSize: 12 }}>
            {status?.log?.join('\n')}
          </Box>
        </Paper>
      )}

      <Dialog open={confirming} onClose={() => setConfirming(false)}>
        <DialogTitle>Run bench in exclusive mode?</DialogTitle>
        <DialogContent>
          {/* The destructive consequences are stated before the click, not after.
              A run evicts models and locks out every other caller. */}
          <DialogContentText component="div">
            This will:
            <ul>
              <li><b>Evict resident models</b> so cold measurements are uncontended.</li>
              <li>
                <b>Turn away every other caller</b> with 429 + Retry-After until the run
                finishes. Clients that honor Retry-After will pause and resume rather than
                fail.
              </li>
            </ul>
            Models: <b>{request.models.join(', ') || 'none'}</b>
            <br />
            Probe classes: <b>{request.classes.join(', ') || 'measurement only'}</b>
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

      {start.data?.corrallm?.startBenchRun?.warning && (
        <Alert severity="warning">{start.data.corrallm.startBenchRun.warning}</Alert>
      )}
      {start.data?.corrallm?.startBenchRun?.ok === false && (
        <Alert severity="error">{start.data.corrallm.startBenchRun.message}</Alert>
      )}
    </Box>
  )
}

export const Route = createFileRoute('/bench')({ component: BenchPage })
