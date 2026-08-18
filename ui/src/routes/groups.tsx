import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import {
  Box,
  Button,
  Chip,
  CircularProgress,
  LinearProgress,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { Panel, PageHeader } from '@/Panel'
import { EntryEditor, openEntry, type EntryEdit } from '@/EntryEditor'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtInt } from '@/format'

const GroupsDoc = graphql(/* GraphQL */ `
  query Groups {
    corrallm {
      reservations {
        reservations {
          model
          lane
          slots
          expiresAt
        }
      }
      groups {
        groups {
          name
          weight
          shareCurrency
          interruptible
          active
          waiting
        }
        backends {
          backend
          capacity
          active
          waiting
          groups {
            group
            active
            waiting
          }
        }
      }
    }
  }
`)

function capPct(active: string, capacity: string): number {
  const a = Number(active)
  const c = Number(capacity)
  if (!Number.isFinite(c) || c <= 0) return 0
  return Math.min(100, (a / c) * 100)
}

// fmtCountdown renders the time left on a lease as "4m 03s" / "42s" / "expired".
function fmtCountdown(expiresAt: string, nowMs: number): string {
  const secs = Math.round((new Date(expiresAt).getTime() - nowMs) / 1000)
  if (secs <= 0) return 'expired'
  const m = Math.floor(secs / 60)
  const s = secs % 60
  return m > 0 ? `${m}m ${String(s).padStart(2, '0')}s` : `${s}s`
}

function Groups() {
  const q = useQuery({
    queryKey: ['groups'],
    queryFn: () => gqlClient.request(GroupsDoc),
    refetchInterval: 15000, // fallback; live updates arrive via SSE (useLiveEvents)
  })

  // Tick a local clock so reservation countdowns update between refetches.
  const [nowMs, setNowMs] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNowMs(Date.now()), 1000)
    return () => clearInterval(id)
  }, [])

  // ABOVE the early returns: a hook after them runs on some renders and not
  // others, and React counts hooks. This page crashed with #310 the instant the
  // query resolved when it sat below.
  const [editing, setEditing] = useState<EntryEdit | null>(null)

  if (q.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Typography color="error">{String(q.error)}</Typography>
      </Box>
    )
  }

  const live = q.data?.corrallm.groups
  const groups = live?.groups ?? []
  const backends = live?.backends ?? []
  const reservations = q.data?.corrallm.reservations?.reservations ?? []

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Groups" />

      <Panel
        title="Priority groups"
        subtitle="Weighted fairshare lanes + live load. Click a row to edit it."
        actions={
          <Button size="small" variant="outlined" onClick={() => setEditing(blankGroup())}>
            Add group
          </Button>
        }
        flush
      >
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Group</TableCell>
                <TableCell align="right">Weight</TableCell>
                <TableCell>Share currency</TableCell>
                <TableCell>Interruptible</TableCell>
                <TableCell align="right">Active</TableCell>
                <TableCell align="right">Waiting</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {groups.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={6}>
                    <Typography color="text.secondary">No groups configured.</Typography>
                  </TableCell>
                </TableRow>
              ) : (
                groups.map((g) => (
                  <TableRow
                    key={g.name}
                    hover
                    sx={{ cursor: 'pointer' }}
                    onClick={() => {
                      void openEntry('group', g.name).then(setEditing)
                    }}
                  >
                    <TableCell>{g.name}</TableCell>
                    <TableCell align="right">{fmtInt(g.weight)}</TableCell>
                    <TableCell>{g.shareCurrency}</TableCell>
                    <TableCell>{g.interruptible ? 'yes' : '—'}</TableCell>
                    <TableCell align="right">{fmtInt(g.active)}</TableCell>
                    <TableCell align="right">
                      {Number(g.waiting) > 0 ? (
                        <Chip size="small" color="warning" label={fmtInt(g.waiting)} />
                      ) : (
                        '0'
                      )}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Panel>

      <Panel
        title="Reservations"
        subtitle="Slots held free for a lane's headroom — short-lived, heartbeat-renewed, auto-expiring"
        flush
      >
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Model</TableCell>
                <TableCell>Group</TableCell>
                <TableCell align="right">Slots held</TableCell>
                <TableCell align="right">Expires in</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {reservations.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={4}>
                    <Typography color="text.secondary">No active reservations.</Typography>
                  </TableCell>
                </TableRow>
              ) : (
                reservations.map((r) => (
                  <TableRow key={`${r.model}/${r.lane}`} hover>
                    <TableCell>{r.model}</TableCell>
                    <TableCell>
                      <Chip size="small" color="info" label={r.lane} />
                    </TableCell>
                    <TableCell align="right">{fmtInt(r.slots)}</TableCell>
                    <TableCell align="right">{fmtCountdown(r.expiresAt, nowMs)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Panel>

      <Panel title="Backend load" subtitle="Admission slots in use per backend" flush>
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Backend</TableCell>
                <TableCell sx={{ width: 200 }}>Utilization</TableCell>
                <TableCell align="right">Active / Capacity</TableCell>
                <TableCell align="right">Waiting</TableCell>
                <TableCell>By group</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {backends.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5}>
                    <Typography color="text.secondary">No backends under load.</Typography>
                  </TableCell>
                </TableRow>
              ) : (
                backends.map((b) => (
                  <TableRow key={b.backend} hover>
                    <TableCell>{b.backend}</TableCell>
                    <TableCell>
                      <LinearProgress
                        variant="determinate"
                        value={capPct(b.active, b.capacity)}
                        sx={{ height: 8, borderRadius: 1 }}
                      />
                    </TableCell>
                    <TableCell align="right">
                      {fmtInt(b.active)} / {fmtInt(b.capacity)}
                    </TableCell>
                    <TableCell align="right">{fmtInt(b.waiting)}</TableCell>
                    <TableCell>
                      {b.groups.length === 0
                        ? '—'
                        : b.groups
                            .map(
                              (g) =>
                                `${g.group}: ${fmtInt(g.active)}${
                                  Number(g.waiting) > 0 ? ` (+${fmtInt(g.waiting)} q)` : ''
                                }`,
                            )
                            .join(', ')}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Panel>
      <EntryEditor
        editing={editing}
        onChange={setEditing}
        onClose={() => setEditing(null)}
        invalidate={['groups', 'config']}
      />
    </Box>
  )
}

// blankGroup seeds a policy unit: who gets served first under load, and what
// they accept when the good backend is full.
function blankGroup(): EntryEdit {
  return {
    kind: 'group',
    existing: false,
    name: '',
    yaml: `# A priority group bundles ALL policy for the keys mapped to it.
weight: 1                 # share under contention, in the share currency
interruptible: true       # may a higher group preempt its in-flight slot?
# acceptDegrade: true     # will it take a lower-quality tier when saturated?
# qualityFloor: 0.5       # ...but no lower than this
onSaturated:
  default: reject
`,
  }
}

export const Route = createFileRoute('/groups')({ component: Groups })
