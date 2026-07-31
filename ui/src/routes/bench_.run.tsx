import { createFileRoute, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Alert,
  AlertTitle,
  Box,
  Chip,
  CircularProgress,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import ExpandMoreIcon from '@mui/icons-material/ExpandMore'
import { Panel, PageHeader } from '@/Panel'
import { BenchProbeDetail } from '@/BenchProbeDetail'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtTime, fmtDuration, fmtInt, capLabel } from '@/format'

/**
 * The RUN view: what did this bench actually do?
 *
 * /bench compares models using their LATEST result each, which is the right
 * default and the wrong thing for a post-mortem: those numbers may come from
 * different runs on different days under different arms. Fixing the run instead
 * makes the comparison internally consistent — same probe set, same machine,
 * same sitting — and is the only view where "this run went wrong" is a question
 * you can even ask.
 */
const RunDoc = graphql(/* GraphQL */ `
  query BenchRunPage($runId: String!, $model: String) {
    corrallm {
      benchRunDetail(runId: $runId, model: $model) {
        runId
        at
        host
        hasArtifacts
        models {
          model
          score
          stages
          stagesPassed
          probes
          skipped
          wallMs
          detail {
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
              isBaseline
              score
              pass
              skipped
              checksPassed
              checksTotal
              wallMs
              newPromptTokens
              completionTokens
              scoreDelta
            }
          }
        }
      }
    }
  }
`)

const n = (v: unknown): number => Number(v ?? 0) || 0
const pct = (v: number) => `${(v * 100).toFixed(0)}%`

/**
 * The subset of a probe row this table renders.
 *
 * A structural type rather than the generated one: it states exactly what the
 * component consumes, so a query field this table stops using cannot silently
 * remain a dependency. Codegen maps Long to string, hence the widened numerics.
 */
type ProbeRow = {
  probe: string
  class?: string | null
  score?: number | string | null
  stages?: number | string | null
  stagesPassed?: number | string | null
  pass?: boolean | null
  skipped?: boolean | null
  skipReason?: string | null
  note?: string | null
  disagreement?: boolean | null
  arms?: ReadonlyArray<{ label: string }> | null
}

