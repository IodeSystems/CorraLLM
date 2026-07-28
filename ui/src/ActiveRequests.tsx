import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Button,
  Chip,
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
import { fmtDuration, fmtInt } from '@/format'

// Live in-flight requests. The activity log holds only FINISHED requests, so a
// long completion, a cold load, or a request queued behind a saturated backend
// shows up nowhere until it ends — exactly when you stop caring. This is the
// live half, shown on both Overview and Activity so neither view lies about an
// idle-looking box that is actually working.

const ActiveDoc = graphql(/* GraphQL */ `
  query ActiveRequests {
    corrallm {
      activeRequests {
        requests {
          id
          served
          backend
          group
          key
          sourceIp
          path
          streaming
          state
          startedAt
          elapsedMs
          retryable
          bytesOut
          chunks
        }
      }
    }
  }
`)

// cancelRequest aborts one live request wherever it is — queued, cold-loading
// or mid-stream.
const CancelDoc = graphql(/* GraphQL */ `
  mutation CancelRequest($id: Long!) {
    corrallm {
      cancelRequest(body: { id: $id }) {
        ok
        message
      }
    }
  }
`)

function stateColor(s: string): 'default' | 'info' | 'warning' | 'success' {
  switch (s) {
    case 'queued':
      return 'warning'
    case 'loading':
      return 'info'
    case 'streaming':
      return 'success'
    default:
      return 'default'
  }
}

const STATE_HINT: Record<string, string> = {
  queued: 'waiting for an admission slot',
  loading: 'holds a slot, waiting for the backend to become ready',
  streaming: 'proxying to/from the backend',
}

export function ActiveRequests() {
  const q = useQuery({
    queryKey: ['active'],
    queryFn: () => gqlClient.request(ActiveDoc),
    // Live view: SSE ('changed') invalidates it on every state transition; this
    // poll only covers a dropped event.
    refetchInterval: 3000,
  })

  // Elapsed must keep counting between refetches — a frozen duration on a
  // 4-minute completion reads as a hung UI. Tick locally off startedAt.
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [])

  const qc = useQueryClient()
  const cancel = useMutation({
    mutationFn: (id: string) => gqlClient.request(CancelDoc, { id }),
    onSettled: () => qc.invalidateQueries({ queryKey: ['active'] }),
  })

  const rows = q.data?.corrallm.activeRequests?.requests ?? []

  return (
    <Panel
      title="Active requests"
      badge={
        <Chip
          size="small"
          color={rows.length ? 'success' : 'default'}
          variant={rows.length ? 'filled' : 'outlined'}
          label={rows.length}
        />
      }
      subtitle={rows.length ? undefined : 'nothing in flight'}
      flush={rows.length > 0}
      dense={rows.length === 0}
    >
      {rows.length === 0 ? (
        <Typography variant="body2" sx={{ color: C.textFaint }}>
          Idle — no requests in flight.
        </Typography>
      ) : (
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell align="right">Elapsed</TableCell>
                <TableCell>State</TableCell>
                <TableCell>Served</TableCell>
                <TableCell>Backend</TableCell>
                <TableCell>Lane</TableCell>
                <TableCell>Key</TableCell>
                <TableCell>Source</TableCell>
                <TableCell>Path</TableCell>
                {/* Live output size. A reply still growing at tens of thousands
                    of chunks is not slow, it is looping — the distinction the
                    metadata-only view could not make. */}
                <TableCell align="right">Output</TableCell>
                <TableCell align="right" />
              </TableRow>
            </TableHead>
            <TableBody>
              {rows.map((r) => {
                const started = Date.parse(r.startedAt)
                const elapsed = Number.isFinite(started)
                  ? Math.max(0, now - started)
                  : Number(r.elapsedMs)
                return (
                  <TableRow key={r.id}>
                    <TableCell align="right">{fmtDuration(elapsed)}</TableCell>
                    <TableCell>
                      <Tooltip title={STATE_HINT[r.state] ?? ''}>
                        <Chip size="small" label={r.state} color={stateColor(r.state)} />
                      </Tooltip>
                    </TableCell>
                    <TableCell>
                      {r.served}
                      {r.streaming && (
                        <Chip size="small" variant="outlined" label="stream" sx={{ ml: 0.5 }} />
                      )}
                      {r.retryable && (
                        <Tooltip title="The caller volunteered this request for preemption: a higher-priority request may take its slot. A request can only widen this, never opt out.">
                          <Chip size="small" variant="outlined" label="retryable" sx={{ ml: 0.5 }} />
                        </Tooltip>
                      )}
                    </TableCell>
                    <TableCell>{r.backend || '—'}</TableCell>
                    <TableCell>{r.group || '—'}</TableCell>
                    <TableCell>{r.key || '—'}</TableCell>
                    <TableCell sx={{ fontFamily: 'monospace' }}>{r.sourceIp || '—'}</TableCell>
                    <TableCell>{r.path}</TableCell>
                    <TableCell align="right" sx={{ fontVariantNumeric: 'tabular-nums' }}>
                      <Tooltip
                        title={`${fmtInt(Number(r.bytesOut))} bytes relayed so far. For a streamed reply the chunk count is roughly the tokens generated — a normal answer is hundreds to a few thousand.`}
                      >
                        <span>{fmtInt(Number(r.chunks))}</span>
                      </Tooltip>
                    </TableCell>
                    <TableCell align="right">
                      <Button
                        size="small"
                        color="error"
                        disabled={cancel.isPending}
                        onClick={() => cancel.mutate(String(r.id))}
                      >
                        Cancel
                      </Button>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </TableContainer>
      )}
    </Panel>
  )
}
