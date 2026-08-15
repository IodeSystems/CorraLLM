import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Autocomplete,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  FormControlLabel,
  Link as MuiLink,
  MenuItem,
  Radio,
  RadioGroup,
  Stack,
  Switch,
  TextField,
  Typography,
  useMediaQuery,
} from '@mui/material'
import { graphql } from '@/gql'
import type { Corrallm_ProviderSpecInput } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { C, theme } from '@/theme'

/**
 * Add or edit a provider.
 *
 * A modal, and a typeahead, because the old inline form asked thirteen
 * questions of which three — host, port, and the path prefix in front of /v1 —
 * are things nobody knows without opening a vendor's docs, and getting the
 * prefix wrong fails as a 404 from an endpoint that plainly exists. The preset
 * table answers those three. Everything it fills stays editable: a preset is a
 * starting point, and a vendor moving a path should cost an edit, not a wait.
 */
const PresetsDoc = graphql(/* GraphQL */ `
  query ProviderPresets {
    corrallm {
      listProviderPresets {
        presets {
          id
          label
          group
          host
          port
          basePath
          api
          secretRef
          needsSecret
          catalog
          docs
          notes
          freeOnly
          minContext
          limit
          quality
        }
      }
      extensions {
        extensions {
          name
        }
      }
    }
  }
`)

const UpsertProviderDoc = graphql(/* GraphQL */ `
  mutation UpsertProvider($body: corrallm_ProviderSpecInput!) {
    corrallm {
      upsertProvider(body: $body) {
        ok
        message
      }
    }
  }
`)

const SetSecretDoc = graphql(/* GraphQL */ `
  mutation SetSecret($body: corrallm_SetSecretInputBodyInput!) {
    corrallm {
      setSecret(body: $body) {
        ok
        message
      }
    }
  }
`)

type Preset = NonNullable<
  NonNullable<ReturnType<typeof usePresets>['data']>['presets']
>[number]

function usePresets() {
  const { data } = useQuery({
    queryKey: ['providerPresets'],
    queryFn: () => gqlClient.request(PresetsDoc),
    // The table ships with the binary and cannot change under a running
    // daemon, so re-fetching it per dialog open is pure latency.
    staleTime: Infinity,
  })
  // Memoised because this array is a DEPENDENCY of the dialog's reset effect.
  // Rebuilt per render it changes identity every render, the effect re-runs,
  // and the draft resets — which showed up as "choosing a preset fills
  // nothing", since the fill was undone before it could paint.
  const extensions = useMemo(
    () => (data?.corrallm?.extensions?.extensions ?? []).map((e) => e.name),
    [data],
  )
  return { data: data?.corrallm?.listProviderPresets, extensions }
}

export type ProviderInitial = {
  extension: string
  name: string
  host: string
  port: string
  basePath: string
  manual: boolean
  discover: null | {
    freeOnly: boolean
    minContext: string
    limit: string
    quality: number
  }
}

type Draft = {
  extension: string
  name: string
  host: string
  port: string
  basePath: string
  secretRef: string
  secretValue: string
  monthlyUSD: string
  rpm: string
  // 'filter' contributes models automatically; 'manual' contributes none until
  // one is picked off the catalogue. Not a switch, because they are two
  // different answers to "where do this provider's models come from" and a
  // switch would imply one is the other turned off.
  source: 'filter' | 'manual'
  freeOnly: boolean
  minContext: string
  limit: string
}

const BLANK: Draft = {
  extension: '',
  name: '',
  host: '',
  port: '443',
  basePath: '',
  secretRef: '',
  secretValue: '',
  monthlyUSD: '',
  rpm: '',
  source: 'filter',
  freeOnly: true,
  minContext: '8192',
  limit: '12',
}

const GROUP_LABEL: Record<string, string> = {
  aggregator: 'Aggregators & inference clouds',
  lab: 'Model labs',
  local: 'Local & self-hosted',
}

