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
  Menu,
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
import AddIcon from '@mui/icons-material/Add'
import ExpandMore from '@mui/icons-material/ExpandMore'
import ExpandLess from '@mui/icons-material/ExpandLess'
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
            from
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
          spentToday
          requestsToday
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
  if (n === 0) return '$0.00'
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
/**
 * What a request is, by the kind of change — "More for sk-x" covered a cap, a
 * top-up and a permission alike (stooges pass 2) — led by who says they asked.
 */
function requestTitle(r: Req, d: Data): string {
  const a = r.asked
  const who = a.from ? `${a.from}: ` : ''
  if (!r.requesterKey) return `${who}new key “${a.label}”`
  const k = d.keys.find((x) => x.key === r.requesterKey)
  const name = k ? keyName(k) : r.requesterKey
  const kinds: string[] = []
  for (const v of a.valueAdds ?? []) kinds.push(`top up ${name} with ${usd(v.usd)}${v.ttl ? ` for ${ttlWords(v.ttl)}` : ''}`)
  if (a.askedLimits) kinds.push(a.limits?.length ? `set ${name}'s limit to ${(a.limits ?? []).map((l) => limitText(l).replace(/^up to /, '')).join(', ')}` : `remove ${name}'s limits`)
  if (a.askedMint) kinds.push(a.mint ? `let ${name} create sub-keys` : `stop ${name} creating sub-keys`)
  if (a.ttl || a.expires) kinds.push(`change when ${name} stops working`)
  const text = kinds.length ? kinds.join(' · ') : `change ${name}`
  return who + text
}

/** "2d" → "2 days", "48h" → "48 hours". */
function ttlWords(t: string): string {
  return t.replace(/^(\d+)d$/, (_, n) => `${n} day${n === '1' ? '' : 's'}`).replace(/^(\d+)h$/, (_, n) => `${n} hour${n === '1' ? '' : 's'}`)
}

/** A model name without the provider prefix it carries ("fireworks-…"). */
function modelName(m: string, d: Data): string {
  const p = d.credentials.map((c) => c.provider).find((pr) => m.startsWith(pr + '-'))
  return p ? m.slice(p.length + 1) : m
}

/**
 * The most a key could still spend between now and `until`, from its hard
 * dollar limits and top-ups: the remaining day (or month, or one-off), each
 * whole day after, and every top-up that is still live. Model-only limits are
 * summed when there is no key-wide one. `about` marks a rolling window, which
 * this can only bound loosely.
 */
