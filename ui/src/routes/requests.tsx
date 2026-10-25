import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  IconButton,
  MenuItem,
  Stack,
  Switch,
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
import DeleteOutline from '@mui/icons-material/DeleteOutlined'
import AddIcon from '@mui/icons-material/Add'
import ContentCopyIcon from '@mui/icons-material/ContentCopy'
import { graphql } from '@/gql'
import type { RequestsQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { Panel, PageHeader, Row } from '@/Panel'
import { Loading } from '@/Loading'
import { extractMessage, fmtAgo, fmtUSD } from '@/format'
import { useReadOnly } from '@/auth'
import { C } from '@/theme'

/**
 * Requests: what agents (and people) asked for, and every key this box handed
 * out at runtime (P62).
 *
 * An agent files POST /v1/access-requests and polls with a claim secret; the
 * owner approves here, EDITING what is granted — the request is a proposal,
 * not an order. The approved key goes to the requester on its next poll, once,
 * so no secret passes through a person or a chat.
 *
 * A grant has three kinds of money, and the page says which is which:
 *   - a budget that refills (per day, per calendar month, …),
 *   - a one-off total (per: total) that never refills,
 *   - a value add: an extra amount with its own expiry, spent only once the
 *     key's own hard budgets refuse.
 * And a key can expire, after which it answers 401 — "disable" is "expire now".
 *
 * A value add raises the KEY's allowance only. A model's or provider's own cap
 * (the model-wide DeepSeek $3/day) still applies to everyone; the approve
 * dialog says so when the asked models have one.
 */

const RequestsDoc = graphql(/* GraphQL */ `
  query Requests {
    corrallm {
      listRequests {
        requests {
          id
          status
          requesterKey
          sourceIp
          via
          filedBy
          asked {
            label
            purpose
            models
            group
            credentials
            limits {
              req
              usd
              per
              hard
              model
            }
            valueAdds {
              usd
              model
              ttl
              expires
              note
            }
            ttl
            expires
            mint
            askedLimits
            askedMint
            
          }
          granted {
            label
            purpose
            models
            group
            credentials
            limits {
              req
              usd
              per
              hard
              model
            }
            valueAdds {
              usd
              model
              ttl
              expires
              note
            }
            ttl
            expires
            mint
            askedLimits
            askedMint
            
          }
          issuedKey
          delivered
          reason
          created
          decided
          decidedBy
        }
        keys {
          key
          kind
          parent
          label
          group
          credentials
          mint
          limits {
            req
            usd
            per
            hard
            model
          }
          created
          expires
          expired
          valueAdds {
            id
            key
            usd
            spent
            model
            expires
            created
            active
            note
          }
          windows {
            scope
            label
            limit
            used
            blocked
            hard
            resetsAt
          }
        }
        credentials {
          scope
          provider
          name
          open
          models
        }
        groups
        models
        rules {
          name
          cidrs
          token
          users
        }
        defaultRules
      }
    }
  }

`)

const ApproveDoc = graphql(/* GraphQL */ `
  mutation ApproveRequest($body: corrallm_ApproveRequestInputBodyInput!) {
    corrallm {
      approveRequest(body: $body) {
        ok
        key
      }
    }
  }
`)

const DenyDoc = graphql(/* GraphQL */ `
  mutation DenyRequest($body: corrallm_DenyRequestInputBodyInput!) {
    corrallm {
      denyRequest(body: $body) {
        ok
      }
    }
  }
`)

const UpdateKeyDoc = graphql(/* GraphQL */ `
  mutation UpdateManagedKey($body: corrallm_UpdateManagedKeyInputBodyInput!) {
    corrallm {
      updateManagedKey(body: $body) {
        ok
      }
    }
  }
`)

const KeyActionDoc = graphql(/* GraphQL */ `
  mutation ManagedKeyAction($body: corrallm_ManagedKeyActionInputBodyInput!) {
    corrallm {
      managedKeyAction(body: $body) {
        ok
      }
    }
  }
`)

const ValueAddDoc = graphql(/* GraphQL */ `
  mutation EditValueAdd($body: corrallm_ValueAddInputBodyInput!) {
    corrallm {
      editValueAdd(body: $body) {
        ok
      }
    }
  }
`)

const RulesDoc = graphql(/* GraphQL */ `
  mutation SetRequestRules($body: corrallm_RequestRulesInputBodyInput!) {
    corrallm {
      setRequestRules(body: $body) {
        ok
        message
      }
    }
  }
`)

type Data = NonNullable<NonNullable<RequestsQuery['corrallm']>['listRequests']>
type Req = Data['requests'][number]
type Key = Data['keys'][number]
type Cred = Data['credentials'][number]

// The editable form of a grant: the dialog's state.
type Lim = { amount: string; unit: 'usd' | 'req'; per: string; hard: boolean; model: string }
type VAdd = { usd: string; model: string; ttl: string; note: string }
type Form = {
  label: string
  group: string
  credentials: string[]
  limits: Lim[]
  valueAdds: VAdd[]
  ttl: string
  mint: boolean
  reason: string
}

const PERS: { v: string; label: string }[] = [
  { v: 'calendar-day', label: 'per day (resets 00:00 UTC)' },
  { v: 'calendar-month', label: 'per month (resets on the 1st)' },
  { v: 'total', label: 'one-off total (never resets)' },
  { v: 'hour', label: 'per hour (rolling)' },
  { v: 'day', label: 'per 24 h (rolling)' },
  { v: 'month', label: 'per 30 days (rolling)' },
]

function perLabel(per: string): string {
  return PERS.find((p) => p.v === per)?.label ?? per
}

type LimitIn = { req?: number | null; usd?: number | null; per: string; hard?: boolean | null; model?: string | null }

function toLim(l: LimitIn): Lim {
  const usd = (l.usd ?? 0) > 0
  return {
    amount: String(usd ? l.usd : (l.req ?? '')),
    unit: usd ? 'usd' : 'req',
    per: l.per,
    hard: !!l.hard,
    model: l.model ?? '',
  }
}

function limitText(l: LimitIn): string {
  const amt = (l.usd ?? 0) > 0 ? fmtUSD(l.usd ?? 0) : `${l.req} requests`
  return `${amt} ${perLabel(l.per)}${l.model ? ` on ${l.model}` : ''}${l.hard ? '' : ' (soft: reported only)'}`
}

/** Credentials a request's models need: restricted ones serving those models. */
function proposedCreds(models: string[], creds: Cred[]): string[] {
  return creds.filter((c) => !c.open && (c.models ?? []).some((m) => models.includes(m))).map((c) => c.scope)
}

function formFromRequest(r: Req, creds: Cred[]): Form {
  const a = r.asked
  return {
    label: a.label ?? '',
    group: a.group ?? '',
    credentials: a.credentials?.length ? a.credentials : proposedCreds(a.models ?? [], creds),
    limits: (a.limits ?? []).map(toLim),
    valueAdds: (a.valueAdds ?? []).map((v) => ({ usd: String(v.usd), model: v.model ?? '', ttl: v.ttl ?? '', note: v.note ?? '' })),
    ttl: a.ttl ?? '',
    mint: !!a.mint,
    reason: '',
  }
}

function formFromKey(k: Key): Form {
  return {
    label: k.label ?? '',
    group: k.group ?? '',
    credentials: k.credentials ?? [],
    limits: k.limits.map(toLim),
    valueAdds: [],
    ttl: '',
    mint: k.mint,
    reason: '',
  }
}

function formToGrant(f: Form) {
  return {
    label: f.label.trim(),
    group: f.group,
    credentials: f.credentials,
    limits: f.limits
      .filter((l) => l.amount.trim() !== '')
      .map((l) => ({
        per: l.per,
        hard: l.hard,
        model: l.model || undefined,
        ...(l.unit === 'usd' ? { usd: Number(l.amount) } : { req: Number(l.amount) }),
      })),
    valueAdds: f.valueAdds
      .filter((v) => v.usd.trim() !== '')
      .map((v) => ({ usd: Number(v.usd), model: v.model || undefined, ttl: v.ttl.trim() || undefined, note: v.note.trim() || undefined })),
    ttl: f.ttl.trim() || undefined,
    mint: f.mint,
  }
}

function useData() {
  return useQuery({
    queryKey: ['requests'],
    queryFn: () => gqlClient.request(RequestsDoc),
    refetchInterval: 10_000,
  })
}

function Requests() {
  const ro = useReadOnly()
  const q = useData()
  const d = q.data?.corrallm?.listRequests
  if (ro) {
    return <Alert severity="info">Requests are for admins: the page shows keys.</Alert>
  }
  if (q.error) return <Alert severity="error">{extractMessage(q.error)}</Alert>
  if (!d) return <Loading />
  const pending = d.requests.filter((r) => r.status === 'pending')
  const decided = d.requests.filter((r) => r.status !== 'pending')
  return (
    <Stack spacing={2}>
      <PageHeader title="Requests">
        {pending.length > 0 && <Chip size="small" color="warning" label={`${pending.length} waiting`} />}
      </PageHeader>
      <Panel
        title="Waiting for a decision"
        subtitle="What agents asked for"
        note="Approve lets you edit what is granted. The requester collects a new key on its next poll, once."
      >
        {pending.length === 0 ? (
          <Row>
            <Typography variant="body2" color="text.secondary">
              Nothing waiting. Agents file with POST /v1/access-requests (see /v1/capabilities).
            </Typography>
          </Row>
        ) : (
          pending.map((r) => <PendingRow key={r.id} r={r} d={d} />)
        )}
      </Panel>
      <KeysPanel d={d} />
      <RulesPanel d={d} />
      {decided.length > 0 && <DecidedPanel rows={decided} />}
    </Stack>
  )
}

function askedSummary(r: Req): string[] {
  const a = r.asked
  const out: string[] = []
  if (a.models?.length) out.push(`models: ${a.models.join(', ')}`)
  if (a.askedLimits) out.push(a.limits?.length ? `budgets: ${(a.limits ?? []).map(limitText).join('; ')}` : 'budgets: none (unlimited)')
  for (const v of a.valueAdds ?? []) out.push(`value add: ${fmtUSD(v.usd)}${v.model ? ` on ${v.model}` : ''}${v.ttl ? `, expires after ${v.ttl}` : ''}`)
  if (a.ttl || a.expires) out.push(`key expires: ${a.ttl ? `after ${a.ttl}` : a.expires}`)
  if (a.askedMint) out.push(a.mint ? 'may mint child keys' : 'no child keys')
  return out
}

function PendingRow({ r, d }: { r: Req; d: Data }) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [denyOpen, setDenyOpen] = useState(false)
  const [reason, setReason] = useState('')
  const [err, setErr] = useState('')
  const deny = useMutation({
    mutationFn: () => gqlClient.request(DenyDoc, { body: { id: r.id, reason } }),
    onSuccess: () => {
      setDenyOpen(false)
      void qc.invalidateQueries({ queryKey: ['requests'] })
    },
    onError: (e) => setErr(extractMessage(e)),
  })
  const who = r.requesterKey ? `More for key ${r.requesterKey}` : `New key “${r.asked.label}”`
  return (
    <Row>
      <Box sx={{ display: 'flex', gap: 2, alignItems: 'flex-start', flexWrap: 'wrap' }}>
        <Box sx={{ flex: 1, minWidth: 260 }}>
          <Typography variant="subtitle2">{who}</Typography>
          <Typography variant="body2" sx={{ my: 0.5, whiteSpace: 'pre-wrap' }}>
            {r.asked.purpose}
          </Typography>
          {askedSummary(r).map((s) => (
            <Typography key={s} variant="caption" sx={{ display: 'block' }} color="text.secondary">
              {s}
            </Typography>
          ))}
          <Typography variant="caption" color="text.secondary">
            from {r.sourceIp}
            {r.filedBy ? ` (${r.filedBy})` : ''} · rule: {r.via} · {fmtAgo(r.created)}
          </Typography>
        </Box>
        <Stack direction="row" spacing={1}>
          <Button variant="contained" size="small" onClick={() => setOpen(true)}>
            Review and approve
          </Button>
          <Button color="error" size="small" onClick={() => setDenyOpen(true)}>
            Deny
          </Button>
        </Stack>
      </Box>
      {open && (
        <GrantDialog
          title={who}
          d={d}
          initial={formFromRequest(r, d.credentials)}
          askedModels={r.asked.models ?? []}
          forConfiguredKey={!!r.requesterKey && d.keys.some((k) => k.key === r.requesterKey && k.kind === 'configured')}
          withValueAdds
          withReason
          submitLabel="Approve"
          onClose={() => setOpen(false)}
          onSubmit={async (f) => {
            await gqlClient.request(ApproveDoc, { body: { id: r.id, granted: formToGrant(f), reason: f.reason } })
            void qc.invalidateQueries({ queryKey: ['requests'] })
          }}
        />
      )}
      <Dialog open={denyOpen} onClose={() => setDenyOpen(false)} fullWidth maxWidth="sm">
        <DialogTitle>Deny: {who}</DialogTitle>
        <DialogContent>
          {err && <Alert severity="error">{err}</Alert>}
          <TextField
            label="Reason (the requester sees it)"
            fullWidth
            margin="dense"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setDenyOpen(false)}>Cancel</Button>
          <Button color="error" onClick={() => deny.mutate()} disabled={deny.isPending}>
            Deny
          </Button>
        </DialogActions>
      </Dialog>
    </Row>
  )
}

