import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Box,
  Button,
  Chip,
  CircularProgress,
  Dialog,
  DialogContent,
  DialogTitle,
  Divider,
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
import { ActiveRequests } from '@/ActiveRequests'
import { Panel, PageHeader } from '@/Panel'
import { C } from '@/theme'
import { fmtBytes, fmtDuration, fmtInt, fmtTime, fmtUSD } from '@/format'

// What each finish_reason means for an operator. "length" is the one that
// matters: the reply is truncated mid-thought, so a run of them says a caller
// is generating without a max_tokens rather than that anything failed — every
// one of those requests is still a 200.
const FINISH_HINT: Record<string, string> = {
  stop: 'the model chose to stop — a complete reply',
  length: 'hit a token or context cap and did NOT finish; the reply is truncated',
  tool_calls: 'stopped to call a tool',
  content_filter: 'stopped by a content filter',
}

const ActivityDoc = graphql(/* GraphQL */ `
  query Activity {
    corrallm {
      recentActivity(limit: "100") {
        records {
          id
          ts
          served
          backend
          key
          sourceIp
          path
          status
          dwellMs
          ttfbMs
          finishReason
          promptTokens
          completionTokens
          cachedTokens
          promptPerSec
          predictedPerSec
          audioBytes
          costUsd
          error
        }
      }
    }
  }
`)

const ActivityDetailDoc = graphql(/* GraphQL */ `
  query ActivityDetail($id: Long!) {
    corrallm {
      activityDetail(id: $id) {
        record {
          id
          ts
          served
          backend
          key
          sourceIp
          path
          status
          dwellMs
          ttfbMs
          finishReason
          queuedMs
          promptTokens
          completionTokens
          cachedTokens
          promptPerSec
          predictedPerSec
          audioBytes
          costUsd
          error
          reqBody
          respBody
        }
      }
    }
  }
`)

function statusColor(statusStr: string | number): 'success' | 'warning' | 'error' | 'default' {
  const status = typeof statusStr === 'string' ? Number(statusStr) : statusStr
  if (status >= 200 && status < 300) return 'success'
  if (status === 429 || status === 503) return 'warning'
  if (status >= 400) return 'error'
  return 'default'
}

function DetailModal({ id, onClose }: { id: string; onClose: () => void }) {
  const navigate = useNavigate()
  const q = useQuery({
    queryKey: ['activityDetail', id],
    queryFn: () => gqlClient.request(ActivityDetailDoc, { id }),
  })
  const rec = q.data?.corrallm.activityDetail?.record
  const chatReplayable = !!rec && (rec.path.includes('chat/completions') || rec.path.endsWith('/completions'))
  return (
    <Dialog open onClose={onClose} maxWidth="md" fullWidth>
      <DialogTitle sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
        Request detail
        {rec && <Chip size="small" label={rec.status} color={statusColor(rec.status)} />}
        <Box sx={{ flexGrow: 1 }} />
        {rec && (
          <Button
            size="small"
            variant="outlined"
            onClick={() => {
              navigate({
                to: '/model',
                search: { name: rec.served, replay: chatReplayable ? rec.id : undefined },
              })
            }}
          >
            {chatReplayable ? 'Replay in console' : 'Open in console'}
          </Button>
        )}
      </DialogTitle>
      <DialogContent dividers>
        {q.isLoading && <CircularProgress />}
        {rec && (
          <Stack spacing={1.5}>
            <Box>
              <Typography variant="body2" color="text.secondary">
                {rec.served} · {rec.backend} · {rec.path} · {fmtTime(rec.ts)}
                {rec.sourceIp && <> · from {rec.sourceIp}</>}
              </Typography>
              <Typography variant="body2" color="text.secondary">
                dwell {fmtDuration(rec.dwellMs)} · ttfb {fmtDuration(rec.ttfbMs)} · queued{' '}
                {fmtDuration(rec.queuedMs)} · {fmtInt(rec.promptTokens)}→
                {fmtInt(rec.completionTokens)} tok · {fmtUSD(rec.costUsd)}
                {Number(rec.cachedTokens) > 0 && <> · {fmtInt(rec.cachedTokens)} cached</>}
                {Number(rec.promptPerSec) > 0 && <> · {Number(rec.promptPerSec).toFixed(1)} tp/s</>}
                {Number(rec.predictedPerSec) > 0 && (
                  <> · {Number(rec.predictedPerSec).toFixed(1)} tg/s</>
                )}
                {Number(rec.audioBytes) > 0 && <> · audio {fmtBytes(rec.audioBytes)}</>}
              </Typography>
            </Box>
            {rec.error && (
              <Box>
                <Typography variant="subtitle2">Error</Typography>
                <Typography variant="body2" color="error" sx={{ fontFamily: 'monospace' }}>
                  {rec.error}
                </Typography>
              </Box>
            )}
            <Divider />
            <Payload title="Request" body={rec.reqBody} />
            <Payload title="Response" body={rec.respBody} />
          </Stack>
        )}
      </DialogContent>
    </Dialog>
  )
}

