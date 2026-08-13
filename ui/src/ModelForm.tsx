import {
  Alert,
  Box,
  Chip,
  FormControlLabel,
  MenuItem,
  Stack,
  Switch,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material'
import { C } from '@/theme'

/**
 * The fields of a model worth a form.
 *
 * Deliberately not every field. A model also carries swap, contextPerRequest,
 * modalities, convert and freeTier, and modelling those here
 * would be a worse YAML editor rather than a better form. The server MERGES
 * this spec onto the stored model instead of replacing it, so a field absent
 * from this shape survives being saved from here — which is what makes a
 * partial form safe, and is the thing that previously made one impossible.
 */
export type ModelSpec = {
  name: string
  cmd: string
  server: string
  proxy: string
  upstream: string
  type: string
  quality: number
  maxConcurrent: number
  maxTokens: number
  persistent: boolean
  stickyTtl: string
  stickyIdleUnload: string
  stickyEvictCost: string
  ramUsage: Record<string, string>
  notes: string
}

export function blankSpec(): ModelSpec {
  return {
    name: '',
    cmd: '',
    server: '',
    proxy: '',
    upstream: '',
    type: 'chat',
    quality: 1,
    maxConcurrent: 1,
    maxTokens: 0,
    persistent: false,
    stickyTtl: '',
    stickyIdleUnload: '',
    stickyEvictCost: '',
    ramUsage: {},
    notes: '',
  }
}

export type ServerOption = {
  server: string
  pools: string[]
  noProcessMemory: boolean
  agentStatus?: string | null
}

// Cost classes the scheduler knows. Free text would be a typo waiting to route
// nothing; the list is short and changes rarely.
const TYPES = ['chat', 'embed', 'stt', 'tts', 'realtime', 'image', 'rerank']

/**
 * ModelForm edits the common shape of a model.
 *
 * Two things it does that a generic form would not, both because getting them
 * wrong is expensive rather than annoying:
 *
 * - ramUsage is offered as one row PER POOL of the selected server, not as free
 *   text. A pool name the server does not declare is accepted by the YAML
 *   editor and then charges every measured footprint against a budget of zero,
 *   which surfaces as a permanent capacity error that reads like a backend
 *   fault. Choosing from the server's own pools makes that unspellable.
 *
 * - a host that cannot measure per-process memory says so, right where the
 *   number has to be typed, because on that host the declared figure is the
 *   only one anything will ever have.
 */
export function ModelForm({
  spec,
  onChange,
  servers,
  advanced,
  existing,
}: {
  spec: ModelSpec
  onChange: (s: ModelSpec) => void
  servers: ServerOption[]
  advanced: string[]
  existing: boolean
}) {
  const set = <K extends keyof ModelSpec>(k: K, v: ModelSpec[K]) => onChange({ ...spec, [k]: v })

  const srv = servers.find((s) => s.server === spec.server)
  const spawned = spec.cmd.trim() !== ''
  // ramUsage is only meaningful for something this daemon spawns; a pure proxy
  // consumes no local pool at all, and offering the field would imply it does.
  const showRAM = spawned && !!srv

  return (
    <Stack spacing={2} sx={{ mt: 1 }}>
      <TextField
        label="Name"
        size="small"
        value={spec.name}
        disabled={existing}
        onChange={(e) => set('name', e.target.value)}
        helperText="The name callers request, and the config key. Renaming means add + delete."
      />

      <TextField
        label="Spawn command"
        size="small"
        value={spec.cmd}
        onChange={(e) => set('cmd', e.target.value)}
        multiline
        minRows={2}
        slotProps={{ input: { sx: { fontFamily: 'monospace', fontSize: 12.5 } } }}
        helperText={
          spawned
            ? 'Run by corrallm on the server below. Leave empty to make this a pure proxy to something already running.'
            : 'Empty: this is a pure proxy — corrallm forwards to it and never starts or stops it.'
        }
      />

      <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
        <TextField
          select
          label="Server"
          size="small"
          sx={{ minWidth: 220 }}
          value={spec.server}
          onChange={(e) => set('server', e.target.value)}
          // A spawned model MUST name a server; config validation rejects it
          // otherwise, and saying so here beats a round trip to find out.
          error={spawned && !spec.server}
          helperText={
            spawned && !spec.server
              ? 'Required: a spawned model draws capacity from somewhere'
              : 'Which machine runs it'
          }
        >
          <MenuItem value="">
            <em>none (pure proxy)</em>
          </MenuItem>
          {servers.map((s) => (
            <MenuItem key={s.server} value={s.server}>
              {s.server}
              {s.agentStatus && s.agentStatus !== 'local' ? ` — ${s.agentStatus}` : ''}
            </MenuItem>
          ))}
        </TextField>

        <TextField
          label="Proxy"
          size="small"
          sx={{ minWidth: 180 }}
          value={spec.proxy}
          onChange={(e) => set('proxy', e.target.value)}
          error={!spec.proxy.trim()}
          helperText={
            !spec.proxy.trim()
              ? 'Required: where to forward'
              : 'A port (5800), host:port, or a URL'
          }
        />

        <TextField
          label="Upstream id"
          size="small"
          sx={{ minWidth: 180 }}
          value={spec.upstream}
          onChange={(e) => set('upstream', e.target.value)}
          helperText="What the backend calls it, if different"
        />
      </Box>

      <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
        <TextField
          select
          label="Type"
          size="small"
          sx={{ minWidth: 140 }}
          value={spec.type}
          onChange={(e) => set('type', e.target.value)}
          helperText="Cost class"
        >
          {TYPES.map((t) => (
            <MenuItem key={t} value={t}>
              {t}
            </MenuItem>
          ))}
        </TextField>

        <TextField
          label="Quality"
          size="small"
          type="number"
          sx={{ width: 130 }}
          value={spec.quality}
          onChange={(e) => set('quality', Number(e.target.value))}
          // Fractional on purpose: a 4-bit Mac tier legitimately sits between
          // two integer tiers, and forcing it onto one of them changes routing.
          slotProps={{ htmlInput: { step: 0.5 } }}
          helperText="Higher wins; fractions fine"
        />

        <TextField
          label="Max concurrent"
          size="small"
          type="number"
          sx={{ width: 150 }}
          value={spec.maxConcurrent}
          onChange={(e) => set('maxConcurrent', Number(e.target.value))}
          helperText="Admission slots (≈ --parallel)"
        />

        <TextField
          label="Max tokens"
          size="small"
          type="number"
          sx={{ width: 140 }}
          value={spec.maxTokens}
          onChange={(e) => set('maxTokens', Number(e.target.value))}
          helperText="0 = no clamp"
        />
      </Box>

      <FormControlLabel
        control={
          <Switch checked={spec.persistent} onChange={(e) => set('persistent', e.target.checked)} />
        }
        label={
          <Typography variant="body2">
            Persistent — preloaded at boot and never evicted
          </Typography>
        }
      />

      <Box>
        <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
          Residency
        </Typography>
        <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
          <TextField
            size="small"
            label="Idle unload"
            value={spec.stickyIdleUnload}
            onChange={(e) => set('stickyIdleUnload', e.target.value)}
            disabled={spec.persistent}
            placeholder="5m"
            helperText="Quiet period, then it unloads itself. Empty = never."
          />
          <TextField
            size="small"
            label="Evict after"
            value={spec.stickyTtl}
            onChange={(e) => set('stickyTtl', e.target.value)}
            disabled={spec.persistent}
            placeholder="2m"
            helperText="Idle this long → first in line as a victim. Never unloads on its own."
          />
          <TextField
            size="small"
            select
            label="Evict cost"
            value={spec.stickyEvictCost}
            onChange={(e) => set('stickyEvictCost', e.target.value)}
            disabled={spec.persistent}
            sx={{ minWidth: 140 }}
            helperText="Resistance once a victim"
          >
            {['', 'low', 'medium', 'high'].map((v) => (
              <MenuItem key={v || 'unset'} value={v}>
                {v || '(unset)'}
              </MenuItem>
            ))}
          </TextField>
        </Box>
      </Box>

      {showRAM && (
        <Box>
          <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
            Declared footprint
            {srv?.noProcessMemory && (
              <Tooltip title="This host cannot attribute memory to a single process (macOS has no nvidia-smi equivalent), so nothing will ever measure this model. The number you type is the only one the scheduler will ever have.">
                <Chip
                  size="small"
                  color="warning"
                  variant="outlined"
                  label="required here"
                  sx={{ ml: 1 }}
                />
              </Tooltip>
            )}
          </Typography>
          <Typography variant="caption" sx={{ color: C.textFaint, display: 'block', mb: 1 }}>
            {srv?.noProcessMemory
              ? `${srv.server} cannot measure per-process memory, so this is authoritative rather than advisory.`
              : 'A bootstrap hint. Once corrallm has measured a real spawn it prefers what it measured.'}
          </Typography>
          <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
            {(srv?.pools ?? []).map((pool) => (
              <TextField
                key={pool}
                label={pool}
                size="small"
                sx={{ width: 150 }}
                value={spec.ramUsage[pool] ?? ''}
                onChange={(e) => {
                  const next = { ...spec.ramUsage }
                  // Delete rather than store "": an empty size fails to parse
                  // at load, and the absent case already means "unknown".
                  if (e.target.value.trim() === '') delete next[pool]
                  else next[pool] = e.target.value
                  set('ramUsage', next)
                }}
                placeholder="16GB"
              />
            ))}
          </Box>
          {/* A pool that is no longer one of the server's own. It cannot be
              typed here, but it CAN arrive from YAML or survive a server
              being resized — and it charges against a budget of zero. */}
          {Object.keys(spec.ramUsage)
            .filter((p) => !(srv?.pools ?? []).includes(p))
            .map((p) => (
              <Alert key={p} severity="error" sx={{ mt: 1 }}>
                ramUsage names pool <code>{p}</code>, which {srv?.server} does not declare. It will
                be charged against a budget of zero and the model will never be schedulable.
              </Alert>
            ))}
        </Box>
      )}

      <TextField
        label="Notes"
        size="small"
        value={spec.notes}
        onChange={(e) => set('notes', e.target.value)}
        multiline
        minRows={2}
        helperText="Why this model is configured the way it is. Kept in the config and shown beside it."
      />

      {advanced.length > 0 && (
        <Alert severity="info">
          This model also sets <strong>{advanced.join(', ')}</strong>, which this form does not
          edit. Saving here leaves those untouched — switch to YAML to change them.
        </Alert>
      )}
    </Stack>
  )
}
