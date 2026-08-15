import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  MenuItem,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material'
import { Panel, PageHeader } from '@/Panel'
import { graphql } from '@/gql'
import type { Corrallm_DecideApprovalInputBodyInput } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'

/**
 * Model approval (P21e2): which DISCOVERED models may serve, on which account,
 * and where they sit in a lane.
 *
 * Only credentials with approvalRequired contribute rows. Everywhere else a
 * discovered model already serves, so listing it here would imply a gate that
 * is not there — the queue is exactly the set of decisions actually owed.
 */
const ApprovalsDoc = graphql(/* GraphQL */ `
  query Approvals {
    corrallm {
      listApprovals {
        approvals {
          provider
          credential
          model
          state
          quality
          note
          atMs
          lanes {
            lane
            order
          }
        }
      }
      overview {
        lanes {
          name
        }
      }
    }
  }
`)

const DecideDoc = graphql(/* GraphQL */ `
  mutation DecideApproval($body: corrallm_DecideApprovalInputBodyInput!) {
    corrallm {
      decideApproval(body: $body) {
        ok
        message
      }
    }
  }
`)

const STATE_COLOR: Record<string, 'default' | 'success' | 'error' | 'warning'> = {
  approved: 'success',
  rejected: 'error',
  pending: 'warning',
}

function ApprovalsPage() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useQuery({
    queryKey: ['approvals'],
    queryFn: () => gqlClient.request(ApprovalsDoc),
  })
  // Lane + order are held per row, so a decision carries its placement in the
  // same click that approves it — the point of the whole feature.
  const [draft, setDraft] = useState<
    Record<string, { lane: string; order: string; quality: string }>
  >({})

  const decide = useMutation({
    mutationFn: (body: Corrallm_DecideApprovalInputBodyInput) =>
      gqlClient.request(DecideDoc, { body }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['approvals'] }),
  })

  if (isLoading) return <CircularProgress sx={{ m: 4 }} />
  if (error)
    return (
      <Alert severity="error" sx={{ m: 2 }}>
        {String(error)}
      </Alert>
    )

  const rows = data?.corrallm?.listApprovals?.approvals ?? []
  const lanes = (data?.corrallm?.overview?.lanes ?? []).map((l) => l.name)
  const key = (r: { provider: string; credential: string; model: string }) =>
    `${r.provider} ${r.credential} ${r.model}`

  const submit = (r: (typeof rows)[number], state: string) => {
    const d = draft[key(r)] ?? { lane: '', order: '', quality: '' }
    decide.mutate({
      provider: r.provider,
      credential: r.credential,
      model: r.model,
      state,
      // Long marshals as a STRING over the wire (see the Long scalar in the
      // generated types) — the same mismatch that 422s a REST PUT built from a
      // GraphQL response. Numbers here are silently the wrong type.
      lanes:
        state === 'approved' && d.lane ? [{ lane: d.lane, order: String(Number(d.order || 0)) }] : [],
      quality: Number(d.quality || 0),
    })
  }

  const pending = rows.filter((r) => r.state === 'pending').length

  return (
    <Box sx={{ p: 2 }}>
      <PageHeader title="Approvals">
        <Typography variant="body2" sx={{ opacity: 0.75 }}>
          Discovered models awaiting a decision, per credential. A provider&apos;s catalogue differs
          by key, so the same model can be wanted on one account and refused on another.
        </Typography>
      </PageHeader>
      <Panel title={`Queue (${pending} pending)`} flush>
        {rows.length === 0 ? (
          <Typography variant="body2" sx={{ p: 2, opacity: 0.7 }}>
            Nothing to decide. A credential only contributes rows here when it sets{' '}
            <code>approvalRequired: true</code> — elsewhere discovered models serve as soon as they
            are found.
          </Typography>
        ) : (
          <TableContainer>
            <Table size="small" stickyHeader>
              <TableHead>
                <TableRow>
                  <TableCell>Model</TableCell>
                  <TableCell>Provider / account</TableCell>
                  <TableCell>State</TableCell>
                  <TableCell>Lane</TableCell>
                  <TableCell>Order</TableCell>
                  <TableCell>Quality</TableCell>
                  <TableCell align="right">Decide</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {rows.map((r) => {
                  const k = key(r)
                  const d = draft[k] ?? { lane: '', order: '', quality: '' }
                  const set = (patch: Partial<typeof d>) =>
                    setDraft((s) => ({ ...s, [k]: { ...d, ...patch } }))
                  return (
                    <TableRow key={k} hover>
                      <TableCell sx={{ fontFamily: 'monospace', fontSize: 12.5 }}>
                        {r.model}
                      </TableCell>
                      <TableCell>
                        {r.provider} / <strong>{r.credential}</strong>
                      </TableCell>
                      <TableCell>
                        <Chip
                          size="small"
                          label={r.state}
                          color={STATE_COLOR[r.state] ?? 'default'}
                        />
                        {r.state === 'approved' && r.lanes.length > 0 && (
                          <Typography variant="caption" sx={{ ml: 1, opacity: 0.75 }}>
                            {r.lanes.map((l) => `${l.lane}@${l.order}`).join(', ')}
                          </Typography>
                        )}
                      </TableCell>
                      <TableCell>
                        <TextField
                          select
                          size="small"
                          value={d.lane}
                          onChange={(e) => set({ lane: e.target.value })}
                          sx={{ minWidth: 120 }}
                        >
                          <MenuItem value="">(none)</MenuItem>
                          {lanes.map((l) => (
                            <MenuItem key={l} value={l}>
                              {l}
                            </MenuItem>
                          ))}
                        </TextField>
                      </TableCell>
                      <TableCell>
                        <Tooltip title="Position in that lane's ladder; lower is tried first.">
                          <TextField
                            size="small"
                            value={d.order}
                            onChange={(e) => set({ order: e.target.value })}
                            sx={{ width: 80 }}
                            placeholder="10"
                          />
                        </Tooltip>
                      </TableCell>
                      <TableCell>
                        <Tooltip title="Replaces the discovery template's uniform guess. 0 keeps it.">
                          <TextField
                            size="small"
                            value={d.quality}
                            onChange={(e) => set({ quality: e.target.value })}
                            sx={{ width: 80 }}
                            placeholder="3"
                          />
                        </Tooltip>
                      </TableCell>
                      <TableCell align="right">
                        <Stack direction="row" spacing={1} justifyContent="flex-end">
                          <Button
                            size="small"
                            variant="outlined"
                            onClick={() => submit(r, 'approved')}
                          >
                            Approve
                          </Button>
                          <Button size="small" color="error" onClick={() => submit(r, 'rejected')}>
                            Reject
                          </Button>
                          {r.state !== 'pending' && (
                            <Tooltip title="Return it to the queue. Rejections are otherwise permanent.">
                              <Button size="small" onClick={() => submit(r, 'pending')}>
                                Undo
                              </Button>
                            </Tooltip>
                          )}
                        </Stack>
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </TableContainer>
        )}
      </Panel>
    </Box>
  )
}

export const Route = createFileRoute('/approvals')({ component: ApprovalsPage })
