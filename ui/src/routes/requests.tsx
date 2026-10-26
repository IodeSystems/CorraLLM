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
  Typography,
} from '@mui/material'
import DeleteOutline from '@mui/icons-material/DeleteOutlined'
import AddIcon from '@mui/icons-material/Add'
import { graphql } from '@/gql'
import type { RequestsQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { Panel, PageHeader, Row } from '@/Panel'
import { Loading } from '@/Loading'
import { extractMessage, fmtAgo } from '@/format'
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
 *   - a top-up (the API's "value add"): an extra amount with its own expiry,
 *     spent only once the key's own hard budgets refuse.
 * And a key can expire, after which it answers 401 — "switch off" is "expire now".
 *
 * Words are the owner's, not the API's (stooges pass 10-25): top-up, "stop it /
 * only warn me" for hard/soft, "sub-keys" for minting, dates instead of "3d",
 * and the one line that decides — at most $X, stops on <date> — leads each
 * request and sits above Approve. Who may file lives in Settings → Requests.
 *
 * A top-up raises the KEY's allowance only. A model's or provider's own cap
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


type Data = NonNullable<NonNullable<RequestsQuery['corrallm']>['listRequests']>
type Req = Data['requests'][number]
type Key = Data['keys'][number]
type Cred = Data['credentials'][number]

// The editable form of a grant: the dialog's state.
type Lim = { amount: string; unit: 'usd' | 'req'; per: string; hard: boolean; model: string }
type TopUp = { usd: string; model: string; ttl: string; note: string }
type Form = {
  label: string
  group: string
  credentials: string[]
  limits: Lim[]
  topUps: TopUp[]
  ttl: string
  mint: boolean
  reason: string
}

// Windows in the words a bill uses. The rolling ones last: nobody budgets in them.
const PERS: { v: string; label: string; short: string }[] = [
  { v: 'calendar-day', label: 'a day (resets at midnight UTC)', short: 'a day' },
  { v: 'calendar-month', label: 'a month (resets on the 1st)', short: 'a month' },
  { v: 'total', label: 'in total (never resets)', short: 'in total' },
  { v: 'hour', label: 'per rolling hour', short: 'an hour' },
  { v: 'day', label: 'per rolling 24 hours', short: 'per 24 hours' },
  { v: 'month', label: 'per rolling 30 days', short: 'per 30 days' },
]
const perShort = (per: string) => PERS.find((p) => p.v === per)?.short ?? per

/** Money as a bill shows it: cents, or "$0". */
function usd(n: number): string {
  if (!Number.isFinite(n)) return '—'
  if (n === 0) return '$0'
  if (n < 0.01) return '<$0.01'
  return `$${n.toFixed(2)}`
}

/** A moment as a person reads it: no seconds, no year when it is this year. */
function when(d: Date | string): string {
  const t = typeof d === 'string' ? new Date(d) : d
  const sameYear = t.getFullYear() === new Date().getFullYear()
  return t.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    ...(sameYear ? {} : { year: 'numeric' }),
    hour: 'numeric',
    minute: '2-digit',
  })
}