function worstCase(k: Key, newTopUps: { usd: number }[], untilMs: number): { usd: number; about: boolean } {
  const hard = k.limits.filter((l) => l.hard && (l.usd ?? 0) > 0)
  const wide = hard.filter((l) => !l.model)
  const use = wide.length ? wide : hard
  let total = 0
  let about = false
  const now = Date.now()
  for (const l of use) {
    const w = k.windows.find((x) => x.scope === (l.model || 'key') && x.label === `usd/${l.per}`)
    const left = Math.max((l.usd ?? 0) - (w?.used ?? 0), 0)
    if (l.per === 'calendar-day') {
      const midnights = Math.max(0, Math.floor(untilMs / DAY) - Math.floor(now / DAY))
      total += left + midnights * (l.usd ?? 0)
    } else if (l.per === 'total' || l.per === 'calendar-month') {
      total += left
    } else {
      about = true
      total += left
    }
  }
  for (const v of k.valueAdds.filter((x) => x.active)) total += Math.max(v.usd - (v.spent ?? 0), 0)
  for (const v of newTopUps) total += v.usd
  return { usd: total, about }
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
  if (ro) return <Alert severity="info">Approvals are for admins: the page shows keys.</Alert>
  if (q.error) return <Alert severity="error">{extractMessage(q.error)}</Alert>
  if (!d) return <Loading />
  const pending = d.requests.filter((r) => r.status === 'pending')
  const decided = d.requests.filter((r) => r.status !== 'pending')
  return (
    <Box sx={{ p: 2, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <PageHeader title="Approvals">
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
  const extra = !r.requesterKey && a.models?.length ? [`for ${a.models.map((m) => modelName(m, d)).join(', ')}`] : []
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
  const line = costLine(r.asked.limits ?? [], r.asked.valueAdds ?? [], r.asked.ttl, !r.requesterKey)
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
        <Typography variant="body2" sx={{ fontWeight: 600 }}>
          {line.text}
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1, whiteSpace: 'pre-wrap' }}>
          “{r.asked.purpose}”
        </Typography>
        <TextField
          label="Reason"
          helperText="The assistant is told this the next time it checks on its request."
          fullWidth
          size="small"
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
  const newTopUps = (g.valueAdds ?? []).map((v) => ({ usd: v.usd, ttl: v.ttl }))
  let summary = costLine(g.limits, newTopUps, g.ttl, !r.requesterKey)
  if (existing && newTopUps.length) {
    // The worst case over the life of the longest new top-up: what the owner
    // is actually agreeing to (stooges pass 2: "At most a $10 top-up" was not
    // a total).
    const until = Math.max(...newTopUps.map((v) => Date.now() + (v.ttl ? ttlMs(v.ttl) : DAY)))
    const w = worstCase(existing, newTopUps, until)
    summary = { text: `${keyName(existing)} could spend ${w.about ? 'about ' : ''}${usd(w.usd)} at most until ${when(new Date(until))}`, open: false }
  }
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
  const showTopUps = (a.valueAdds?.length ?? 0) > 0 || f.topUps.length > 0
  const showExpiry = !r.requesterKey || !!a.ttl || !!a.expires
  const paid = d.credentials.filter((c) => f.credentials.includes(c.scope))
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="md">
      <DialogTitle>Approve: {title}?</DialogTitle>
      <DialogContent>
        <Stack spacing={2.5} sx={{ mt: 0.5 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <Typography variant="body2" color="text.secondary" sx={{ whiteSpace: 'pre-wrap' }}>
            “{a.purpose}”
          </Typography>
          {existing && <KeyNow k={existing} d={d} />}
          {!existing && (
            <Box>
              <Typography variant="subtitle2">What it can use</Typography>
              <Typography variant="body2">
                {a.models?.length ? a.models.map((m) => modelName(m, d)).join(', ') : 'any model'}
                {' · '}
                {paid.length ? `paid account: ${paid.map((c) => c.provider).join(', ')}` : 'no paid account'}
              </Typography>
            </Box>
          )}
          {showLimits && <LimitsEditor f={f} set={set} d={d} />}
          {showTopUps ? (
            <TopUpsEditor f={f} set={set} d={d} />
          ) : (
            <Box>
              <Button size="small" startIcon={<AddIcon />} onClick={() => set({ topUps: [{ usd: '', model: '', ttl: '2d', note: '' }] })}>
                Add a top-up
              </Button>
            </Box>
          )}
          {showExpiry && (
            <Box>
              <Typography variant="subtitle2">When it stops</Typography>
              <TextField
                label="Key expires after"
                placeholder="7d, 48h — empty: never"
                size="small"
                value={f.ttl}
                onChange={(e) => set({ ttl: e.target.value })}
                helperText={f.ttl ? (endsAt(f.ttl) ? `On ${endsAt(f.ttl)}, if you approve now` : 'Write it as 7d or 48h') : 'Never'}
                error={!!f.ttl && !endsAt(f.ttl)}
                sx={{ width: 300, mt: 1.5 }}
              />
            </Box>
          )}
          {existing && a.askedMint && (
            <FormControlLabel control={<Switch checked={f.mint} onChange={(e) => set({ mint: e.target.checked })} />} label="May create its own sub-keys, with budgets you cannot see here" />
          )}
          {!existing && (
            <Box>
              <Button size="small" color="inherit" endIcon={more ? <ExpandLess /> : <ExpandMore />} onClick={() => setMore(!more)}>
                More settings: name, priority, paid accounts, sub-keys
              </Button>
              {more && <MoreSettings f={f} set={set} d={d} askedModels={a.models ?? []} />}
            </Box>
          )}
          <TextField label="Note to the assistant (optional)" size="small" value={f.reason} onChange={(e) => set({ reason: e.target.value })} />
        </Stack>
      </DialogContent>
      <Box sx={{ px: 3, pt: 1.5, borderTop: `1px solid ${C.border}` }}>
        <Typography variant="subtitle2" sx={{ color: summary.open ? C.warn : undefined }}>
          {summary.text}
        </Typography>
        <Typography variant="caption" color="text.secondary">
          {existing
            ? 'Takes effect at once. You can change or switch it off later under Keys.'
            : 'The assistant gets the key the next time it checks, once. You can switch it off later under Keys.'}
        </Typography>
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
function KeyNow({ k, d }: { k: Key; d: Data }) {
  const parts = [
    `spent ${usd(k.spentToday)} today`,
    k.limits.length ? k.limits.map((l) => limitText({ ...l, model: l.model ? modelName(l.model, d) : l.model })).join('; ') : 'no spending limit',
    ...k.valueAdds.filter((v) => v.active).map((v) => `${usd(v.usd - (v.spent ?? 0))} top-up left${v.expires ? ` until ${when(v.expires)}` : ''}`),
    ...(k.mint ? ['may create sub-keys'] : []),
  ]
  return (
    <Box sx={{ borderTop: `1px solid ${C.border}`, borderBottom: `1px solid ${C.border}`, py: 1.25 }}>
      <Typography variant="caption" color="text.secondary">
        {keyName(k)} now
      </Typography>
      <Typography variant="body2">{parts.join(' · ')}</Typography>
      <Typography variant="caption" color="text.secondary">
        Approving changes only what was asked.
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
          <TextField label="Amount" size="small" type="number" value={l.amount} onChange={(e) => setLim(i, { amount: e.target.value })} sx={{ width: 130 }} />
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
                {modelName(m, d)}
              </MenuItem>
            ))}
          </TextField>
          <TextField select size="small" label="When reached" value={l.hard ? 'stop' : 'warn'} onChange={(e) => setLim(i, { hard: e.target.value === 'stop' })} sx={{ width: 170 }}>
            <MenuItem value="stop">stop it</MenuItem>
            <MenuItem value="warn">only warn me</MenuItem>
          </TextField>
          <Button size="small" color="inherit" onClick={() => set({ limits: f.limits.filter((_, j) => j !== i) })}>
            Remove
          </Button>
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
        Used only after the limits are reached.
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
            {modelName(m, d)}
          </MenuItem>
        ))}
      </TextField>
      <TextField
        label="Top-up expires after"
        placeholder="2d, 48h"
        size="small"
        value={v.ttl}
        onChange={(e) => set({ ttl: e.target.value })}
        helperText={v.ttl ? (endsAt(v.ttl) ? `On ${endsAt(v.ttl)}` : 'Write it as 2d or 48h') : 'Until spent'}
        error={!!v.ttl && !endsAt(v.ttl)}
        sx={{ width: 200 }}
      />
      {remove && (
        <Button size="small" color="inherit" onClick={remove}>
          Remove
        </Button>
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
  // A configured key nobody set anything on here is not something handed out.
  const managed = (k: Key) => k.kind !== 'configured' || k.mint || k.limits.length > 0 || k.valueAdds.length > 0
  const keys = d.keys.filter((k) => showAll || managed(k))
  const hidden = d.keys.length - d.keys.filter(managed).length
  return (
    <Panel
      title="Keys you have handed out"
      actions={
        hidden > 0 && (
          <FormControlLabel
            control={<Switch size="small" checked={showAll} onChange={(e) => setShowAll(e.target.checked)} />}
            label={<Typography variant="body2" color="text.secondary">{`Also show ${hidden} other configured key${hidden === 1 ? '' : 's'}`}</Typography>}
          />
        )
      }
      flush
    >
      <TableContainer>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Key</TableCell>
              <TableCell>Spent today</TableCell>
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
  const [menu, setMenu] = useState<HTMLElement | null>(null)
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
  const who = k.kind === 'child' ? `sub-key of ${parent ? keyName(parent) : shortId(k.parent ?? '')}` : k.kind === 'configured' ? 'set up in the config' : ''
  const configured = k.kind === 'configured'
  return (
    <TableRow sx={{ opacity: k.expired ? 0.55 : 1, verticalAlign: 'top' }}>
      <TableCell sx={{ minWidth: 220 }}>
        <Typography variant="body2" sx={{ fontWeight: 600 }}>
          {keyName(k)}
        </Typography>
        <Typography variant="caption" color="text.secondary" sx={{ display: 'block', whiteSpace: 'nowrap' }}>
          {[who, k.mint ? 'may create sub-keys' : ''].filter(Boolean).join(' · ')}
        </Typography>
        {err && (
          <Typography variant="caption" color="error" sx={{ display: 'block' }}>
            {err}
          </Typography>
        )}
      </TableCell>
      <TableCell>
        <Typography variant="body2">
          {usd(k.spentToday)}
          {k.limits.length === 0 ? ' · no limit' : ''}
        </Typography>
        {k.limits.map((l, i) => {
          const isUsd = (l.usd ?? 0) > 0
          const w = k.windows.find((x) => x.scope === (l.model || 'key') && x.label === `${isUsd ? 'usd' : 'req'}/${l.per}`)
          const fmt = (n: number) => (isUsd ? usd(n) : `${Math.round(n)} requests`)
          return (
            <Typography key={i} variant="caption" color={w?.blocked ? 'error' : 'text.secondary'} sx={{ display: 'block' }}>
              {fmt(w?.used ?? 0)} of {fmt(isUsd ? (l.usd ?? 0) : (l.req ?? 0))} {perShort(l.per)}
              {l.model ? ` on ${modelName(l.model, d)}` : ''}
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
          <Box key={v.id} sx={{ opacity: v.active ? 1 : 0.55, mb: 0.5 }}>
            <Typography variant="body2">
              {usd(Math.max(v.usd - (v.spent ?? 0), 0))} of {usd(v.usd)}
              {v.model ? ` on ${modelName(v.model, d)}` : ''}
              {!v.active ? ' (over)' : ''}
            </Typography>
            <Typography variant="caption" color="text.secondary">
              {v.expires ? `until ${when(v.expires)} · ` : ''}
              <Box component="span" role="button" tabIndex={0} sx={{ cursor: 'pointer', textDecoration: 'underline' }} onClick={() => delTopUp.mutate(v.id ?? '')}>
                remove this top-up
              </Box>
            </Typography>
          </Box>
        ))}
      </TableCell>
      <TableCell sx={{ whiteSpace: 'nowrap' }}>
        <Typography variant="body2" sx={{ color: k.expired ? C.error : undefined }} color={k.expires ? undefined : 'text.secondary'}>
          {configured ? 'never' : k.expires ? `${k.expired ? 'stopped ' : ''}${when(k.expires)}` : 'never'}
        </Typography>
      </TableCell>
      <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
        <Button size="small" onClick={() => setEdit(true)}>
          Edit
        </Button>
        <Button size="small" onClick={() => setAddTopUp(true)}>
          Top up
        </Button>
        <Button size="small" color="inherit" endIcon={<ExpandMore />} onClick={(e) => setMenu(e.currentTarget)}>
          More
        </Button>
        <Menu anchorEl={menu} open={!!menu} onClose={() => setMenu(null)}>
          <MenuItem onClick={() => void navigator.clipboard.writeText(k.key).then(() => setMenu(null))}>Copy the key</MenuItem>
          {configured ? (
            <MenuItem disabled>Switch off or delete: edit the config (set up there)</MenuItem>
          ) : (
            [
              k.expired ? (
                <MenuItem key="on" onClick={() => (setMenu(null), act.mutate('enable'))}>
                  Switch on again
                </MenuItem>
              ) : (
                <MenuItem key="off" onClick={() => (setMenu(null), act.mutate('disable'))}>
                  Switch off now (stays listed; can be switched on again)
                </MenuItem>
              ),
              <MenuItem key="del" sx={{ color: C.error }} onClick={() => (setMenu(null), setConfirmDelete(true))}>
                Delete…
              </MenuItem>,
            ]
          )}
        </Menu>
      </TableCell>
      {edit && <EditKeyDialog k={k} d={d} onClose={() => setEdit(false)} onDone={refresh} />}
      {addTopUp && <TopUpDialog k={k} d={d} onClose={() => setAddTopUp(false)} onDone={refresh} />}
      <Dialog open={confirmDelete} onClose={() => setConfirmDelete(false)}>
        <DialogTitle>Delete {keyName(k)}?</DialogTitle>
        <DialogContent>
          <Typography variant="body2">
            The key stops working at once, and so do any sub-keys it made. Its top-ups are removed. To stop it but keep it listed,
            use More → Switch off now instead.
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
              label="Key expires after"
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
              // The key's real expiry, not one recomputed from the ttl now
              // (stooges pass 2: one key showed two stop times).
              const issued = r.issuedKey ? d.keys.find((k) => k.key === r.issuedKey) : undefined
              let line = r.status === 'approved' ? costLine(g.limits ?? [], g.valueAdds ?? [], undefined, false).text : ''
              if (issued) line += issued.expires ? ` · stops working ${when(issued.expires)}` : ' · never stops by itself'
              return (
                <TableRow key={r.id} sx={{ verticalAlign: 'top' }}>
                  <TableCell>
                    <Typography variant="body2" sx={{ fontWeight: 600 }}>
                      {requestTitle(r, d)}
                    </Typography>
                    <Typography variant="caption" color="text.secondary">
                      “{r.asked.purpose}”
                    </Typography>
                  </TableCell>
                  <TableCell sx={{ minWidth: 220 }}>
                    <Chip size="small" color={r.status === 'approved' ? 'success' : 'default'} label={r.status} />
                    <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
                      {fmtAgo(r.decided)}
                      {r.decidedBy && r.decidedBy !== 'admin token' ? ` by ${r.decidedBy}` : ''}
                    </Typography>
                    {r.issuedKey && (
                      <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
                        {r.delivered ? 'the assistant has its key' : 'the assistant has not collected its key yet'}
                      </Typography>
                    )}
                    {r.status === 'denied' && (
                      <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
                        the assistant is told on its next check
                      </Typography>
                    )}
                  </TableCell>
                  <TableCell>
                    <Typography variant="body2">{line}</Typography>
                    {r.reason && (
                      <Typography variant="caption" color="text.secondary">
                        your note: {r.reason}
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

export const Route = createFileRoute('/approvals')({ component: Requests })
