import { createFileRoute, redirect } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import {
  Box,
  Button,
  Chip,
  LinearProgress,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { Panel } from '@/Panel'
import { EntryEditor, openEntry, type EntryEdit } from '@/EntryEditor'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { fmtInt } from '@/format'
import { Loading } from '@/Loading'

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

/**
 * The lane half of Callers (P30 phase C).
 *
 * This was the /groups page. A group is a property of the CALLERS in it — a key
 * maps to exactly one, and the weight that decides who wins under contention
 * lives here — so asking "who is calling this box and what are they allowed"
 * meant two pages, one listing the keys and one listing what their lanes get.
 * They are one page now. /groups redirects.
 *
 * Backend load stays with these panels rather than moving to Machines: its rows
 * break each backend's slots down BY GROUP, which answers "whose work is winning
 * on that backend", not "what does the hardware have".
 */
export function GroupPanels() {
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
    return <Loading />
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
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
      <Panel
        title="Priority groups"
        // A PROPERTY THAT REACHES LIVE TRAFFIC EXPLAINS ITSELF, the same way a
        // control that reaches it does. Lenny run 5 found his own team's key in
        // `batch` — weight 1, interruptible yes — beside another caller in
        // `interactive` at weight 10, and the page never said what either word
        // costs a request. He was right that no OB rule covered it: it is not a
        // control, so OB-1/OB-2 do not reach it. It is OB-12 now.
        subtitle="Caller groups decide who wins when the box is busy. Weight is the share: a group at 10 gets ten times the turns of one at 1, and only while there is contention — an idle box serves everyone at once. Interruptible means a request from this group can be stopped mid-answer to make room for a higher one; a group that is not interruptible always finishes. Click a row to edit it."
        actions={
          <Button size="small" variant="outlined" onClick={() => setEditing(blankGroup())}>
            Add group
          </Button>
        }
        // Adding a group changes how the existing ones divide the box, which is
        // not visible from the button (run 5 left it alone for exactly that).
        badge={
          <Typography variant="caption" sx={{ color: C.textFaint }}>
            a new group takes its share from the others; nothing already running stops
          </Typography>
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
                    <Typography sx={{
                      color: "text.secondary"
                    }}>No groups configured.</Typography>
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
        subtitle="Slots held free for a group's headroom — short-lived, heartbeat-renewed, auto-expiring"
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
                    <Typography sx={{
                      color: "text.secondary"
                    }}>No active reservations.</Typography>
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
                    <Typography sx={{
                      color: "text.secondary"
                    }}>No backends under load.</Typography>
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
  );
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

// The old address, kept: links live in notes and chat history.
export const Route = createFileRoute('/groups')({
  beforeLoad: () => {
    throw redirect({ to: '/callers' })
  },
})
