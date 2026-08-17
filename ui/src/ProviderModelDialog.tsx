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
import { ModelForm, blankSpec, specFromGql, type ModelSpec, type ServerOption } from '@/ModelForm'
import { graphql } from '@/gql'
import type { Corrallm_ModelSpecInput } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { C, theme } from '@/theme'

/**
 * Add or edit a model of a provider — local or remote.
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

const LocalModelSpecDoc = graphql(/* GraphQL */ `
  query LocalModelSpec($name: String!) {
    corrallm {
      modelSpec(name: $name) {
        exists
        advanced
        spec {
          name
          cmd
          server
          proxy
          upstream
          type
          quality
          maxConcurrent
          maxTokens
          persistent
          stickyTtl
          stickyIdleUnload
          stickyEvictCost
          ramUsage
          notes
        }
      }
    }
  }
`)

const UpsertLocalModelDoc = graphql(/* GraphQL */ `
  mutation UpsertProviderModel(
    $name: String!
    $provider: String
    $extension: String
    $body: corrallm_ModelSpecInput!
  ) {
    corrallm {
      upsertModel(name: $name, provider: $provider, extension: $extension, body: $body) {
        ok
        message
      }
    }
  }
`)

export function ProviderModelDialog(props: {
  open: boolean
  onClose: () => void
  /** The provider this model is authored under. */
  provider: string
  /**
   * The extension holding a REMOTE provider. Absent means a top-level (local)
   * provider.
   *
   * The two are genuinely different models, not a cosmetic difference: a local
   * model owns a process and a port, a remote one is an id on somebody else's
   * endpoint and inherits the provider's target. So the form shows a different
   * field set, and the write goes to a different place in the config.
   */
  extension?: string
  /**
   * The id of an existing model to edit. Absent means create.
   *
   * Editing does NOT send a provider: the server already knows where an
   * existing model was authored and writes it back there. Sending one would be
   * a second opinion about a question that is already settled.
   */
  editId?: string
}) {
  const { open, onClose, provider, extension, editId } = props
  const remote = !!extension
  const qc = useQueryClient()
  const wide = useMediaQuery(theme.breakpoints.up('sm'))
  const [id, setId] = useState('')
  const [spec, setSpec] = useState<ModelSpec>(blankSpec())
  const [err, setErr] = useState('')
  const editing = !!editId

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

  // Loaded through the SAME query the Config page uses, so the two editors
  // cannot disagree about what a stored model looks like.
  const { data: existing, isFetching: loadingSpec } = useQuery({
    queryKey: ['localModelSpec', provider, editId],
    queryFn: () => gqlClient.request(LocalModelSpecDoc, { name: `${provider}-${editId}` }),
    enabled: open && !!editId,
  })

  useEffect(() => {
    if (!open) return
    setErr('')
    setId(editId ?? '')
    if (!editId) {
      setSpec(blankSpec())
      return
    }
    const s = existing?.corrallm?.modelSpec?.spec
    setSpec(s ? specFromGql(s) : blankSpec())
  }, [open, editId, existing])

  const served = id ? `${provider}-${id}` : ''

  const upsert = useMutation({
    mutationFn: (v: { name: string; body: Corrallm_ModelSpecInput }) =>
      gqlClient.request(UpsertLocalModelDoc, {
        name: v.name,
        // A remote model needs the target on EDIT too: unlike a local one, the
        // handler cannot infer the extension from the stored model alone.
        provider: remote || !editing ? provider : null,
        extension: extension ?? null,
        body: v.body,
      }),
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

  // A remote model needs nothing but an id: its endpoint comes from the
  // provider. A local one must say how to reach it.
  const incomplete = !id.trim() || (!remote && !spec.cmd.trim() && !spec.proxy.trim())

  return (
    <Dialog open={open} onClose={onClose} maxWidth="md" fullWidth fullScreen={!wide}>
      <DialogTitle>
        {editing ? `Edit ${provider}-${editId}` : `Add a model to ${provider}`}
      </DialogTitle>
      <DialogContent dividers>
        <Stack spacing={2.5} sx={{ pt: 0.5 }}>
          <TextField
            size="small"
            autoFocus={!editing}
            label="Model id"
            value={id}
            disabled={editing}
            onChange={(e) => setId(e.target.value)}
            placeholder="Qwen3.8-27B"
            helperText={
              editing
                ? `Callers ask for ${served}. Renaming is add + delete — the served name is an identity, and metrics and residency key on it.`
                : !served
                  ? 'As written under the provider. The served name gets the provider prefix.'
                  : remote
                    ? // Bare-name precedence belongs to a top-level provider. A
                      // remote provider has none, so promising that the
                      // unprefixed name resolves here would simply be untrue.
                      `Callers ask for ${served}.`
                    : `Callers ask for ${served} — and for ${id} too, while this provider claims bare names.`
            }
          />
          <Typography variant="caption" sx={{ color: C.textFaint, mt: -1 }}>
            {remote ? (
              <>
                A remote model is an id on this provider&apos;s endpoint — it inherits the
                provider&apos;s host and key, so it has no command, server or port of its own. Set{' '}
                <strong>Upstream id</strong> if the provider calls it something other than the id
                above.
              </>
            ) : (
              <>
                A local model owns its process, so it needs a <strong>cmd</strong> to run — or a{' '}
                <strong>proxy</strong>, if something is already listening on that port.
              </>
            )}
          </Typography>
          <ModelForm
            spec={spec}
            onChange={setSpec}
            servers={servers}
            advanced={[]}
            existing={editing}
            hide={
              remote ? ['name', 'process', 'proxy', 'footprint', 'residency'] : ['name']
            }
          />
          {err && <Alert severity="error">{err}</Alert>}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button
          variant="contained"
          onClick={save}
          disabled={incomplete || upsert.isPending || loadingSpec}
        >
          {upsert.isPending ? 'Saving…' : editing ? 'Save changes' : 'Add model'}
        </Button>
      </DialogActions>
    </Dialog>
  )
}
