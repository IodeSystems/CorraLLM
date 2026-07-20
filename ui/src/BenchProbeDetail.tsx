import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Alert,
  Box,
  Chip,
  CircularProgress,
  Paper,
  Stack,
  Tab,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Tabs,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtDuration, fmtInt } from '@/format'

/**
 * Why a probe scored what it did.
 *
 * A pass rate says a probe went badly and nothing else. Everything that explains
 * it — the failing assertion and its detail, what the model actually said, which
 * tools it called — was being produced on every run and thrown away at the API
 * boundary. This is the drill-in that surfaces it.
 *
 * Loaded lazily: the transcript and journal are read off disk per probe, so they
 * are fetched only once their tab is opened rather than on every row render.
 */
const ProbeDetailDoc = graphql(/* GraphQL */ `
  query BenchProbeDetail($runId: String!, $model: String!, $probe: String!) {
    corrallm {
      benchProbeDetail(runId: $runId, model: $model, probe: $probe) {
        arms {
          label
          toolset
          toolFormat
          runMode
          stages {
            stage
            prompt
            pass
            limitBreached
            note
            turns
            toolCalls
            newPromptTokens
            completionTokens
            invalidArgRetries
            jsonErrors
            repeatedCalls
            baitCalls
            brokenIntermediates
            compactions
            tokPerSec
            wallMs
            checks {
              idx
              kind
              desc
              pass
              detail
            }
          }
        }
      }
    }
  }
`)

const TranscriptDoc = graphql(/* GraphQL */ `
  query BenchTranscript($runId: String!, $model: String!, $probe: String!, $toolset: String) {
    corrallm {
      benchTranscript(runId: $runId, model: $model, probe: $probe, toolset: $toolset) {
        available
        reason
        truncated
        entries {
          kind
          toolName
          content
          createdAt
        }
      }
    }
  }
`)

const JournalDoc = graphql(/* GraphQL */ `
  query BenchJournal($runId: String!, $model: String!, $probe: String!, $toolset: String) {
    corrallm {
      benchJournal(runId: $runId, model: $model, probe: $probe, toolset: $toolset) {
        available
        reason
        truncated
        entries {
          ts
          tool
          args
          resultBytes
          poisoned
          bait
        }
      }
    }
  }
`)

const n = (v: unknown): number => Number(v ?? 0) || 0

type Props = { runId: string; model: string; probe: string; toolset?: string }

/** Monospace block for prompts, args, and message bodies. */
function Mono({ children }: { children: React.ReactNode }) {
  return (
    <Box
      component="pre"
      sx={{
        m: 0,
        p: 1,
        fontSize: 12,
        lineHeight: 1.45,
        whiteSpace: 'pre-wrap',
        wordBreak: 'break-word',
        bgcolor: 'action.hover',
        borderRadius: 1,
        maxHeight: 320,
        overflow: 'auto',
      }}
    >
      {children}
    </Box>
  )
}

