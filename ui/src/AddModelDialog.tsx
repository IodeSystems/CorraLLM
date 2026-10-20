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
  InputAdornment,
  MenuItem,
  Stack,
  TextField,
  ToggleButton,
  ToggleButtonGroup,
  Typography,
  useMediaQuery,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { ProviderModelDialog } from '@/ProviderModelDialog'
import { C, theme } from '@/theme'

/**
 * Add a model, starting from WHAT KIND it is.
 *
 * The kind decides every other field, so it is asked first instead of being
 * inferred from which button was clicked:
 *
 * - Remote API: a model on an OpenAI-compatible endpoint (Fireworks, DeepSeek,
 *   OpenRouter…). Base URL, model id and key; optionally who may use it, a
 *   spend cap and its price. Written in one edit by addRemoteModel.
 * - Local server: something already listening on this network.
 * - Local process: a model corrallm starts and stops itself.
 *
 * The two local kinds reuse ProviderModelDialog, which owns those fields.
 */
export type ModelKind = 'remote' | 'port' | 'process'

const KINDS: { value: ModelKind; label: string; help: string }[] = [
  { value: 'remote', label: 'Remote API', help: 'An OpenAI-compatible endpoint, such as Fireworks or DeepSeek' },
  { value: 'port', label: 'Local server', help: 'Something already running; corrallm forwards to it' },
  { value: 'process', label: 'Local process', help: 'A model corrallm starts and stops' },
]

export function AddModelDialog(props: {
  open: boolean
  onClose: () => void
  /** Start on this kind. The chooser still shows, so it can be changed. */
  initialKind?: ModelKind
  /** Local provider for the two local kinds. Default: local. */
  localProvider?: string
}) {
  const { open, onClose, initialKind = 'remote', localProvider = 'local' } = props
  const [kind, setKind] = useState<ModelKind>(initialKind)
  useEffect(() => {
    if (open) setKind(initialKind)
  }, [open, initialKind])

  if (open && kind !== 'remote') {
    return (
      <ProviderModelDialog
        open
        onClose={onClose}
        provider={localProvider}
        kind={kind}
        header={<KindChooser kind={kind} onChange={setKind} />}
      />
    )
  }
  return (
    <RemoteModelDialog
      open={open}
      onClose={onClose}
      header={<KindChooser kind={kind} onChange={setKind} />}
    />
  )
}

function KindChooser(props: { kind: ModelKind; onChange: (k: ModelKind) => void }) {
  const help = KINDS.find((k) => k.value === props.kind)?.help
  return (
    <Box>
      <ToggleButtonGroup
        exclusive
        size="small"
        color="primary"
        value={props.kind}
        onChange={(_, v: ModelKind | null) => v && props.onChange(v)}
      >
        {KINDS.map((k) => (
          <ToggleButton key={k.value} value={k.value} sx={{ px: 2 }}>
            {k.label}
          </ToggleButton>
        ))}
      </ToggleButtonGroup>
      <Typography variant="caption" sx={{ display: 'block', color: C.textFaint, mt: 0.5 }}>
        {help}
      </Typography>
    </Box>
  )
}

const RemoteDataDoc = graphql(/* GraphQL */ `
  query AddRemoteModelData {
    corrallm {
      listProviderPresets {
        presets {
          id
          label
          host
          port
          basePath
          secretRef
          needsSecret
        }
      }
      listProviders {
        secrets
      }
      keys(windowHours: "0") {
        keys {
          key
          recognized
        }
      }
    }
  }
`)

const AddRemoteModelDoc = graphql(/* GraphQL */ `
  mutation AddRemoteModel($body: corrallm_RemoteModelSpecInput!) {
    corrallm {
      addRemoteModel(body: $body) {
        ok
        message
      }
    }
  }
`)

type KeyMode = 'stored' | 'new' | 'none'