const DAY = 86_400_000
/** "2d", "48h", "1d12h" → ms; NaN when it does not parse. Mirrors the server. */
function ttlMs(s: string): number {
  const m = /^\s*(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?\s*$/.exec(s)
  if (!m || !(m[1] || m[2] || m[3])) return NaN
  return Number(m[1] ?? 0) * DAY + Number(m[2] ?? 0) * 3_600_000 + Number(m[3] ?? 0) * 60_000
}
/** When something given a ttl now would stop. */
function endsAt(ttl: string): string | null {
  const ms = ttlMs(ttl)
  return Number.isFinite(ms) ? when(new Date(Date.now() + ms)) : null
}

type LimitIn = { req?: number | null; usd?: number | null; per: string; hard?: boolean | null; model?: string | null }

function toLim(l: LimitIn): Lim {
  const isUsd = (l.usd ?? 0) > 0
  return { amount: String(isUsd ? l.usd : (l.req ?? '')), unit: isUsd ? 'usd' : 'req', per: l.per, hard: !!l.hard, model: l.model ?? '' }
}

function limitText(l: LimitIn): string {
  const amt = (l.usd ?? 0) > 0 ? usd(l.usd ?? 0) : `${l.req} requests`
  return `${l.hard ? 'up to' : 'warn past'} ${amt} ${perShort(l.per)}${l.model ? ` on ${l.model}` : ''}`
}

/**
 * The one line the owner decides on: the most it can spend, and when it stops.
 * Every finding of the stooges pass said this was missing or buried.
 */
function costLine(limits: LimitIn[], topUps: { usd: number; ttl?: string | null }[], ttl: string | null | undefined, newKey: boolean): { text: string; open: boolean } {
  const parts: string[] = []
  const money = limits.filter((l) => l.hard && (l.usd ?? 0) > 0)
  for (const l of money) parts.push(`${usd(l.usd ?? 0)} ${perShort(l.per)}${l.model ? ` on ${l.model}` : ''}`)
  for (const t of topUps) parts.push(`a ${usd(t.usd)} top-up${t.ttl ? ` for ${t.ttl.replace(/d$/, ' days').replace(/h$/, ' hours')}` : ''}`)
  const open = newKey && money.length === 0
  let text = parts.length ? `At most ${parts.join(', plus ')}` : open ? 'No spending limit' : 'No change to what it may spend'
  if (newKey || ttl) {
    const end = ttl ? endsAt(ttl) : null
    text += end ? ` · stops working ${end}` : ' · never stops by itself'
  }
  return { text, open }
}

/** Accounts a request's models need: allowlisted ones serving those models. */
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
    topUps: (a.valueAdds ?? []).map((v) => ({ usd: String(v.usd), model: v.model ?? '', ttl: v.ttl ?? '', note: v.note ?? '' })),
    ttl: a.ttl ?? '',
    mint: !!a.mint,
    reason: '',
  }
}

function formFromKey(k: Key): Form {
  return { label: k.label ?? '', group: k.group ?? '', credentials: k.credentials ?? [], limits: k.limits.map(toLim), topUps: [], ttl: '', mint: k.mint, reason: '' }
}

function formToGrant(f: Form) {
  return {
    label: f.label.trim(),
    group: f.group,
    credentials: f.credentials,
    limits: f.limits
      .filter((l) => l.amount.trim() !== '')
      .map((l) => ({ per: l.per, hard: l.hard, model: l.model || undefined, ...(l.unit === 'usd' ? { usd: Number(l.amount) } : { req: Number(l.amount) }) })),
    valueAdds: f.topUps
      .filter((v) => v.usd.trim() !== '')
      .map((v) => ({ usd: Number(v.usd), model: v.model || undefined, ttl: v.ttl.trim() || undefined, note: v.note.trim() || undefined })),
    ttl: f.ttl.trim() || undefined,
    mint: f.mint,
  }
}

const keyName = (k: Pick<Key, 'key' | 'label'>) => k.label || k.key
function shortId(k: string): string {
  return k.length > 16 ? `${k.slice(0, 7)}…${k.slice(-4)}` : k
}
/** Who a request is from, in the words its card leads with. */
function requestTitle(r: Req, d: Data): string {
  if (!r.requesterKey) return `New key “${r.asked.label}”`
  const k = d.keys.find((x) => x.key === r.requesterKey)
  return `More for ${k ? keyName(k) : r.requesterKey}`
}

// An empty select shows its label as a placeholder unless told to keep it up
// and render the empty choice (stooges pass: "Model" read as unset).
const selectSlots = { select: { displayEmpty: true }, inputLabel: { shrink: true } }

function useData() {
  return useQuery({ queryKey: ['requests'], queryFn: () => gqlClient.request(RequestsDoc), refetchInterval: 10_000 })
}

