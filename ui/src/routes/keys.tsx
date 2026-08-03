import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
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
import { gqlClient } from '@/gqlClient'
import { fmtInt } from '@/format'

/**
 * Keys: who is talking to this box, and whether anybody decided what they get.
 *
 * Weight is the thing that actually schedules a caller, and it lives on the
 * GROUP. A key is only a pointer to one — so this page is really "which lane is
 * each caller in", and the weight column is shown because it is the consequence
 * the operator cares about, not because a key has one.
 *
 * The reason this page exists rather than the Config page covering it: an
 * unassigned key was INVISIBLE. corrallm accepts any key and resolves an unknown
 * one to the fallback lane, so a caller nobody had ever thought about looked
 * exactly like one deliberately placed there. Config can only show you what is
 * written down; this joins that with what has actually been seen, which is the
 * only way to manage keys on a box that mints them freely.
 *
 * Unrecognized rows sort first because they are the ones that need a decision.
 */
const KeysDoc = graphql(/* GraphQL */ `
  query Keys {
    corrallm {
      keys(windowHours: "0") {
        keys {
          key
          hash
          group
          weight
          recognized
          requests
        }
        unknownAllowed
        unknownGroup
      }
      groups {
        groups {
          name
          weight
        }
      }
    }
  }
`)

/**
 * Assignment goes through the same config-entry editor every other kind uses,
 * so persistence, validation and reload are shared rather than reimplemented
 * here. A key entry's whole YAML is the group name, which is why this is a
 * dropdown and not a text editor: unlike a model, there is exactly one field,
 * and offering free text would only invite the typo the server already rejects.
 */
const PutKeyDoc = graphql(/* GraphQL */ `
  mutation PutKeyGroup($name: String!, $body: corrallm_PutEntryYAMLInputBodyInput!) {
    corrallm {
      putEntryYaml(kind: "key", name: $name, body: $body) {
        ok
        message
      }
    }
  }
`)

const DeleteKeyDoc = graphql(/* GraphQL */ `
  mutation DeleteKeyAssignment($name: String!) {
    corrallm {
      deleteEntry(kind: "key", name: $name) {
        ok
        message
      }
    }
  }
`)

// The server's own error text is the useful part — "no priority group", "config
// is hand-written and will not be rewritten". A transport wrapper says nothing
// actionable.
function extractMessage(e: unknown): string {
  const any = e as { response?: { errors?: { message?: string }[] }; message?: string }
  return any?.response?.errors?.[0]?.message || any?.message || String(e)
}