interface Draft {
  preset: string
  provider: string
  baseUrl: string
  modelId: string
  name: string
  keyMode: KeyMode
  secretRef: string
  secretValue: string
  maxTokens: string
  allow: string[]
  usdDay: string
  usdMonth: string
  rpm: string
  inputPerM: string
  cachedPerM: string
  outputPerM: string
}

const blank: Draft = {
  preset: '',
  provider: '',
  baseUrl: '',
  modelId: '',
  name: '',
  keyMode: 'stored',
  secretRef: '',
  secretValue: '',
  maxTokens: '',
  allow: [],
  usdDay: '',
  usdMonth: '',
  rpm: '',
  inputPerM: '',
  cachedPerM: '',
  outputPerM: '',
}

/** Mirrors the server's default: the model id's last path segment. */
function suffixOf(modelId: string): string {
  const last = modelId.slice(modelId.lastIndexOf('/') + 1)
  return last.replace(/[^A-Za-z0-9._-]/g, '-')
}

const num = (s: string) => (s.trim() === '' ? 0 : Number(s))

function RemoteModelDialog(props: { open: boolean; onClose: () => void; header: React.ReactNode }) {
  const { open, onClose, header } = props
  const qc = useQueryClient()
  const wide = useMediaQuery(theme.breakpoints.up('sm'))
  const [d, setD] = useState<Draft>(blank)
  const [err, setErr] = useState('')
  const set = (patch: Partial<Draft>) => setD((s) => ({ ...s, ...patch }))

  const { data } = useQuery({
    queryKey: ['addRemoteModelData'],
    queryFn: () => gqlClient.request(RemoteDataDoc),
    enabled: open,
  })
  const presets = useMemo(
    () =>
      (data?.corrallm?.listProviderPresets?.presets ?? []).filter(
        (p) => p.host !== 'localhost' && p.host !== '127.0.0.1',
      ),
    [data],
  )
  const secrets = data?.corrallm?.listProviders?.secrets ?? []
  const callers = (data?.corrallm?.keys?.keys ?? []).filter((k) => k.recognized).map((k) => k.key)

  useEffect(() => {
    if (open) {
      setD(blank)
      setErr('')
    }
  }, [open])

  const choosePreset = (id: string) => {
    const p = presets.find((x) => x.id === id)
    if (!p) {
      set({ preset: '' })
      return
    }
    const port = p.port && String(p.port) !== '443' ? `:${p.port}` : ''
    const ref = p.secretRef ?? ''
    set({
      preset: id,
      provider: p.id,
      baseUrl: `https://${p.host}${port}${p.basePath ?? ''}/v1`,
      secretRef: ref,
      keyMode: !p.needsSecret ? 'none' : secrets.includes(ref) ? 'stored' : 'new',
    })
  }

  const name = d.name.trim() || suffixOf(d.modelId.trim())
  const served = d.provider && name ? `${d.provider}-${name}` : ''
  const priced = d.inputPerM.trim() !== '' && d.outputPerM.trim() !== ''

  const add = useMutation({
    mutationFn: () => {
      const limits = [
        ...(num(d.usdDay) > 0 ? [{ usd: num(d.usdDay), per: 'day', hard: true }] : []),
        ...(num(d.usdMonth) > 0 ? [{ usd: num(d.usdMonth), per: 'month', hard: true }] : []),
        ...(num(d.rpm) > 0 ? [{ req: num(d.rpm), per: 'minute' }] : []),
      ]
      return gqlClient.request(AddRemoteModelDoc, {
        body: {
          provider: d.provider.trim(),
          baseUrl: d.baseUrl.trim(),
          modelId: d.modelId.trim(),
          name: d.name.trim() || null,
          secretRef: d.keyMode === 'none' ? '' : d.secretRef.trim(),
          secretValue: d.keyMode === 'new' ? d.secretValue : '',
          maxTokens: String(num(d.maxTokens)),
          allow: d.allow,
          limits,
          pricing: priced
            ? {
                inputPerM: num(d.inputPerM),
                cachedPerM: num(d.cachedPerM),
                outputPerM: num(d.outputPerM),
              }
            : null,
        },
      })
    },
  })

  const save = async () => {
    setErr('')
    try {
      const r = await add.mutateAsync()
      const res = r.corrallm?.addRemoteModel
      if (!res?.ok) {
        setErr(res?.message ?? 'save refused')
        return
      }
      qc.invalidateQueries({ queryKey: ['providers'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['addRemoteModelData'] })
      onClose()
    } catch (e) {
      setErr(String(e))
    }
  }

  const keyIncomplete =
    (d.keyMode === 'stored' && !d.secretRef) ||
    (d.keyMode === 'new' && (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(d.secretRef) || !d.secretValue))
  const incomplete =
    !/^[a-z0-9][a-z0-9-]*$/.test(d.provider) || !d.baseUrl.trim() || !d.modelId.trim() || keyIncomplete

  return (
    <Dialog open={open} onClose={onClose} maxWidth="md" fullWidth fullScreen={!wide}>
      <DialogTitle>Add a model</DialogTitle>
      <DialogContent dividers>
        <Stack spacing={2.5} sx={{ pt: 0.5 }}>
          {header}

          <Section title="Endpoint">
            <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
              <TextField
                select
                size="small"
                label="Provider preset"
                sx={{ minWidth: 220 }}
                value={d.preset}
                onChange={(e) => choosePreset(e.target.value)}
                helperText="Fills the URL and key name"
              >
                <MenuItem value="">
                  <em>Custom</em>
                </MenuItem>
                {presets.map((p) => (
                  <MenuItem key={p.id} value={p.id}>
                    {p.label}
                  </MenuItem>
                ))}
              </TextField>
              <TextField
                size="small"
                label="Provider name"
                sx={{ width: 200 }}
                value={d.provider}
                onChange={(e) => set({ provider: e.target.value.toLowerCase() })}
                error={!!d.provider && !/^[a-z0-9][a-z0-9-]*$/.test(d.provider)}
                helperText="Prefix of the served name"
              />
            </Box>
            <TextField
              size="small"
              fullWidth
              label="Base URL"
              placeholder="https://api.fireworks.ai/inference/v1"
              value={d.baseUrl}
              onChange={(e) => set({ baseUrl: e.target.value })}
              helperText="As in the provider's OpenAI-compatible docs"
            />
            <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
              <TextField
                size="small"
                label="Model ID"
                sx={{ flex: 2, minWidth: 260 }}
                placeholder="accounts/fireworks/models/deepseek-v4p1-flash"
                value={d.modelId}
                onChange={(e) => set({ modelId: e.target.value })}
                helperText="Exactly as the provider names it"
              />
              <TextField
                size="small"
                label="Short name"
                sx={{ flex: 1, minWidth: 180 }}
                placeholder={suffixOf(d.modelId) || 'deepseek-v4p1-flash'}
                value={d.name}
                onChange={(e) => set({ name: e.target.value })}
                helperText={served ? `Callers ask for ${served}` : 'Default: last part of the ID'}
              />
            </Box>
          </Section>

          <Section title="API key">
            <TextField
              select
              size="small"
              label="Key"
              sx={{ width: 220 }}
              value={d.keyMode}
              onChange={(e) => set({ keyMode: e.target.value as KeyMode })}
            >
              <MenuItem value="stored">Use a stored key</MenuItem>
              <MenuItem value="new">Store a new key</MenuItem>
              <MenuItem value="none">No key</MenuItem>
            </TextField>
            {d.keyMode === 'stored' && (
              <TextField
                select
                size="small"
                label="Stored key"
                sx={{ width: 320 }}
                value={secrets.includes(d.secretRef) ? d.secretRef : ''}
                onChange={(e) => set({ secretRef: e.target.value })}
                helperText={secrets.length ? 'Names only; values are never shown' : 'None stored yet'}
              >
                {secrets.map((s) => (
                  <MenuItem key={s} value={s}>
                    {s}
                  </MenuItem>
                ))}
              </TextField>
            )}
            {d.keyMode === 'new' && (
              <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
                <TextField
                  size="small"
                  label="Key name"
                  sx={{ width: 260 }}
                  placeholder="FIREWORKS_API_KEY"
                  value={d.secretRef}
                  onChange={(e) => set({ secretRef: e.target.value.toUpperCase() })}
                  helperText="Stored outside the config"
                />
                <TextField
                  size="small"
                  type="password"
                  label="Key value"
                  autoComplete="off"
                  sx={{ flex: 1, minWidth: 240 }}
                  value={d.secretValue}
                  onChange={(e) => set({ secretValue: e.target.value })}
                  helperText="Write-only; never displayed again"
                />
              </Box>
            )}
          </Section>

          <Section title="Access and budget" note="Optional. A spend cap refuses requests once reached.">
            <Autocomplete
              multiple
              freeSolo
              size="small"
              options={callers}
              value={d.allow}
              onChange={(_, v) => set({ allow: v })}
              renderInput={(params) => (
                <TextField
                  {...params}
                  label="Allowed callers"
                  helperText="corrallm keys that may use this model. Empty = all callers."
                />
              )}
            />
            <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
              <Money label="Spend cap per day" value={d.usdDay} onChange={(v) => set({ usdDay: v })} />
              <Money label="Spend cap per month" value={d.usdMonth} onChange={(v) => set({ usdMonth: v })} />
              <TextField
                size="small"
                type="number"
                label="Requests per minute"
                sx={{ width: 180 }}
                value={d.rpm}
                onChange={(e) => set({ rpm: e.target.value })}
                helperText="Empty = no limit"
              />
              <TextField
                size="small"
                type="number"
                label="Max output tokens"
                sx={{ width: 180 }}
                value={d.maxTokens}
                onChange={(e) => set({ maxTokens: e.target.value })}
                helperText="When a caller sets none"
              />
            </Box>
          </Section>

          <Section title="Price" note="Per 1M tokens, from the provider's pricing page. Needed for cost and spend caps.">
            <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
              <Money label="Input" value={d.inputPerM} onChange={(v) => set({ inputPerM: v })} />
              <Money
                label="Cached input"
                value={d.cachedPerM}
                onChange={(v) => set({ cachedPerM: v })}
                help="Empty = same as input"
              />
              <Money label="Output" value={d.outputPerM} onChange={(v) => set({ outputPerM: v })} />
            </Box>
            {(num(d.usdDay) > 0 || num(d.usdMonth) > 0) && !priced && (
              <Alert severity="warning">
                A spend cap without a price never fills: every request counts as $0.
              </Alert>
            )}
          </Section>

          {err && <Alert severity="error">{err}</Alert>}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" onClick={save} disabled={incomplete || add.isPending}>
          {add.isPending ? 'Saving…' : 'Add model'}
        </Button>
      </DialogActions>
    </Dialog>
  )
}

function Section(props: { title: string; note?: string; children: React.ReactNode }) {
  return (
    <Stack spacing={1.5}>
      <Divider textAlign="left">
        <Typography variant="overline" sx={{ color: C.textFaint }}>
          {props.title}
        </Typography>
      </Divider>
      {props.note && (
        <Typography variant="caption" sx={{ color: C.textFaint, mt: -1 }}>
          {props.note}
        </Typography>
      )}
      {props.children}
    </Stack>
  )
}

function Money(props: { label: string; value: string; onChange: (v: string) => void; help?: string }) {
  return (
    <TextField
      size="small"
      type="number"
      label={props.label}
      sx={{ width: 180 }}
      value={props.value}
      onChange={(e) => props.onChange(e.target.value)}
      helperText={props.help}
      slotProps={{
        htmlInput: { min: 0, step: 0.001 },
        input: { startAdornment: <InputAdornment position="start">$</InputAdornment> },
      }}
    />
  )
}
