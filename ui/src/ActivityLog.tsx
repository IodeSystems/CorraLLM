import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { windowKey, windowVars, type TimeWindow } from '@/TimeWindow'
import { useState } from 'react'
import {
  Box,
  Button,
  Chip,
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
import { Name, Panel } from '@/Panel'
import { C } from '@/theme'
import { fmtBytes, fmtDuration, fmtInt, fmtTime, fmtUSD } from '@/format'
import { Loading } from '@/Loading'

/**
 * The completed-request log, and the one place that knows how to render it.
 *
 * It lives outside routes/activity.tsx because the same table is now the answer
 * to two different questions: "what has this box been doing" and "what has THIS
 * CALLER been doing". The second is what makes the key roster actionable — a row
 * saying a key did 9,971 requests invites "doing what", and before the filter
 * existed the only way to answer was to read every row and squint.
 *
 * Filtering happens in SQL, not here: a client-side filter over the newest page
 * of rows shows nothing at all for a key that has been quiet while a busier one
 * filled the window. Paging happens in SQL too, and only where it is cheap —
 * see `pageable` below, which is the load-bearing comment in this file.
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
  query Activity(
    $limit: Long!
    $offset: Long!
    $key: String
    $served: String
    $placement: String
    $from: Long
    $to: Long
  ) {
    corrallm {
      recentActivity(
        limit: $limit
        offset: $offset
        key: $key
        served: $served
        placement: $placement
        from: $from
        to: $to
      ) {
        hasMore
        records {
          id
          ts
          placement
          served
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
          speedSource
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
          placement
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
          speedSource
          audioBytes
          costUsd
          error
          retryAfterMs
          reqBody
          respBody
          respHeaders
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
        {q.isLoading && <Loading size={20} minHeight={120} />}
        {rec && (
          <Stack spacing={1.5}>
            <Box>
              <Typography variant="body2" sx={{
                color: "text.secondary"
              }}>
                {rec.served} · {rec.placement || '—'} · {rec.path} · {fmtTime(rec.ts)}
                {rec.sourceIp && <> · from {rec.sourceIp}</>}
              </Typography>
              <Typography variant="body2" sx={{
                color: "text.secondary"
              }}>
                dwell {fmtDuration(rec.dwellMs)} · ttfb {fmtDuration(rec.ttfbMs)} · queued{' '}
                {fmtDuration(rec.queuedMs)} · {fmtInt(rec.cachedTokens)} cached ·{' '}
                {fmtInt(Math.max(0, Number(rec.promptTokens) - Number(rec.cachedTokens)))}{' '}
                processed · {fmtInt(rec.completionTokens)} generated · {fmtUSD(rec.costUsd)}
                {Number(rec.promptPerSec) > 0 && <> · {Number(rec.promptPerSec).toFixed(1)} tok/s prefill</>}
                {Number(rec.predictedPerSec) > 0 && (
                  <> · {Number(rec.predictedPerSec).toFixed(1)} tok/s decode</>
                )}
                {rec.speedSource && <> ({SPEED_SOURCE[rec.speedSource] ?? rec.speedSource})</>}
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
            {rec.respHeaders && <Payload title="Upstream headers" body={rec.respHeaders} />}
          </Stack>
        )}
      </DialogContent>
    </Dialog>
  );
}

// Who measured a row's speeds. Empty = the backend's own timings.
const SPEED_SOURCE: Record<string, string> = {
  provider: 'from provider headers',
  proxy: 'measured at proxy; includes network and provider queue',
}

function Speed({ v, source }: { v: number; source: string }) {
  if (!(v > 0)) return <>—</>
  if (!source) return <>{v.toFixed(1)}</>
  return (
    <Tooltip title={SPEED_SOURCE[source] ?? source}>
      <span style={{ color: C.textMuted }}>~{v.toFixed(1)}</span>
    </Tooltip>
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
  limit = 25,
  window,
  title = 'Recent',
  subtitle = 'Newest first; click a row for payloads',
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
  // The page's window, when the page has one. Without it the log keeps its old
  // behaviour — the newest `limit` rows, whenever they were — which is a LIMIT,
  // not a window, and is why "show me 09:00" was unanswerable here.
  window?: TimeWindow
  title?: string
  subtitle?: string
  action?: React.ReactNode
}) {
  const [selected, setSelected] = useState<string | null>(null)
  const [page, setPage] = useState(0)
  // PAUSE pins the upper bound of the query to the newest row on screen. A busy
  // embed caller writes rows faster than anyone can click one, and every SSE
  // invalidation slid the table under the pointer. Pinned, the same rows come
  // back on every refetch and Newer/Older page through that fixed set. The row's
  // own timestamp, not the browser clock: a skewed clock would drop or admit the
  // last few seconds.
  const [pausedAt, setPausedAt] = useState<number | null>(null)

  // PAGING IS OFFERED ONLY WHERE AN INDEX COVERS THE FILTER, and that is not a
  // style choice — it is the difference between a page-turn costing 0.002s and
  // costing 0.091s and climbing.
  //
  // The window (ts) and key filters ride idx_activity_ts and
  // idx_activity_key_rollup's (key, ts) prefix, so SQLite skips the offset's
  // worth of rows in the index without touching the heap. `served` and
  // `placement` are in NEITHER index, so the same skip fetches every passed-over
  // heap row — captured req/resp bodies included, ~85 KB each in production —
  // on the way to a page nobody will read. Measured at offset 50,000 on a
  // 60k-row/940 MB table: 0.002s covered, 0.091s not, and the gap widens with
  // depth. It fails SILENTLY, which is exactly why the guard is here and not in
  // a comment telling somebody to be careful.
  //
  // So the model/placement views keep the old shape — the newest `limit` rows,
  // no pager — and say so, rather than offering a control that gets slower the
  // more it is used. (store.RecentActivityPage's doc comment carries the
  // measurement; a served-covering index is the fix if that view ever needs one.)
  const pageable = !filterModel && !filterPlacement

  const windowK = window ? windowKey(window) : 'nowindow'
  // A filter change means a different list, so page 2 of the old one is not page
  // 2 of the new one. Reset during render rather than in an effect: an effect
  // would paint one frame of the wrong offset's rows first.
  const [pageScope, setPageScope] = useState('')
  const scope = `${filterKey ?? ''}|${filterModel ?? ''}|${filterPlacement ?? ''}|${windowK}`
  if (scope !== pageScope) {
    setPageScope(scope)
    if (page !== 0) setPage(0)
  }
  const offset = pageable ? page * limit : 0
  // An absolute window is already frozen; a pause can only narrow its end.
  const winTo = window ? Number(windowVars(window).to) : 0
  const to = pausedAt != null && (winTo === 0 || pausedAt < winTo) ? pausedAt : winTo

  const q = useQuery({
    // filterKey is part of the cache key, or switching callers would show the
    // previous one's rows until the refetch landed.
    queryKey: ['activity', filterKey ?? '', filterModel ?? '', filterPlacement ?? '', limit, offset, windowK, to],
    queryFn: () =>
      gqlClient.request(ActivityDoc, {
        limit: String(limit),
        offset: String(offset),
        key: filterKey || undefined,
        served: filterModel || undefined,
        placement: filterPlacement || undefined,
        from: window ? windowVars(window).from : '0',
        to: String(to),
      }),
    // Keep the previous page on screen while the next one loads. Without it
    // every page-turn blanks the table to a spinner, which reads as a reload.
    placeholderData: (prev) => prev,
    // fallback; live updates arrive via SSE (useLiveEvents). A frozen window has
    // nothing to refetch — new rows are outside it by definition.
    refetchInterval: window?.kind === 'absolute' || pausedAt != null ? false : 15000,
  })

  const records = q.data?.corrallm.recentActivity?.records ?? []
  const hasMore = q.data?.corrallm.recentActivity?.hasMore ?? false

  const body = q.isLoading ? (
    <Loading size={20} minHeight={120} />
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : (
    <TableContainer>
      <Table size="small" stickyHeader sx={{ '& .MuiTableCell-root': { px: 1 } }}>
        <TableHead>
          <TableRow>
            <TableCell>Time</TableCell>
            {/* Dropped when the page is already ABOUT one model — the column
                would repeat the title on every row. */}
            {!hideModel && <TableCell>Served</TableCell>}
            {/* WHERE it ran — one per (box, cmd), the thing the config calls a
                `placement`. With a model placed on more than one box, the model
                name no longer says which machine, quant or context served it.
                The jargon is the gloss, never the column head (OB-3). */}
            <TableCell>Ran on</TableCell>
            <TableCell>Key</TableCell>
            <TableCell>Source</TableCell>
            <TableCell>Path</TableCell>
            <TableCell align="right">Status</TableCell>
            {/* Why the model stopped. "length" means it hit a cap and did NOT
                finish — a run of them is a caller generating without a
                max_tokens, which is invisible from status alone (all 200). */}
            <TableCell>Finish</TableCell>
            <TableCell align="right">Dwell</TableCell>
            {/* CACHED · PROCESSED · GENERATED. The headers were "Prompt",
                "Completion" and "Cached" in that order, which is two problems in
                three words: Prompt INCLUDES the cached prefix, so the first two
                columns overlapped, and the vocabulary was not the one the cost
                model, the metrics and the operator use. */}
            <TableCell align="right">
              <Tooltip title="Prompt tokens this request got from the backend's cache instead of having reprocessed. A dash means it reported none.">
                <span>Cached</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Prompt tokens actually recomputed for this request: prompt minus cached.">
                <span>Processed</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Completion tokens written back.">
                <span>Generated</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">Prefill tok/s</TableCell>
            <TableCell align="right">Decode tok/s</TableCell>
            <TableCell align="right">Audio</TableCell>
            <TableCell align="right">Cost</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {records.length === 0 ? (
            <TableRow>
              <TableCell colSpan={16}>
                <Typography sx={{
                  color: "text.secondary"
                }}>
                  {offset > 0
                    ? 'Nothing further back in this window.'
                    : filterKey
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
                {!hideModel && <TableCell><Name>{r.served}</Name></TableCell>}
                <TableCell>{r.placement ? <Name max={160}>{r.placement}</Name> : '—'}</TableCell>
                <TableCell>{r.key ? <Name max={160}>{r.key}</Name> : '—'}</TableCell>
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
                  {Number(r.cachedTokens) > 0 ? fmtInt(r.cachedTokens) : '—'}
                </TableCell>
                <TableCell align="right">
                  {Number(r.audioBytes) > 0
                    ? '—'
                    : fmtInt(
                        Math.max(0, Number(r.promptTokens) - Number(r.cachedTokens)),
                      )}
                </TableCell>
                <TableCell align="right">
                  {Number(r.audioBytes) > 0 ? '—' : fmtInt(r.completionTokens)}
                </TableCell>
                <TableCell align="right">
                  <Speed v={Number(r.promptPerSec)} source={r.speedSource} />
                </TableCell>
                <TableCell align="right">
                  <Speed v={Number(r.predictedPerSec)} source={r.speedSource} />
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

  // "Rows 26–50", not "page 2 of ?". There is no total — deliberately, because a
  // COUNT(*) is a second scan over the same filter on every page-turn — so the
  // label says what is on screen rather than inventing a denominator.
  const first = records.length === 0 ? 0 : offset + 1
  const last = offset + records.length

  // Only offered where something moves: an absolute window is already still.
  const pauseButton =
    window?.kind === 'absolute' ? null : pausedAt != null ? (
      <Tooltip title={`Showing requests before ${fmtTime(String(pausedAt))}. Resume to follow new ones again.`}>
        <Button
          size="small"
          variant="contained"
          color="warning"
          onClick={() => {
            setPausedAt(null)
            setPage(0)
          }}
        >
          Paused · Resume
        </Button>
      </Tooltip>
    ) : (
      <Tooltip title="Stop new requests from pushing these rows down, so one can be clicked.">
        <Button
          size="small"
          variant="outlined"
          onClick={() => setPausedAt(records.length > 0 && offset === 0 ? Number(records[0].ts) + 1 : Date.now())}
        >
          Pause
        </Button>
      </Tooltip>
    )

  return (
    <>
      <Panel
        title={title}
        subtitle={subtitle}
        badge={
          <Stack direction="row" spacing={1} sx={{
            alignItems: "center"
          }}>
            <Chip
              size="small"
              variant="outlined"
              label={pageable && offset > 0 ? `${fmtInt(first)}–${fmtInt(last)}` : records.length}
            />
            {action}
          </Stack>
        }
        actions={
          pageable ? (
            <Stack direction="row" spacing={0.5} sx={{ alignItems: 'center' }}>
              {q.isFetching && <Typography variant="caption" sx={{ color: C.textFaint }}>loading…</Typography>}
              {pauseButton}
              <Button size="small" disabled={page === 0 || q.isFetching} onClick={() => setPage((p) => p - 1)}>
                Newer
              </Button>
              <Button size="small" disabled={!hasMore || q.isFetching} onClick={() => setPage((p) => p + 1)}>
                Older
              </Button>
            </Stack>
          ) : (
            // Not a pager it cannot afford: see `pageable` above.
            <Stack direction="row" spacing={0.5} sx={{ alignItems: 'center' }}>
              {pauseButton}
              <Typography variant="caption" sx={{ color: C.textFaint }}>
                newest {limit}
              </Typography>
            </Stack>
          )
        }
        flush
      >
        {body}
      </Panel>
      {selected && <DetailModal id={selected} onClose={() => setSelected(null)} />}
    </>
  );
}
