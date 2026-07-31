import { createFileRoute, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  AlertTitle,
  Box,
  Chip,
  CircularProgress,
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
import { Panel, PageHeader } from '@/Panel'
import { ProbeMarkdown } from '@/ProbeMarkdown'
import { BenchProbeDetail } from '@/BenchProbeDetail'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtTime, capLabel } from '@/format'

/**
 * The SUITE-AND-TEST view: one probe, every model, every run.
 *
 * The other two bench views fix a model or a run and compare across probes. That
 * cannot answer questions about the PROBE itself — whether it is measuring
 * anything, whether it is even reachable — because each of them sees a single
 * observation of it. A probe no model has ever passed looks, from a model page,
 * like a model weakness repeated N times; here it reads as what it is.
 */
const ProbeDoc = graphql(/* GraphQL */ `
  query BenchProbePage($probe: String!) {
    corrallm {
      benchProbeHistory(probe: $probe) {
        probe
        models
        passRate
        catalog {
          name
          dir
          class
          source
          summary
          description
          run
          requires
          stages
          checks
          error
        }
        runs {
          runId
          model
          at
          probe
          class
          score
          stages
          stagesPassed
          pass
          skipped
          skipReason
          note
          disagreement
          arms {
            label
            toolset
            runMode
            isBaseline
            score
            pass
            skipped
            skipReason
            checksPassed
            checksTotal
            wallMs
            newPromptTokens
            completionTokens
            note
          }
        }
      }
    }
  }
`)

const n = (v: unknown): number => Number(v ?? 0) || 0
const pct = (v: number) => `${(v * 100).toFixed(0)}%`