/** The approve / edit form. */
function GrantDialog(props: {
  title: string
  d: Data
  initial: Form
  askedModels: string[]
  forConfiguredKey: boolean
  withValueAdds?: boolean
  withReason?: boolean
  withExpiry?: boolean
  submitLabel: string
  onClose: () => void
  onSubmit: (f: Form) => Promise<void>
}) {
  const { d } = props
  const [f, setF] = useState<Form>(props.initial)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const set = (p: Partial<Form>) => setF((cur) => ({ ...cur, ...p }))
  const restricted = d.credentials.filter((c) => !c.open)
  const setLim = (i: number, p: Partial<Lim>) => set({ limits: f.limits.map((l, j) => (j === i ? { ...l, ...p } : l)) })
  const setVA = (i: number, p: Partial<VAdd>) => set({ valueAdds: f.valueAdds.map((v, j) => (j === i ? { ...v, ...p } : v)) })
  const submit = async () => {
    setBusy(true)
    setErr('')
    try {
      await props.onSubmit(f)
      props.onClose()
    } catch (e) {
      setErr(extractMessage(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog open onClose={props.onClose} fullWidth maxWidth="md">
      <DialogTitle>{props.title}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ mt: 1 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <Stack direction="row" spacing={2}>
            <TextField label="Label" size="small" value={f.label} onChange={(e) => set({ label: e.target.value })} sx={{ flex: 1 }} />
            <TextField
              select
              label="Priority group"
              size="small"
              value={f.group}
              onChange={(e) => set({ group: e.target.value })}
              sx={{ minWidth: 200 }}
            >
              <MenuItem value="">fallback group</MenuItem>
              {d.groups.map((g) => (
                <MenuItem key={g} value={g}>
                  {g}
                </MenuItem>
              ))}
            </TextField>
          </Stack>

          <Box>
            <Typography variant="subtitle2">Paid accounts it may use</Typography>
            <Typography variant="caption" color="text.secondary">
              Accounts with an allowlist. Open accounts (no allowlist) are usable by every key already.
            </Typography>
            {restricted.length === 0 && (
              <Typography variant="body2" color="text.secondary">
                None: every account is open.
              </Typography>
            )}
            {restricted.map((c) => (
              <FormControlLabel
                key={c.scope}
                sx={{ display: 'flex' }}
                control={
                  <Checkbox
                    size="small"
                    checked={f.credentials.includes(c.scope)}
                    onChange={(e) =>
                      set({
                        credentials: e.target.checked ? [...f.credentials, c.scope] : f.credentials.filter((x) => x !== c.scope),
                      })
                    }
                  />
                }
                label={
                  <Typography variant="body2">
                    {c.provider}/{c.name}
                    <Typography component="span" variant="caption" color="text.secondary">
                      {' '}
                      — {(c.models ?? []).join(', ') || 'no models'}
                      {(c.models ?? []).some((m) => props.askedModels.includes(m)) ? ' (asked for)' : ''}
                    </Typography>
                  </Typography>
                }
              />
            ))}
            {props.forConfiguredKey && (
              <Typography variant="caption" color="warning.main" sx={{ display: 'block' }}>
                A configured key is added to an account's allowlist here; unticking does not remove it (edit the provider).
              </Typography>
            )}
          </Box>

          <Box>
            <Typography variant="subtitle2">Budgets</Typography>
            <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
              No budgets = unlimited. Hard refuses once spent; soft is only reported.
            </Typography>
            {f.limits.map((l, i) => (
              <Stack key={i} direction="row" spacing={1} sx={{ mt: 1, alignItems: 'center' }}>
                <TextField
                  label={l.unit === 'usd' ? 'Amount ($)' : 'Amount (requests)'}
                  size="small"
                  type="number"
                  value={l.amount}
                  onChange={(e) => setLim(i, { amount: e.target.value })}
                  sx={{ width: 150 }}
                />
                <TextField select size="small" label="Unit" value={l.unit} onChange={(e) => setLim(i, { unit: e.target.value as Lim['unit'] })} sx={{ width: 120 }}>
                  <MenuItem value="usd">dollars</MenuItem>
                  <MenuItem value="req">requests</MenuItem>
                </TextField>
                <TextField select size="small" label="Window" value={l.per} onChange={(e) => setLim(i, { per: e.target.value })} sx={{ width: 240 }}>
                  {PERS.map((p) => (
                    <MenuItem key={p.v} value={p.v}>
                      {p.label}
                    </MenuItem>
                  ))}
                </TextField>
                <TextField select size="small" label="Model" value={l.model} onChange={(e) => setLim(i, { model: e.target.value })} sx={{ flex: 1 }}>
                  <MenuItem value="">every model</MenuItem>
                  {d.models.map((m) => (
                    <MenuItem key={m} value={m}>
                      {m}
                    </MenuItem>
                  ))}
                </TextField>
                <FormControlLabel control={<Switch size="small" checked={l.hard} onChange={(e) => setLim(i, { hard: e.target.checked })} />} label="Hard" />
                <IconButton size="small" aria-label="Remove budget" onClick={() => set({ limits: f.limits.filter((_, j) => j !== i) })}>
                  <DeleteOutline fontSize="small" />
                </IconButton>
              </Stack>
            ))}
            <Button
              size="small"
              startIcon={<AddIcon />}
              sx={{ mt: 1 }}
              onClick={() => set({ limits: [...f.limits, { amount: '', unit: 'usd', per: 'calendar-day', hard: true, model: '' }] })}
            >
              Add budget
            </Button>
          </Box>

          {props.withValueAdds && (
            <Box>
              <Typography variant="subtitle2">Value adds</Typography>
              <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
                A one-time extra amount, spent only after the budgets above refuse. It does not lift a model's own cap.
              </Typography>
              {f.valueAdds.map((v, i) => (
                <Stack key={i} direction="row" spacing={1} sx={{ mt: 1, alignItems: 'center' }}>
                  <TextField label="Amount ($)" size="small" type="number" value={v.usd} onChange={(e) => setVA(i, { usd: e.target.value })} sx={{ width: 130 }} />
                  <TextField select size="small" label="Model" value={v.model} onChange={(e) => setVA(i, { model: e.target.value })} sx={{ flex: 1 }}>
                    <MenuItem value="">any model</MenuItem>
                    {d.models.map((m) => (
                      <MenuItem key={m} value={m}>
                        {m}
                      </MenuItem>
                    ))}
                  </TextField>
                  <TextField
                    label="Expires after"
                    placeholder="2d, 48h; empty = until spent"
                    size="small"
                    value={v.ttl}
                    onChange={(e) => setVA(i, { ttl: e.target.value })}
                    sx={{ width: 220 }}
                  />
                  <TextField label="Note" size="small" value={v.note} onChange={(e) => setVA(i, { note: e.target.value })} sx={{ width: 160 }} />
                  <IconButton size="small" aria-label="Remove value add" onClick={() => set({ valueAdds: f.valueAdds.filter((_, j) => j !== i) })}>
                    <DeleteOutline fontSize="small" />
                  </IconButton>
                </Stack>
              ))}
              <Button size="small" startIcon={<AddIcon />} sx={{ mt: 1 }} onClick={() => set({ valueAdds: [...f.valueAdds, { usd: '', model: '', ttl: '2d', note: '' }] })}>
                Add value add
              </Button>
            </Box>
          )}

          <Stack direction="row" spacing={2} sx={{ alignItems: 'center' }}>
            {!props.forConfiguredKey && (props.withExpiry ?? true) && (
              <TextField
                label="Key expires after"
                placeholder="7d, 48h; empty = never"
                size="small"
                value={f.ttl}
                onChange={(e) => set({ ttl: e.target.value })}
                helperText="After this the key answers 401. Counted from now."
                sx={{ width: 280 }}
              />
            )}
            <FormControlLabel
              control={<Switch checked={f.mint} onChange={(e) => set({ mint: e.target.checked })} />}
              label="May mint child keys (and set their budgets and expiry)"
            />
          </Stack>
          {props.withReason && (
            <TextField label="Note to the requester (optional)" size="small" value={f.reason} onChange={(e) => set({ reason: e.target.value })} />
          )}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={props.onClose}>Cancel</Button>
        <Button variant="contained" onClick={() => void submit()} disabled={busy}>
          {props.submitLabel}
        </Button>
      </DialogActions>
    </Dialog>
  )
}

function shortKey(k: string): string {
  return k.length > 18 ? `${k.slice(0, 10)}…${k.slice(-4)}` : k
}

function CopyKey({ k }: { k: string }) {
  return (
    <Tooltip title={`Copy ${k}`}>
      <IconButton size="small" aria-label="Copy key" onClick={() => void navigator.clipboard.writeText(k)}>
        <ContentCopyIcon sx={{ fontSize: 14 }} />
      </IconButton>
    </Tooltip>
  )
}

function KeysPanel({ d }: { d: Data }) {
  const [showAll, setShowAll] = useState(false)
  // Configured keys with nothing managed here would crowd the table; listed
  // when they have a budget, a value add or the mint permission.
  const keys = d.keys.filter((k) => showAll || k.kind !== 'configured' || k.mint || k.limits.length > 0 || k.valueAdds.length > 0)
  return (
    <Panel
      title="Keys"
      subtitle="Keys handed out here or minted by other keys, and configured keys with a budget, value add or mint permission."
      actions={
        <FormControlLabel control={<Switch size="small" checked={showAll} onChange={(e) => setShowAll(e.target.checked)} />} label="Every configured key" />
      }
      flush
    >
      <TableContainer>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Key</TableCell>
              <TableCell>Kind</TableCell>
              <TableCell>Budgets (used / limit)</TableCell>
              <TableCell>Value adds (left)</TableCell>
              <TableCell>Expires</TableCell>
              <TableCell align="right">Actions</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {keys.map((k) => (
              <KeyRow key={k.key} k={k} d={d} />
            ))}
          </TableBody>
        </Table>
      </TableContainer>
    </Panel>
  )
}

function KeyRow({ k, d }: { k: Key; d: Data }) {
  const qc = useQueryClient()
  const [edit, setEdit] = useState(false)
  const [addVA, setAddVA] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [err, setErr] = useState('')
  const refresh = () => void qc.invalidateQueries({ queryKey: ['requests'] })
  const act = useMutation({
    mutationFn: (action: 'disable' | 'enable' | 'delete') => gqlClient.request(KeyActionDoc, { body: { key: k.key, action } }),
    onSuccess: () => {
      setConfirmDelete(false)
      refresh()
    },
    onError: (e) => setErr(extractMessage(e)),
  })
  const delVA = useMutation({
    mutationFn: (id: string) => gqlClient.request(ValueAddDoc, { body: { action: 'delete', valueAdd: { id, usd: 0 } } }),
    onSuccess: refresh,
    onError: (e) => setErr(extractMessage(e)),
  })
  const kind = k.kind === 'child' ? `child of ${shortKey(k.parent ?? '')}` : k.kind
  return (
    <TableRow sx={{ opacity: k.expired ? 0.55 : 1, verticalAlign: 'top' }}>
      <TableCell>
        <Box sx={{ display: 'flex', alignItems: 'center' }}>
          <Typography variant="body2" sx={{ fontFamily: 'monospace' }}>
            {shortKey(k.key)}
          </Typography>
          <CopyKey k={k.key} />
        </Box>
        {k.label && (
          <Typography variant="caption" color="text.secondary">
            {k.label}
          </Typography>
        )}
        {err && (
          <Typography variant="caption" color="error" sx={{ display: 'block' }}>
            {err}
          </Typography>
        )}
      </TableCell>
      <TableCell>
        <Typography variant="body2">{kind}</Typography>
        <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
          {k.group || 'fallback group'}
          {k.mint ? ' · may mint' : ''}
        </Typography>
        {(k.credentials ?? []).map((c) => (
          <Typography key={c} variant="caption" color="text.secondary" sx={{ display: 'block' }}>
            {c.replace(/^cred:/, '')}
          </Typography>
        ))}
      </TableCell>
      <TableCell>
        {k.limits.length === 0 && (
          <Typography variant="body2" color="text.secondary">
            unlimited
          </Typography>
        )}
        {k.limits.map((l, i) => {
          const w = k.windows.find((x) => x.scope === (l.model || 'key') && x.label.endsWith('/' + l.per) && x.label.startsWith((l.usd ?? 0) > 0 ? 'usd' : 'req'))
          const usd = (l.usd ?? 0) > 0
          const fmt = (n: number) => (usd ? fmtUSD(n) : `${Math.round(n)} req`)
          return (
            <Typography key={i} variant="body2" sx={{ color: w?.blocked ? C.error : undefined }}>
              {w ? `${fmt(w.used)} / ` : ''}
              {limitText(l)}
              {w?.blocked ? ' — full' : ''}
            </Typography>
          )
        })}
      </TableCell>
      <TableCell>
        {k.valueAdds.length === 0 && (
          <Typography variant="body2" color="text.secondary">
            none
          </Typography>
        )}
        {k.valueAdds.map((v) => (
          <Box key={v.id} sx={{ display: 'flex', alignItems: 'center', opacity: v.active ? 1 : 0.55 }}>
            <Typography variant="body2">
              {fmtUSD(Math.max(v.usd - (v.spent ?? 0), 0))} of {fmtUSD(v.usd)}
              {v.model ? ` on ${v.model}` : ''}
              {v.expires ? ` · until ${new Date(v.expires).toLocaleString()}` : ''}
              {!v.active ? ' (done)' : ''}
            </Typography>
            <IconButton size="small" aria-label="Remove value add" onClick={() => delVA.mutate(v.id ?? '')}>
              <DeleteOutline sx={{ fontSize: 16 }} />
            </IconButton>
          </Box>
        ))}
      </TableCell>
      <TableCell>
        {k.kind === 'configured' ? (
          <Typography variant="body2" color="text.secondary">
            never (configured)
          </Typography>
        ) : k.expires ? (
          <Typography variant="body2" sx={{ color: k.expired ? C.error : undefined }}>
            {k.expired ? 'expired ' : ''}
            {new Date(k.expires).toLocaleString()}
          </Typography>
        ) : (
          <Typography variant="body2" color="text.secondary">
            never
          </Typography>
        )}
      </TableCell>
      <TableCell align="right">
        <Stack direction="row" spacing={0.5} sx={{ justifyContent: 'flex-end', flexWrap: 'wrap' }}>
          <Button size="small" onClick={() => setEdit(true)}>
            Edit
          </Button>
          <Button size="small" onClick={() => setAddVA(true)}>
            Add value
          </Button>
          {k.kind !== 'configured' &&
            (k.expired ? (
              <Button size="small" onClick={() => act.mutate('enable')}>
                Enable
              </Button>
            ) : (
              <Button size="small" color="warning" onClick={() => act.mutate('disable')}>
                Disable now
              </Button>
            ))}
          {k.kind !== 'configured' && (
            <Button size="small" color="error" onClick={() => setConfirmDelete(true)}>
              Delete
            </Button>
          )}
        </Stack>
      </TableCell>
      {edit && (
        <GrantDialog
          title={`Edit ${k.label || shortKey(k.key)}`}
          d={d}
          initial={formFromKey(k)}
          askedModels={[]}
          forConfiguredKey={k.kind === 'configured'}
          submitLabel="Save"
          onClose={() => setEdit(false)}
          onSubmit={async (f) => {
            const g = formToGrant(f)
            await gqlClient.request(UpdateKeyDoc, { body: { key: k.key, edit: { ...g, valueAdds: [] } } })
            refresh()
          }}
        />
      )}
      {addVA && <AddValueDialog k={k} d={d} onClose={() => setAddVA(false)} onDone={refresh} />}
      <Dialog open={confirmDelete} onClose={() => setConfirmDelete(false)}>
        <DialogTitle>Delete {k.label || shortKey(k.key)}?</DialogTitle>
        <DialogContent>
          <Typography variant="body2">
            The key stops working at once (401), and so do its child keys. Its value adds are removed. To stop it but keep the
            record, use Disable now instead.
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirmDelete(false)}>Cancel</Button>
          <Button color="error" onClick={() => act.mutate('delete')} disabled={act.isPending}>
            Delete
          </Button>
        </DialogActions>
      </Dialog>
    </TableRow>
  )
}

function AddValueDialog({ k, d, onClose, onDone }: { k: Key; d: Data; onClose: () => void; onDone: () => void }) {
  const [v, setV] = useState<VAdd>({ usd: '', model: '', ttl: '2d', note: '' })
  const [err, setErr] = useState('')
  const add = useMutation({
    mutationFn: () =>
      gqlClient.request(ValueAddDoc, {
        body: { action: 'add', valueAdd: { key: k.key, usd: Number(v.usd), model: v.model || undefined, ttl: v.ttl.trim() || undefined, note: v.note || undefined } },
      }),
    onSuccess: () => {
      onDone()
      onClose()
    },
    onError: (e) => setErr(extractMessage(e)),
  })
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>Value add for {k.label || shortKey(k.key)}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ mt: 1 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <Typography variant="body2" color="text.secondary">
            Spent only after the key's own hard budgets refuse. {k.limits.some((l) => l.hard) ? '' : 'This key has no hard budget, so a value add will not be used until it has one.'}
          </Typography>
          <TextField label="Amount ($)" type="number" size="small" value={v.usd} onChange={(e) => setV({ ...v, usd: e.target.value })} />
          <TextField select label="Model" size="small" value={v.model} onChange={(e) => setV({ ...v, model: e.target.value })}>
            <MenuItem value="">any model</MenuItem>
            {d.models.map((m) => (
              <MenuItem key={m} value={m}>
                {m}
              </MenuItem>
            ))}
          </TextField>
          <TextField label="Expires after" placeholder="2d, 48h; empty = until spent" size="small" value={v.ttl} onChange={(e) => setV({ ...v, ttl: e.target.value })} />
          <TextField label="Note" size="small" value={v.note} onChange={(e) => setV({ ...v, note: e.target.value })} />
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" onClick={() => add.mutate()} disabled={add.isPending || !(Number(v.usd) > 0)}>
          Add
        </Button>
      </DialogActions>
    </Dialog>
  )
}

type RuleForm = { name: string; cidrs: string; token: string; users: string }

const splitList = (s: string) =>
  s
    .split(/[,\s]+/)
    .map((x) => x.trim())
    .filter(Boolean)

function RulesPanel({ d }: { d: Data }) {
  const qc = useQueryClient()
  const [useDefault, setUseDefault] = useState(d.defaultRules)
  const [rules, setRules] = useState<RuleForm[]>(
    d.rules.map((r) => ({ name: r.name ?? '', cidrs: (r.cidrs ?? []).join(', '), token: r.token ?? '', users: (r.users ?? []).join(', ') })),
  )
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)
  const save = useMutation({
    mutationFn: () =>
      gqlClient.request(RulesDoc, {
        body: {
          useDefault,
          rules: rules.map((r) => ({ name: r.name.trim(), cidrs: splitList(r.cidrs), token: r.token.trim(), users: splitList(r.users) })),
        },
      }),
    onSuccess: () => {
      setMsg({ ok: true, text: 'Saved.' })
      void qc.invalidateQueries({ queryKey: ['requests'] })
    },
    onError: (e) => setMsg({ ok: false, text: extractMessage(e) }),
  })
  const setRule = (i: number, p: Partial<RuleForm>) => setRules(rules.map((r, j) => (j === i ? { ...r, ...p } : r)))
  return (
    <Panel
      title="Who may ask"
      subtitle="Filing a request grants nothing; these rules only decide who can put one on this page."
    >
      <Row>
        <Stack spacing={1.5}>
          {msg && <Alert severity={msg.ok ? 'success' : 'error'}>{msg.text}</Alert>}
          <FormControlLabel
            control={<Switch checked={useDefault} onChange={(e) => setUseDefault(e.target.checked)} />}
            label="Default: direct connections from this machine and private networks (not through the public endpoint)"
          />
          {!useDefault && (
            <>
              <Typography variant="body2" color="text.secondary">
                A request is accepted when any rule matches. Within a rule, every field you fill must match. No rules = nobody may ask.
              </Typography>
              {rules.map((r, i) => (
                <Stack key={i} direction="row" spacing={1} useFlexGap sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
                  <TextField label="Name" size="small" value={r.name} onChange={(e) => setRule(i, { name: e.target.value })} sx={{ width: 140 }} />
                  <TextField
                    label="Subnets"
                    placeholder="192.168.1.0/24, 127.0.0.0/8"
                    helperText="Direct connections only"
                    size="small"
                    value={r.cidrs}
                    onChange={(e) => setRule(i, { cidrs: e.target.value })}
                    sx={{ width: 240 }}
                  />
                  <TextField
                    label="Shared token"
                    placeholder="${ACCESS_REQUEST_TOKEN}"
                    helperText="Sent as X-Corrallm-Request-Token"
                    size="small"
                    value={r.token}
                    onChange={(e) => setRule(i, { token: e.target.value })}
                    sx={{ width: 240 }}
                  />
                  <TextField
                    label="Dashboard users"
                    placeholder="carl@example.com"
                    helperText="SSO emails, also under Access"
                    size="small"
                    value={r.users}
                    onChange={(e) => setRule(i, { users: e.target.value })}
                    sx={{ width: 240 }}
                  />
                  <IconButton aria-label="Remove rule" onClick={() => setRules(rules.filter((_, j) => j !== i))}>
                    <DeleteOutline fontSize="small" />
                  </IconButton>
                </Stack>
              ))}
              <Box>
                <Button size="small" startIcon={<AddIcon />} onClick={() => setRules([...rules, { name: '', cidrs: '', token: '', users: '' }])}>
                  Add rule
                </Button>
              </Box>
              {rules.some((r) => r.token && !r.token.trim().startsWith('${')) && (
                <Alert severity="warning">
                  A literal token is stored in the config, where config history keeps it. Prefer a credential-store reference like
                  {' ${ACCESS_REQUEST_TOKEN}'} (corrallm config secret set; read at start-up).
                </Alert>
              )}
            </>
          )}
          <Box>
            <Button variant="contained" size="small" onClick={() => save.mutate()} disabled={save.isPending}>
              Save rules
            </Button>
          </Box>
        </Stack>
      </Row>
    </Panel>
  )
}