function Payload({ title, body }: { title: string; body: string }) {
  return (
    <Box>
      <Typography variant="subtitle2">{title}</Typography>
      <Box
        component="pre"
        sx={{
          m: 0,
          p: 1,
          bgcolor: C.canvas,
          border: `1px solid ${C.border}`,
          borderRadius: 1,
          fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
          fontSize: '0.75rem',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
          maxHeight: 240,
          overflow: 'auto',
        }}
      >
        {body || '—'}
      </Box>
    </Box>
  )
}

function Activity() {
  const [selected, setSelected] = useState<string | null>(null)
  const q = useQuery({
    queryKey: ['activity'],
    queryFn: () => gqlClient.request(ActivityDoc),
    refetchInterval: 15000, // fallback; live updates arrive via SSE (useLiveEvents)
  })

  const records = q.data?.corrallm.recentActivity?.records ?? []

  // The live section renders independently of the history query — a slow or
  // failed activity fetch must not hide what is running right now.
  const history = q.isLoading ? (
    <Box sx={{ p: 2 }}>
      <CircularProgress />
    </Box>
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : (
    <TableContainer>
        <Table size="small" stickyHeader>
          <TableHead>
            <TableRow>
              <TableCell>Time</TableCell>
              <TableCell>Served</TableCell>
              <TableCell>Backend</TableCell>
              <TableCell>Key</TableCell>
              <TableCell>Source</TableCell>
              <TableCell>Path</TableCell>
              <TableCell align="right">Status</TableCell>
              {/* Why the model stopped. "length" means it hit a cap and did NOT
                  finish — a run of them is a caller generating without a
                  max_tokens, which is invisible from status alone (all 200). */}
              <TableCell>Finish</TableCell>
              <TableCell align="right">Dwell</TableCell>
              <TableCell align="right">Prompt</TableCell>
              <TableCell align="right">Completion</TableCell>
              <TableCell align="right">Cached</TableCell>
              <TableCell align="right">tp/s</TableCell>
              <TableCell align="right">tg/s</TableCell>
              <TableCell align="right">Audio</TableCell>
              <TableCell align="right">Cost</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {records.length === 0 ? (
              <TableRow>
                <TableCell colSpan={16}>
                  <Typography color="text.secondary">No activity yet.</Typography>
                </TableCell>
              </TableRow>
            ) : (
              records.map((r, i) => (
                <TableRow
                  key={i}
                  hover
                  sx={{ cursor: 'pointer' }}
                  onClick={() => setSelected(r.id)}
                >
                  <TableCell>{fmtTime(r.ts)}</TableCell>
                  <TableCell>{r.served}</TableCell>
                  <TableCell>{r.backend}</TableCell>
                  <TableCell>{r.key || '—'}</TableCell>
                  <TableCell sx={{ fontFamily: 'monospace' }}>{r.sourceIp || '—'}</TableCell>
                  <TableCell>{r.path}</TableCell>
                  <TableCell align="right">
                    {r.error ? (
                      <Tooltip title={r.error}>
                        <Chip size="small" label={r.status} color={statusColor(r.status)} />
                      </Tooltip>
                    ) : (
                      <Chip size="small" label={r.status} color={statusColor(r.status)} />
                    )}
                  </TableCell>
                  <TableCell>
                    {r.finishReason ? (
                      <Tooltip title={FINISH_HINT[r.finishReason] ?? r.finishReason}>
                        <Chip
                          size="small"
                          variant="outlined"
                          color={r.finishReason === 'length' ? 'warning' : 'default'}
                          label={r.finishReason}
                        />
                      </Tooltip>
                    ) : (
                      <span style={{ color: C.textFaint }}>—</span>
                    )}
                  </TableCell>
                  <TableCell align="right">{fmtDuration(r.dwellMs)}</TableCell>
                  <TableCell align="right">{Number(r.audioBytes) > 0 ? '—' : fmtInt(r.promptTokens)}</TableCell>
                  <TableCell align="right">
                    {Number(r.audioBytes) > 0 ? '—' : fmtInt(r.completionTokens)}
                  </TableCell>
                  <TableCell align="right">
                    {Number(r.cachedTokens) > 0 ? fmtInt(r.cachedTokens) : '—'}
                  </TableCell>
                  <TableCell align="right">
                    {Number(r.promptPerSec) > 0 ? Number(r.promptPerSec).toFixed(1) : '—'}
                  </TableCell>
                  <TableCell align="right">
                    {Number(r.predictedPerSec) > 0 ? Number(r.predictedPerSec).toFixed(1) : '—'}
                  </TableCell>
                  <TableCell align="right">{Number(r.audioBytes) > 0 ? fmtBytes(r.audioBytes) : '—'}</TableCell>
                  <TableCell align="right">{fmtUSD(r.costUsd)}</TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </TableContainer>
  )

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Activity" />
      {/* In-flight first: the table below only ever holds FINISHED requests. */}
      <ActiveRequests />
      <Panel
        title="Recent"
        subtitle="Completed requests, newest first — click a row for payloads"
        badge={<Chip size="small" variant="outlined" label={records.length} />}
        flush
      >
        {history}
      </Panel>
      {selected && <DetailModal id={selected} onClose={() => setSelected(null)} />}
    </Box>
  )
}

export const Route = createFileRoute('/activity')({ component: Activity })
