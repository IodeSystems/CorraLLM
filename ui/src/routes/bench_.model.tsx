import { createFileRoute, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  Alert,
  Box,
  Chip,
  CircularProgress,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { LineChart } from '@mui/x-charts'
import { Panel, PageHeader } from '@/Panel'
import { C, SERIES } from '@/theme'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtTime, fmtDuration, fmtInt } from '@/format'

/**
 * The MODEL view: how has this model moved between runs?
 *
 * /bench shows one number per model — its latest — which cannot distinguish a
 * model that has always scored 70% from one that scored 90% last week. That
 * difference is usually the thing worth acting on, and it only exists across
 * runs.
 *
 * Deliberately NOT the model console: /model?name=X is the operational view
 * (load it, probe it, chat with it). This is the measurement history, and it
 * links INTO the runs rather than duplicating them.
 */
const ModelDoc = graphql(/* GraphQL */ `
  query BenchModelPage($model: String!) {
    corrallm {
      benchResults(model: $model, limit: 50) {
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
    }
  }
`)

const n = (v: unknown): number => Number(v ?? 0) || 0
const pct = (v: number) => `${(v * 100).toFixed(0)}%`

function ModelBenchPage() {
  const { name } = Route.useSearch()
  const { data, isLoading } = useQuery({
    queryKey: ['benchModelPage', name],
    queryFn: () => gqlClient.request(ModelDoc, { model: name }),
    enabled: !!name,
  })

  if (!name)
    return <Alert severity="warning">No model named. Use /bench/model?name=&lt;model&gt;.</Alert>
  if (isLoading)
    return (
      <Box sx={{ p: 4 }}>
        <CircularProgress />
      </Box>
    )

  const results = data?.corrallm?.benchResults?.results ?? []
  // Oldest first for the trend line; the table stays newest-first below.
  const chron = [...results].sort((a, b) => n(a.at) - n(b.at))

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title={name}>
        <Link to="/bench" style={{ color: 'inherit' }}>
          <Chip size="small" variant="outlined" label="← bench" clickable />
        </Link>
        <Link to="/model" search={{ name }} style={{ color: 'inherit' }}>
          <Chip size="small" variant="outlined" label="model console →" clickable />
        </Link>
        <Typography variant="body2" color="text.secondary">
          Measurement history
        </Typography>
      </PageHeader>

      {results.length === 0 ? (
        <Alert severity="info">This model has never been benched.</Alert>
      ) : (
        <>
          {chron.length > 1 && (
            <Panel title="Score over time">
              <Typography
                variant="caption"
                color="text.secondary"
                sx={{ px: 2, pt: 1, display: 'block' }}
              >
                Run-wide pass rate, which mixes every capability this model was measured on. A step
                here is as likely to be a change in the PROBE SET as in the model — check that the
                probe count moved with it before reading it as a regression.
              </Typography>
              <Box sx={{ p: 1 }}>
                <LineChart
                  height={240}
                  colors={SERIES}
                  xAxis={[
                    {
                      data: chron.map((_, i) => i),
                      valueFormatter: (i: number) => fmtTime(n(chron[i]?.at) * 1000),
                    },
                  ]}
                  yAxis={[{ min: 0, max: 1 }]}
                  series={[
                    {
                      data: chron.map((r) => Number(n(r.score).toFixed(3))),
                      label: 'pass rate',
                      showMark: true,
                    },
                  ]}
                />
              </Box>
            </Panel>
          )}

          <Panel title="Runs">
            <Typography
              variant="caption"
              color="text.secondary"
              sx={{ px: 2, pt: 1, display: 'block' }}
            >
              Newest first. Open a run to see this model probe by probe, with the stage-level
              evidence behind each verdict.
            </Typography>
            <TableContainer>
              <Table size="small">
                <TableHead>
                  <TableRow>
                    <TableCell>Run</TableCell>
                    <TableCell>When</TableCell>
                    <TableCell align="right">Score</TableCell>
                    <TableCell align="right">Stages</TableCell>
                    <TableCell align="right">Processed</TableCell>
                    <TableCell align="right">Generated</TableCell>
                    <TableCell align="right">tok/s</TableCell>
                    <TableCell align="right">VRAM MiB</TableCell>
                    <TableCell align="right">Wall</TableCell>
                    <TableCell>Classes</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {results.map((r) => (
                    <TableRow key={r.runId} hover>
                      <TableCell>
                        <Link
                          to="/bench/run"
                          search={{ id: r.runId, model: name }}
                          style={{ color: C.accent }}
                        >
                          <Typography variant="caption">{r.runId}</Typography>
                        </Link>
                      </TableCell>
                      <TableCell>
                        <Typography variant="caption">{fmtTime(n(r.at) * 1000)}</Typography>
                      </TableCell>
                      <TableCell align="right">{pct(n(r.score))}</TableCell>
                      <TableCell align="right">
                        {r.stagesPassed}/{r.stages}
                      </TableCell>
                      <TableCell align="right">{fmtInt(n(r.tokensProcessed))}</TableCell>
                      <TableCell align="right">{fmtInt(n(r.tokensGenerated))}</TableCell>
                      <TableCell align="right">{n(r.tokPerSec).toFixed(0)}</TableCell>
                      <TableCell align="right">{fmtInt(n(r.footprintMiB))}</TableCell>
                      <TableCell align="right">{fmtDuration(n(r.wallMs))}</TableCell>
                      <TableCell>
                        <Typography variant="caption">{r.classes || '—'}</Typography>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          </Panel>
        </>
      )}
    </Box>
  )
}

export const Route = createFileRoute('/bench_/model')({
  validateSearch: (s: Record<string, unknown>): { name: string } => ({
    name: String(s.name ?? ''),
  }),
  component: ModelBenchPage,
})