function ChecksAndStages({ runId, model, probe }: Props) {
  const q = useQuery({
    queryKey: ['benchProbeDetail', runId, model, probe],
    queryFn: () => gqlClient.request(ProbeDetailDoc, { runId, model, probe }),
  })
  if (q.isLoading) return <CircularProgress size={20} />
  const arms = q.data?.corrallm?.benchProbeDetail?.arms ?? []
  if (arms.length === 0) {
    return (
      <Alert severity="info">
        No stage detail recorded for this probe. Runs benched before per-stage detail was
        persisted only have their score.
      </Alert>
    )
  }
  return (
    <Stack spacing={2}>
      {arms.map((arm) => (
        <Box key={arm.label}>
          <Chip size="small" label={arm.label} sx={{ mb: 1 }} />
          {(arm.stages ?? []).map((s) => (
            <Paper key={s.stage} variant="outlined" sx={{ p: 1.5, mb: 1 }}>
              <Stack direction="row" spacing={1} alignItems="center" sx={{ mb: 1 }}>
                <Typography variant="subtitle2">Stage {s.stage}</Typography>
                <Chip
                  size="small"
                  color={s.pass ? 'success' : 'error'}
                  label={s.pass ? 'pass' : 'fail'}
                />
                {s.limitBreached && (
                  <Tooltip title="A turn or tool-call budget was hit. This does not by itself veto passing checks.">
                    <Chip size="small" color="warning" variant="outlined" label="limit breached" />
                  </Tooltip>
                )}
                <Box sx={{ flexGrow: 1 }} />
                <Typography variant="caption" color="text.secondary">
                  {s.turns} turns · {s.toolCalls} tool calls · {fmtInt(n(s.newPromptTokens))} in ·{' '}
                  {fmtInt(n(s.completionTokens))} out · {n(s.tokPerSec).toFixed(0)} tok/s ·{' '}
                  {fmtDuration(n(s.wallMs))}
                </Typography>
              </Stack>
              {!!s.note && (
                <Typography variant="body2" color="error" sx={{ mb: 1 }}>
                  {s.note}
                </Typography>
              )}
              {/* Failure-shaped counters only when non-zero: a row of zeros is
                  noise on the probes that went fine. */}
              <Stack direction="row" spacing={1} sx={{ mb: 1 }} flexWrap="wrap" useFlexGap>
                {n(s.baitCalls) > 0 && (
                  <Tooltip title="Calls to a declared bait tool — what adversarial probes are scored on.">
                    <Chip size="small" color="error" label={`bait calls ${s.baitCalls}`} />
                  </Tooltip>
                )}
                {n(s.brokenIntermediates) > 0 && (
                  <Tooltip title="Mutating tool calls that left the workspace failing its safety check.">
                    <Chip size="small" color="warning" label={`broken states ${s.brokenIntermediates}`} />
                  </Tooltip>
                )}
                {n(s.repeatedCalls) > 0 && (
                  <Chip size="small" variant="outlined" label={`repeated calls ${s.repeatedCalls}`} />
                )}
                {n(s.invalidArgRetries) > 0 && (
                  <Chip size="small" variant="outlined" label={`bad tool args ${s.invalidArgRetries}`} />
                )}
                {n(s.jsonErrors) > 0 && (
                  <Chip size="small" variant="outlined" label={`json errors ${s.jsonErrors}`} />
                )}
                {n(s.compactions) > 0 && (
                  <Chip size="small" variant="outlined" label={`compactions ${s.compactions}`} />
                )}
              </Stack>
              {!!s.prompt && (
                <>
                  <Typography variant="caption" color="text.secondary">
                    Prompt
                  </Typography>
                  <Mono>{s.prompt}</Mono>
                </>
              )}
              {(s.checks ?? []).length > 0 && (
                <TableContainer sx={{ mt: 1 }}>
                  <Table size="small">
                    <TableHead>
                      <TableRow>
                        <TableCell>Check</TableCell>
                        <TableCell>Kind</TableCell>
                        <TableCell>Result</TableCell>
                        <TableCell>Detail</TableCell>
                      </TableRow>
                    </TableHead>
                    <TableBody>
                      {(s.checks ?? []).map((c) => (
                        <TableRow key={c.idx} hover>
                          <TableCell>{c.desc}</TableCell>
                          <TableCell>
                            <Typography variant="caption">{c.kind}</Typography>
                          </TableCell>
                          <TableCell>
                            <Chip
                              size="small"
                              color={c.pass ? 'success' : 'error'}
                              label={c.pass ? 'pass' : 'fail'}
                            />
                          </TableCell>
                          <TableCell>
                            <Typography variant="caption" sx={{ wordBreak: 'break-word' }}>
                              {c.detail || '—'}
                            </Typography>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </TableContainer>
              )}
            </Paper>
          ))}
        </Box>
      ))}
    </Stack>
  )
}

function Transcript({ runId, model, probe, toolset }: Props) {
  const q = useQuery({
    queryKey: ['benchTranscript', runId, model, probe, toolset],
    queryFn: () => gqlClient.request(TranscriptDoc, { runId, model, probe, toolset }),
  })
  if (q.isLoading) return <CircularProgress size={20} />
  const t = q.data?.corrallm?.benchTranscript
  if (!t?.available) {
    return <Alert severity="info">{t?.reason || 'No transcript recorded for this probe.'}</Alert>
  }
  return (
    <Stack spacing={1}>
      {t.truncated && <Alert severity="warning">Transcript truncated — showing the first 2000 entries.</Alert>}
      {(t.entries ?? []).map((e, i) => (
        <Box key={i}>
          <Stack direction="row" spacing={1} alignItems="center">
            <Chip
              size="small"
              variant={e.kind === 'user' ? 'filled' : 'outlined'}
              label={e.kind + (e.toolName ? `: ${e.toolName}` : '')}
            />
          </Stack>
          <Mono>{e.content}</Mono>
        </Box>
      ))}
      <Typography variant="caption" color="text.secondary">
        Message bodies are capped at 2 KiB each by the recorder, so long tool results appear
        truncated.
      </Typography>
    </Stack>
  )
}

function Journal({ runId, model, probe, toolset }: Props) {
  const q = useQuery({
    queryKey: ['benchJournal', runId, model, probe, toolset],
    queryFn: () => gqlClient.request(JournalDoc, { runId, model, probe, toolset }),
  })
  if (q.isLoading) return <CircularProgress size={20} />
  const j = q.data?.corrallm?.benchJournal
  if (!j?.available) {
    return <Alert severity="info">{j?.reason || 'No tool calls recorded for this probe.'}</Alert>
  }
  if ((j.entries ?? []).length === 0) {
    return <Alert severity="info">This probe made no tool calls.</Alert>
  }
  return (
    <TableContainer>
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>#</TableCell>
            <TableCell>Tool</TableCell>
            <TableCell>Args</TableCell>
            <TableCell align="right">Result bytes</TableCell>
            <TableCell>Flags</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {(j.entries ?? []).map((e, i) => (
            <TableRow key={i} hover>
              <TableCell>{i + 1}</TableCell>
              <TableCell>{e.tool}</TableCell>
              <TableCell sx={{ maxWidth: 420 }}>
                <Typography variant="caption" sx={{ wordBreak: 'break-all' }}>
                  {e.args}
                </Typography>
              </TableCell>
              <TableCell align="right">{fmtInt(n(e.resultBytes))}</TableCell>
              <TableCell>
                <Stack direction="row" spacing={0.5}>
                  {e.bait && (
                    <Tooltip title="A declared bait tool the model should not have called.">
                      <Chip size="small" color="error" label="bait" />
                    </Tooltip>
                  )}
                  {e.poisoned && (
                    <Tooltip title="This tool returned content carrying a prompt-injection payload.">
                      <Chip size="small" color="warning" label="poisoned" />
                    </Tooltip>
                  )}
                </Stack>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  )
}

/** Tabbed drill-in for one probe: checks and metrics, the conversation, the tool calls. */
export function BenchProbeDetail(props: Props) {
  const [tab, setTab] = useState(0)
  return (
    <Box>
      <Tabs value={tab} onChange={(_, v) => setTab(v)} sx={{ mb: 1 }}>
        <Tab label="Checks & stages" />
        <Tab label="Transcript" />
        <Tab label="Tool calls" />
      </Tabs>
      {/* Mounted only when selected so the on-disk reads happen on demand. */}
      {tab === 0 && <ChecksAndStages {...props} />}
      {tab === 1 && <Transcript {...props} />}
      {tab === 2 && <Journal {...props} />}
    </Box>
  )
}
