import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  TextField,
  Typography,
  useMediaQuery,
} from '@mui/material'
import { ModelForm, blankSpec, type ModelSpec, type ServerOption } from '@/ModelForm'
import { graphql } from '@/gql'
import type { Corrallm_ModelSpecInput } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { C, theme } from '@/theme'

/**
 * Add a model to a local provider.
 *
 * Reuses ModelForm rather than growing a second one. The Config page already
 * edits a model with these fields and the server MERGES a spec onto whatever is
 * stored, so two forms over one shape would be two things to keep in step for
 * no gain — and the divergence would show up as a field that silently stops
 * being editable in one of them.
 *
 * What this adds over that form is the provider: the id is written under
 * `providers.<name>.models`, and the served name is <provider>-<id>, which the
 * dialog shows as you type because the prefix rule is the one thing about the
 * local provider that surprises people.
 */
const ServersDoc = graphql(/* GraphQL */ `
  query LocalModelServers {
    corrallm {
      overview {
        servers {
          server
          pools {
            pool
          }
          noProcessMemory
        }
      }
    }
  }
`)

const UpsertLocalModelDoc = graphql(/* GraphQL */ `
  mutation UpsertLocalModel($name: String!, $provider: String, $body: corrallm_ModelSpecInput!) {
    corrallm {
      upsertModel(name: $name, provider: $provider, body: $body) {
        ok
        message
      }
    }
  }
`)

export function LocalModelDialog(props: {
  open: boolean
  onClose: () => void
  /** The local provider this model is authored under. */
  provider: string
}) {
  const { open, onClose, provider } = props
  const qc = useQueryClient()
  const wide = useMediaQuery(theme.breakpoints.up('sm'))
  const [id, setId] = useState('')
  const [spec, setSpec] = useState<ModelSpec>(blankSpec())
  const [err, setErr] = useState('')

  const { data } = useQuery({
    queryKey: ['localModelServers'],
    queryFn: () => gqlClient.request(ServersDoc),
    enabled: open,
  })
  const servers: ServerOption[] = (data?.corrallm?.overview?.servers ?? []).map((s) => ({
    server: s.server,
    pools: (s.pools ?? []).map((p) => p.pool),
    noProcessMemory: s.noProcessMemory ?? false,
  }))

  useEffect(() => {
    if (!open) return
    setId('')
    setSpec(blankSpec())
    setErr('')
  }, [open])

  const served = id ? `${provider}-${id}` : ''

  const upsert = useMutation({
    mutationFn: (v: { name: string; body: Corrallm_ModelSpecInput }) =>
      gqlClient.request(UpsertLocalModelDoc, { name: v.name, provider, body: v.body }),
  })

  const save = async () => {
    setErr('')
    try {
      const r = await upsert.mutateAsync({
        name: served,
        body: {
          // The form's own `name` field is not the identity here — the id plus
          // the provider is — so it is sent as the served name to keep the two
          // from disagreeing.
          name: served,
          cmd: spec.cmd,
          server: spec.server,
          proxy: spec.proxy,
          upstream: spec.upstream,
          type: spec.type,
          quality: spec.quality,
          maxConcurrent: String(spec.maxConcurrent),
          maxTokens: String(spec.maxTokens),
          persistent: spec.persistent,
          stickyTtl: spec.stickyTtl,
          stickyIdleUnload: spec.stickyIdleUnload,
          stickyEvictCost: spec.stickyEvictCost,
          // A JSON map, matching what the Config editor sends. Reshaping it into
          // a list of pairs here got a 422 from the server, which is the kind of
          // divergence reusing one form is supposed to prevent.
          ramUsage: spec.ramUsage,
          notes: spec.notes,
        },
      })
      const res = r.corrallm?.upsertModel
      if (!res?.ok) {
        setErr(res?.message ?? 'save refused')
        return
      }
      qc.invalidateQueries({ queryKey: ['providers'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
      onClose()
    } catch (e) {
      setErr(String(e))
    }
  }

  const incomplete = !id.trim() || (!spec.cmd.trim() && !spec.proxy.trim())

  return (
    <Dialog open={open} onClose={onClose} maxWidth="md" fullWidth fullScreen={!wide}>
      <DialogTitle>Add a model to {provider}</DialogTitle>
      <DialogContent dividers>
        <Stack spacing={2.5} sx={{ pt: 0.5 }}>
          <TextField
            size="small"
            autoFocus
            label="Model id"
            value={id}
            onChange={(e) => setId(e.target.value)}
            placeholder="Qwen3.8-27B"
            helperText={
              served
                ? `Callers ask for ${served} — and for ${id} too, while this provider claims bare names.`
                : 'As written under the provider. The served name gets the provider prefix.'
            }
          />
          <Typography variant="caption" sx={{ color: C.textFaint, mt: -1 }}>
            A local model owns its process, so it needs a <strong>cmd</strong> to run — or a{' '}
            <strong>proxy</strong>, if something is already listening on that port.
          </Typography>
          <ModelForm
            spec={spec}
            onChange={setSpec}
            servers={servers}
            advanced={[]}
            existing={false}
            hideName
          />
          {err && <Alert severity="error">{err}</Alert>}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" onClick={save} disabled={incomplete || upsert.isPending}>
          {upsert.isPending ? 'Saving…' : 'Add model'}
        </Button>
      </DialogActions>
    </Dialog>
  )
}