function Requests() {
  const ro = useReadOnly()
  const q = useData()
  const d = q.data?.corrallm?.listRequests
  if (ro) return <Alert severity="info">Requests are for admins: the page shows keys.</Alert>
  if (q.error) return <Alert severity="error">{extractMessage(q.error)}</Alert>
  if (!d) return <Loading />
  const pending = d.requests.filter((r) => r.status === 'pending')
  const decided = d.requests.filter((r) => r.status !== 'pending')
  return (
    <Box sx={{ p: 2, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <PageHeader title="Requests">
        {pending.length > 0 && <Chip size="small" color="info" label={`${pending.length} waiting`} />}
      </PageHeader>
      <Panel title="Waiting for you">
        {pending.length === 0 ? (
          <Row>
            <Typography variant="body2" color="text.secondary">
              Nothing is waiting.
            </Typography>
          </Row>
        ) : (
          pending.map((r) => <PendingRow key={r.id} r={r} d={d} />)
        )}
      </Panel>
      {decided.length > 0 && <DecidedPanel rows={decided} d={d} />}
      <KeysPanel d={d} />
    </Box>
  )
}

function PendingRow({ r, d }: { r: Req; d: Data }) {
  const [open, setOpen] = useState(false)
  const [denyOpen, setDenyOpen] = useState(false)
  const a = r.asked
  const title = requestTitle(r, d)
  const line = costLine(a.limits ?? [], a.valueAdds ?? [], a.ttl, !r.requesterKey)
  const extra = [
    ...(a.askedMint ? [a.mint ? 'wants to create its own sub-keys' : 'gives up creating sub-keys'] : []),
    ...(a.models?.length ? [`for ${a.models.join(', ')}`] : []),
  ]
  const odd = r.via !== 'default (direct from LAN)'
  return (
    <Row>
      <Box sx={{ display: 'flex', gap: 2, alignItems: 'flex-start', flexWrap: 'wrap' }}>
        <Box sx={{ flex: 1, minWidth: 260 }}>
          <Typography variant="subtitle2">{title}</Typography>
          <Typography variant="body2" sx={{ fontWeight: 600, color: line.open ? C.warn : undefined }}>
            {line.text}
          </Typography>
          {extra.length > 0 && (
            <Typography variant="body2" color="text.secondary">
              {extra.join(' · ')}
            </Typography>
          )}
          <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5, whiteSpace: 'pre-wrap' }}>
            “{a.purpose}”
          </Typography>
          <Typography variant="caption" color="text.secondary">
            asked {fmtAgo(r.created)}
            {r.filedBy ? ` by ${r.filedBy}` : ''}
            {odd ? ` · from ${r.sourceIp} (${r.via})` : ''}
          </Typography>
        </Box>
        <Stack direction="row" spacing={1}>
          <Button size="small" color="inherit" onClick={() => setDenyOpen(true)}>
            Deny
          </Button>
          <Button variant="outlined" size="small" onClick={() => setOpen(true)}>
            Review
          </Button>
        </Stack>
      </Box>
      {open && <ApproveDialog r={r} d={d} title={title} onClose={() => setOpen(false)} />}
      {denyOpen && <DenyDialog r={r} title={title} onClose={() => setDenyOpen(false)} />}
    </Row>
  )
}

function DenyDialog({ r, title, onClose }: { r: Req; title: string; onClose: () => void }) {
  const qc = useQueryClient()
  const [reason, setReason] = useState('')
  const [err, setErr] = useState('')
  const deny = useMutation({
    mutationFn: () => gqlClient.request(DenyDoc, { body: { id: r.id, reason } }),
    onSuccess: () => {
      onClose()
      void qc.invalidateQueries({ queryKey: ['requests'] })
    },
    onError: (e) => setErr(extractMessage(e)),
  })
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>Deny: {title}?</DialogTitle>
      <DialogContent>
        {err && <Alert severity="error">{err}</Alert>}
        <TextField
          label="Reason"
          helperText="The assistant is told this the next time it checks on its request."
          fullWidth
          margin="dense"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" color="error" onClick={() => deny.mutate()} disabled={deny.isPending}>
          Deny
        </Button>
      </DialogActions>
    </Dialog>
  )
}

/**
 * Approve. A request for a NEW key opens the whole grant, spending first and
 * the rest under More settings. A request for more on an EXISTING key shows
 * the key as it is now and only the change asked for — the server applies
 * nothing else (proxy.onlyAsked).
 */
