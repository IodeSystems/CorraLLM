import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'
import {
  Alert,
  Autocomplete,
  Box,
  Button,
  Checkbox,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  InputAdornment,
  Menu,
  MenuItem,
  Select,
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
import SearchIcon from '@mui/icons-material/Search'
import ExpandMore from '@mui/icons-material/ExpandMore'
import ExpandLess from '@mui/icons-material/ExpandLess'
import WarningAmberOutlined from '@mui/icons-material/WarningAmberOutlined'
import { graphql } from '@/gql'
import type { RequestsQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { Panel, PageHeader } from '@/Panel'
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
            budgets {
              on
              limits {
                req
                usd
                per
                hard
              }
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
            askedModels
            askedBudgets
            askedMint
          }
          granted {
            label
            purpose
            models
            group
            budgets {
              on
              limits {
                req
                usd
                per
                hard
              }
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
            askedModels
            askedBudgets
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
          models
          budgets {
            on
            limits {
              req
              usd
              per
              hard
            }
          }
          warnings
          mint
          created
          expires
          expired
          disabled
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

const CreateKeyDoc = graphql(/* GraphQL */ `
  mutation CreateKey($body: corrallm_CreateKeyInputBodyInput!) {
    corrallm {
      createKey(body: $body) {
        ok
        key
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

// The editable form of a grant: the dialog's state.
type Lim = { amount: string; unit: 'usd' | 'req'; per: string; hard: boolean }
type TopUp = { usd: string; model: string; ttl: string; note: string }
/** One budget as the form edits it: a selector and what may be spent on it. */
type Budget = { on: string; limits: Lim[] }
type Form = {
  label: string
  group: string
  /** Selectors naming what the key may use; empty = every model. */
  models: string[]
  budgets: Budget[]
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

type LimitIn = { req?: number | null; usd?: number | null; per: string; hard?: boolean | null }
type BudgetIn = { on: string; limits?: LimitIn[] | null }

function toLim(l: LimitIn): Lim {
  const isUsd = (l.usd ?? 0) > 0
  return { amount: String(isUsd ? l.usd : (l.req ?? '')), unit: isUsd ? 'usd' : 'req', per: l.per, hard: !!l.hard }
}

/**
 * A selector as the server stores it: a bare name means model:<name>. Mirrors
 * config.NormalizeSelector.
 */
function normSel(s: string): string {
  const t = s.trim()
  if (t === '' || t === '*' || t === 'local' || t === 'proxy' || /^(provider|cred|model):/.test(t)) return t
  return `model:${t}`
}

/**
 * What a selector names, in words, for running text: "everything", a model's
 * name, "local models", "paid providers", "fireworks models", "the
 * fireworks/main account". The one place a selector becomes words; the page
 * never shows the selector itself (UX review 10-28).
 */
function selWords(s: string): string {
  const t = normSel(s)
  if (t === '*') return 'everything'
  if (t === 'local') return 'local models'
  if (t === 'proxy') return 'paid providers'
  const [kind, name] = [t.slice(0, t.indexOf(':')), t.slice(t.indexOf(':') + 1)]
  const glob = name.includes('*')
  if (kind === 'provider') return glob ? `models of providers matching ${name}` : `${name} models`
  if (kind === 'cred') return glob ? `accounts matching ${name}` : `the ${name} account`
  if (kind === 'model') return glob ? `models matching ${name}` : name
  return t
}

/** A selector as a choice in a picker: "Every model", "Local models", a model's name. */
function selOption(s: string): string {
  if (normSel(s) === '*') return 'Every model'
  const w = selWords(s)
  return normSel(s).startsWith('model:') ? w : w.charAt(0).toUpperCase() + w.slice(1)
}

/** What a selector covers, for the field's helper text. */
function selHint(s: string): string {
  const t = normSel(s)
  if (t === '') return ''
  if (t === '*') return ''
  if (t === 'local') return 'Models that run on this box'
  if (t === 'proxy') return 'Models served by a remote provider'
  const glob = t.includes('*')
  if (t.startsWith('provider:')) return glob ? 'Every provider whose name matches' : 'Every model of this provider'
  if (t.startsWith('cred:')) return glob ? 'Every account whose name matches' : 'One account'
  return glob ? 'Every model whose name matches' : 'One model'
}

/**
 * Selectors to offer: the kinds, then each provider, account and model this
 * box has. Free text is allowed too — a glob, or a name added later.
 */
function selectorOptions(d: Data): string[] {
  const out = ['*', 'local', 'proxy']
  for (const c of d.credentials) out.push(`provider:${c.provider}`)
  for (const c of d.credentials) out.push(c.scope)
  for (const m of d.models) out.push(`model:${m}`)
  return [...new Set(out)]
}

function limitText(l: LimitIn, on: string): string {
  const amt = (l.usd ?? 0) > 0 ? usd(l.usd ?? 0) : `${l.req} requests`
  return `${l.hard ? 'up to' : 'warn past'} ${amt} ${perShort(l.per)} on ${selWords(on)}`
}

/** Every limit of every budget, each with the selector it sits on. */
function flatLimits(budgets: BudgetIn[]): { on: string; l: LimitIn }[] {
  return budgets.flatMap((b) => (b.limits ?? []).map((l) => ({ on: b.on, l })))
}

/** A key's budgets in one line; "no limit" when it has none. */
function budgetsText(budgets: BudgetIn[]): string {
  const all = flatLimits(budgets)
  return all.length ? all.map((x) => limitText(x.l, x.on).replace(/^up to /, '')).join(', ') : 'no limit'
}

/** A key's models in one line. */
function modelsText(models: string[] | null | undefined): string {
  return models?.length ? models.map(selWords).join(', ') : 'every model'
}

/**
 * The one line the owner decides on: the most it can spend, and when it stops.
 * Every finding of the stooges pass said this was missing or buried.
 *
 * Budgets nest — every one that matches a request is charged — so the most a
 * key can spend is what its budgets on everything ("*") allow. A narrower
 * budget only limits a part of that. With no budget on everything, the
 * narrower ones are added up, and the rest has no limit.
 */
function costLine(budgets: BudgetIn[], topUps: { usd: number; ttl?: string | null }[], ttl: string | null | undefined, newKey: boolean): { text: string; open: boolean } {
  const money = flatLimits(budgets).filter((x) => x.l.hard && (x.l.usd ?? 0) > 0)
  const wide = money.filter((x) => x.on === '*')
  const fmt = (x: { on: string; l: LimitIn }) => `${usd(x.l.usd ?? 0)} ${perShort(x.l.per)}${x.on === '*' ? '' : ` on ${selWords(x.on)}`}`
  const parts = (wide.length ? wide : money).map(fmt)
  for (const t of topUps) parts.push(`a ${usd(t.usd)} top-up${t.ttl ? ` for ${t.ttl.replace(/d$/, ' days').replace(/h$/, ' hours')}` : ''}`)
  const open = newKey && wide.length === 0
  let text = parts.length ? `At most ${parts.join(', plus ')}` : open ? 'No spending limit' : 'No change to what it may spend'
  if (wide.length && money.length > wide.length) text += `, of it at most ${money.filter((x) => x.on !== '*').map(fmt).join(', ')}`
  if (!wide.length && money.length) text += newKey ? '; no limit on anything else' : ''
  if (newKey || ttl) {
    const end = ttl ? endsAt(ttl) : null
    text += end ? ` · expires ${end}` : ' · never expires'
  }
  return { text, open }
}

/** A grant's budgets as the form edits them. */
function toBudgets(list: BudgetIn[] | null | undefined): Budget[] {
  return (list ?? []).map((b) => ({ on: b.on, limits: (b.limits ?? []).map(toLim) }))
}

/** The form's budgets back to the wire; a budget with no selector or no amount is dropped. */
function fromBudgets(list: Budget[]) {
  return list
    .map((b) => ({
      on: normSel(b.on),
      limits: b.limits
        .filter((l) => l.amount.trim() !== '')
        .map((l) => ({ per: l.per, hard: l.hard, ...(l.unit === 'usd' ? { usd: Number(l.amount) } : { req: Number(l.amount) }) })),
    }))
    .filter((b) => b.on !== '' && b.limits.length > 0)
}

function formFromRequest(r: Req): Form {
  const a = r.asked
  return {
    label: a.label ?? '',
    group: a.group ?? '',
    models: a.models ?? [],
    budgets: toBudgets(a.budgets),
    topUps: (a.valueAdds ?? []).map((v) => ({ usd: String(v.usd), model: v.model ?? '', ttl: v.ttl ?? '', note: v.note ?? '' })),
    ttl: a.ttl ?? '',
    mint: !!a.mint,
    reason: '',
  }
}

function formFromKey(k: Key): Form {
  return { label: k.label ?? '', group: k.group ?? '', models: k.models, budgets: toBudgets(k.budgets), topUps: [], ttl: '', mint: k.mint, reason: '' }
}

function formToGrant(f: Form) {
  return {
    label: f.label.trim(),
    group: f.group,
    models: [...new Set(f.models.map(normSel).filter(Boolean))],
    budgets: fromBudgets(f.budgets),
    valueAdds: f.topUps
      .filter((v) => v.usd.trim() !== '')
      .map((v) => ({ usd: Number(v.usd), model: v.model || undefined, ttl: v.ttl.trim() || undefined, note: v.note.trim() || undefined })),
    ttl: f.ttl.trim() || undefined,
    mint: f.mint,
  }
}

/**
 * A key string as the page shows it: the first four and last four characters
 * (owner 10-28). A key IS its own credential, so the page never prints one
 * whole unless the owner presses the eye on that row. A key of eight
 * characters or fewer shows its first four ("sk-a…"), since four and four would
 * be the whole key.
 */
export function maskKey(key: string): string {
  if (key.length > 8) return `${key.slice(0, 4)}…${key.slice(-4)}`
  return `${key.slice(0, Math.min(4, key.length - 1))}…`
}

/**
 * The name of the assistant that asked about this key — the `from` of the
 * newest request it filed with the key or was given it by — or ''.
 */
function askedName(key: string, d: Data): string {
  const rs = d.requests.filter((r) => (r.requesterKey === key || r.issuedKey === key) && r.asked.from)
  rs.sort((a, b) => new Date(b.created).getTime() - new Date(a.created).getTime())
  return rs[0]?.asked.from ?? ''
}

/**
 * A key's name wherever the page names it (owner 10-28): its label, else the
 * assistant that asked about it, else its masked id. A configured key has a
 * label only when someone gave it one here.
 */
const keyName = (k: Pick<Key, 'key' | 'label'>, d: Data) => k.label || askedName(k.key, d) || maskKey(k.key)

/** The search box matches these words of a request. */
function requestMatches(r: Req, q: string): boolean {
  const a = r.asked
  return [a.label, a.from, a.purpose, r.requesterKey, r.issuedKey, ...(a.models ?? [])].some((v) => !!v && v.toLowerCase().includes(q))
}

/** The search box matches these words of a key; a sub-key also matches on its parent. */
function keyMatches(k: Key, q: string, d: Data): boolean {
  const parent = k.parent ? d.keys.find((p) => p.key === k.parent) : undefined
  return [k.key, k.label, k.parent, parent?.label, ...k.models, ...askers(k.key, d)].some((v) => !!v && v.toLowerCase().includes(q))
}
type Decision = { head: string; working?: string; open: boolean }

/**
 * The one line a decision rests on, the same on the card, in Approve and in
 * Deny. "At most" always means the most that could be spent (stooges pass 4:
 * on a top-up card it meant the top-up's size, in the dialog the total).
 */
function decision(
  budgets: BudgetIn[],
  topUps: { usd: number; ttl?: string | null }[],
  ttl: string | null | undefined,
  newKey: boolean,
  existing?: Key,
): Decision {
  if (existing && topUps.length) {
    const until = Math.max(...topUps.map((v) => Date.now() + (v.ttl ? ttlMs(v.ttl) : DAY)))
    const w = worstCase(existing, topUps, until)
    return { head: `At most ${w.about ? 'about ' : ''}${usd(w.usd)} until ${when(new Date(until))}`, working: `${w.parts.join(' + ')}`, open: false }
  }
  if (existing && budgets.length === 0 && topUps.length === 0 && !ttl) {
    return { head: 'No change to what it may spend', open: false }
  }
  const c = costLine(budgets, topUps, ttl, newKey)
  return { head: c.text, open: c.open }
}

/**
 * What a request is, by the kind of change — "More for sk-x" covered a cap, a
 * top-up and a permission alike (stooges pass 2) — led by who says they asked.
 */
function requestTitle(r: Req, d: Data): string {
  const { who, what } = requestParts(r, d)
  return `${who} asked: ${what}`
}

/** Who asked (self-reported, with the key it holds) and what it asks. */
function requestParts(r: Req, d: Data): { who: string; what: string } {
  const a = r.asked
  const k = r.requesterKey ? d.keys.find((x) => x.key === r.requesterKey) : undefined
  const name = k ? keyName(k, d) : (r.requesterKey ?? '')
  // The key is named once: as the asker when it gave no name, else in what it asks.
  const self = !a.from || a.from === name
  const who = a.from || (r.requesterKey ? `key ${name}` : 'an unnamed assistant')
  return { who, what: requestWhat(r, self ? null : `key ${name}`, k) }
}

/** What a request asks; `key` names the key, or null when the asker already did ("it"). */
function requestWhat(r: Req, key: string | null, k?: Key): string {
  const a = r.asked
  if (!r.requesterKey) return `new key “${a.label}”`
  const it = key ?? 'it'
  const its = key ? `${key}'s` : 'its'
  const kinds: string[] = []
  for (const v of a.valueAdds ?? []) kinds.push(`${key ? `top up ${key}` : 'top it up'} with ${usd(v.usd)}${v.ttl ? ` for ${ttlWords(v.ttl)}` : ''}`)
  if (a.askedBudgets) {
    // Now → asked, so a cap reads as a cap (stooges pass 3: "$2.00 a day" alone
    // could not say whether it tightened or loosened anything).
    kinds.push(`change ${its} limit from ${k ? budgetsText(k.budgets) : '?'} to ${budgetsText(a.budgets ?? [])}`)
  }
  if (a.askedModels) kinds.push(`change what ${it} may use from ${k ? modelsText(k.models) : '?'} to ${modelsText(a.models)}`)
  if (a.askedMint) kinds.push(a.mint ? `let ${it} create sub-keys` : `stop ${it} creating sub-keys`)
  if (a.ttl || a.expires) kinds.push(`change when ${it} expires`)
  return kinds.length ? kinds.join(' · ') : `change ${it}`
}

/**
 * What stays if the owner denies, from what the request is about: "If you
 * deny, sk-a… keeps $3.00 a day on everything and $5.00 of top-up."
 */
function keepsLine(r: Req, k: Key | undefined, d: Data): string {
  if (!k) return 'If you deny, no key is made.'
  const a = r.asked
  const parts: string[] = []
  if (a.askedBudgets || a.valueAdds?.length) parts.push(budgetsText(k.budgets))
  if (a.valueAdds?.length) {
    const left = k.valueAdds.filter((v) => v.active).reduce((n, v) => n + Math.max(v.usd - (v.spent ?? 0), 0), 0)
    parts.push(left > 0 ? `${usd(left)} of top-up` : 'no top-up')
  }
  if (a.askedModels) parts.push(`access to ${modelsText(k.models)}`)
  if (a.askedMint) parts.push(k.mint ? 'the right to create sub-keys' : 'no right to create sub-keys')
  if (a.ttl || a.expires) parts.push(k.expires ? `its expiry on ${when(k.expires)}` : 'no expiry')
  const list = parts.length > 1 ? `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}` : (parts[0] ?? 'what it has')
  return `If you deny, ${keyName(k, d)} keeps ${list}.`
}

/** "2d" → "2 days", "48h" → "48 hours". */
function ttlWords(t: string): string {
  return t.replace(/^(\d+)d$/, (_, n) => `${n} day${n === '1' ? '' : 's'}`).replace(/^(\d+)h$/, (_, n) => `${n} hour${n === '1' ? '' : 's'}`)
}

/**
 * The most a key could still spend between now and `until`, from its hard
 * dollar limits and top-ups: the remaining day (or month, or one-off), each
 * whole day after, and every top-up that is still live. Budgets nest, so only
 * those on everything ("*") bound the total; with none, the narrower ones are
 * summed. `about` marks a rolling window, which this can only bound loosely.
 */
function worstCase(k: Key, newTopUps: { usd: number }[], untilMs: number): { usd: number; about: boolean; parts: string[] } {
  const parts: string[] = []
  const hard = flatLimits(k.budgets).filter((x) => x.l.hard && (x.l.usd ?? 0) > 0)
  const wide = hard.filter((x) => x.on === '*')
  const use = wide.length ? wide : hard
  let total = 0
  let about = false
  const now = Date.now()
  for (const { on, l } of use) {
    const w = k.windows.find((x) => x.scope === on && x.label === `usd/${l.per}`)
    const left = Math.max((l.usd ?? 0) - (w?.used ?? 0), 0)
    const sfx = on === '*' ? '' : ` on ${selWords(on)}`
    if (l.per === 'calendar-day') {
      const midnights = Math.max(0, Math.floor(untilMs / DAY) - Math.floor(now / DAY))
      total += left + midnights * (l.usd ?? 0)
      parts.push(`${usd(left)} left today${sfx}`)
      if (midnights) parts.push(`${usd(midnights * (l.usd ?? 0))} for ${midnights} more day${midnights === 1 ? '' : 's'}${sfx}`)
    } else if (l.per === 'total' || l.per === 'calendar-month') {
      total += left
      parts.push(`${usd(left)} left ${l.per === 'total' ? 'of its total' : 'this month'}${sfx}`)
    } else {
      about = true
      total += left
      parts.push(`about ${usd(left)} ${perShort(l.per)}${sfx}`)
    }
  }
  for (const v of k.valueAdds.filter((x) => x.active)) {
    total += Math.max(v.usd - (v.spent ?? 0), 0)
    parts.push(`${usd(Math.max(v.usd - (v.spent ?? 0), 0))} top-up it already has`)
  }
  for (const v of newTopUps) {
    total += v.usd
    parts.push(`${usd(v.usd)} new top-up`)
  }
  return { usd: total, about, parts }
}

// An empty select shows its label as a placeholder unless told to keep it up
// and render the empty choice (stooges pass: "Model" read as unset).
const selectSlots = { select: { displayEmpty: true }, inputLabel: { shrink: true } }
// Helper text on the field's own left edge, not indented 14 px past it.
const helperFlush = { formHelperText: { sx: { mx: 0 } } }

// Text buttons by role (UX review round 3). Neutral — Edit, More, Show, Copy,
// Remove — is quiet, so it does not compete with the numbers; dismiss (Cancel,
// in a dialog) is plain text at full strength.
const quiet = { minWidth: 0, fontWeight: 400, color: 'text.secondary' } as const
const quieter = { ...quiet, px: 0.5, py: 0, fontSize: 12, lineHeight: 1.5 } as const
const dismiss = { color: 'text.primary', fontWeight: 600 } as const

// The width kept at the end of every form row in a dialog, for the control
// that removes that row, so the fields' right edges line up whether a row has
// one or not.
const END_W = 128

/** One form row of a dialog: fields that share the width, then the end slot. */
function Row({ children, end, mt = 1.5 }: { children: ReactNode; end?: ReactNode; mt?: number }) {
  return (
    <Stack direction="row" spacing={1} sx={{ alignItems: 'flex-start', mt }}>
      <Stack direction="row" spacing={1} sx={{ flex: 1, minWidth: 0, alignItems: 'flex-start' }}>
        {children}
      </Stack>
      <Box sx={{ width: END_W, flexShrink: 0, textAlign: 'right', pt: 0.5 }}>{end}</Box>
    </Stack>
  )
}

/** The control at the end of a row that removes it. */
function RemoveButton({ label, onClick }: { label: string; onClick: () => void }) {
  return (
    <Button size="small" sx={{ ...quiet, px: 0.5 }} onClick={onClick}>
      {label}
    </Button>
  )
}

/** One warning line: the icon alone in amber, the sentence as the server wrote it. */
function WarningLine({ text }: { text: string }) {
  return (
    <Stack direction="row" spacing={0.5} sx={{ alignItems: 'flex-start', mt: 0.25 }}>
      <WarningAmberOutlined sx={{ fontSize: 14, color: C.warn, mt: '2px' }} />
      <Typography variant="caption" color="text.secondary">
        {text}
      </Typography>
    </Stack>
  )
}

function useData() {
  return useQuery({ queryKey: ['requests'], queryFn: () => gqlClient.request(RequestsDoc), refetchInterval: 10_000 })
}

function Requests() {
  const ro = useReadOnly()
  const q = useData()
  const d = q.data?.corrallm?.listRequests
  const [search, setSearch] = useState('')
  if (ro) return <Alert severity="info">API Keys is for admins: the page shows keys.</Alert>
  if (q.error) return <Alert severity="error">{extractMessage(q.error)}</Alert>
  if (!d) return <Loading />
  const query = search.trim()
  const needle = query.toLowerCase()
  // The chip counts everything waiting; the search only narrows the lists.
  const waiting = d.requests.filter((r) => r.status === 'pending').length
  const pending = d.requests.filter((r) => r.status === 'pending' && (!needle || requestMatches(r, needle)))
  const decided = d.requests.filter((r) => r.status !== 'pending' && (!needle || requestMatches(r, needle)))
  return (
    <Box sx={{ p: 2, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <PageHeader title="API Keys">
        {waiting > 0 && <Chip size="small" color="info" label={`${waiting} waiting`} sx={{ fontSize: 12 }} />}
        <TextField
          size="small"
          placeholder="Search keys and requests"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          sx={{ ml: 'auto', width: 280 }}
          slotProps={{
            input: {
              startAdornment: (
                <InputAdornment position="start">
                  <SearchIcon fontSize="small" />
                </InputAdornment>
              ),
            },
            htmlInput: { 'aria-label': 'Search keys and requests' },
          }}
        />
      </PageHeader>
      <Panel title="Waiting for you" subtitle={needle ? requestsMatch(pending.length) : undefined} flush>
        {pending.length === 0 ? (
          <Box sx={{ px: 2, py: 1.5 }}>
            <Typography variant="body2" color="text.secondary">
              {needle ? 'Nothing waiting matches the search.' : 'Nothing is waiting.'}
            </Typography>
          </Box>
        ) : (
          pending.map((r) => <PendingRow key={r.id} r={r} d={d} />)
        )}
      </Panel>
      {decided.length > 0 && <DecidedPanel rows={decided} d={d} subtitle={needle ? requestsMatch(decided.length) : undefined} />}
      <KeysPanel d={d} needle={needle} query={query} pendingKeys={pending.map((r) => r.requesterKey ?? '').filter(Boolean)} />
    </Box>
  )
}

const requestsMatch = (n: number) => `${n} request${n === 1 ? ' matches' : 's match'}`

function PendingRow({ r, d }: { r: Req; d: Data }) {
  const [open, setOpen] = useState(false)
  const [denyOpen, setDenyOpen] = useState(false)
  const a = r.asked
  const { who, what } = requestParts(r, d)
  const existingKey = r.requesterKey ? d.keys.find((k) => k.key === r.requesterKey) : undefined
  const line = decision(a.budgets ?? [], a.valueAdds ?? [], a.ttl, !r.requesterKey, existingKey)
  const odd = r.via !== 'default (direct from LAN)'
  return (
    <Box sx={{ px: 2, py: 1.5, borderBottom: `1px solid ${C.border}`, '&:last-of-type': { borderBottom: 0 } }}>
      <Box sx={{ display: 'flex', gap: 2, alignItems: 'flex-start', flexWrap: 'wrap' }}>
        <Box sx={{ flex: 1, minWidth: 260 }}>
          <Typography variant="body2" sx={{ fontWeight: 600 }}>
            {who} asks: {what}
            {!r.requesterKey && a.models?.length ? ` for ${a.models.map(selWords).join(', ')}` : ''}
          </Typography>
          <Typography variant="body1" sx={{ fontWeight: 600, color: line.open ? C.warn : undefined }}>
            {line.head}
          </Typography>
          <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5, whiteSpace: 'pre-wrap', maxWidth: '72ch' }}>
            “{a.purpose}”
          </Typography>
          <Typography variant="caption" color="text.secondary">
            {fmtAgo(r.created)}
            {r.filedBy ? ` · filed by ${r.filedBy}` : ''}
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
      {open && <ApproveDialog r={r} d={d} title={`Approve: ${what}?`} who={who} onClose={() => setOpen(false)} />}
      {denyOpen && <DenyDialog r={r} d={d} title={`Deny: ${what}?`} who={who} onClose={() => setDenyOpen(false)} />}
    </Box>
  )
}

function DenyDialog({ r, d, title, who, onClose }: { r: Req; d: Data; title: string; who: string; onClose: () => void }) {
  const existing = r.requesterKey ? d.keys.find((k) => k.key === r.requesterKey) : undefined
  const line = decision(r.asked.budgets ?? [], r.asked.valueAdds ?? [], r.asked.ttl, !r.requesterKey, existing)
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
      <DialogTitle>
        {title}
        <Typography variant="body2" color="text.secondary">
          asked by {who}
        </Typography>
        <Typography variant="body2">{keepsLine(r, existing, d)}</Typography>
      </DialogTitle>
      <DialogContent>
        <Stack spacing={2.5} sx={{ mt: 0.5 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <Typography variant="body2" color="text.secondary" sx={{ whiteSpace: 'pre-wrap', maxWidth: '72ch' }}>
            “{r.asked.purpose}”
          </Typography>
          {existing && <KeyNow k={existing} d={d} />}
          <TextField
            label="Reason (optional)"
            helperText="The assistant is told this the next time it checks on its request."
            fullWidth
            size="small"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            slotProps={{ ...helperFlush, inputLabel: { shrink: true } }}
          />
        </Stack>
      </DialogContent>
      <DecideFooter line={line} />
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button sx={dismiss} onClick={onClose}>
          Cancel
        </Button>
        <Button variant="contained" onClick={() => deny.mutate()} disabled={deny.isPending}>
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
function ApproveDialog({ r, d, title, who, onClose }: { r: Req; d: Data; title: string; who: string; onClose: () => void }) {
  const qc = useQueryClient()
  const existing = r.requesterKey ? d.keys.find((k) => k.key === r.requesterKey) : undefined
  const a = r.asked
  const [f, setF] = useState<Form>(formFromRequest(r))
  const [more, setMore] = useState(false)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const set = (p: Partial<Form>) => setF((cur) => ({ ...cur, ...p }))
  const g = formToGrant(f)
  const newTopUps = (g.valueAdds ?? []).map((v) => ({ usd: v.usd, ttl: v.ttl }))
  // The worst case over the life of the longest new top-up: what the owner is
  // actually agreeing to (stooges pass 2: "At most a $10 top-up" was no total).
  const summary = decision(g.budgets, newTopUps, g.ttl, !r.requesterKey, existing)
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
  const showBudgets = !r.requesterKey || !!a.askedBudgets
  const showModels = !!r.requesterKey && !!a.askedModels
  const showTopUps = (a.valueAdds?.length ?? 0) > 0 || f.topUps.length > 0
  const showExpiry = !r.requesterKey || !!a.ttl || !!a.expires
  const addTopUp = (
    <Button size="small" startIcon={<AddIcon />} sx={{ px: 0 }} onClick={() => set({ topUps: [newTopUp()] })}>
      Add a top-up
    </Button>
  )
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>
        {title}
        <Typography variant="body2" color="text.secondary">
          asked by {who}
        </Typography>
      </DialogTitle>
      <DialogContent>
        <Stack spacing={2.5} sx={{ mt: 0.5 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <Typography variant="body2" color="text.secondary" sx={{ whiteSpace: 'pre-wrap', maxWidth: '72ch' }}>
            “{a.purpose}”
          </Typography>
          {existing && <KeyNow k={existing} d={d} />}
          {showModels && <ModelsEditor f={f} set={set} d={d} />}
          {showBudgets && <BudgetsEditor f={f} set={set} d={d} extra={!showTopUps && addTopUp} />}
          {showTopUps && <TopUpsEditor f={f} set={set} d={d} />}
          {!showTopUps && !showBudgets && <Box>{addTopUp}</Box>}
          {showExpiry && (
            <Box>
              <Typography variant="subtitle2">Expires</Typography>
              <Row>
                <TextField
                  label="Key expires after"
                  placeholder="7d, 48h — empty: never"
                  size="small"
                  fullWidth
                  value={f.ttl}
                  onChange={(e) => set({ ttl: e.target.value })}
                  helperText={f.ttl ? (endsAt(f.ttl) ? `On ${endsAt(f.ttl)}, if you approve now` : 'Write it as 7d or 48h') : 'Never'}
                  error={!!f.ttl && !endsAt(f.ttl)}
                  slotProps={{ ...helperFlush, inputLabel: { shrink: true } }}
                />
              </Row>
            </Box>
          )}
          {existing && a.askedMint && (
            <FormControlLabel control={<Switch checked={f.mint} onChange={(e) => set({ mint: e.target.checked })} />} label="May create its own sub-keys, with budgets you cannot see here" />
          )}
          {!existing && (
            <Box>
              <Button size="small" sx={{ ...quiet, px: 0, fontSize: 14 }} endIcon={more ? <ExpandLess /> : <ExpandMore />} onClick={() => setMore(!more)}>
                More settings: name, priority, which models, sub-keys
              </Button>
              {more && <MoreSettings f={f} set={set} d={d} />}
            </Box>
          )}
          <Row>
            <TextField
              label="Note to the assistant (optional)"
              size="small"
              fullWidth
              value={f.reason}
              onChange={(e) => set({ reason: e.target.value })}
              slotProps={{ inputLabel: { shrink: true } }}
            />
          </Row>
        </Stack>
      </DialogContent>
      <DecideFooter line={summary}>
        {/* What a new key may use, said once: here, not also as a line above the form. */}
        {!existing && <Typography variant="body2">May use {modelsText(g.models)}</Typography>}
        <Typography variant="caption" color="text.secondary">
          {existing ? 'Takes effect at once.' : 'The assistant gets the key the next time it checks.'}
        </Typography>
      </DecideFooter>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button sx={dismiss} onClick={onClose}>
          Cancel
        </Button>
        <Button variant="contained" onClick={() => void submit()} disabled={busy || (!!f.ttl && !endsAt(f.ttl))}>
          Approve
        </Button>
      </DialogActions>
    </Dialog>
  )
}

/**
 * The foot of a decide dialog, above its buttons: the one line the decision
 * rests on, and how it was worked out. Approve and Deny share it.
 */
function DecideFooter({ line, children }: { line: Decision; children?: ReactNode }) {
  return (
    <Box sx={{ px: 3, pt: 1.5, borderTop: `1px solid ${C.border}` }}>
      <Typography variant="body1" sx={{ fontWeight: 600, color: line.open ? C.warn : undefined }}>
        {line.head}
      </Typography>
      {line.working && (
        <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
          = {line.working}
        </Typography>
      )}
      {children}
    </Box>
  )
}

/**
 * A key as it stands, for a request that would change it: the two facts a
 * top-up or a cap is decided on, one short line each.
 */
function KeyNow({ k, d }: { k: Key; d: Data }) {
  const top = k.valueAdds.filter((v) => v.active)
  return (
    <Box sx={{ borderTop: `1px solid ${C.border}`, borderBottom: `1px solid ${C.border}`, py: 1.25 }}>
      <Typography variant="caption" color="text.secondary">
        {keyName(k, d)} now
      </Typography>
      <Typography variant="body2">Used: {spentLines(k).main}</Typography>
      {top.map((v) => (
        <Typography key={v.id} variant="body2">
          Top-up left: {usd(Math.max(v.usd - (v.spent ?? 0), 0))}
          {v.expires ? ` until ${when(v.expires)}` : ''}
        </Typography>
      ))}
    </Box>
  )
}

/**
 * A selector field. The choices show in words ("Every model", "Local models",
 * a model's name) and the value kept is the selector. A person may also type
 * one: a model's name, or a glob such as *deepseek*.
 *
 * The field keeps its own text: MUI would write the chosen option's words
 * back into the box on every value change, which would rewrite "local" to
 * "Local models" in the middle of typing it.
 */
function SelectorField({
  value,
  onChange,
  options,
  label,
  helper,
}: {
  value: string
  onChange: (v: string) => void
  options: string[]
  label: string
  helper: string
}) {
  const [text, setText] = useState(value ? selOption(value) : '')
  // Typed words that name a choice are that choice; anything else is a selector as typed.
  const resolve = (t: string) => options.find((o) => o === t.trim() || selOption(o).toLowerCase() === t.trim().toLowerCase()) ?? t.trim()
  // A value changed from outside (a limit above this one removed) replaces the text.
  const [seen, setSeen] = useState(value)
  if (value !== seen) {
    setSeen(value)
    if (resolve(text) !== value) setText(value ? selOption(value) : '')
  }
  return (
    <Autocomplete
      freeSolo
      size="small"
      options={options}
      getOptionLabel={selOption}
      value={value || null}
      onChange={(_, v) => onChange(v == null ? '' : resolve(v))}
      inputValue={text}
      onInputChange={(_, v, reason) => {
        if (reason === 'reset') return
        setText(v)
        if (reason === 'input') onChange(resolve(v))
      }}
      renderInput={(p) => (
        <TextField
          {...p}
          label={label}
          placeholder="Pick one, or type a model name"
          helperText={helper || undefined}
          slotProps={{ ...p.slotProps, formHelperText: { sx: { mx: 0 } } }}
        />
      )}
      sx={{ flex: 1, minWidth: 0 }}
    />
  )
}

const newTopUp = (): TopUp => ({ usd: '', model: '', ttl: '2d', note: '' })

/**
 * What a key may spend, as LIMITS by what they apply to (the wire calls one a
 * budget). Each limit has one or more periods: "$20 a day" and "$100 a month"
 * on the same models. Limits nest: every one that matches a request is
 * charged, so "$20 a day on everything" and "$3 a day on *deepseek*" are two
 * limits, and a deepseek request counts against both.
 *
 * One word, "limit", for the thing a person sets (UX review round 2: "budget",
 * "limit" and "add a limit on these" read as three things). `warnings` are the
 * server's sentences about sub-keys promised more than the key has: said here,
 * next to the limits they are about, and nowhere in the key list.
 */
function BudgetsEditor({
  f,
  set,
  d,
  extra,
  warnings = [],
}: {
  f: Form
  set: (p: Partial<Form>) => void
  d: Data
  extra?: ReactNode
  warnings?: string[]
}) {
  const setB = (i: number, p: Partial<Budget>) => set({ budgets: f.budgets.map((x, j) => (j === i ? { ...x, ...p } : x)) })
  const blank: Lim = { amount: '', unit: 'usd', per: 'calendar-day', hard: true }
  // A limit on everything already covers a key that may use one model or every
  // model; a limit for other models is only worth offering past that.
  const covered = f.budgets.some((b) => normSel(b.on) === '*') && f.models.length <= 1
  return (
    <Box>
      <Typography variant="subtitle2">Spending limits</Typography>
      {f.budgets.length === 0 && (
        <Typography variant="body2" sx={{ color: C.warn }}>
          None: it may spend without limit.
        </Typography>
      )}
      {f.budgets.length > 1 && (
        <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
          Every limit that matches a request counts: a limit on one model sits inside a limit on everything.
        </Typography>
      )}
      {warnings.map((w) => (
        <WarningLine key={w} text={w} />
      ))}
      {f.budgets.map((b, i) => (
        <Box key={i} sx={{ mt: 1.5, ...(i > 0 ? { pt: 0.5, borderTop: `1px solid ${C.border}` } : {}) }}>
          <Row end={<RemoveButton label="Remove this limit" onClick={() => set({ budgets: f.budgets.filter((_, j) => j !== i) })} />}>
            <SelectorField
              label="Applies to"
              value={b.on}
              onChange={(on) => setB(i, { on })}
              options={selectorOptions(d)}
              helper={b.on.trim() === '' ? 'Which models this counts' : selHint(b.on)}
            />
          </Row>
          <LimitRows limits={b.limits} setLimits={(limits) => setB(i, { limits })} blank={blank} />
        </Box>
      ))}
      <Stack direction="row" spacing={2} sx={{ mt: 1 }}>
        {!covered && (
          <Button
            size="small"
            startIcon={<AddIcon />}
            sx={{ px: 0 }}
            onClick={() => set({ budgets: [...f.budgets, { on: f.budgets.some((b) => b.on === '*') ? '' : '*', limits: [blank] }] })}
          >
            {f.budgets.length ? 'Limit for other models' : 'Add a limit'}
          </Button>
        )}
        {extra}
      </Stack>
    </Box>
  )
}

/**
 * The periods of one limit: how much (one field, its unit picked at its end),
 * over what, and whether reaching it refuses or only warns. Every control
 * shows on every row; hiding two of them behind a button left people unable
 * to find them (UX review round 2).
 */
function LimitRows({ limits, setLimits, blank }: { limits: Lim[]; setLimits: (l: Lim[]) => void; blank: Lim }) {
  const setLim = (i: number, p: Partial<Lim>) => setLimits(limits.map((l, j) => (j === i ? { ...l, ...p } : l)))
  return (
    <Box>
      {limits.map((l, i) => (
        <Box key={i}>
          <Row end={limits.length > 1 && <RemoveButton label="Remove period" onClick={() => setLimits(limits.filter((_, j) => j !== i))} />}>
            <TextField
              label={l.unit === 'usd' ? 'Amount ($)' : 'Amount (requests)'}
              size="small"
              type="number"
              value={l.amount}
              onChange={(e) => setLim(i, { amount: e.target.value })}
              sx={{ flex: 1, minWidth: 0 }}
              slotProps={{
                inputLabel: { shrink: true },
                input: {
                  endAdornment: (
                    <InputAdornment position="end">
                      <Select
                        variant="standard"
                        disableUnderline
                        value={l.unit}
                        onChange={(e) => setLim(i, { unit: e.target.value as Lim['unit'] })}
                        inputProps={{ 'aria-label': 'Counts in' }}
                        sx={{ fontSize: 14 }}
                      >
                        <MenuItem value="usd">$</MenuItem>
                        <MenuItem value="req">requests</MenuItem>
                      </Select>
                    </InputAdornment>
                  ),
                },
              }}
            />
            <TextField select size="small" label="Over" value={l.per} onChange={(e) => setLim(i, { per: e.target.value })} sx={{ flex: 1, minWidth: 0 }}>
              {PERS.map((p) => (
                <MenuItem key={p.v} value={p.v}>
                  {p.label}
                </MenuItem>
              ))}
            </TextField>
          </Row>
          <FormControlLabel
            control={<Checkbox size="small" checked={!l.hard} onChange={(e) => setLim(i, { hard: !e.target.checked })} />}
            label={<Typography variant="body2">Only warn, don't block</Typography>}
          />
        </Box>
      ))}
      <Button size="small" startIcon={<AddIcon />} sx={{ px: 0 }} onClick={() => setLimits([...limits, blank])}>
        Another period
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
      <Button size="small" startIcon={<AddIcon />} sx={{ mt: 1, px: 0 }} onClick={() => set({ topUps: [...f.topUps, newTopUp()] })}>
        Add a top-up
      </Button>
    </Box>
  )
}

/**
 * One top-up: amount, what it applies to, when it expires. "Applies to" is a
 * plain select — a top-up names one model this box has, or every model, and
 * nothing may be typed.
 */
function TopUpRow({ v, set, remove, d }: { v: TopUp; set: (p: Partial<TopUp>) => void; remove?: () => void; d: Data }) {
  return (
    <Box>
      <Row end={remove && <RemoveButton label="Remove this top-up" onClick={remove} />}>
        <TextField
          label="Amount ($)"
          size="small"
          type="number"
          value={v.usd}
          onChange={(e) => set({ usd: e.target.value })}
          slotProps={{ inputLabel: { shrink: true } }}
          sx={{ width: 120, flexShrink: 0 }}
        />
        <TextField select size="small" label="Applies to" value={v.model} onChange={(e) => set({ model: e.target.value })} slotProps={selectSlots} sx={{ flex: 1, minWidth: 0 }}>
          <MenuItem value="">Every model</MenuItem>
          {d.models.map((m) => (
            <MenuItem key={m} value={m}>
              {selOption(m)}
            </MenuItem>
          ))}
        </TextField>
      </Row>
      <Row>
        <TextField
          label="Top-up expires after"
          placeholder="2d, 48h"
          size="small"
          fullWidth
          value={v.ttl}
          onChange={(e) => set({ ttl: e.target.value })}
          helperText={v.ttl ? (endsAt(v.ttl) ? `On ${endsAt(v.ttl)}` : 'Write it as 2d or 48h') : 'Until spent'}
          error={!!v.ttl && !endsAt(v.ttl)}
          slotProps={{ ...helperFlush, inputLabel: { shrink: true } }}
        />
      </Row>
    </Box>
  )
}

/**
 * What a key may see and use, as SELECTORS: `local`, `proxy`,
 * `provider:fireworks`, `cred:fireworks/main`, `model:*deepseek*`, or a bare
 * model name. Empty = every model.
 *
 * A list rather than a checklist of what this box happens to have, because
 * the permission outlives it: `provider:fireworks` keeps working when a model
 * is added there, and a selector naming something that has gone is a chip an
 * operator can see and remove instead of a checkbox that vanished.
 */
function ModelsEditor({ f, set, d }: { f: Form; set: (p: Partial<Form>) => void; d: Data }) {
  // Typed words that name a choice ("local models") are that choice.
  const fromWords = (t: string) => selectorOptions(d).find((o) => selOption(o).toLowerCase() === t.trim().toLowerCase()) ?? normSel(t)
  return (
    <Box>
      <Typography variant="subtitle2">Models it may use</Typography>
      <Row mt={1}>
      <Autocomplete
        multiple
        freeSolo
        autoSelect
        size="small"
        options={selectorOptions(d).filter((s) => s !== '*')}
        getOptionLabel={selOption}
        value={f.models}
        onChange={(_, v) => set({ models: [...new Set(v.map(fromWords).filter(Boolean))] })}
        renderInput={(p) => (
          <TextField
            {...p}
            placeholder={f.models.length ? '' : 'Every model — or pick some'}
            helperText={
              f.models.length
                ? 'Only what these name. * is a wildcard in a name; a bare name is one model.'
                : 'Every model.'
            }
            slotProps={{ ...p.slotProps, formHelperText: { sx: { mx: 0 } } }}
          />
        )}
        sx={{ flex: 1, minWidth: 0 }}
      />
      </Row>
    </Box>
  )
}

function MoreSettings({ f, set, d, namePlaceholder }: { f: Form; set: (p: Partial<Form>) => void; d: Data; namePlaceholder?: string }) {
  return (
    <Stack spacing={2} sx={{ mt: 1.5 }}>
      <Row mt={0}>
        <TextField
          label="Name"
          size="small"
          value={f.label}
          placeholder={namePlaceholder}
          onChange={(e) => set({ label: e.target.value })}
          slotProps={namePlaceholder ? { inputLabel: { shrink: true } } : undefined}
          sx={{ flex: 1, minWidth: 0 }}
        />
        <TextField select label="Priority" size="small" value={f.group} onChange={(e) => set({ group: e.target.value })} slotProps={selectSlots} sx={{ width: 180, flexShrink: 0 }}>
          <MenuItem value="">the default</MenuItem>
          {d.groups.map((g) => (
            <MenuItem key={g} value={g}>
              {g}
            </MenuItem>
          ))}
        </TextField>
      </Row>
      <ModelsEditor f={f} set={set} d={d} />
      <FormControlLabel control={<Switch checked={f.mint} onChange={(e) => set({ mint: e.target.checked })} />} label="May create its own sub-keys" />
    </Stack>
  )
}

function KeysPanel({ d, pendingKeys, needle, query }: { d: Data; pendingKeys: string[]; needle: string; query: string }) {
  const [showAll, setShowAll] = useState(false)
  const [create, setCreate] = useState(false)
  // A configured key nobody set anything on here is not something handed out —
  // unless a request is waiting on it, when the owner needs to see it now.
  const managed = (k: Key) =>
    k.kind !== 'configured' || k.mint || k.budgets.length > 0 || k.models.length > 0 || k.valueAdds.length > 0 || pendingKeys.includes(k.key)
  // A search looks at every key, configured ones included: the owner is
  // looking for one by name. A parent whose sub-key matches shows too, dimmed,
  // so the sub-key keeps its place under it.
  const matched = needle ? d.keys.filter((k) => keyMatches(k, needle, d)) : []
  const context = d.keys.filter((p) => !matched.includes(p) && matched.some((k) => k.parent === p.key))
  const shown = needle ? d.keys.filter((k) => matched.includes(k) || context.includes(k)) : d.keys.filter((k) => showAll || managed(k))
  const hidden = d.keys.length - d.keys.filter(managed).length
  // Sub-keys under their parent, the way a person reads ownership.
  const tops = shown.filter((k) => k.kind !== 'child' || !shown.some((p) => p.key === k.parent))
  const ordered = tops.flatMap((t) => [t, ...shown.filter((k) => k.kind === 'child' && k.parent === t.key)])
  return (
    <Panel
      title="Keys"
      subtitle={needle ? `${matched.length} key${matched.length === 1 ? ' matches' : 's match'} “${query}”` : undefined}
      actions={
        <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
          {hidden > 0 && !needle && (
            <FormControlLabel
              control={<Switch size="small" checked={showAll} onChange={(e) => setShowAll(e.target.checked)} />}
              label={<Typography variant="body2" color="text.secondary">{`Also show ${hidden} other configured key${hidden === 1 ? '' : 's'}`}</Typography>}
            />
          )}
          <Button size="small" variant="outlined" startIcon={<AddIcon />} onClick={() => setCreate(true)}>
            New key
          </Button>
        </Stack>
      }
      flush
    >
      <TableContainer>
        {/* Fixed widths: a search that hides rows must not move the columns. */}
        <Table size="small" sx={{ tableLayout: 'fixed', minWidth: 960, '& th': { fontSize: 12 } }}>
          <TableHead>
            <TableRow>
              <TableCell>Key</TableCell>
              <TableCell sx={{ width: 80 }}>On</TableCell>
              <TableCell align="right" sx={{ width: 230 }}>
                Used
              </TableCell>
              <TableCell align="right" sx={{ width: 160 }}>
                Top-ups left
              </TableCell>
              <TableCell sx={{ width: 150 }}>Expires</TableCell>
              <TableCell align="right" sx={{ width: 130 }} />
            </TableRow>
          </TableHead>
          <TableBody>
            {ordered.map((k) => (
              <KeyRow key={k.key} k={k} d={d} nested={k.kind === 'child' && tops.some((t) => t.key === k.parent)} context={context.includes(k)} />
            ))}
            {ordered.length === 0 && (
              <TableRow>
                <TableCell colSpan={6}>
                  <Typography variant="body2" color="text.secondary">
                    {needle ? 'No key matches the search.' : 'No keys.'}
                  </Typography>
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </TableContainer>
      {create && <CreateKeyDialog d={d} onClose={() => setCreate(false)} />}
    </Panel>
  )
}

/**
 * What a key used against its limits, in one form for every row: the main
 * dollar limit ("$X of $Y a day"), or "$X today · no limit"; then every other
 * limit, requests last, one per line in the same form. "on <model>" shows only
 * where the row does not already say it — not for a budget on everything, not
 * once per limit when every budget is on the same thing, and not when the "may
 * use" line names it.
 */
function spentLines(k: Key): { main: string; rest: string[]; blocked: boolean } {
  const all = flatLimits(k.budgets)
  const sels = [...new Set(all.map((x) => normSel(x.on)))]
  const only = sels.length === 1 ? sels[0] : null
  const named = (on: string) => k.models.some((m) => normSel(m) === normSel(on))
  const onWords = (on: string) => (normSel(on) === '*' || only !== null || named(on) ? '' : `on ${selWords(on)}`)
  const fmtL = ({ on, l }: { on: string; l: LimitIn }) => {
    const isUsd = (l.usd ?? 0) > 0
    const w = k.windows.find((x) => x.scope === on && x.label === `${isUsd ? 'usd' : 'req'}/${l.per}`)
    const used = isUsd ? usd(w?.used ?? 0) : String(Math.round(w?.used ?? 0))
    const of = isUsd ? usd(l.usd ?? 0) : `${l.req ?? 0} requests`
    return {
      text: `${used} of ${of} ${perShort(l.per)}${l.hard ? '' : ' (warning only)'}`,
      blocked: !!w?.blocked,
    }
  }
  const line = (x: { on: string; l: LimitIn }) => [fmtL(x).text, onWords(x.on)].filter(Boolean).join(' ')
  // Every budget on one thing the row does not name yet: say it once.
  const once = only !== null && only !== '*' && !named(only) ? `on ${selWords(only)}` : ''
  const blocked = all.some((x) => fmtL(x).blocked)
  const dollars = all.filter((x) => (x.l.usd ?? 0) > 0)
  // The main line: a dollar limit on everything, else any dollar limit.
  const main = dollars.find((x) => x.on === '*') ?? dollars[0]
  const others = [...dollars.filter((x) => x !== main), ...all.filter((x) => !((x.l.usd ?? 0) > 0))].map(line)
  if (!main) return { main: `${usd(k.spentToday)} today · no ${all.length ? 'dollar ' : ''}limit`, rest: [...others, once].filter(Boolean), blocked }
  return { main: fmtL(main).text, rest: [onWords(main.on) || once, ...others].filter(Boolean), blocked }
}

/** Who said, in a request, that they hold this key or were given it. */
function askers(key: string, d: Data): string[] {
  return [...new Set(d.requests.flatMap((r) => ((r.requesterKey === key || r.issuedKey === key) && r.asked.from ? [r.asked.from] : [])))]
}

function KeyRow({ k, d, nested, context }: { k: Key; d: Data; nested: boolean; context: boolean }) {
  const qc = useQueryClient()
  const [edit, setEdit] = useState(false)
  const [addTopUp, setAddTopUp] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [menu, setMenu] = useState<HTMLElement | null>(null)
  const [err, setErr] = useState('')
  const [reveal, setReveal] = useState(false)
  const [copied, setCopied] = useState(false)
  const copy = () =>
    void navigator.clipboard.writeText(k.key).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
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
  const configured = k.kind === 'configured'
  const parent = k.parent ? d.keys.find((p) => p.key === k.parent) : undefined
  const who = configured
    ? 'set up in the config'
    : k.kind === 'child' && !nested
      ? `sub-key of ${parent ? keyName(parent, d) : maskKey(k.parent ?? '')}`
      : k.kind === 'child'
        ? 'sub-key'
        : ''
  const sp = spentLines(k)
  // A top-up is spent only once a hard limit refuses; with none it would never be used.
  const stops = k.budgets.some((b) => (b.limits ?? []).some((l) => l.hard))
  const asked = askers(k.key, d)
  // A name, when it has one: its label, or the assistant that asked about it.
  // A name taken from the asker is not said again as "asked by".
  const name = k.label || askedName(k.key, d)
  // Off for two different reasons — an admin switched it off, or its expiry
  // passed — and the switch is the same either way: one control to turn a key
  // back on, whichever stopped it.
  const off = k.disabled || k.expired
  const stoppedBy = k.disabled ? 'switched off' : k.expired ? 'expired' : ''
  // One caption of facts, so no line holds a single word ("sub-key").
  const facts = [
    who,
    stoppedBy,
    k.mint ? 'may create sub-keys' : '',
    k.label && asked.length ? `asked by ${asked.join(', ')}` : '',
    k.models.length ? `may use ${modelsText(k.models)}` : '',
  ].filter(Boolean)
  // Show and Copy come BEFORE the key, so they stay put when it is revealed:
  // the whole key then shows, wrapping onto further lines (owner 10-28).
  // Copy gives the whole key without revealing it.
  return (
    <TableRow sx={{ opacity: off ? 0.55 : context ? 0.7 : 1, verticalAlign: 'top' }}>
      <TableCell sx={{ pl: nested ? 4 : undefined }}>
        {name && (
          <Typography variant="body2" sx={{ fontWeight: 600 }}>
            {name}
          </Typography>
        )}
        <Stack direction="row" spacing={0.5} sx={{ alignItems: 'flex-start' }}>
          <Button size="small" sx={{ ...quieter, width: 40, flexShrink: 0, pl: 0, justifyContent: 'flex-start' }} onClick={() => setReveal(!reveal)} aria-label={reveal ? `Hide ${keyName(k, d)}` : `Show ${keyName(k, d)}`}>
            {reveal ? 'Hide' : 'Show'}
          </Button>
          <Button size="small" sx={{ ...quieter, width: 52, flexShrink: 0 }} onClick={copy} aria-label={`Copy ${keyName(k, d)}`}>
            {copied ? 'Copied' : 'Copy'}
          </Button>
          <Typography
            variant={name ? 'caption' : 'body2'}
            sx={{
              fontFamily: 'monospace',
              wordBreak: 'break-all',
              minWidth: 0,
              fontWeight: name ? undefined : 600,
              color: reveal || !name ? 'text.primary' : 'text.secondary',
            }}
          >
            {reveal ? k.key : maskKey(k.key)}
          </Typography>
        </Stack>
        {context && (
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
            shown because its sub-keys match
          </Typography>
        )}
        {facts.length > 0 && (
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
            {facts.join(' · ')}
          </Typography>
        )}
        {err && (
          <Typography variant="caption" color="error" sx={{ display: 'block' }}>
            {err}
          </Typography>
        )}
      </TableCell>
      <TableCell>
        {/* The switch's own inset pulled back, so its track starts under the header's first letter. */}
        <Switch
          size="small"
          checked={!off}
          onChange={(e) => act.mutate(e.target.checked ? 'enable' : 'disable')}
          slotProps={{ input: { 'aria-label': off ? `Switch ${keyName(k, d)} on` : `Switch ${keyName(k, d)} off` } }}
          sx={{ ml: '-4px' }}
        />
      </TableCell>
      <TableCell align="right" sx={{ fontVariantNumeric: 'tabular-nums' }}>
        <Typography variant="body2" sx={{ color: sp.blocked ? C.error : undefined, whiteSpace: 'nowrap' }}>
          {sp.main}
          {sp.blocked ? ' — reached' : ''}
        </Typography>
        {sp.rest.map((t) => (
          <Typography key={t} variant="caption" color="text.secondary" sx={{ display: 'block' }}>
            {t}
          </Typography>
        ))}
      </TableCell>
      <TableCell align="right" sx={{ fontVariantNumeric: 'tabular-nums' }}>
        {k.valueAdds.length === 0 && (
          <Typography variant="body2" color="text.secondary">
            —
          </Typography>
        )}
        {k.valueAdds.map((v) => (
          <Box key={v.id} sx={{ opacity: v.active ? 1 : 0.55 }}>
            <Typography variant="body2">
              {usd(Math.max(v.usd - (v.spent ?? 0), 0))} left
              {!v.active ? ' (over)' : ''}
            </Typography>
            <Typography variant="caption" color="text.secondary" sx={{ display: 'block', whiteSpace: 'nowrap' }}>
              {[v.model ? `on ${selWords(v.model)}` : '', v.expires ? `until ${when(v.expires)}` : ''].filter(Boolean).join(' · ')}
            </Typography>
          </Box>
        ))}
      </TableCell>
      <TableCell sx={{ whiteSpace: 'nowrap' }}>
        <Typography variant="body2" sx={{ color: k.expired ? C.error : undefined }} color={!configured && k.expires ? undefined : 'text.secondary'}>
          {!configured && k.expires ? `${k.expired ? 'stopped ' : ''}${when(k.expires)}` : 'never'}
        </Typography>
      </TableCell>
      <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
        <Button size="small" sx={quiet} onClick={() => setEdit(true)}>
          Edit
        </Button>
        <Button size="small" sx={quiet} endIcon={<ExpandMore />} onClick={(e) => setMenu(e.currentTarget)}>
          More
        </Button>
        <Menu anchorEl={menu} open={!!menu} onClose={() => setMenu(null)}>
          {stops && <MenuItem onClick={() => (setMenu(null), setAddTopUp(true))}>Top up…</MenuItem>}
          {k.valueAdds
            .filter((v) => v.active)
            .map((v) => (
              <MenuItem key={v.id} onClick={() => (setMenu(null), delTopUp.mutate(v.id ?? ''))}>
                Take back the {usd(Math.max(v.usd - (v.spent ?? 0), 0))} top-up
              </MenuItem>
            ))}
          {configured ? (
            <MenuItem disabled>Delete: edit the config (set up there)</MenuItem>
          ) : (
            <MenuItem sx={{ color: C.error }} onClick={() => (setMenu(null), setConfirmDelete(true))}>
              Delete…
            </MenuItem>
          )}
        </Menu>
      </TableCell>
      {edit && <EditKeyDialog k={k} d={d} onClose={() => setEdit(false)} onDone={refresh} />}
      {addTopUp && <TopUpDialog k={k} d={d} onClose={() => setAddTopUp(false)} onDone={refresh} />}
      <Dialog open={confirmDelete} onClose={() => setConfirmDelete(false)} fullWidth maxWidth="sm">
        <DialogTitle>Delete {keyName(k, d)}?</DialogTitle>
        <DialogContent>
          <Typography variant="body2" sx={{ maxWidth: '72ch' }}>
            The key stops working at once, and so do any sub-keys it made. Its top-ups are removed, and it cannot be brought back.
            {off ? '' : ' Switching it off stops it the same way but keeps it, and it can be switched on again.'}
          </Typography>
        </DialogContent>
        <DialogActions sx={{ px: 3, pb: 2 }}>
          <Button sx={dismiss} onClick={() => setConfirmDelete(false)}>
            Cancel
          </Button>
          <Button color="error" onClick={() => act.mutate('delete')} disabled={act.isPending}>
            Delete for good
          </Button>
          {!off && (
            <Button variant="contained" onClick={() => act.mutate('disable')} disabled={act.isPending}>
              Switch off instead
            </Button>
          )}
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
  // Where sub-keys promise more than this key, or its parent, has: said here,
  // next to the limits, and not in the key list (UX review round 3).
  const parent = k.parent ? d.keys.find((p) => p.key === k.parent) : undefined
  const warnings = [...k.warnings, ...(parent?.warnings ?? [])]
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>Edit {keyName(k, d)}</DialogTitle>
      <DialogContent>
        <Stack spacing={2.5} sx={{ mt: 0.5 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <BudgetsEditor f={f} set={set} d={d} warnings={warnings} />
          {k.kind === 'child' && <ModelsEditor f={f} set={set} d={d} />}
          {k.kind !== 'configured' && (
            <Row mt={0}>
              <TextField
                label="Key expires after"
                placeholder="7d, 48h — empty: keep as is"
                size="small"
                fullWidth
                value={f.ttl}
                onChange={(e) => set({ ttl: e.target.value })}
                helperText={f.ttl ? (endsAt(f.ttl) ? `On ${endsAt(f.ttl)}` : 'Write it as 7d or 48h') : k.expires ? `Now: ${when(k.expires)}` : 'Now: never'}
                error={!!f.ttl && !endsAt(f.ttl)}
                slotProps={{ ...helperFlush, inputLabel: { shrink: true } }}
              />
            </Row>
          )}
          {k.kind !== 'child' ? (
            <MoreSettings f={f} set={set} d={d} namePlaceholder={askedName(k.key, d) || maskKey(k.key)} />
          ) : (
            <Row mt={0}>
              <TextField
                label="Name"
                size="small"
                fullWidth
                value={f.label}
                placeholder={askedName(k.key, d) || maskKey(k.key)}
                onChange={(e) => set({ label: e.target.value })}
                slotProps={{ inputLabel: { shrink: true } }}
              />
            </Row>
          )}
        </Stack>
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button sx={dismiss} onClick={onClose}>
          Cancel
        </Button>
        <Button variant="contained" onClick={() => save.mutate()} disabled={save.isPending || (!!f.ttl && !endsAt(f.ttl))}>
          Save
        </Button>
      </DialogActions>
    </Dialog>
  )
}

/**
 * Making a key, on the owner's own initiative rather than from a request.
 *
 * The form is the same one Approve uses — label, priority, the models it may
 * use, its spending limits, an expiry — because a key made here and a key
 * granted there are the same object; what differs is only which page you were
 * on when you decided. The one deliberate difference is that the key is created
 * SWITCHED OFF: a grant is an answer to someone waiting, while a key made here
 * is usually destined for a machine that is not listening yet, and it should
 * not be able to spend anything before its first caller is expected.
 *
 * The key is shown ONCE, and only once it is switched on: it is a bearer
 * credential, and there is nowhere else to read it back.
 */
function CreateKeyDialog({ d, onClose }: { d: Data; onClose: () => void }) {
  const qc = useQueryClient()
  const [f, setF] = useState<Form>({ label: '', group: '', models: [], budgets: [], topUps: [], ttl: '', mint: false, reason: '' })
  const [err, setErr] = useState('')
  const [made, setMade] = useState('')
  const set = (p: Partial<Form>) => setF((cur) => ({ ...cur, ...p }))
  const create = useMutation({
    mutationFn: () => gqlClient.request(CreateKeyDoc, { body: { key: { ...formToGrant(f), valueAdds: [], purpose: 'made by an admin' } } }),
    onSuccess: (r) => {
      setMade(r.corrallm?.createKey?.key ?? '')
      void qc.invalidateQueries({ queryKey: ['requests'] })
    },
    onError: (e) => setErr(extractMessage(e)),
  })
  const line = costLine(formToGrant(f).budgets, [], f.ttl, true)
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>{made ? 'Key made' : 'New key'}</DialogTitle>
      <DialogContent>
        {made ? (
          <Stack spacing={2} sx={{ mt: 0.5 }}>
            <Alert severity="warning">Copy it now: it is not shown again.</Alert>
            <TextField value={made} size="small" fullWidth slotProps={{ input: { readOnly: true, sx: { fontFamily: 'monospace' } } }} />
            <Typography variant="body2">
              It is <b>switched off</b>. It answers 401 until you switch it on in the Keys table, which is
              also where its spending is reported.
            </Typography>
          </Stack>
        ) : (
          <Stack spacing={2.5} sx={{ mt: 0.5 }}>
            {err && <Alert severity="error">{err}</Alert>}
            <Typography variant="body2" color={line.open ? C.warn : undefined} sx={{ fontWeight: 600 }}>
              {line.text}
            </Typography>
            <MoreSettings f={f} set={set} d={d} />
            <BudgetsEditor f={f} set={set} d={d} />
            <Row mt={0}>
              <TextField
                label="Key expires after"
                placeholder="7d, 48h — empty: never"
                size="small"
                fullWidth
                value={f.ttl}
                onChange={(e) => set({ ttl: e.target.value })}
                helperText={f.ttl ? (endsAt(f.ttl) ? `On ${endsAt(f.ttl)}` : 'Write it as 7d or 48h') : 'Never expires'}
                error={!!f.ttl && !endsAt(f.ttl)}
                slotProps={{ ...helperFlush, inputLabel: { shrink: true } }}
              />
            </Row>
          </Stack>
        )}
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        {made ? (
          <Button variant="contained" onClick={onClose}>
            Done
          </Button>
        ) : (
          <>
            <Button sx={dismiss} onClick={onClose}>
          Cancel
        </Button>
            <Button
              variant="contained"
              onClick={() => create.mutate()}
              disabled={create.isPending || f.label.trim() === '' || (!!f.ttl && !endsAt(f.ttl))}
            >
              Make the key, switched off
            </Button>
          </>
        )}
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
  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>Top up {keyName(k, d)}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ mt: 0.5 }}>
          {err && <Alert severity="error">{err}</Alert>}
          <Typography variant="body2" color="text.secondary">
            Used only after its limits are reached.
          </Typography>
          <TopUpRow v={v} set={(p) => setV({ ...v, ...p })} d={d} />
        </Stack>
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button sx={dismiss} onClick={onClose}>
          Cancel
        </Button>
        <Button variant="contained" onClick={() => add.mutate()} disabled={add.isPending || !(Number(v.usd) > 0) || (!!v.ttl && !endsAt(v.ttl))}>
          Add {Number(v.usd) > 0 ? usd(Number(v.usd)) : ''}
        </Button>
      </DialogActions>
    </Dialog>
  )
}

function DecidedPanel({ rows, d, subtitle }: { rows: Req[]; d: Data; subtitle?: string }) {
  const [open, setOpen] = useState(false)
  return (
    <Panel
      title={`Decided · ${rows.length}`}
      subtitle={subtitle}
      actions={
        <Button size="small" sx={quiet} endIcon={open ? <ExpandLess /> : <ExpandMore />} onClick={() => setOpen(!open)}>
          {open ? 'Hide' : 'Show'}
        </Button>
      }
      flush
    >
      {open && (
      <TableContainer>
        <Table size="small">
          <TableBody>
            {rows.map((r) => {
              const g = r.granted
              // The key's real expiry, not one recomputed from the ttl now
              // (stooges pass 2: one key showed two stop times).
              const issued = r.issuedKey ? d.keys.find((k) => k.key === r.issuedKey) : undefined
              let line = r.status === 'approved' ? costLine(g.budgets ?? [], g.valueAdds ?? [], undefined, false).text : ''
              if (issued) line += issued.expires ? ` · expires ${when(issued.expires)}` : ' · never expires'
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
      )}
    </Panel>
  )
}

export const Route = createFileRoute('/approvals')({ component: Requests })