/** One model's probe list within the run, each probe opening to its evidence. */
function ModelProbes({
  runId,
  model,
  detail,
}: {
  runId: string
  model: string
  detail: readonly ProbeRow[]
}) {
  const [open, setOpen] = useState<string | null>(null)
  return (
    <TableContainer>
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>Probe</TableCell>
            <TableCell>Class</TableCell>
            <TableCell align="right">Score</TableCell>
            <TableCell align="right">Stages</TableCell>
            <TableCell>Arms</TableCell>
            <TableCell>Outcome</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {detail.map((p) => (
            <>
              <TableRow
                key={p.probe}
                hover
                selected={open === p.probe}
                sx={{ cursor: p.skipped ? 'default' : 'pointer' }}
                onClick={() => !p.skipped && setOpen(open === p.probe ? null : p.probe)}
              >
                <TableCell>
                  <Link
                    to="/bench/probe"
                    search={{ name: p.probe }}
                    style={{ color: 'inherit' }}
                    onClick={(e) => e.stopPropagation()}
                    title="What does this probe measure?"
                  >
                    {p.probe}
                  </Link>
                </TableCell>
                <TableCell>
                  <Typography variant="caption">{capLabel(p.class ?? undefined) || '—'}</Typography>
                </TableCell>
                <TableCell align="right">{p.skipped ? '—' : pct(n(p.score))}</TableCell>
                <TableCell align="right">
                  {p.skipped ? '—' : `${p.stagesPassed}/${p.stages}`}
                </TableCell>
                <TableCell>
                  <Typography variant="caption">
                    {(p.arms ?? []).map((a) => a.label).join(', ') || '—'}
                  </Typography>
                </TableCell>
                <TableCell>
                  {p.skipped ? (
                    <Chip size="small" variant="outlined" label={p.skipReason || 'skipped'} />
                  ) : p.disagreement ? (
                    <Chip size="small" color="error" label="arms disagree" />
                  ) : p.pass ? (
                    <Chip size="small" color="success" label="pass" />
                  ) : (
                    <Chip size="small" color="warning" label={p.note || 'fail'} />
                  )}
                </TableCell>
              </TableRow>
              {open === p.probe && (
                <TableRow key={`${p.probe}-detail`}>
                  <TableCell colSpan={6} sx={{ p: 0 }}>
                    <Box sx={{ p: 2 }}>
                      <BenchProbeDetail runId={runId} model={model} probe={p.probe} />
                    </Box>
                  </TableCell>
                </TableRow>
              )}
            </>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  )
}

function RunPage() {
  const { id, model } = Route.useSearch()
  const { data, isLoading } = useQuery({
    queryKey: ['benchRunPage', id, model ?? ''],
    queryFn: () => gqlClient.request(RunDoc, { runId: id, model: model ?? null }),
    enabled: !!id,
  })

  if (!id) return <Alert severity="warning">No run named. Use /bench/run?id=&lt;runId&gt;.</Alert>
  if (isLoading)
    return (
      <Box sx={{ p: 4 }}>
        <CircularProgress />
      </Box>
    )

  const run = data?.corrallm?.benchRunDetail
  const models = run?.models ?? []

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title={`Run ${id}`}>
        <Link to="/bench" style={{ color: 'inherit' }}>
          <Chip size="small" variant="outlined" label="← bench" clickable />
        </Link>
        {n(run?.at) > 0 && <Chip size="small" label={fmtTime(n(run?.at) * 1000)} />}
        {run?.host && <Chip size="small" variant="outlined" label={run.host} />}
        {model && (
          <Link to="/bench/run" search={{ id }} style={{ color: 'inherit' }}>
            <Chip size="small" color="info" label={`filtered: ${model} ✕`} clickable />
          </Link>
        )}
      </PageHeader>

      {run && !run.hasArtifacts && (
        <Alert severity="info">
          <AlertTitle>Replay artifacts are gone for this run</AlertTitle>
          Scores, stages and per-check verdicts live in the database and are intact. Transcripts
          and tool-call journals lived in <code>out/</code> on the bench host and were pruned, so
          the drill-in below will show verdicts without the conversation.
        </Alert>
      )}

      {models.length === 0 ? (
        <Alert severity="info">No results recorded for this run.</Alert>
      ) : (
        <>
          <Panel title="Models in this run">
            <Typography variant="caption" color="text.secondary" sx={{ px: 2, pt: 1, display: 'block' }}>
              One sitting, one probe set, one machine — so unlike the /bench comparison these
              numbers are directly against each other. Skipped probes are excluded from a model's
              score rather than counted as failures.
            </Typography>
            <TableContainer>
              <Table size="small">
                <TableHead>
                  <TableRow>
                    <TableCell>Model</TableCell>
                    <TableCell align="right">Score</TableCell>
                    <TableCell align="right">Stages</TableCell>
                    <TableCell align="right">Probes</TableCell>
                    <TableCell align="right">Skipped</TableCell>
                    <TableCell align="right">Wall</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {models.map((m) => (
                    <TableRow key={m.model} hover>
                      <TableCell>
                        <Link to="/bench/model" search={{ name: m.model }} style={{ color: 'inherit' }}>
                          {m.model}
                        </Link>
                      </TableCell>
                      <TableCell align="right">{pct(n(m.score))}</TableCell>
                      <TableCell align="right">
                        {m.stagesPassed}/{m.stages}
                      </TableCell>
                      <TableCell align="right">{fmtInt(n(m.probes))}</TableCell>
                      <TableCell align="right">{fmtInt(n(m.skipped))}</TableCell>
                      <TableCell align="right">{fmtDuration(n(m.wallMs))}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          </Panel>

          {models.map((m) => (
            <Accordion key={m.model} defaultExpanded={models.length === 1}>
              <AccordionSummary expandIcon={<ExpandMoreIcon />}>
                <Stack direction="row" spacing={1.5} alignItems="center" sx={{ width: '100%' }}>
                  <Typography variant="subtitle2">{m.model}</Typography>
                  <Chip size="small" label={pct(n(m.score))} />
                  <Typography variant="caption" color="text.secondary">
                    {m.probes} probe(s){n(m.skipped) > 0 ? `, ${m.skipped} skipped` : ''}
                  </Typography>
                </Stack>
              </AccordionSummary>
              <AccordionDetails sx={{ p: 0 }}>
                <ModelProbes runId={id} model={m.model} detail={m.detail ?? []} />
              </AccordionDetails>
            </Accordion>
          ))}
        </>
      )}
    </Box>
  )
}

export const Route = createFileRoute('/bench_/run')({
  validateSearch: (s: Record<string, unknown>): { id: string; model?: string } => ({
    id: String(s.id ?? ''),
    model: s.model ? String(s.model) : undefined,
  }),
  component: RunPage,
})
