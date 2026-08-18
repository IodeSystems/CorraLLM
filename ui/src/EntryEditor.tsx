import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  TextField,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { extractMessage } from '@/format'

/**
 * The YAML editor for one config entry — a host, lane, priority group or
 * extension.
 *
 * It was inlined in the Config page, which is why every one of those things had
 * to live on that page: the panel and the only editor that could open it were
 * the same component. Splitting them is what lets each entry sit on the page it
 * belongs to — lanes and extensions with the models they compose, groups with
 * the live group view — instead of all four collecting on a page named after a
 * file.
 *
 * MODELS ARE NOT HERE. They have a real form (ModelForm) with a trial runner,
 * and they are authored on Providers, where the provider that owns the model is
 * known. This is the fallback for the things that have no form: a text box, and
 * a server that checks it twice.
 */

const EntryYamlDoc = graphql(/* GraphQL */ `
  query EntryYaml($kind: String!, $name: String!) {
    corrallm {
      entryYaml(kind: $kind, name: $name) {
        kind
        name
        yaml
      }
    }
  }
`)

const PutYamlDoc = graphql(/* GraphQL */ `
  mutation PutEntryYaml($kind: String!, $name: String!, $body: corrallm_PutEntryYAMLInputBodyInput!) {
    corrallm {
      putEntryYaml(kind: $kind, name: $name, body: $body) {
        ok
        message
      }
    }
  }
`)

const DeleteEntryDoc = graphql(/* GraphQL */ `
  mutation DeleteEntry($kind: String!, $name: String!) {
    corrallm {
      deleteEntry(kind: $kind, name: $name) {
        ok
        message
      }
    }
  }
`)

/** The config sections this editor can edit. Models are excluded on purpose. */
export type EntryKind = 'server' | 'lane' | 'group' | 'extension'

export type EntryEdit = {
  kind: EntryKind
  existing: boolean
  name: string
  yaml: string
}

/**
 * openEntry loads an existing entry's stored YAML.
 *
 * The STORED text, not a re-render of the read view: the read view is lossy —
 * a resolved proxy target cannot be turned back into the port that was written
 * — so round-tripping through it would rewrite fields nobody touched.
 */
export async function openEntry(kind: EntryKind, name: string): Promise<EntryEdit> {
  const d = await gqlClient.request(EntryYamlDoc, { kind, name })
  return { kind, existing: true, name, yaml: d.corrallm.entryYaml?.yaml ?? '' }
}

/**
 * EntryEditor renders the dialog when `editing` is set.
 *
 * `invalidate` names the query keys that go stale on save — the caller knows
 * which page it is on and therefore what needs refetching, and passing them in
 * beats this component guessing at every page's key.
 */
export function EntryEditor({
  editing,
  onChange,
  onClose,
  invalidate = ['config'],
}: {
  editing: EntryEdit | null
  onChange: (e: EntryEdit) => void
  onClose: () => void
  invalidate?: string[]
}) {
  const qc = useQueryClient()
  const [err, setErr] = useState('')

  const done = () => {
    setErr('')
    onClose()
    for (const key of invalidate) void qc.invalidateQueries({ queryKey: [key] })
  }

  const save = useMutation({
    mutationFn: (f: EntryEdit) =>
      gqlClient.request(PutYamlDoc, { kind: f.kind, name: f.name, body: { yaml: f.yaml } }),
    onSuccess: done,
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  const del = useMutation({
    mutationFn: (f: EntryEdit) =>
      gqlClient.request(DeleteEntryDoc, { kind: f.kind, name: f.name }),
    onSuccess: done,
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  if (!editing) return null

  return (
    <Dialog open onClose={onClose} maxWidth="md" fullWidth>
      <DialogTitle>
        {editing.existing ? `Edit ${editing.kind} ${editing.name}` : `Add a ${editing.kind}`}
      </DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ mt: 1 }}>
          <TextField
            label="Name"
            size="small"
            value={editing.name}
            disabled={editing.existing}
            onChange={(e) => onChange({ ...editing, name: e.target.value })}
            helperText="The config key. Renaming means add + delete."
          />
          <TextField
            label="Configuration (YAML)"
            value={editing.yaml}
            onChange={(e) => onChange({ ...editing, yaml: e.target.value })}
            multiline
            minRows={18}
            slotProps={{ input: { sx: { fontFamily: 'monospace', fontSize: 12.5 } } }}
            helperText={`The ${editing.kind} exactly as it appears in the config file. Checked twice on save: unknown keys are rejected, then the whole config is validated — an unknown server, or a lane this would break, is caught here rather than at the next restart.`}
          />
        </Stack>
      </DialogContent>
      {/* OUTSIDE DialogContent on purpose. This used to sit at the top of the
          scrolling content, which made a rejected Delete look like a dead
          button: the YAML is long, so you are scrolled to the bottom when you
          reach Delete, and the server's reason ("member of lane(s) chat —
          remove it there first") rendered somewhere above the fold. The error
          has to sit next to the control that produced it. */}
      {err && (
        <Alert severity="error" sx={{ mx: 3, mb: 1 }}>
          {err}
        </Alert>
      )}
      <DialogActions>
        {editing.existing && (
          <Button
            color="error"
            onClick={() => del.mutate(editing)}
            disabled={del.isPending}
            sx={{ mr: 'auto' }}
          >
            Delete
          </Button>
        )}
        <Button onClick={onClose}>Cancel</Button>
        <Button
          variant="contained"
          onClick={() => save.mutate(editing)}
          disabled={save.isPending || !editing.name.trim()}
        >
          Save
        </Button>
      </DialogActions>
    </Dialog>
  )
}
