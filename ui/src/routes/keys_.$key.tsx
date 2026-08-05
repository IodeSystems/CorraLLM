import { createFileRoute, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Chip,
  CircularProgress,
  Stack,
  Tooltip,
  Typography,
} from '@mui/material'
import { Panel, PageHeader } from '@/Panel'
import { ActivityLog } from '@/ActivityLog'
import { KeyLaneActions } from '@/KeyLane'
import { KeyCharts } from '@/KeyCharts'
import { KeysDoc } from './keys'
import { gqlClient } from '@/gqlClient'
import { fmtAgo, fmtDuration, fmtInt, fmtUSD } from '@/format'

/**
 * One caller, end to end: which lane it is in, what it has consumed, and every
 * request it made.
 *
 * The roster answers "who is here"; this answers "and what are they doing",
 * which is the question that actually decides a lane. Cost and request count
 * disagree often enough to matter — a caller running thousands of embeddings is
 * loud and cheap, one running a few long generations is the opposite — and the
 * lane you want depends on which kind it is.
 *
 * It reuses the roster's own query rather than adding a per-key endpoint: the
 * roster is a few dozen rows, already cached under the same key, and a
 * one-key-shaped API would be a second thing to keep consistent with it.
 */
function KeyDetail() {
  const { key } = Route.useParams()
  const [err, setErr] = useState('')

  const q = useQuery({
    queryKey: ['keys'],
    queryFn: () => gqlClient.request(KeysDoc),
    refetchInterval: 30000,
  })

  if (q.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }

  const roster = q.data?.corrallm.keys
  const groups = q.data?.corrallm.groups?.groups ?? []
  const row = roster?.keys.find((k) => k.key === key)

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title={key}>
        <Link to="/keys">← all keys</Link>
      </PageHeader>

      {err && (
        <Alert severity="error" onClose={() => setErr('')}>
          {err}
        </Alert>
      )}

      {/* A key can be reached by URL before it has ever called and without being
          configured — that is not an error, it is the state every stranger is in
          before enrollment, so say so instead of rendering an empty page. */}
      {!row ? (
        <Alert severity="info">
          This key is neither configured nor present in recorded traffic. It would still be served
          {roster?.unknownAllowed ? (
            <>
              {' '}
              in the <code>{roster?.unknownGroup}</code> lane, like any unrecognized key.
            </>
          ) : (
            <> — no: unrecognized keys are refused (401) on this box.</>
          )}
        </Alert>
      ) : (
        <Panel title="Caller" subtitle="Lane assignment and what this key has consumed">
          <Stack spacing={2}>
            <Stack direction="row" spacing={3} flexWrap="wrap" useFlexGap alignItems="center">
              <Stat label="Group">
                {row.recognized ? (
                  <Chip size="small" label={row.group} />
                ) : (
                  <Tooltip title="Nobody assigned this key. It is being served in the fallback lane because corrallm accepts any key, not because anyone chose this.">
                    <Chip size="small" color="warning" label={`${row.group} (unassigned)`} />
                  </Tooltip>
                )}
              </Stat>
              <Stat label="Weight">{fmtInt(Number(row.weight))}</Stat>
              <Stat label="Requests">{fmtInt(Number(row.requests))}</Stat>
              <Stat label="Last seen">
                <Tooltip title={row.lastSeen || 'never seen in recorded traffic'}>
                  <span>{fmtAgo(row.lastSeen)}</span>
                </Tooltip>
              </Stat>
              <Stat label="Cost">{fmtUSD(row.costUSD)}</Stat>
              <Stat label="Dwell">{fmtDuration(Number(row.dwellMS))}</Stat>
              <Stat label="Hash">
                <span style={{ fontFamily: 'monospace' }}>{row.hash}</span>
              </Stat>
              <Box sx={{ flexGrow: 1 }} />
              <KeyLaneActions
                keyName={key}
                group={row.group}
                recognized={row.recognized}
                groups={groups}
                onError={setErr}
              />
            </Stack>
            {/* Weight is on the group, so the honest phrasing is what the lane
                buys, not what the key has. */}
            <Typography variant="body2" color="text.secondary">
              Scheduling weight comes from the <code>{row.group}</code> group, and is compared
              against other groups <b>per backend</b> — heavy traffic on an uncontended model does
              not cost this caller anything on a contended one.
            </Typography>
          </Stack>
        </Panel>
      )}

      {/* What this caller actually spends it on. The totals above say how much;
          only the model split says whether it is loud-and-cheap or the reverse,
          which is the distinction that decides a lane. */}
      <KeyCharts filterKey={key} />

      <ActivityLog
        filterKey={key}
        title="Requests"
        subtitle="Everything this key has run, newest first — click a row for payloads"
      />
    </Box>
  )
}

function Stat({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Box>
      <Typography variant="caption" color="text.secondary" display="block">
        {label}
      </Typography>
      <Typography variant="body2" component="div">
        {children}
      </Typography>
    </Box>
  )
}

export const Route = createFileRoute('/keys_/$key')({ component: KeyDetail })
