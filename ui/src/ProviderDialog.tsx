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
      listProviders {
        pools {
          extension
          lanes {
            lane
          }
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

/**
 * The accounts to save: the first one edited by the form (its key name and
 * requests/minute), every other field and account passed through unchanged.
 */
function credentialsFor(existing: CredentialValue[], secretRef: string, rpm: string) {
  if (!secretRef && existing.length === 0) return []
  const [first, ...rest] = existing.length ? existing : [{ name: 'default', secretRef: '', headerName: '', allow: [], limits: [] }]
  const edited = {
    name: first.name,
    secretRef,
    headerName: first.headerName,
    allow: first.allow,
    limits: [
      ...first.limits.filter((l) => !isRpm(l)),
      ...(rpm ? [{ req: Number(rpm), per: 'minute' }] : []),
    ],
  }
  return [edited, ...rest]
}

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
  // WHAT EACH CHOICE DOES TO A REQUEST, which is the only reason to ask.
  //
  // The field was labelled "Extension" — the config's word for the block this
  // ends up in — with the helper "Groups it in config", which describes what
  // happens to the FILE. The choice is not cosmetic: a provider added to a
  // POOLING extension has its free models enrolled into that extension's lane
  // automatically, and one added anywhere else does not. That is the sentence
  // the field owed and did not say (OB-3, OB-12).
  const meaning = useMemo(() => {
    const byName = new Map<string, string>()
    const pools = data?.corrallm?.listProviders?.pools ?? []
    for (const p of pools) {
      const lanes = (p.lanes ?? []).map((l) => l.lane).filter(Boolean)
      byName.set(
        p.extension,
        lanes.length > 0
          ? `its free models join the ${lanes.join(', ')} lane automatically`
          : 'it pools its members catalogues',
      )
    }
    for (const e of data?.corrallm?.extensions?.extensions ?? []) {
      if (byName.has(e.name)) continue
      // Deliberately says only what is KNOWN here. Whether this extension runs
      // a local program is not on this query, and guessing it from the process
      // state would be wrong for anything not currently loaded.
      byName.set(e.name, 'grouped here — nothing is enrolled into a lane automatically')
    }
    return byName
  }, [data])
  return { data: data?.corrallm?.listProviderPresets, extensions, meaning }
}

// wireFormat names the protocol in words rather than echoing the config token.
// Every preset is "openai" today; anything else is shown as itself rather than
// guessed at, so a second format arriving does not silently read as the first.
function wireFormat(api?: string | null): string {
  if (!api || api === 'openai') return 'OpenAI-compatible'
  return api
}

export type LimitValue = { req?: number; usd?: number; sec?: number; per: string; hard?: boolean }

/** Normalises a limit as the query returns it into what the mutation takes. */
export function toLimit(l: {
  req?: number | null
  usd?: number | null
  sec?: number | null
  per: string
  hard?: boolean | null
}): LimitValue {
  return {
    ...(l.req ? { req: l.req } : {}),
    ...(l.usd ? { usd: l.usd } : {}),
    ...(l.sec ? { sec: l.sec } : {}),
    per: l.per,
    ...(l.hard ? { hard: true } : {}),
  }
}

export type CredentialValue = {
  name: string
  secretRef: string
  headerName: string
  allow: string[]
  limits: LimitValue[]
}

export type ProviderInitial = {
  extension: string
  name: string
  host: string
  port: string
  basePath: string
  manual: boolean
  directory: null | {
    freeOnly: boolean
    minContext: string
  }
  /**
   * Carried so a save sends them back. The form edits one key name, a
   * requests/minute limit and a monthly cap; everything else on the provider's
   * accounts (allowlists, spend caps, other accounts) must survive the save.
   */
  limits: LimitValue[]
  credentials: CredentialValue[]
  /** The provider-wide cap: in flight across all its models, and its queue. */
  cap: { maxConcurrent: number; maxQueueDepth: number; maxWait: string }
  /** Serving the whole catalogue, and the defaults each such model gets. Null: off. */
  passthrough: null | {
    maxConcurrent: number
    maxQueueDepth: number
    maxWait: string
    type: string
    inputPerM: number
    outputPerM: number
    models: number
  }
}

const isRpm = (l: LimitValue) => !!l.req && l.per === 'minute'
const isMonthlyUsd = (l: LimitValue) => !!l.usd && l.per === 'month' && !l.hard

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
  freeOnly: boolean
  minContext: string
  capConcurrent: string
  capQueue: string
  capWait: string
  pt: boolean
  ptConcurrent: string
  ptQueue: string
  ptWait: string
  ptInputPerM: string
  ptOutputPerM: string
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
  freeOnly: true,
  minContext: '8192',
  capConcurrent: '',
  capQueue: '',
  capWait: '',
  pt: false,
  ptConcurrent: '',
  ptQueue: '',
  ptWait: '',
  ptInputPerM: '',
  ptOutputPerM: '',
}