function ApproveDialog({ r, d, title, onClose }: { r: Req; d: Data; title: string; onClose: () => void }) {
  const qc = useQueryClient()
  const existing = r.requesterKey ? d.keys.find((k) => k.key === r.requesterKey) : undefined
  const a = r.asked
  const [f, setF] = useState<Form>(formFromRequest(r, d.credentials))
  const [more, setMore] = useState(false)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const set = (p: Partial<Form>) => setF((cur) => ({ ...cur, ...p }))
  const g = formToGrant(f)
  const line = costLine(g.limits, (g.valueAdds ?? []).map((v) => ({ usd: v.usd, ttl: v.ttl })), g.ttl, !r.requesterKey)
  const submit = async () => {
    setBusy(true)
    setErr('')
    try {
      await gqlClient.request(ApproveDoc, { body: { id: r.id, granted: g, reason: f.reason } })
      void qc.invalidateQueries({ queryKey: ['requests'] })
      onClose()
    } catch (e) {
      setErr(extractMessage(e))
    } finally {
      setBusy(false)
    }
  }
  const showLimits = !r.requesterKey || !!a.askedLimits
  const showTopUps = !r.requesterKey || (a.valueAdds?.length ?? 0) > 0
  const showExpiry = !r.requesterKey || !!a.ttl || !!a.expires
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="md">
      <DialogTitle>Approve: {title}?</DialogTitle>
      <DialogContent>
        <Stack spacing={2.5} sx={{ mt: 0.5 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <Typography variant="body2" color="text.secondary" sx={{ whiteSpace: 'pre-wrap' }}>
            “{a.purpose}”
          </Typography>
          {existing && <KeyNow k={existing} />}
          {showLimits && <LimitsEditor f={f} set={set} d={d} />}
          {showTopUps && <TopUpsEditor f={f} set={set} d={d} />}
          {showExpiry && (
            <TextField
              label="Stops working after"
              placeholder="7d, 48h — empty: never"
              size="small"
              value={f.ttl}
              onChange={(e) => set({ ttl: e.target.value })}
              helperText={f.ttl ? (endsAt(f.ttl) ? `On ${endsAt(f.ttl)}, if you approve now` : 'Write it as 7d or 48h') : 'Never'}
              error={!!f.ttl && !endsAt(f.ttl)}
              sx={{ width: 300 }}
            />
          )}
          {existing && a.askedMint && (
            <FormControlLabel control={<Switch checked={f.mint} onChange={(e) => set({ mint: e.target.checked })} />} label="May create its own sub-keys, with budgets you cannot see here" />
          )}
          {!existing && (
            <Box>
              <Button size="small" color="inherit" onClick={() => setMore(!more)}>
                {more ? 'Fewer settings' : 'More settings: name, priority, paid accounts, sub-keys'}
              </Button>
              {more && <MoreSettings f={f} set={set} d={d} askedModels={a.models ?? []} />}
            </Box>
          )}
          <TextField label="Note to the assistant (optional)" size="small" value={f.reason} onChange={(e) => set({ reason: e.target.value })} />
        </Stack>
      </DialogContent>
      <Box sx={{ px: 3, pt: 1, borderTop: `1px solid ${C.border}` }}>
        <Typography variant="subtitle2" sx={{ color: line.open ? C.warn : undefined }}>
          {line.text}
        </Typography>
        {!existing && (
          <Typography variant="caption" color="text.secondary">
            The assistant gets the key the next time it checks, once. You can switch it off later under Keys.
          </Typography>
        )}
      </Box>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" onClick={() => void submit()} disabled={busy || (!!f.ttl && !endsAt(f.ttl))}>
          Approve
        </Button>
      </DialogActions>
    </Dialog>
  )
}

/** A key as it stands, for a request that would change it. */
function KeyNow({ k }: { k: Key }) {
  const parts = [
    k.limits.length ? k.limits.map(limitText).join('; ') : 'no spending limit',
    ...k.valueAdds.filter((v) => v.active).map((v) => `${usd(v.usd - (v.spent ?? 0))} top-up left${v.expires ? ` until ${when(v.expires)}` : ''}`),
    ...(k.mint ? ['may create sub-keys'] : []),
  ]
  return (
    <Box sx={{ p: 1.5, border: `1px solid ${C.border}`, borderRadius: 1 }}>
      <Typography variant="caption" color="text.secondary">
        {keyName(k)} now
      </Typography>
      <Typography variant="body2">{parts.join(' · ')}</Typography>
      <Typography variant="caption" color="text.secondary">
        Approving changes only what was asked. To change anything else, use Edit under Keys.
      </Typography>
    </Box>
  )
}

function LimitsEditor({ f, set, d }: { f: Form; set: (p: Partial<Form>) => void; d: Data }) {
  const setLim = (i: number, p: Partial<Lim>) => set({ limits: f.limits.map((l, j) => (j === i ? { ...l, ...p } : l)) })
  return (
    <Box>
      <Typography variant="subtitle2">Spending limits</Typography>
      {f.limits.length === 0 && (
        <Typography variant="body2" sx={{ color: C.warn }}>
          None: it may spend without limit.
        </Typography>
      )}
      {f.limits.map((l, i) => (
        <Stack key={i} direction="row" spacing={1} useFlexGap sx={{ mt: 1.5, alignItems: 'center', flexWrap: 'wrap' }}>
          <TextField label={l.unit === 'usd' ? 'Amount ($)' : 'Requests'} size="small" type="number" value={l.amount} onChange={(e) => setLim(i, { amount: e.target.value })} sx={{ width: 130 }} />
          <TextField select size="small" label="Of" value={l.unit} onChange={(e) => setLim(i, { unit: e.target.value as Lim['unit'] })} sx={{ width: 120 }}>
            <MenuItem value="usd">dollars</MenuItem>
            <MenuItem value="req">requests</MenuItem>
          </TextField>
          <TextField select size="small" label="Over" value={l.per} onChange={(e) => setLim(i, { per: e.target.value })} sx={{ width: 250 }}>
            {PERS.map((p) => (
              <MenuItem key={p.v} value={p.v}>
                {p.label}
              </MenuItem>
            ))}
          </TextField>
          <TextField select size="small" label="On" value={l.model} onChange={(e) => setLim(i, { model: e.target.value })} slotProps={selectSlots} sx={{ minWidth: 200, flex: 1 }}>
            <MenuItem value="">every model</MenuItem>
            {d.models.map((m) => (
              <MenuItem key={m} value={m}>
                {m}
              </MenuItem>
            ))}
          </TextField>
          <TextField select size="small" label="When reached" value={l.hard ? 'stop' : 'warn'} onChange={(e) => setLim(i, { hard: e.target.value === 'stop' })} sx={{ width: 170 }}>
            <MenuItem value="stop">stop it</MenuItem>
            <MenuItem value="warn">only warn me</MenuItem>
          </TextField>
          <IconButton size="small" aria-label="Remove this limit" onClick={() => set({ limits: f.limits.filter((_, j) => j !== i) })}>
            <DeleteOutline fontSize="small" />
          </IconButton>
        </Stack>
      ))}
      <Button size="small" startIcon={<AddIcon />} sx={{ mt: 1 }} onClick={() => set({ limits: [...f.limits, { amount: '', unit: 'usd', per: 'calendar-day', hard: true, model: '' }] })}>
        Add a limit
      </Button>
    </Box>
  )
}

function TopUpsEditor({ f, set, d }: { f: Form; set: (p: Partial<Form>) => void; d: Data }) {
  const setT = (i: number, p: Partial<TopUp>) => set({ topUps: f.topUps.map((v, j) => (j === i ? { ...v, ...p } : v)) })
  return (
    <Box>
      <Typography variant="subtitle2">Top-ups</Typography>
      <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
        Extra money on top of the limits, used once they are reached, gone when it runs out or expires.
      </Typography>
      {f.topUps.map((v, i) => (
        <TopUpRow key={i} v={v} set={(p) => setT(i, p)} remove={() => set({ topUps: f.topUps.filter((_, j) => j !== i) })} d={d} />
      ))}
      <Button size="small" startIcon={<AddIcon />} sx={{ mt: 1 }} onClick={() => set({ topUps: [...f.topUps, { usd: '', model: '', ttl: '2d', note: '' }] })}>
        Add a top-up
      </Button>
    </Box>
  )
}

function TopUpRow({ v, set, remove, d }: { v: TopUp; set: (p: Partial<TopUp>) => void; remove?: () => void; d: Data }) {
  return (
    <Stack direction="row" spacing={1} useFlexGap sx={{ mt: 1.5, alignItems: 'center', flexWrap: 'wrap' }}>
      <TextField label="Amount ($)" size="small" type="number" value={v.usd} onChange={(e) => set({ usd: e.target.value })} sx={{ width: 130 }} />
      <TextField select size="small" label="On" value={v.model} onChange={(e) => set({ model: e.target.value })} slotProps={selectSlots} sx={{ minWidth: 200, flex: 1 }}>
        <MenuItem value="">every model</MenuItem>
        {d.models.map((m) => (
          <MenuItem key={m} value={m}>
            {m}
          </MenuItem>
        ))}
      </TextField>
      <TextField
        label="Expires after"
        placeholder="2d, 48h"
        size="small"
        value={v.ttl}
        onChange={(e) => set({ ttl: e.target.value })}
        helperText={v.ttl ? (endsAt(v.ttl) ? `On ${endsAt(v.ttl)}` : 'Write it as 2d or 48h') : 'Until spent'}
        error={!!v.ttl && !endsAt(v.ttl)}
        sx={{ width: 200 }}
      />
      {remove && (
        <IconButton size="small" aria-label="Remove this top-up" onClick={remove}>
          <DeleteOutline fontSize="small" />
        </IconButton>
      )}
    </Stack>
  )
}

function MoreSettings({ f, set, d, askedModels }: { f: Form; set: (p: Partial<Form>) => void; d: Data; askedModels: string[] }) {
  const restricted = d.credentials.filter((c) => !c.open)
  return (
    <Stack spacing={2} sx={{ mt: 1.5 }}>
      <Stack direction="row" spacing={2}>
        <TextField label="Name" size="small" value={f.label} onChange={(e) => set({ label: e.target.value })} sx={{ flex: 1 }} />
        <TextField select label="Priority" size="small" value={f.group} onChange={(e) => set({ group: e.target.value })} slotProps={selectSlots} sx={{ minWidth: 200 }}>
          <MenuItem value="">the default</MenuItem>
          {d.groups.map((g) => (
            <MenuItem key={g} value={g}>
              {g}
            </MenuItem>
          ))}
        </TextField>
      </Stack>
      {restricted.length > 0 && (
        <Box>
          <Typography variant="subtitle2">Paid accounts it may use</Typography>
          {restricted.map((c) => (
            <FormControlLabel
              key={c.scope}
              sx={{ display: 'flex' }}
              control={
                <Checkbox
                  size="small"
                  checked={f.credentials.includes(c.scope)}
                  onChange={(e) => set({ credentials: e.target.checked ? [...f.credentials, c.scope] : f.credentials.filter((x) => x !== c.scope) })}
                />
              }
              label={
                <Typography variant="body2">
                  {c.provider}
                  {c.name !== 'default' ? ` (${c.name})` : ''}
                  {(c.models ?? []).some((m) => askedModels.includes(m)) ? ' — needed for what it asked' : ''}
                </Typography>
              }
            />
          ))}
        </Box>
      )}
      <FormControlLabel control={<Switch checked={f.mint} onChange={(e) => set({ mint: e.target.checked })} />} label="May create its own sub-keys" />
    </Stack>
  )
}

function KeysPanel({ d }: { d: Data }) {
  const [showAll, setShowAll] = useState(false)
  // Configured keys with nothing managed here would crowd the table.
  const keys = d.keys.filter((k) => showAll || k.kind !== 'configured' || k.mint || k.limits.length > 0 || k.valueAdds.length > 0)
  return (
    <Panel
      title="Keys you have handed out"
      actions={<FormControlLabel control={<Switch size="small" checked={showAll} onChange={(e) => setShowAll(e.target.checked)} />} label="Show keys with no limits" />}
      flush
    >
      <TableContainer>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Key</TableCell>
              <TableCell>Spent</TableCell>
              <TableCell>Top-ups left</TableCell>
              <TableCell>Stops working</TableCell>
              <TableCell align="right" />
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
  const [addTopUp, setAddTopUp] = useState(false)
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
  const delTopUp = useMutation({
    mutationFn: (id: string) => gqlClient.request(ValueAddDoc, { body: { action: 'delete', valueAdd: { id, usd: 0 } } }),
    onSuccess: refresh,
    onError: (e) => setErr(extractMessage(e)),
  })
  const parent = k.parent ? d.keys.find((x) => x.key === k.parent) : undefined
  const who = k.kind === 'child' ? `sub-key of ${parent ? keyName(parent) : shortId(k.parent ?? '')}` : k.kind === 'configured' ? 'set up in the config' : 'approved here'
  return (
    <TableRow sx={{ opacity: k.expired ? 0.55 : 1, verticalAlign: 'top' }}>
      <TableCell>
        <Typography variant="body2" sx={{ fontWeight: 600 }}>
          {keyName(k)}
        </Typography>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
          {k.label && (
            <Typography variant="caption" color="text.secondary" sx={{ fontFamily: 'monospace', whiteSpace: 'nowrap' }}>
              {shortId(k.key)}
            </Typography>
          )}
          <Button size="small" color="inherit" sx={{ minWidth: 0, px: 0.5, py: 0, fontSize: 12 }} onClick={() => void navigator.clipboard.writeText(k.key)}>
            copy key
          </Button>
        </Box>
        <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
          {who}
          {k.mint ? ' · may create sub-keys' : ''}
        </Typography>
        {err && (
          <Typography variant="caption" color="error" sx={{ display: 'block' }}>
            {err}
          </Typography>
        )}
      </TableCell>
      <TableCell>
        {k.limits.length === 0 && (
          <Typography variant="body2" sx={{ color: C.warn }}>
            no limit
          </Typography>
        )}
        {k.limits.map((l, i) => {
          const isUsd = (l.usd ?? 0) > 0
          const w = k.windows.find((x) => x.scope === (l.model || 'key') && x.label === `${isUsd ? 'usd' : 'req'}/${l.per}`)
          const fmt = (n: number) => (isUsd ? usd(n) : `${Math.round(n)} requests`)
          return (
            <Typography key={i} variant="body2" sx={{ color: w?.blocked ? C.error : undefined }}>
              {fmt(w?.used ?? 0)} of {fmt(isUsd ? (l.usd ?? 0) : (l.req ?? 0))} {perShort(l.per)}
              {l.model ? ` on ${l.model}` : ''}
              {!l.hard ? ' (warning only)' : ''}
              {w?.blocked ? ' — reached' : ''}
            </Typography>
          )
        })}
      </TableCell>
      <TableCell>
        {k.valueAdds.length === 0 && (
          <Typography variant="body2" color="text.secondary">
            —
          </Typography>
        )}
        {k.valueAdds.map((v) => (
          <Box key={v.id} sx={{ display: 'flex', alignItems: 'center', opacity: v.active ? 1 : 0.55 }}>
            <Typography variant="body2">
              {usd(Math.max(v.usd - (v.spent ?? 0), 0))} of {usd(v.usd)}
              {v.model ? ` on ${v.model}` : ''}
              {v.expires ? `, until ${when(v.expires)}` : ''}
              {!v.active ? ' (over)' : ''}
            </Typography>
            <Button size="small" color="inherit" sx={{ minWidth: 0, px: 0.5, py: 0, fontSize: 12 }} onClick={() => delTopUp.mutate(v.id ?? '')}>
              remove
            </Button>
          </Box>
        ))}
      </TableCell>
      <TableCell sx={{ whiteSpace: 'nowrap' }}>
        <Typography variant="body2" sx={{ color: k.expired ? C.error : undefined }} color={k.expires ? undefined : 'text.secondary'}>
          {k.kind === 'configured' ? 'never' : k.expires ? `${k.expired ? 'stopped ' : ''}${when(k.expires)}` : 'never'}
        </Typography>
      </TableCell>
      <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
        <Button size="small" onClick={() => setEdit(true)}>
          Edit
        </Button>
        <Button size="small" onClick={() => setAddTopUp(true)}>
          Top up
        </Button>
        {k.kind !== 'configured' &&
          (k.expired ? (
            <Button size="small" onClick={() => act.mutate('enable')}>
              Switch on
            </Button>
          ) : (
            <Button size="small" color="inherit" onClick={() => act.mutate('disable')}>
              Switch off
            </Button>
          ))}
        {k.kind !== 'configured' && (
          <Button size="small" color="error" onClick={() => setConfirmDelete(true)}>
            Delete
          </Button>
        )}
      </TableCell>
      {edit && <EditKeyDialog k={k} d={d} onClose={() => setEdit(false)} onDone={refresh} />}
      {addTopUp && <TopUpDialog k={k} d={d} onClose={() => setAddTopUp(false)} onDone={refresh} />}
      <Dialog open={confirmDelete} onClose={() => setConfirmDelete(false)}>
        <DialogTitle>Delete {keyName(k)}?</DialogTitle>
        <DialogContent>
          <Typography variant="body2">
            The key stops working at once, and so do any sub-keys it made. Its top-ups are removed. To stop it but keep it listed,
            use Switch off instead.
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirmDelete(false)}>Cancel</Button>
          <Button variant="contained" color="error" onClick={() => act.mutate('delete')} disabled={act.isPending}>
            Delete
          </Button>
        </DialogActions>
      </Dialog>
    </TableRow>
  )
}

function EditKeyDialog({ k, d, onClose, onDone }: { k: Key; d: Data; onClose: () => void; onDone: () => void }) {
  const [f, setF] = useState<Form>(formFromKey(k))
  const [err, setErr] = useState('')
  const set = (p: Partial<Form>) => setF((cur) => ({ ...cur, ...p }))
  const save = useMutation({
    mutationFn: () => gqlClient.request(UpdateKeyDoc, { body: { key: k.key, edit: { ...formToGrant(f), valueAdds: [] } } }),
    onSuccess: () => {
      onDone()
      onClose()
    },
    onError: (e) => setErr(extractMessage(e)),
  })
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="md">
      <DialogTitle>Edit {keyName(k)}</DialogTitle>
      <DialogContent>
        <Stack spacing={2.5} sx={{ mt: 0.5 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <LimitsEditor f={f} set={set} d={d} />
          {k.kind !== 'configured' && (
            <TextField
              label="Stops working after"
              placeholder="7d, 48h — empty: keep as is"
              size="small"
              value={f.ttl}
              onChange={(e) => set({ ttl: e.target.value })}
              helperText={f.ttl ? (endsAt(f.ttl) ? `On ${endsAt(f.ttl)}` : 'Write it as 7d or 48h') : k.expires ? `Now: ${when(k.expires)}` : 'Now: never'}
              error={!!f.ttl && !endsAt(f.ttl)}
              sx={{ width: 300 }}
            />
          )}
          {k.kind !== 'child' && <MoreSettings f={f} set={set} d={d} askedModels={[]} />}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" onClick={() => save.mutate()} disabled={save.isPending || (!!f.ttl && !endsAt(f.ttl))}>
          Save
        </Button>
      </DialogActions>
    </Dialog>
  )
}

function TopUpDialog({ k, d, onClose, onDone }: { k: Key; d: Data; onClose: () => void; onDone: () => void }) {
  const [v, setV] = useState<TopUp>({ usd: '', model: '', ttl: '2d', note: '' })
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
  const stops = k.limits.some((l) => l.hard)
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>Top up {keyName(k)}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ mt: 0.5 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <Typography variant="body2" color="text.secondary">
            {stops ? 'Used only after its limits are reached.' : 'This key has no limit that stops it, so a top-up would never be used.'}
          </Typography>
          <TopUpRow v={v} set={(p) => setV({ ...v, ...p })} d={d} />
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" onClick={() => add.mutate()} disabled={add.isPending || !(Number(v.usd) > 0) || (!!v.ttl && !endsAt(v.ttl))}>
          Add {Number(v.usd) > 0 ? usd(Number(v.usd)) : ''}
        </Button>
      </DialogActions>
    </Dialog>
  )
}

function DecidedPanel({ rows, d }: { rows: Req[]; d: Data }) {
  return (
    <Panel title="Decided" flush>
      <TableContainer>
        <Table size="small">
          <TableBody>
            {rows.map((r) => {
              const g = r.granted
              const line = r.status === 'approved' ? costLine(g.limits ?? [], g.valueAdds ?? [], g.ttl, !r.requesterKey).text : ''
              return (
                <TableRow key={r.id} sx={{ verticalAlign: 'top' }}>
                  <TableCell>
                    <Typography variant="body2">{requestTitle(r, d)}</Typography>
                    <Typography variant="caption" color="text.secondary">
                      “{r.asked.purpose}”
                    </Typography>
                  </TableCell>
                  <TableCell>
                    <Chip size="small" color={r.status === 'approved' ? 'success' : 'default'} label={r.status} />
                    <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
                      {fmtAgo(r.decided)}
                      {r.decidedBy && r.decidedBy !== 'admin token' ? ` by ${r.decidedBy}` : ''}
                    </Typography>
                  </TableCell>
                  <TableCell>
                    <Typography variant="body2">{line}</Typography>
                    {r.reason && (
                      <Typography variant="caption" color="text.secondary">
                        your note: {r.reason}
                      </Typography>
                    )}
                  </TableCell>
                  <TableCell>
                    {r.issuedKey && (
                      <Typography variant="caption" color="text.secondary">
                        {r.delivered ? 'the assistant has its key' : 'the assistant has not collected its key yet'}
                      </Typography>
                    )}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </TableContainer>
    </Panel>
  )
}

export const Route = createFileRoute('/requests')({ component: Requests })