function ProbePage() {
  const { name } = Route.useSearch()
  const [open, setOpen] = useState<{ runId: string; model: string } | null>(null)

  const { data, isLoading } = useQuery({
    queryKey: ['benchProbePage', name],
    queryFn: () => gqlClient.request(ProbeDoc, { probe: name }),
    enabled: !!name,
  })

  if (!name) return <Alert severity="warning">No probe named. Use /bench/probe?name=&lt;probe&gt;.</Alert>
  if (isLoading)
    return (
      <Box sx={{ p: 4 }}>
        <CircularProgress />
      </Box>
    )

  const h = data?.corrallm?.benchProbeHistory
  const cat = h?.catalog
  const runs = h?.runs ?? []

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title={name}>
        <Link to="/bench" style={{ color: 'inherit' }}>
          <Chip size="small" variant="outlined" label="← all probes" clickable />
        </Link>
        {cat?.class && <Chip size="small" label={capLabel(cat.class)} />}
        {cat?.run && <Chip size="small" variant="outlined" label={`residency: ${cat.run}`} />}
        {cat?.requires && cat.requires !== 'chat' && (
          <Chip size="small" variant="outlined" label={`needs ${cat.requires}`} />
        )}
        {cat?.source && <Chip size="small" variant="outlined" label={cat.source} />}
      </PageHeader>

      {/* The description is the point of this page. It goes ABOVE the numbers:
          a pass rate you cannot interpret is not information yet. */}
      {cat?.error ? (
        <Alert severity="error">
          <AlertTitle>This probe fails to load</AlertTitle>
          {cat.error}
        </Alert>
      ) : cat?.description ? (
        <Panel title="What this probe measures">
          <Box sx={{ p: 2 }}>
            <ProbeMarkdown text={cat.description} />
          </Box>
        </Panel>
      ) : (
        <Alert severity="info">
          <AlertTitle>No description</AlertTitle>
          This probe is not in the current library — it was renamed or removed and its results
          outlived it. The numbers below are still valid; there is just nothing left to describe
          what they measured.
        </Alert>
      )}

      <Stack direction="row" spacing={2} flexWrap="wrap" useFlexGap>
        <Chip label={`${h?.models ?? 0} model(s) measured`} />
        <Chip
          color={n(h?.passRate) >= 0.5 ? 'success' : 'warning'}
          label={`${pct(n(h?.passRate))} pass rate`}
          title="Over non-skipped observations. A probe nothing passes is a probe to go and read."
        />
        {cat && (
          <Chip variant="outlined" label={`${cat.stages} stage(s) · ${cat.checks} check(s)`} />
        )}
      </Stack>

      {n(h?.passRate) === 0 && n(h?.models) > 1 && (
        <Alert severity="warning">
          <AlertTitle>No model has ever passed this probe</AlertTitle>
          With more than one model measured, that is more likely to be a broken premise or an
          unsatisfiable check than a shared model weakness. Read the description above before
          treating this as a finding about the models.
        </Alert>
      )}

      {runs.length === 0 ? (
        <Alert severity="info">This probe has no recorded results yet.</Alert>
      ) : (
        <Panel title="Every model that ran it">
          <TableContainer>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Model</TableCell>
                  <TableCell>Run</TableCell>
                  <TableCell>When</TableCell>
                  <TableCell align="right">Score</TableCell>
                  <TableCell align="right">Stages</TableCell>
                  <TableCell>Arms</TableCell>
                  <TableCell>Outcome</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {runs.map((r, i) => {
                  const key = `${r.runId}/${r.model}`
                  const isOpen = open?.runId === r.runId && open?.model === r.model
                  return (
                    <TableRow
                      key={`${key}-${i}`}
                      hover
                      selected={isOpen}
                      sx={{ cursor: r.skipped ? 'default' : 'pointer' }}
                      onClick={() =>
                        !r.skipped && setOpen(isOpen ? null : { runId: r.runId, model: r.model })
                      }
                    >
                      <TableCell>
                        <Link
                          to="/bench/model"
                          search={{ name: r.model }}
                          style={{ color: 'inherit' }}
                          onClick={(e) => e.stopPropagation()}
                        >
                          {r.model}
                        </Link>
                      </TableCell>
                      <TableCell>
                        <Link
                          to="/bench/run"
                          search={{ id: r.runId }}
                          style={{ color: 'inherit' }}
                          onClick={(e) => e.stopPropagation()}
                        >
                          <Typography variant="caption">{r.runId}</Typography>
                        </Link>
                      </TableCell>
                      <TableCell>
                        <Typography variant="caption">{fmtTime(n(r.at) * 1000)}</Typography>
                      </TableCell>
                      <TableCell align="right">{r.skipped ? '—' : pct(n(r.score))}</TableCell>
                      <TableCell align="right">
                        {r.skipped ? '—' : `${r.stagesPassed}/${r.stages}`}
                      </TableCell>
                      <TableCell>
                        <Typography variant="caption">
                          {(r.arms ?? []).map((a) => a.label).join(', ') || '—'}
                        </Typography>
                      </TableCell>
                      <TableCell>
                        {r.skipped ? (
                          <Chip size="small" variant="outlined" label={r.skipReason || 'skipped'} />
                        ) : r.disagreement ? (
                          <Chip
                            size="small"
                            color="error"
                            label="arms disagree"
                            title="Arms of the same probe reached different verdicts — the finding a pooled score hides."
                          />
                        ) : r.pass ? (
                          <Chip size="small" color="success" label="pass" />
                        ) : (
                          <Chip size="small" color="warning" label={r.note || 'fail'} />
                        )}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </TableContainer>
          <Typography variant="caption" color="text.secondary" sx={{ p: 2, display: 'block' }}>
            Click a row for the stage-by-stage evidence: prompts, per-check verdicts, the
            transcript and the tool-call journal.
          </Typography>
        </Panel>
      )}

      {open && (
        <Paper sx={{ p: 2 }}>
          <Typography variant="subtitle2" sx={{ mb: 1 }}>
            {open.model} · {open.runId}
          </Typography>
          <BenchProbeDetail runId={open.runId} model={open.model} probe={name} />
        </Paper>
      )}
    </Box>
  )
}

export const Route = createFileRoute('/bench_/probe')({
  validateSearch: (s: Record<string, unknown>): { name: string } => ({
    name: String(s.name ?? ''),
  }),
  component: ProbePage,
})