// '' for 0, so an unset number shows as an empty field rather than a "0" that
// reads like a limit of nothing.
const num = (n?: number | null) => (n ? String(n) : '')
// '30' is read as seconds; a duration such as 2m is kept as written.
const toWait = (v: string) => {
  const t = v.trim()
  return /^\d+(\.\d+)?$/.test(t) ? `${t}s` : t
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
  /**
   * Which group a NEW provider joins, when the dialog was opened from one.
   *
   * Separate from `initial` on purpose: `initial` means EDITING, and reusing it
   * to carry a default would disable the name field on a provider that does not
   * exist yet.
   */
  defaultExtension?: string
  secrets: string[]
}) {
  const { open, onClose, initial, defaultExtension, secrets } = props
  const qc = useQueryClient()
  // Six sections of form; on a phone a centred dialog turns that into a
  // scroll-in-a-scroll with fields clipped at both edges.
  const wide = useMediaQuery(theme.breakpoints.up('sm'))
  const { data, extensions, meaning } = usePresets()
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
            freeOnly: initial.directory?.freeOnly ?? false,
            minContext: initial.directory?.minContext ?? '0',
            secretRef: initial.credentials[0]?.secretRef ?? '',
            rpm: String(initial.credentials[0]?.limits.find(isRpm)?.req ?? ''),
            monthlyUSD: String(initial.limits.find(isMonthlyUsd)?.usd ?? ''),
            capConcurrent: num(initial.cap.maxConcurrent),
            capQueue: num(initial.cap.maxQueueDepth),
            capWait: initial.cap.maxWait,
            pt: initial.passthrough != null,
            ptConcurrent: num(initial.passthrough?.maxConcurrent),
            ptQueue: num(initial.passthrough?.maxQueueDepth),
            ptWait: initial.passthrough?.maxWait ?? '',
            ptInputPerM: num(initial.passthrough?.inputPerM),
            ptOutputPerM: num(initial.passthrough?.outputPerM),
          }
        : BLANK,
    )
  }, [open, initial])

  // The extension list arrives asynchronously, so the default is filled in when
  // it lands — but never OVER a choice already made, which is what separates
  // this from the reset above.
  useEffect(() => {
    if (!open || extensions.length === 0) return
    // Opened FROM a group, the answer is already given: that is the group. The
    // fallback only covers the global button, where nobody has said.
    const fallback = extensions.includes('free') ? 'free' : extensions[0]
    const wanted =
      defaultExtension && extensions.includes(defaultExtension) ? defaultExtension : fallback
    setD((s) => (s.extension ? s : { ...s, extension: wanted }))
  }, [open, extensions, defaultExtension])

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
        manual: true,
        limits: [
          ...(initial?.limits ?? []).filter((l) => !isMonthlyUsd(l)),
          ...(d.monthlyUSD ? [{ usd: Number(d.monthlyUSD), per: 'month' }] : []),
        ],
        directory: {
          freeOnly: d.freeOnly,
          inputModality: '',
          outputModality: '',
          minContext: String(Number(d.minContext || 0)),
          limit: '0',
          quality: 0,
        },
        credentials: credentialsFor(initial?.credentials ?? [], d.secretRef, d.rpm),
        maxConcurrent: String(Number(d.capConcurrent || 0)),
        maxQueueDepth: String(Number(d.capQueue || 0)),
        maxWait: toWait(d.capWait),
        passthrough: {
          enabled: d.pt,
          maxConcurrent: String(Number(d.ptConcurrent || 0)),
          maxQueueDepth: String(Number(d.ptQueue || 0)),
          maxWait: toWait(d.ptWait),
          type: initial?.passthrough?.type ?? '',
          inputPerM: Number(d.ptInputPerM || 0),
          outputPerM: Number(d.ptOutputPerM || 0),
        },
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
      qc.invalidateQueries({ queryKey: ['overview'] })
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
              size="small"
              label="Name"
              value={d.name}
              onChange={(e) => set({ name: e.target.value })}
              disabled={editing}
              fullWidth
              helperText={`Served models become ${d.name || '<name>'}-<model id>`}
            />
            {/* The fact somebody expects to see here, rather than a question
                about the config's own nesting. It is READ-ONLY because it is
                not a choice: corrallm builds `basePath + "/v1/..."` and reaches
                one wire format. Gemini and Z.ai are absent from the preset
                table for exactly that reason, which is why the field exists in
                the data before anything offers to change it. */}
            {/* Not a TextField. It is a fact, not an input, and dressing it
                as one both invited a click that does nothing and lost the
                argument for width against the fullWidth Name beside it —
                clipping "OpenAI-compatible" to "OpenAI-compatib". */}
            <Box sx={{ minWidth: 170, pt: 0.5 }}>
              <Typography variant="caption" sx={{ color: C.textFaint, display: 'block' }}>
                Speaks
              </Typography>
              <Typography variant="body2" sx={{ whiteSpace: 'nowrap' }}>
                {wireFormat(preset?.api)}
              </Typography>
              <Typography variant="caption" sx={{ color: C.textMuted }}>
                The only format corrallm reaches
              </Typography>
            </Box>
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
            <Typography variant="caption">Provider limit</Typography>
          </Divider>
          <Typography variant="caption" sx={{ color: C.textFaint, mt: -1 }}>
            Requests in flight across ALL this provider&apos;s models together, on top of each
            model&apos;s own limit. Empty = no provider limit. Past it, a request gets a 429 with
            Retry-After — unless you set a line below: then it waits, and a full line or a wait
            past the longest wait gets the 429. A caller whose group falls back moves on instead.
          </Typography>
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
            <TextField
              size="small"
              label="Requests in flight"
              value={d.capConcurrent}
              onChange={(e) => set({ capConcurrent: e.target.value.replace(/\D/g, '') })}
              sx={{ width: 160 }}
              helperText="All models together"
            />
            <TextField
              size="small"
              label="Requests waiting"
              value={d.capQueue}
              onChange={(e) => set({ capQueue: e.target.value.replace(/\D/g, '') })}
              sx={{ width: 160 }}
              helperText="Empty = box default"
              disabled={!d.capConcurrent}
            />
            <TextField
              size="small"
              label="Longest wait (seconds)"
              value={d.capWait}
              onChange={(e) => set({ capWait: e.target.value })}
              sx={{ width: 180 }}
              helperText="Or a duration, e.g. 2m"
              disabled={!d.capConcurrent}
            />
          </Stack>

          <Divider textAlign="left">
            <Typography variant="caption">Serve the whole catalogue</Typography>
          </Divider>
          <FormControlLabel
            control={<Switch size="small" checked={d.pt} onChange={(e) => set({ pt: e.target.checked })} />}
            label={
              <Typography variant="body2">
                Serve every model this provider lists, as {d.name || '<name>'}-&lt;model id&gt;
              </Typography>
            }
          />
          <Typography variant="caption" sx={{ color: C.textFaint, mt: -1.5 }}>
            Only ids in the provider&apos;s /v1/models are served; the list is read every 30 minutes.
            Prices come from the list where it has them.
            {initial?.passthrough ? ` Serving ${initial.passthrough.models} models now.` : ''} To
            give one model its own settings, add it by hand under the same name.
          </Typography>
          {d.pt && (
            <>
              <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
                <TextField
                  size="small"
                  label="Requests in flight, each"
                  value={d.ptConcurrent}
                  onChange={(e) => set({ ptConcurrent: e.target.value.replace(/\D/g, '') })}
                  sx={{ width: 180 }}
                  helperText="Per model. Empty = 1"
                />
                <TextField
                  size="small"
                  label="Requests waiting, each"
                  value={d.ptQueue}
                  onChange={(e) => set({ ptQueue: e.target.value.replace(/\D/g, '') })}
                  sx={{ width: 180 }}
                  helperText="Per model, if the caller's group queues"
                />
                <TextField
                  size="small"
                  label="Longest wait (seconds)"
                  value={d.ptWait}
                  onChange={(e) => set({ ptWait: e.target.value })}
                  sx={{ width: 180 }}
                  helperText="Per model. Or e.g. 2m"
                />
              </Stack>
              <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
                <TextField
                  size="small"
                  label="$ per 1M input tokens"
                  value={d.ptInputPerM}
                  onChange={(e) => set({ ptInputPerM: e.target.value })}
                  sx={{ width: 180 }}
                  helperText="Only where the list has no price"
                />
                <TextField
                  size="small"
                  label="$ per 1M output tokens"
                  value={d.ptOutputPerM}
                  onChange={(e) => set({ ptOutputPerM: e.target.value })}
                  sx={{ width: 180 }}
                  helperText="Only where the list has no price"
                />
              </Stack>
              <Typography variant="caption" sx={{ color: C.textFaint, mt: -1 }}>
                With a $ limit on this provider, a model with no price from the list or here is not
                served, so no spend goes uncounted.
              </Typography>
            </>
          )}

          <Divider textAlign="left">
            <Typography variant="caption">Directory defaults</Typography>
          </Divider>
          <Typography variant="caption" sx={{ color: C.textFaint, mt: -1 }}>
            Models are chosen from this provider&apos;s directory — nothing is imported
            automatically. These are just what <strong>Browse</strong> opens pre-filtered to, and
            you can clear them there.
          </Typography>
          {/* MOVED DOWN HERE, and asked by its consequence.
              It used to be the second field in the dialog, labelled with the
              config's word for the block the entry lands in and helped by
              "Groups it in config" — a question about the file, asked before
              the operator had said what the thing even was. It belongs beside
              the pooling it decides, which this section was already describing
              in prose. */}
          <TextField
            select
            size="small"
            label="Group it with"
            value={d.extension}
            onChange={(e) => set({ extension: e.target.value })}
            disabled={editing}
            sx={{ maxWidth: 420 }}
            // The CLOSED control shows the name alone. Without this it renders
            // the whole menu item, so the description appeared twice — once
            // inside the box and again as the helper under it.
            slotProps={{ select: { renderValue: (v) => String(v) } }}
            helperText={meaning.get(d.extension) ?? 'Which group of providers this joins'}
          >
            {extensions.map((x) => (
              <MenuItem key={x} value={x}>
                <Box>
                  <Typography variant="body2">{x}</Typography>
                  <Typography variant="caption" sx={{ color: C.textMuted }}>
                    {meaning.get(x)}
                  </Typography>
                </Box>
              </MenuItem>
            ))}
          </TextField>
          {(
            <Stack
              direction={{ xs: 'column', sm: 'row' }}
              spacing={2}
              useFlexGap
              sx={{
                alignItems: { sm: 'center' },
                flexWrap: "wrap"
              }}>
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
  );
}