export function ProviderDialog(props: {
  open: boolean
  onClose: () => void
  /** Editing an existing provider; absent means adding a new one. */
  initial?: ProviderInitial
  secrets: string[]
}) {
  const { open, onClose, initial, secrets } = props
  const qc = useQueryClient()
  // Six sections of form; on a phone a centred dialog turns that into a
  // scroll-in-a-scroll with fields clipped at both edges.
  const wide = useMediaQuery(theme.breakpoints.up('sm'))
  const { data, extensions } = usePresets()
  const presets = useMemo(() => data?.presets ?? [], [data])
  const [preset, setPreset] = useState<Preset | null>(null)
  const [d, setD] = useState<Draft>(BLANK)
  const [err, setErr] = useState('')
  const editing = initial != null

  // Reset when the dialog OPENS, and only then. A dialog that reopens holding
  // the last provider's host is how you end up with two entries pointing at one
  // endpoint — but a reset that re-fires while it is open destroys whatever the
  // operator (or a preset) just filled in.
  useEffect(() => {
    if (!open) return
    setErr('')
    setPreset(null)
    setD(
      initial
        ? {
            ...BLANK,
            extension: initial.extension,
            name: initial.name,
            host: initial.host,
            port: initial.port,
            basePath: initial.basePath,
            source: initial.discover ? 'filter' : 'manual',
            freeOnly: initial.discover?.freeOnly ?? false,
            minContext: initial.discover?.minContext ?? '0',
            limit: initial.discover?.limit ?? '0',
          }
        : BLANK,
    )
  }, [open, initial])

  // The extension list arrives asynchronously, so the default is filled in when
  // it lands — but never OVER a choice already made, which is what separates
  // this from the reset above.
  useEffect(() => {
    if (!open || extensions.length === 0) return
    setD((s) =>
      s.extension ? s : { ...s, extension: extensions.includes('free') ? 'free' : extensions[0] },
    )
  }, [open, extensions])

  const set = (patch: Partial<Draft>) => setD((s) => ({ ...s, ...patch }))

  // Choosing a preset fills the fields nobody memorises and leaves the rest.
  // It never overwrites a name the operator already typed.
  const applyPreset = (p: Preset | null) => {
    setPreset(p)
    if (!p) return
    setD((s) => ({
      ...s,
      name: s.name || p.id,
      host: p.host,
      port: String(p.port),
      basePath: p.basePath,
      secretRef: p.needsSecret ? p.secretRef : '',
      freeOnly: p.freeOnly,
      minContext: String(p.minContext),
      limit: String(p.limit),
      // A big catalogue is not something to enrol by filter — that is a guess
      // over hundreds of models. Presets that suggest no cap say "choose them
      // yourself", and the radio starts there.
      source: p.group === 'local' || p.limit === '0' ? 'manual' : s.source,
    }))
  }

  const setSecret = useMutation({
    mutationFn: (body: { name: string; value: string }) =>
      gqlClient.request(SetSecretDoc, { body }),
  })
  const upsert = useMutation({
    mutationFn: (body: Corrallm_ProviderSpecInput) =>
      gqlClient.request(UpsertProviderDoc, { body }),
  })

  const save = async () => {
    setErr('')
    try {
      // The secret goes to its own endpoint FIRST, so config never references
      // something that does not resolve yet.
      if (d.secretRef && d.secretValue) {
        await setSecret.mutateAsync({ name: d.secretRef, value: d.secretValue })
      }
      const r = await upsert.mutateAsync({
        extension: d.extension,
        name: d.name,
        host: d.host,
        port: String(Number(d.port || 443)),
        basePath: d.basePath,
        manual: d.source === 'manual',
        limits: d.monthlyUSD ? [{ usd: Number(d.monthlyUSD), per: 'month' }] : [],
        discover:
          d.source === 'filter'
            ? {
                freeOnly: d.freeOnly,
                inputModality: 'text',
                outputModality: 'text',
                minContext: String(Number(d.minContext || 0)),
                limit: String(Number(d.limit || 0)),
                quality: preset?.quality ?? 3,
              }
            : null,
        credentials: d.secretRef
          ? [
              {
                name: 'main',
                secretRef: d.secretRef,
                limits: d.rpm ? [{ req: Number(d.rpm), per: 'minute' }] : [],
              },
            ]
          : [],
      })
      const res = r.corrallm?.upsertProvider
      // The mutation reports refusal in the body, not as an error — a rejected
      // save that closed the dialog would look like it worked.
      if (!res?.ok) {
        setErr(res?.message ?? 'save refused')
        return
      }
      setD((s) => ({ ...s, secretValue: '' }))
      qc.invalidateQueries({ queryKey: ['providers'] })
      onClose()
    } catch (e) {
      setErr(String(e))
    }
  }

  const busy = upsert.isPending || setSecret.isPending
  const needsName = !d.name.trim() || !d.host.trim() || !d.extension.trim()

  return (
    <Dialog open={open} onClose={onClose} maxWidth="sm" fullWidth fullScreen={!wide}>
      <DialogTitle>{editing ? `Edit ${initial?.name}` : 'Add a provider'}</DialogTitle>
      <DialogContent dividers>
        <Stack spacing={2.5} sx={{ pt: 0.5 }}>
          {!editing && (
            <Autocomplete
              options={presets}
              value={preset}
              onChange={(_, v) => applyPreset(v)}
              groupBy={(o) => GROUP_LABEL[o.group] ?? o.group}
              getOptionLabel={(o) => o.label}
              isOptionEqualToValue={(a, b) => a.id === b.id}
              renderInput={(params) => (
                <TextField
                  {...params}
                  size="small"
                  autoFocus
                  label="Known provider"
                  helperText="Fills the endpoint for you. Leave empty for a custom endpoint — every field below stays editable either way."
                />
              )}
              renderOption={(liProps, o) => {
                const { key, ...rest } = liProps as { key?: string } & Record<string, unknown>
                return (
                  <li key={key ?? o.id} {...rest}>
                    <Box>
                      <Typography variant="body2">{o.label}</Typography>
                      <Typography variant="caption" sx={{ color: C.textFaint }}>
                        {o.host}
                        {o.basePath}
                        {o.catalog === 'public' ? ' · catalogue is public' : ''}
                      </Typography>
                    </Box>
                  </li>
                )
              }}
            />
          )}

          {preset?.notes && (
            <Alert severity="info" sx={{ py: 0.5 }}>
              <Typography variant="body2">{preset.notes}</Typography>
              {preset.docs && (
                <MuiLink href={preset.docs} target="_blank" rel="noreferrer" variant="caption">
                  {preset.docs}
                </MuiLink>
              )}
            </Alert>
          )}

          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
            <TextField
              select
              size="small"
              label="Extension"
              value={d.extension}
              onChange={(e) => set({ extension: e.target.value })}
              sx={{ minWidth: 140 }}
              disabled={editing}
              helperText="Groups it in config"
            >
              {extensions.map((x) => (
                <MenuItem key={x} value={x}>
                  {x}
                </MenuItem>
              ))}
            </TextField>
            <TextField
              size="small"
              label="Name"
              value={d.name}
              onChange={(e) => set({ name: e.target.value })}
              disabled={editing}
              fullWidth
              helperText={`Served models become ${d.name || '<name>'}-<model id>`}
            />
          </Stack>

          <Divider textAlign="left">
            <Typography variant="caption">Endpoint</Typography>
          </Divider>
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
            <TextField
              size="small"
              label="Host"
              value={d.host}
              onChange={(e) => set({ host: e.target.value })}
              placeholder="openrouter.ai"
              fullWidth
            />
            <TextField
              size="small"
              label="Port"
              value={d.port}
              onChange={(e) => set({ port: e.target.value })}
              sx={{ width: 90 }}
            />
            <TextField
              size="small"
              label="Base path"
              value={d.basePath}
              onChange={(e) => set({ basePath: e.target.value })}
              placeholder="/api"
              sx={{ width: 130 }}
            />
          </Stack>
          <Typography variant="caption" sx={{ color: C.textFaint, mt: -1.5 }}>
            Requests go to{' '}
            <code>
              https://{d.host || '<host>'}
              {d.basePath}/v1/chat/completions
            </code>
            . The base path is the part BEFORE /v1.
          </Typography>

          <Divider textAlign="left">
            <Typography variant="caption">Account</Typography>
          </Divider>
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
            <Autocomplete
              freeSolo
              options={secrets}
              value={d.secretRef}
              onInputChange={(_, v) => set({ secretRef: v.toUpperCase() })}
              sx={{ flex: 1 }}
              renderInput={(params) => (
                <TextField
                  {...params}
                  size="small"
                  label="Secret name"
                  helperText="Stored outside config, referenced as ${NAME}"
                />
              )}
            />
            <TextField
              size="small"
              type="password"
              label="Secret value"
              value={d.secretValue}
              onChange={(e) => set({ secretValue: e.target.value })}
              sx={{ flex: 1 }}
              helperText={
                secrets.includes(d.secretRef)
                  ? 'Already stored — leave blank to keep it'
                  : 'Write-only, never displayed again'
              }
            />
          </Stack>
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
            <TextField
              size="small"
              label="Requests / minute"
              value={d.rpm}
              onChange={(e) => set({ rpm: e.target.value })}
              sx={{ width: 160 }}
            />
            <TextField
              size="small"
              label="Provider $ / month"
              value={d.monthlyUSD}
              onChange={(e) => set({ monthlyUSD: e.target.value })}
              sx={{ width: 170 }}
              helperText="Across all its accounts"
            />
          </Stack>
          <Divider textAlign="left">
            <Typography variant="caption">Where its models come from</Typography>
          </Divider>
          <RadioGroup
            value={d.source}
            onChange={(e) => set({ source: e.target.value as Draft['source'] })}
          >
            <FormControlLabel
              value="filter"
              control={<Radio size="small" />}
              label={
                <Typography variant="body2">
                  Discover automatically, through a filter
                  <Typography variant="caption" sx={{ display: 'block', color: C.textFaint }}>
                    Re-enumerated on every refresh, so a churning roster keeps working.
                  </Typography>
                </Typography>
              }
            />
            <FormControlLabel
              value="manual"
              control={<Radio size="small" />}
              label={
                <Typography variant="body2">
                  Pick models by hand from its catalogue
                  <Typography variant="caption" sx={{ display: 'block', color: C.textFaint }}>
                    Contributes nothing until you browse it and choose.
                  </Typography>
                </Typography>
              }
            />
          </RadioGroup>
          {d.source === 'filter' && (
            <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} alignItems={{ sm: 'center' }} sx={{ pl: { sm: 3.5 } }} flexWrap="wrap" useFlexGap>
              <FormControlLabel
                control={
                  <Switch
                    size="small"
                    checked={d.freeOnly}
                    onChange={(e) => set({ freeOnly: e.target.checked })}
                  />
                }
                label={<Typography variant="body2">Free only</Typography>}
              />
              <TextField
                size="small"
                label="Min context"
                value={d.minContext}
                onChange={(e) => set({ minContext: e.target.value })}
                sx={{ width: 120 }}
              />
              <TextField
                size="small"
                label="Max models"
                value={d.limit}
                onChange={(e) => set({ limit: e.target.value })}
                sx={{ width: 120 }}
              />
            </Stack>
          )}

          {err && <Alert severity="error">{err}</Alert>}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" onClick={save} disabled={needsName || busy}>
          {busy ? 'Saving…' : editing ? 'Save changes' : 'Add provider'}
        </Button>
      </DialogActions>
    </Dialog>
  )
}
