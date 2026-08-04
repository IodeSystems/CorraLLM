import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  MenuItem,
  Stack,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtInt } from '@/format'

/**
 * Enroll / change / unassign a key's lane.
 *
 * Self-contained because two pages need it — the roster row and the per-key
 * page — and the interesting part is not the buttons but the invariants they
 * encode, which must not drift between the two:
 *
 *  - Weight lives on the GROUP. A key is a pointer to one, so this is a
 *    dropdown of configured groups, never free text: the server rejects an
 *    unknown group precisely because it would resolve to the fallback lane and
 *    look like it worked.
 *  - "Unassign", never "Delete". corrallm accepts any key, so removing the
 *    entry drops the lane and the caller keeps working at the fallback weight.
 *    A Delete button would promise a lockout it cannot deliver.
 */

// Assignment goes through the same config-entry editor every other kind uses,
// so persistence, validation and reload are shared rather than reimplemented.
// A key entry's whole YAML is the group name.
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
export function extractMessage(e: unknown): string {
  const any = e as { response?: { errors?: { message?: string }[] }; message?: string }
  return any?.response?.errors?.[0]?.message || any?.message || String(e)
}

export type LaneGroup = { name: string; weight: number | string }

export function KeyLaneActions({
  keyName,
  group,
  recognized,
  groups,
  onError,
}: {
  keyName: string
  group: string
  recognized: boolean
  groups: readonly LaneGroup[]
  onError: (msg: string) => void
}) {
  const qc = useQueryClient()
  const [draft, setDraft] = useState<string | null>(null)

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['keys'] })
    qc.invalidateQueries({ queryKey: ['config'] })
  }

  const assign = useMutation({
    mutationFn: (g: string) =>
      gqlClient.request(PutKeyDoc, { name: keyName, body: { yaml: `${g}\n` } }),
    onSuccess: () => {
      setDraft(null)
      onError('')
      invalidate()
    },
    onError: (e: unknown) => onError(extractMessage(e)),
  })

  const unassign = useMutation({
    mutationFn: () => gqlClient.request(DeleteKeyDoc, { name: keyName }),
    onSuccess: () => {
      onError('')
      invalidate()
    },
    onError: (e: unknown) => onError(extractMessage(e)),
  })

  return (
    <>
      <Stack direction="row" spacing={1} justifyContent="flex-end">
        <Button
          size="small"
          variant={recognized ? 'text' : 'contained'}
          onClick={() => setDraft(group)}
        >
          {recognized ? 'Change' : 'Enroll'}
        </Button>
        {recognized && (
          <Tooltip title="Drop the lane assignment. The caller keeps working, in the fallback lane — this does not lock anyone out.">
            <Button
              size="small"
              color="warning"
              disabled={unassign.isPending}
              onClick={() => unassign.mutate()}
            >
              Unassign
            </Button>
          </Tooltip>
        )}
      </Stack>

      <Dialog open={draft !== null} onClose={() => setDraft(null)} fullWidth maxWidth="sm">
        <DialogTitle>Assign a lane</DialogTitle>
        <DialogContent>
          <Stack spacing={2} sx={{ mt: 1 }}>
            <Typography variant="body2" color="text.secondary">
              Weight lives on the group, not the key — this chooses which lane{' '}
              <code>{keyName}</code> is scheduled in.
            </Typography>
            <TextField
              select
              label="Group"
              value={draft ?? ''}
              onChange={(e) => setDraft(e.target.value)}
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
          <Button onClick={() => setDraft(null)}>Cancel</Button>
          <Button
            variant="contained"
            disabled={!draft || assign.isPending}
            onClick={() => draft && assign.mutate(draft)}
          >
            Save
          </Button>
        </DialogActions>
      </Dialog>
    </>
  )
}
