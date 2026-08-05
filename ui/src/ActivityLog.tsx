import { useNavigate } from '@tanstack/react-router'
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
import { Panel } from '@/Panel'
import { C } from '@/theme'
import { fmtBytes, fmtDuration, fmtInt, fmtTime, fmtUSD } from '@/format'

/**
 * The completed-request log, and the one place that knows how to render it.
 *
 * It lives outside routes/activity.tsx because the same table is now the answer
 * to two different questions: "what has this box been doing" and "what has THIS
 * CALLER been doing". The second is what makes the key roster actionable — a row
 * saying a key did 9,971 requests invites "doing what", and before the filter
 * existed the only way to answer was to read every row and squint.
 *
 * Filtering happens in SQL, not here: a client-side filter over the newest 100
 * rows shows nothing at all for a key that has been quiet while a busier one
 * filled the window.
 */

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
  query Activity($limit: Long!, $key: String, $served: String, $placement: String) {
    corrallm {
      recentActivity(limit: $limit, key: $key, served: $served, placement: $placement) {
        records {
          id
          ts
          placement
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
          retryAfterMs
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
          retryAfterMs
          reqBody
          respBody
        }
      }
    }
  }
`)

export function statusColor(
  statusStr: string | number,
): 'success' | 'warning' | 'error' | 'default' {
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
  const chatReplayable =
    !!rec && (rec.path.includes('chat/completions') || rec.path.endsWith('/completions'))
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
                to: '/m/$name',
                params: { name: rec.served },
                search: { replay: chatReplayable ? rec.id : undefined },
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

export function ActivityLog({
  filterKey,
  filterModel,
  filterPlacement,
  hideModel = false,
  limit = 100,
  title = 'Recent',
  subtitle = 'Completed requests, newest first — click a row for payloads',
  action,
}: {
  filterKey?: string
  // filterModel scopes to one served model. Paired with hideModel it gives the
  // per-model view: the SAME table, minus the column whose value is now in the
  // page title. Two tables that merely look alike drift — one gains a column,
  // the other gains a formatter — so this is one component with a narrower
  // question rather than a second implementation of the same rows.
  filterModel?: string
  // filterPlacement narrows to ONE way of serving the model. Separate from
  // filterModel because "how does this model behave" and "how does it behave on
  // that box" are different questions, and with two placements an answer to the
  // first describes neither.
  filterPlacement?: string
  hideModel?: boolean
  limit?: number
  title?: string
  subtitle?: string
  action?: React.ReactNode
}) {
  const [selected, setSelected] = useState<string | null>(null)
  const q = useQuery({
    // filterKey is part of the cache key, or switching callers would show the
    // previous one's rows until the refetch landed.
    queryKey: ['activity', filterKey ?? '', filterModel ?? '', filterPlacement ?? '', limit],
    queryFn: () =>
      gqlClient.request(ActivityDoc, {
        limit: String(limit),
        key: filterKey || undefined,
        served: filterModel || undefined,
        placement: filterPlacement || undefined,
      }),
    refetchInterval: 15000, // fallback; live updates arrive via SSE (useLiveEvents)
  })

  const records = q.data?.corrallm.recentActivity?.records ?? []

  const body = q.isLoading ? (
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
            {/* Dropped when the page is already ABOUT one model — the column
                would repeat the title on every row. */}
            {!hideModel && <TableCell>Served</TableCell>}
            <TableCell>Backend</TableCell>
            {/* WHERE it ran. With a model placed on more than one box, backend
                no longer says which machine, quant or context served it. */}
            <TableCell>Placement</TableCell>
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
                <Typography color="text.secondary">
                  {filterKey
                    ? 'No activity recorded for this key.'
                    : filterModel
                      ? 'No activity recorded for this model.'
                      : 'No activity yet.'}
                </Typography>
              </TableCell>
            </TableRow>
          ) : (
            records.map((r, i) => (
              <TableRow key={i} hover sx={{ cursor: 'pointer' }} onClick={() => setSelected(r.id)}>
                <TableCell>{fmtTime(r.ts)}</TableCell>
                {!hideModel && <TableCell>{r.served}</TableCell>}
                <TableCell>{r.backend}</TableCell>
                <TableCell>{r.placement || '—'}</TableCell>
                <TableCell>{r.key || '—'}</TableCell>
                <TableCell sx={{ fontFamily: 'monospace' }}>{r.sourceIp || '—'}</TableCell>
                <TableCell>{r.path}</TableCell>
                <TableCell align="right">
                  {r.error ? (
                    // The promise rides in the tooltip rather than its own
                    // column: it is set on 429s only, so a column would be a
                    // stripe of em-dashes. The "Come back later" panel is where
                    // it earns a table of its own.
                    <Tooltip
                      title={
                        Number(r.retryAfterMs) > 0
                          ? `${r.error} — told to come back in ${fmtDuration(r.retryAfterMs)}`
                          : r.error
                      }
                    >
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
                <TableCell align="right">
                  {Number(r.audioBytes) > 0 ? '—' : fmtInt(r.promptTokens)}
                </TableCell>
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
                <TableCell align="right">
                  {Number(r.audioBytes) > 0 ? fmtBytes(r.audioBytes) : '—'}
                </TableCell>
                <TableCell align="right">{fmtUSD(r.costUsd)}</TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>
    </TableContainer>
  )

  return (
    <>
      <Panel
        title={title}
        subtitle={subtitle}
        badge={
          <Stack direction="row" spacing={1} alignItems="center">
            <Chip size="small" variant="outlined" label={records.length} />
            {action}
          </Stack>
        }
        flush
      >
        {body}
      </Panel>
      {selected && <DetailModal id={selected} onClose={() => setSelected(null)} />}
    </>
  )
}
