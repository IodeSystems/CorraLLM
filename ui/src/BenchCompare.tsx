import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Autocomplete,
  Box,
  Button,
  Chip,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  TextField,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import type { BenchSideQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { fmtTime } from '@/format'
import { C } from '@/theme'

// One model's last run: the run-wide row and the per-capability breakdown.
const BenchSideDoc = graphql(/* GraphQL */ `
  query BenchSide($model: String!) {
    corrallm {
      benchResults(model: $model, limit: 1) {
        results {
          runId
          at
          score
          stages
          stagesPassed
          tokPerSec
          classes
        }
      }
      benchProbes(model: $model) {
        runId
        capabilities {
          capability
          score
          stages
          stagesPassed
          probes {
            probe
            score
            stages
            stagesPassed
            pass
            skipped
          }
        }
      }
    }
  }
`)

// The models that have at least one run: the latest row per model.
const BenchedModelsDoc = graphql(/* GraphQL */ `
  query BenchedModels {
    corrallm {
      benchResults {
        results {
          model
        }
      }
    }
  }
`)

type Side = BenchSideQuery['corrallm']
type Cap = NonNullable<NonNullable<Side['benchProbes']>['capabilities']>[number]

const n = (v: unknown): number => Number(v ?? 0) || 0
const pct = (v: unknown) => `${(n(v) * 100).toFixed(0)}%`

function useBenchSide(model: string | null) {
  return useQuery({
    queryKey: ['benchSide', model],
    queryFn: () => gqlClient.request(BenchSideDoc, { model: model! }),
    enabled: !!model,
  })
}

// Capabilities with at least one probe that ran. A capability whose probes
// were all skipped has no score, and showing 0% would claim a failure.
function ranCaps(side: Side | undefined): Cap[] {
  return (side?.benchProbes?.capabilities ?? []).filter((c) => (c.probes ?? []).some((p) => !p.skipped))
}

/** The model's last benchmark, for the Info tab. */
export function LatestBench({ name, onOpen }: { name: string; onOpen: () => void }) {
  const q = useBenchSide(name)
  const last = q.data?.corrallm?.benchResults?.results?.[0]
  const caps = ranCaps(q.data?.corrallm)
  return (
    <Box>
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center', mb: 0.5 }}>
        <Typography variant="subtitle2">Latest benchmark</Typography>
        <Box sx={{ flexGrow: 1 }} />
        <Button size="small" onClick={onOpen}>
          {last ? 'Bench tab: run, history, compare' : 'Run a benchmark'}
        </Button>
      </Stack>
      {q.isLoading ? null : !last ? (
        <Typography variant="body2" sx={{ color: 'text.secondary' }}>
          No benchmark has been run against this model.
        </Typography>
      ) : (
        <Stack direction="row" spacing={1} sx={{ flexWrap: 'wrap', alignItems: 'center', rowGap: 1 }}>
          <Typography variant="body2" sx={{ color: 'text.secondary' }}>
            {fmtTime(n(last.at) * 1000)}
          </Typography>
          {caps.map((c) => (
            <Chip
              key={c.capability}
              size="small"
              variant="outlined"
              label={`${c.capability} ${pct(c.score)} (${c.stagesPassed}/${c.stages})`}
            />
          ))}
          {n(last.tokPerSec) > 0 && (
            <Chip size="small" variant="outlined" label={`${n(last.tokPerSec).toFixed(0)} tok/s`} />
          )}
        </Stack>
      )}
    </Box>
  )
}

function Score({ v, stages, passed }: { v: unknown; stages: unknown; passed: unknown }) {
  return (
    <>
      <b>{pct(v)}</b>{' '}
      <span style={{ color: C.textFaint }}>
        ({n(passed)}/{n(stages)})
      </span>
    </>
  )
}

function Delta({ a, b }: { a?: number; b?: number }) {
  if (a === undefined || b === undefined) return <>—</>
  const d = (a - b) * 100
  const color = d > 0 ? 'success.main' : d < 0 ? 'error.main' : 'text.secondary'
  return (
    <Typography variant="body2" component="span" sx={{ color }}>
      {d > 0 ? '+' : ''}
      {d.toFixed(0)}
    </Typography>
  )
}

/**
 * This model's last run beside another model's, capability by capability and
 * probe by probe. Only the per-capability scores compare: the run-wide score
 * counts different probe sets for different models.
 */
export function BenchCompare({ name }: { name: string }) {
  const [other, setOther] = useState<string | null>(null)
  const models = useQuery({
    queryKey: ['benchedModels'],
    queryFn: () => gqlClient.request(BenchedModelsDoc),
    staleTime: 30000,
  })
  const options = (models.data?.corrallm?.benchResults?.results ?? [])
    .map((r) => r.model)
    .filter((m) => m !== name)
    .sort()
  const mine = useBenchSide(name)
  const theirs = useBenchSide(other)

  const capsA = ranCaps(mine.data?.corrallm)
  const capsB = ranCaps(theirs.data?.corrallm)
  const capNames = [...new Set([...capsA, ...capsB].map((c) => c.capability))].sort()
  const lastA = mine.data?.corrallm?.benchResults?.results?.[0]
  const lastB = theirs.data?.corrallm?.benchResults?.results?.[0]

  return (
    <Paper sx={{ p: 2 }}>
      <Stack direction="row" spacing={2} sx={{ alignItems: 'center', mb: 1, flexWrap: 'wrap', rowGap: 1 }}>
        <Typography variant="subtitle1">Compare with another model</Typography>
        <Autocomplete
          size="small"
          sx={{ minWidth: 320, flexGrow: 1, maxWidth: 480 }}
          options={options}
          value={other}
          onChange={(_, v) => setOther(v)}
          loading={models.isLoading}
          renderInput={(p) => <TextField {...p} label="Model with a benchmark" />}
        />
      </Stack>
      <Typography variant="body2" sx={{ color: 'text.secondary', mb: 1 }}>
        Last run of each model, per capability and per probe. A dash means that model did not run
        it. Δ is percentage points, this model minus the other.
      </Typography>
      {other && !theirs.isLoading && capsB.length === 0 && (
        <Typography variant="body2" sx={{ color: 'text.secondary' }}>
          {other} has no per-capability results.
        </Typography>
      )}
      {other && capNames.length > 0 && (
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Capability / probe</TableCell>
                <TableCell align="right">{name}</TableCell>
                <TableCell align="right">{other}</TableCell>
                <TableCell align="right">Δ</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              <TableRow>
                <TableCell sx={{ color: 'text.secondary' }}>run</TableCell>
                <TableCell align="right" sx={{ color: 'text.secondary' }}>
                  {lastA ? fmtTime(n(lastA.at) * 1000) : '—'}
                </TableCell>
                <TableCell align="right" sx={{ color: 'text.secondary' }}>
                  {lastB ? fmtTime(n(lastB.at) * 1000) : '—'}
                </TableCell>
                <TableCell />
              </TableRow>
              <TableRow>
                <TableCell sx={{ color: 'text.secondary' }}>tok/s (run mean)</TableCell>
                <TableCell align="right">{lastA && n(lastA.tokPerSec) > 0 ? n(lastA.tokPerSec).toFixed(0) : '—'}</TableCell>
                <TableCell align="right">{lastB && n(lastB.tokPerSec) > 0 ? n(lastB.tokPerSec).toFixed(0) : '—'}</TableCell>
                <TableCell />
              </TableRow>
              {capNames.map((cap) => {
                const a = capsA.find((c) => c.capability === cap)
                const b = capsB.find((c) => c.capability === cap)
                const probeNames = [
                  ...new Set(
                    [...(a?.probes ?? []), ...(b?.probes ?? [])].filter((p) => !p.skipped).map((p) => p.probe),
                  ),
                ].sort()
                const probeOf = (c: Cap | undefined, p: string) =>
                  (c?.probes ?? []).find((x) => x.probe === p && !x.skipped)
                return [
                  <TableRow key={cap} sx={{ bgcolor: C.canvas }}>
                    <TableCell>
                      <b>{cap}</b>
                    </TableCell>
                    <TableCell align="right">
                      {a ? <Score v={a.score} stages={a.stages} passed={a.stagesPassed} /> : '—'}
                    </TableCell>
                    <TableCell align="right">
                      {b ? <Score v={b.score} stages={b.stages} passed={b.stagesPassed} /> : '—'}
                    </TableCell>
                    <TableCell align="right">
                      <Delta a={a ? n(a.score) : undefined} b={b ? n(b.score) : undefined} />
                    </TableCell>
                  </TableRow>,
                  ...probeNames.map((p) => {
                    const pa = probeOf(a, p)
                    const pb = probeOf(b, p)
                    return (
                      <TableRow key={`${cap}/${p}`} hover>
                        <TableCell sx={{ pl: 4, fontFamily: 'monospace', fontSize: '0.8rem' }}>{p}</TableCell>
                        <TableCell align="right">
                          {pa ? <Score v={pa.score} stages={pa.stages} passed={pa.stagesPassed} /> : '—'}
                        </TableCell>
                        <TableCell align="right">
                          {pb ? <Score v={pb.score} stages={pb.stages} passed={pb.stagesPassed} /> : '—'}
                        </TableCell>
                        <TableCell align="right">
                          <Delta a={pa ? n(pa.score) : undefined} b={pb ? n(pb.score) : undefined} />
                        </TableCell>
                      </TableRow>
                    )
                  }),
                ]
              })}
            </TableBody>
          </Table>
        </TableContainer>
      )}
    </Paper>
  )
}