function DecidedPanel({ rows }: { rows: Req[] }) {
  return (
    <Panel title="Decided" subtitle="Most recent first." flush>
      <TableContainer>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Request</TableCell>
              <TableCell>Decision</TableCell>
              <TableCell>Granted</TableCell>
              <TableCell>Key</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.id} sx={{ verticalAlign: 'top' }}>
                <TableCell>
                  <Typography variant="body2">{r.requesterKey ? `more for ${shortKey(r.requesterKey)}` : `new key “${r.asked.label}”`}</Typography>
                  <Typography variant="caption" color="text.secondary">
                    {r.asked.purpose}
                  </Typography>
                </TableCell>
                <TableCell>
                  <Chip size="small" color={r.status === 'approved' ? 'success' : 'default'} label={r.status} />
                  <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
                    {r.decidedBy} · {fmtAgo(r.decided)}
                  </Typography>
                  {r.reason && <Typography variant="caption">{r.reason}</Typography>}
                </TableCell>
                <TableCell>
                  {r.status === 'approved' &&
                    [
                      ...(r.granted.limits ?? []).map(limitText),
                      ...(r.granted.valueAdds ?? []).map((v) => `value add ${fmtUSD(v.usd)}${v.ttl ? ` for ${v.ttl}` : ''}`),
                      ...(r.granted.ttl ? [`expires after ${r.granted.ttl}`] : []),
                      ...(r.granted.mint ? ['may mint'] : []),
                    ].map((s) => (
                      <Typography key={s} variant="caption" sx={{ display: 'block' }}>
                        {s}
                      </Typography>
                    ))}
                </TableCell>
                <TableCell>
                  {r.issuedKey && (
                    <>
                      <Typography variant="body2" sx={{ fontFamily: 'monospace' }}>
                        {shortKey(r.issuedKey)}
                      </Typography>
                      <Typography variant="caption" color="text.secondary">
                        {r.delivered ? 'collected by the requester' : 'not collected yet'}
                      </Typography>
                    </>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableContainer>
    </Panel>
  )
}

export const Route = createFileRoute('/requests')({ component: Requests })