function Keys() {
  const qc = useQueryClient()
  const [err, setErr] = useState('')
  const [assigning, setAssigning] = useState<{ key: string; group: string } | null>(null)

  const q = useQuery({
    queryKey: ['keys'],
    queryFn: () => gqlClient.request(KeysDoc),
    refetchInterval: 30000,
  })

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['keys'] })
    qc.invalidateQueries({ queryKey: ['config'] })
  }

  const assign = useMutation({
    mutationFn: (v: { key: string; group: string }) =>
      gqlClient.request(PutKeyDoc, { name: v.key, body: { yaml: `${v.group}\n` } }),
    onSuccess: () => {
      setAssigning(null)
      setErr('')
      invalidate()
    },
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  const unassign = useMutation({
    mutationFn: (key: string) => gqlClient.request(DeleteKeyDoc, { name: key }),
    onSuccess: () => {
      setErr('')
      invalidate()
    },
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

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

  const data = q.data?.corrallm.keys
  const keys = data?.keys ?? []
  const groups = q.data?.corrallm.groups?.groups ?? []
  const unassigned = keys.filter((k) => !k.recognized).length

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Keys" />

      {err && (
        <Alert severity="error" onClose={() => setErr('')}>
          {err}
        </Alert>
      )}

      {/* The standing policy, stated. It used to be implicit — accept anyone at
          weight 1 — which is a policy wearing the clothes of a fallback, and
          left the reader to infer it from an absence. */}
      <Alert severity={data?.unknownAllowed ? 'info' : 'warning'}>
        {data?.unknownAllowed ? (
          <>
            Unrecognized keys are <b>served</b>, in the <code>{data?.unknownGroup}</code> lane.
            {unassigned > 0 && (
              <>
                {' '}
                {fmtInt(unassigned)} {unassigned === 1 ? 'key has' : 'keys have'} called without
                being assigned one — they are scheduled by default, not by decision.
              </>
            )}
          </>
        ) : (
          <>
            Unrecognized keys are <b>refused</b> (401). Only keys assigned a group below can use
            this box.
          </>
        )}
      </Alert>

      <Panel
        title="Caller keys"
        subtitle="Configured lanes, plus keys seen in traffic that nobody has assigned"
        flush
      >
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Key</TableCell>
                <TableCell>Hash</TableCell>
                <TableCell>Group</TableCell>
                <TableCell align="right">Weight</TableCell>
                <TableCell align="right">Requests</TableCell>
                <TableCell align="right">Actions</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {keys.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={6}>
                    <Typography color="text.secondary">
                      No keys configured, and none seen in traffic yet.
                    </Typography>
                  </TableCell>
                </TableRow>
              ) : (
                keys.map((k) => (
                  <TableRow key={k.hash} hover>
                    <TableCell sx={{ fontFamily: 'monospace' }}>{k.key}</TableCell>
                    <TableCell sx={{ fontFamily: 'monospace', color: 'text.secondary' }}>
                      {k.hash}
                    </TableCell>
                    <TableCell>
                      {k.recognized ? (
                        <Chip size="small" label={k.group} />
                      ) : (
                        <Tooltip title="Nobody assigned this key. It is being served in the fallback lane because corrallm accepts any key, not because anyone chose this.">
                          <Chip size="small" color="warning" label={`${k.group} (unassigned)`} />
                        </Tooltip>
                      )}
                    </TableCell>
                    <TableCell align="right">{fmtInt(Number(k.weight))}</TableCell>
                    <TableCell align="right">{fmtInt(Number(k.requests))}</TableCell>
                    <TableCell align="right">
                      <Stack direction="row" spacing={1} justifyContent="flex-end">
                        <Button
                          size="small"
                          variant={k.recognized ? 'text' : 'contained'}
                          onClick={() => setAssigning({ key: k.key, group: k.group })}
                        >
                          {k.recognized ? 'Change' : 'Enrol'}
                        </Button>
                        {k.recognized && (
                          // "Unassign", never "revoke": corrallm accepts any
                          // key, so removing the entry drops the lane
                          // assignment and the caller keeps working at the
                          // fallback weight. A button labelled Delete would
                          // promise a lockout it cannot deliver.
                          <Tooltip title="Drop the lane assignment. The caller keeps working, in the fallback lane — this does not lock anyone out.">
                            <Button
                              size="small"
                              color="warning"
                              disabled={unassign.isPending}
                              onClick={() => unassign.mutate(k.key)}
                            >
                              Unassign
                            </Button>
                          </Tooltip>
                        )}
                      </Stack>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Panel>

      <Dialog open={!!assigning} onClose={() => setAssigning(null)} fullWidth maxWidth="sm">
        <DialogTitle>Assign a lane</DialogTitle>
        <DialogContent>
          <Stack spacing={2} sx={{ mt: 1 }}>
            <Typography variant="body2" color="text.secondary">
              Weight lives on the group, not the key — this chooses which lane{' '}
              <code>{assigning?.key}</code> is scheduled in.
            </Typography>
            <TextField
              select
              label="Group"
              value={assigning?.group ?? ''}
              onChange={(e) =>
                setAssigning((a) => (a ? { ...a, group: e.target.value } : a))
              }
              helperText="Only configured groups. Assigning an unknown one is refused: it would resolve to the fallback lane and look like it worked."
            >
              {groups.map((g) => (
                <MenuItem key={g.name} value={g.name}>
                  {g.name} (weight {fmtInt(Number(g.weight))})
                </MenuItem>
              ))}
            </TextField>
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setAssigning(null)}>Cancel</Button>
          <Button
            variant="contained"
            disabled={!assigning?.group || assign.isPending}
            onClick={() => assigning && assign.mutate(assigning)}
          >
            Save
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  )
}

export const Route = createFileRoute('/keys')({ component: Keys })
