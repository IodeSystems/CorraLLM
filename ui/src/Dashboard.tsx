import type { ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Autocomplete,
  Box,
  Chip,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  TextField,
  ToggleButton,
  ToggleButtonGroup,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { Panel } from '@/Panel'
import { MetricChart } from '@/Charts'
import { TimeWindowPicker, windowKey, windowPhrase, windowVars, type TimeWindow } from '@/TimeWindow'
import { fmtInt, fmtKwh, fmtUSD } from '@/format'
import { C, seriesColor } from '@/theme'

/**
 * THE DASHBOARD — what the box did over a span, summed, and split however the
 * question needs (P56).
 *
 * Four sections, one control row. The control row is the point: every number
 * below is a sum or a distribution over the SAME slice — the window, the
 * callers and models kept, and what the rows are split by. "Traffic was a bit
 * convoluted" was eight panels that each picked their own axis; here the
 * reader picks it once.
 *
 *   Metrics      speed, cache, turns and context, per row of the split
 *   Cost         what it would have cost on a paid API, what was paid, what
 *                the electricity cost — the same rows
 *   Utilization  per device: serving time and measured compute
 *   Budgets      every declared budget and allocation, with its spend rate
 *
 * The requests themselves are on /traffic. The machines are on /machines.
 */

export type GroupBy = 'none' | 'key' | 'model' | 'key,model'

export type Split = { keys: string[]; models: string[]; groupBy: GroupBy }

const RollupDoc = graphql(/* GraphQL */ `
  query DashboardRollup(
    $windowHours: Long!
    $from: Long
    $to: Long
    $keys: String
    $models: String
    $groupBy: String
  ) {
    corrallm {
      usageRollup(windowHours: $windowHours, from: $from, to: $to, keys: $keys, models: $models, groupBy: $groupBy) {
        costPerKwh
        baselineType
        rows {
          ...DashboardRow
        }
        total {
          ...DashboardRow
        }
      }
      # Callers for the filter, from the cheap per-key rollup rather than a
      # second pass of the one above.
      usageByKey(windowHours: $windowHours, from: $from, to: $to) {
        rows {
          key
        }
      }
    }
  }
  fragment DashboardRow on corrallm_RollupRow {
    key
    served
    requests
    promptTokens
    cachedTokens
    completionTokens
    cacheHitRate
    cacheReports
    prefillN
    prefillFusedTps
    prefillRawTps
    tgN
    tgAvg
    tgP10
    ctxRequests
    ctxRequestAvg
    ctxRequestP90
    ctxSessions
    ctxTailAvg
    ctxTailP90
    turnsAvg
    turnsP90
    costUsd
    energyUsd
    energyKwh
    paidUsd
    unattributedUsd
    baselineUsd
  }
`)

const UtilDoc = graphql(/* GraphQL */ `
  query DashboardUtilization($windowHours: Long!, $bucketMinutes: Long!, $from: Long, $to: Long) {
    corrallm {
      gpuUtilization(windowHours: $windowHours, bucketMinutes: $bucketMinutes, from: $from, to: $to) {
        bucketMinutes
        buckets
        pools {
          server
          pool
          models
          busyPct
          busy
          computeN
          computePct
          compute {
            n
            mean
            high
            low
          }
        }
      }
    }
  }
`)

const LedgerDoc = graphql(/* GraphQL */ `
  query DashboardLedger {
    corrallm {
      quotaLedger {
        backends {
          backend
          windows {
            label
            limit
            used
            hard
            resetsIn
            ratePerHour
            rateSpanSec
            exhaustsInSec
            projectedAtReset
          }
        }
      }
    }
  }
`)

const spanMs = (w: TimeWindow) => (w.kind === 'absolute' ? w.toMS - w.fromMS : w.minutes * 60_000)

// The bucket follows the span, so a one-hour window is not one bar.
function bucketMinutesFor(w: TimeWindow): string {
  const hours = spanMs(w) / 3_600_000
  return String(hours <= 6 ? 5 : hours <= 24 ? 15 : hours <= 168 ? 60 : 240)
}

const pct = (n: number) => `${Math.round(n * 100)}%`
const tps = (n: number) => (n > 0 ? `${Math.round(n).toLocaleString()} tok/s` : '—')
const toks = (n: number) => (n > 0 ? `${Math.round(n).toLocaleString()} tok` : '—')

function fmtDur(sec: number): string {
  if (sec <= 0) return '—'
  if (sec < 3600) return `${Math.round(sec / 60)} min`
  if (sec < 86400) return `${(sec / 3600).toFixed(1)} h`
  return `${(sec / 86400).toFixed(1)} d`
}

// ── Controls ────────────────────────────────────────────────────────────────

const GROUPS: { v: GroupBy; label: string }[] = [
  { v: 'none', label: 'Total' },
  { v: 'key', label: 'By key' },
  { v: 'model', label: 'By model' },
  { v: 'key,model', label: 'Key × model' },
]

function Controls(props: {
  window: TimeWindow
  split: Split
  keyOptions: string[]
  modelOptions: string[]
  onWindow: (w: TimeWindow) => void
  onSplit: (s: Split) => void
}) {
  const { window, split, keyOptions, modelOptions, onWindow, onSplit } = props
  const label = (k: string) => k || '(unkeyed)'
  return (
    <Box sx={{ display: 'flex', gap: 1.5, alignItems: 'center', flexWrap: 'wrap' }}>
      <TimeWindowPicker value={window} onChange={onWindow} />
      <Autocomplete
        multiple
        size="small"
        options={keyOptions}
        value={split.keys}
        getOptionLabel={label}
        onChange={(_, keys) => onSplit({ ...split, keys })}
        renderInput={(p) => <TextField {...p} label="API keys" placeholder={split.keys.length ? '' : 'all'} />}
        sx={{ minWidth: 220, flex: '1 1 220px', maxWidth: 420 }}
      />
      <Autocomplete
        multiple
        size="small"
        options={modelOptions}
        value={split.models}
        onChange={(_, models) => onSplit({ ...split, models })}
        renderInput={(p) => <TextField {...p} label="Models" placeholder={split.models.length ? '' : 'all'} />}
        sx={{ minWidth: 220, flex: '1 1 220px', maxWidth: 420 }}
      />
      <ToggleButtonGroup
        size="small"
        exclusive
        value={split.groupBy}
        onChange={(_, v: GroupBy | null) => v && onSplit({ ...split, groupBy: v })}
        aria-label="Split rows by"
      >
        {GROUPS.map((g) => (
          <ToggleButton key={g.v} value={g.v} sx={{ textTransform: 'none', px: 1.5 }}>
            {g.label}
          </ToggleButton>
        ))}
      </ToggleButtonGroup>
    </Box>
  )
}

// ── Metrics and cost: one query, two tables over the same rows ──────────────

type Row = {
  key: string
  served: string
  requests: number
  cacheHitRate: number
  cacheReports: number
  prefillN: number
  prefillFusedTps: number
  prefillRawTps: number
  tgN: number
  tgAvg: number
  tgP10: number
  ctxRequests: number
  ctxRequestAvg: number
  ctxRequestP90: number
  ctxSessions: number
  ctxTailAvg: number
  ctxTailP90: number
  turnsAvg: number
  turnsP90: number
  costUsd: number
  energyUsd: number
  energyKwh: number
  paidUsd: number
  unattributedUsd: number
  baselineUsd: number
}

type RawRow = { [K in keyof Row]: Row[K] | string | null | undefined }

function toRow(r: RawRow): Row {
  const n = (v: unknown) => Number(v ?? 0)
  return {
    key: String(r.key ?? ''),
    served: String(r.served ?? ''),
    requests: n(r.requests),
    cacheHitRate: n(r.cacheHitRate),
    cacheReports: n(r.cacheReports),
    prefillN: n(r.prefillN),
    prefillFusedTps: n(r.prefillFusedTps),
    prefillRawTps: n(r.prefillRawTps),
    tgN: n(r.tgN),
    tgAvg: n(r.tgAvg),
    tgP10: n(r.tgP10),
    ctxRequests: n(r.ctxRequests),
    ctxRequestAvg: n(r.ctxRequestAvg),
    ctxRequestP90: n(r.ctxRequestP90),
    ctxSessions: n(r.ctxSessions),
    ctxTailAvg: n(r.ctxTailAvg),
    ctxTailP90: n(r.ctxTailP90),
    turnsAvg: n(r.turnsAvg),
    turnsP90: n(r.turnsP90),
    costUsd: n(r.costUsd),
    energyUsd: n(r.energyUsd),
    energyKwh: n(r.energyKwh),
    paidUsd: n(r.paidUsd),
    unattributedUsd: n(r.unattributedUsd),
    baselineUsd: n(r.baselineUsd),
  }
}

function rowLabel(r: Row, g: GroupBy): string {
  const key = r.key || '(unkeyed)'
  switch (g) {
    case 'key':
      return key
    case 'model':
      return r.served || '(none)'
    case 'key,model':
      return `${key} · ${r.served || '(none)'}`
    default:
      return 'All traffic'
  }
}

/** Head cell with the unit under the name, so no number on the page is bare. */
function Head({ name, unit, hint }: { name: string; unit: string; hint?: string }) {
  const body = (
    <Box component="span" sx={{ display: 'inline-flex', flexDirection: 'column', alignItems: 'flex-end' }}>
      <span>{name}</span>
      <Typography component="span" variant="caption" sx={{ color: C.textFaint, lineHeight: 1.2 }}>
        {unit}
      </Typography>
    </Box>
  )
  return (
    <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
      {hint ? <Tooltip title={hint}>{body}</Tooltip> : body}
    </TableCell>
  )
}

function Num({ children, muted }: { children: ReactNode; muted?: boolean }) {
  return (
    <TableCell align="right" sx={{ whiteSpace: 'nowrap', fontVariantNumeric: 'tabular-nums', color: muted ? C.textFaint : undefined }}>
      {children}
    </TableCell>
  )
}

function avgP(avg: number, p: number, n: number, f: (x: number) => string, tail = 'p90') {
  if (n <= 0) return '—'
  return (
    <>
      {f(avg)}
      <Typography component="span" variant="caption" sx={{ color: C.textMuted, ml: 0.75 }}>
        {tail} {f(p)}
      </Typography>
    </>
  )
}

function MetricsTable({ rows, total, groupBy, onPick }: { rows: Row[]; total?: Row; groupBy: GroupBy; onPick: (r: Row) => void }) {
  const body = (r: Row, isTotal: boolean) => (
    <TableRow
      key={isTotal ? '(total)' : `${r.key}\u0000${r.served}`}
      hover={!isTotal}
      onClick={isTotal || groupBy === 'none' ? undefined : () => onPick(r)}
      sx={{ cursor: isTotal || groupBy === 'none' ? undefined : 'pointer', ...(isTotal ? { '& td': { fontWeight: 600, borderTop: `2px solid ${C.border}` } } : {}) }}
    >
      <TableCell sx={{ maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
        {isTotal ? 'Total' : rowLabel(r, groupBy)}
      </TableCell>
      <Num>{fmtInt(r.requests)}</Num>
      <Num muted={r.cacheReports === 0}>{r.cacheReports > 0 ? pct(r.cacheHitRate) : 'not reported'}</Num>
      <Num>{r.prefillN > 0 ? tps(r.prefillFusedTps) : '—'}</Num>
      <Num>{r.prefillN > 0 ? tps(r.prefillRawTps) : '—'}</Num>
      <Num>{avgP(r.tgAvg, r.tgP10, r.tgN, tps, 'p10')}</Num>
      <Num>{avgP(r.turnsAvg, r.turnsP90, r.ctxSessions, (x) => x.toFixed(1))}</Num>
      <Num>{avgP(r.ctxRequestAvg, r.ctxRequestP90, r.ctxRequests, toks)}</Num>
      <Num>{avgP(r.ctxTailAvg, r.ctxTailP90, r.ctxSessions, toks)}</Num>
    </TableRow>
  )
  return (
    <TableContainer>
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>{GROUPS.find((g) => g.v === groupBy)?.label ?? ''}</TableCell>
            <Head name="Requests" unit="count" />
            <Head name="Cache hit" unit="% of prompt tokens" hint="Prompt tokens served from the KV cache instead of reprocessed, token-weighted. 'not reported' = the backend reports no cache; not a measured 0%." />
            <Head name="Prefill, fused" unit="tok/s" hint="Prompt tokens INCLUDING the cached prefix, per second of prefill: the rate a prompt was effectively read at." />
            <Head name="Prefill, raw" unit="tok/s" hint="New (uncached) prompt tokens per second of prefill: what the GPU actually processed." />
            <Head name="Decode (tg)" unit="tok/s, mean · p10" hint="Generation speed per request. p10 is the slow tail, where a slowdown shows first." />
            <Head name="Turns" unit="per session, mean · p90" hint="Served turns per conversation session." />
            <Head name="Request context" unit="tokens, mean · p90" hint="Context each request carried (prompt + completion at that turn), every turn counted." />
            <Head name="Final context" unit="tokens, mean · p90" hint="Context at each session's LAST turn: how big a conversation gets by the end." />
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((r) => body(r, false))}
          {total && rows.length > 1 && body(total, true)}
        </TableBody>
      </Table>
    </TableContainer>
  )
}

function CostTable({
  rows,
  total,
  groupBy,
  hours,
  baselineType,
  costPerKwh,
  onPick,
}: {
  rows: Row[]
  total?: Row
  groupBy: GroupBy
  hours: number
  baselineType: string
  costPerKwh: number
  onPick: (r: Row) => void
}) {
  const body = (r: Row, isTotal: boolean) => (
    <TableRow
      key={isTotal ? '(total)' : `${r.key}\u0000${r.served}`}
      hover={!isTotal}
      onClick={isTotal || groupBy === 'none' ? undefined : () => onPick(r)}
      sx={{ cursor: isTotal || groupBy === 'none' ? undefined : 'pointer', ...(isTotal ? { '& td': { fontWeight: 600, borderTop: `2px solid ${C.border}` } } : {}) }}
    >
      <TableCell sx={{ maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
        {isTotal ? 'Total' : rowLabel(r, groupBy)}
      </TableCell>
      <Num muted={r.baselineUsd === 0}>{r.baselineUsd > 0 ? fmtUSD(r.baselineUsd) : '—'}</Num>
      <Num muted={r.paidUsd === 0}>{fmtUSD(r.paidUsd)}</Num>
      <Num muted={r.energyUsd === 0}>
        {fmtUSD(r.energyUsd)}
        {r.energyKwh > 0 && (
          <Typography component="span" variant="caption" sx={{ color: C.textMuted, ml: 0.75 }}>
            {fmtKwh(r.energyKwh)}
          </Typography>
        )}
      </Num>
      <Num muted={r.unattributedUsd === 0}>{r.unattributedUsd > 0 ? fmtUSD(r.unattributedUsd) : '—'}</Num>
      <Num>{fmtUSD(r.costUsd)}</Num>
      <Num>{hours > 0 ? `${fmtUSD(r.costUsd / hours)}/h` : '—'}</Num>
      <Num>{hours > 0 && r.baselineUsd > 0 ? `${fmtUSD(r.baselineUsd / hours)}/h` : '—'}</Num>
    </TableRow>
  )
  return (
    <TableContainer>
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>{GROUPS.find((g) => g.v === groupBy)?.label ?? ''}</TableCell>
            <Head
              name="Estimated API"
              unit={baselineType ? `$ at ${baselineType}` : '$ (no paid type configured)'}
              hint="What these tokens would have cost on a paid API: each model's costEquivalent, else the default baseline. Covers token-billed traffic on hardware corrallm runs; paid and audio rows are left out."
            />
            <Head name="Paid" unit="$ billed by providers" />
            <Head name="Electricity" unit={costPerKwh > 0 ? `$ at $${costPerKwh}/kWh` : '$ (no rate set)'} />
            <Head name="Unattributed" unit="$" hint="Metered on this box's energy coefficients for a model corrallm does not run and whose price nothing declares: a config gap, neither electricity nor a bill." />
            <Head name="Actual" unit="$ paid + electricity" />
            <Head name="Spend rate" unit="actual $ per hour" />
            <Head name="API rate" unit="estimated $ per hour" />
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((r) => body(r, false))}
          {total && rows.length > 1 && body(total, true)}
        </TableBody>
      </Table>
    </TableContainer>
  )
}

// ── Utilization ─────────────────────────────────────────────────────────────

function Utilization({ window }: { window: TimeWindow }) {
  const vars = windowVars(window)
  const q = useQuery({
    queryKey: ['dashboardUtil', windowKey(window)],
    queryFn: () =>
      gqlClient.request(UtilDoc, {
        windowHours: vars.windowHours,
        bucketMinutes: bucketMinutesFor(window),
        from: vars.from,
        to: vars.to,
      }),
    refetchInterval: window.kind === 'absolute' ? false : 30000,
  })
  const u = q.data?.corrallm?.gpuUtilization
  const buckets = (u?.buckets ?? []).map(Number)
  const pools = u?.pools ?? []
  const bm = u?.bucketMinutes ?? Number(bucketMinutesFor(window))
  return (
    <Panel
      title="Utilization"
      subtitle={`Per device, ${windowPhrase(window)}`}
      help="Serving: share of time at least one request was being served on the device (from the request log). Compute: the card's own utilization, sampled every 10 s from nvidia-smi — kernels running. Serving above compute is time a slot was held but the card idled (network, between steps); compute above serving is work corrallm did not route (a bench, another process)."
      flush
    >
      {q.isLoading ? (
        <Typography variant="body2" sx={{ color: C.textMuted, p: 1.75 }}>
          Loading…
        </Typography>
      ) : pools.length === 0 ? (
        <Typography variant="body2" sx={{ color: C.textMuted, p: 1.75 }}>
          No device served anything or reported utilization {windowPhrase(window)}.
        </Typography>
      ) : (
        <>
          <TableContainer>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Device</TableCell>
                  <Head name="Serving" unit="% of window" />
                  <Head name="Compute" unit="% mean of samples" />
                  <Head name="Compute peak" unit={`% highest sample`} />
                  <TableCell>Models</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {pools.map((p) => {
                  const n = Number(p.computeN)
                  const peak = Math.max(0, ...p.compute.filter((c) => Number(c.n) > 0).map((c) => c.high))
                  return (
                    <TableRow key={`${p.server}/${p.pool}`}>
                      <TableCell>
                        {p.server} · {p.pool}
                      </TableCell>
                      <Num>{pct(p.busyPct)}</Num>
                      <Num muted={n === 0}>{n > 0 ? `${Math.round(p.computePct)}%` : 'not reported'}</Num>
                      <Num muted={n === 0}>{n > 0 ? `${Math.round(peak)}%` : '—'}</Num>
                      <TableCell sx={{ color: C.textMuted }}>{p.models.join(', ') || '—'}</TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </TableContainer>
          <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap', p: 1.5 }}>
            {pools.map((p) => (
              <MetricChart
                key={`${p.server}/${p.pool}`}
                title={`${p.server} · ${p.pool} (% per ${bm} min)`}
                series={[
                  { key: 'serving', color: seriesColor(0), values: p.busy.map((v) => v * 100) },
                  ...(Number(p.computeN) > 0
                    ? [
                        { key: 'compute mean', color: seriesColor(1), values: p.compute.map((c) => c.mean) },
                        { key: 'compute high', color: seriesColor(2), values: p.compute.map((c) => c.high) },
                      ]
                    : []),
                ]}
                fmt={(v) => `${Math.round(v)}%`}
                buckets={buckets}
              />
            ))}
          </Box>
        </>
      )}
    </Panel>
  )
}

// ── Budgets ─────────────────────────────────────────────────────────────────

function scopeLabel(s: string): ReactNode {
  if (s === '*') return 'whole box'
  if (s.startsWith('key:')) return <>key <b>{s.slice(4) || '(unkeyed)'}</b></>
  if (s.startsWith('prov:')) return <>provider <b>{s.slice(5)}</b></>
  return s
}

function Budgets({ keys, models }: { keys: string[]; models: string[] }) {
  const q = useQuery({
    queryKey: ['dashboardLedger'],
    queryFn: () => gqlClient.request(LedgerDoc),
    refetchInterval: 30000,
  })
  // The filters apply here too, by the only reading a scope has: a key scope
  // is kept when its key is, a model scope when its model is. Credential,
  // provider and box budgets stay — every request counts against them.
  const kept = (s: string) => {
    if (s.startsWith('key:')) return keys.length === 0 || keys.includes(s.slice(4))
    if (s === '*' || s.startsWith('prov:') || s.includes('@')) return true
    return models.length === 0 || models.includes(s)
  }
  const rows = (q.data?.corrallm?.quotaLedger?.backends ?? [])
    .filter((b) => b.windows && b.windows.length > 0 && kept(b.backend))
    .flatMap((b) => (b.windows ?? []).map((w) => ({ scope: b.backend, ...w })))
  const money = (label: string, v: number) => (label.startsWith('usd/') ? fmtUSD(v) : `${fmtInt(Math.round(v))} req`)
  return (
    <Panel
      title="Budgets"
      subtitle="Every declared budget and key allocation"
      help="Budgets declared on a model, credential, provider or the whole box, and allocations on API keys (keys.<key>.limits). Hard: a full window refuses with 429. Soft: reported, and only steers routing. Rate is the last hour's charges (or since corrallm started, when sooner); the projection assumes it holds."
      flush
    >
      {q.isLoading ? (
        <Typography variant="body2" sx={{ color: C.textMuted, p: 1.75 }}>
          Loading…
        </Typography>
      ) : rows.length === 0 ? (
        <Typography variant="body2" sx={{ color: C.textMuted, p: 1.75 }}>
          No budget or allocation is declared{keys.length + models.length > 0 ? ' for this selection' : ''}. Every key is unlimited.
        </Typography>
      ) : (
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Scope</TableCell>
                <TableCell>Window</TableCell>
                <Head name="Used" unit="of limit" />
                <Head name="Rate" unit="per hour" />
                <Head name="Runs out in" unit="at this rate" />
                <Head name="At reset" unit="projected, calendar windows" />
                <Head name="Resets in" unit="time" />
              </TableRow>
            </TableHead>
            <TableBody>
              {rows.map((w) => {
                const full = w.used >= w.limit
                const spanSec = Number(w.rateSpanSec)
                const exhaustsSec = Number(w.exhaustsInSec)
                return (
                  <TableRow key={`${w.scope}/${w.label}`}>
                    <TableCell>{scopeLabel(w.scope)}</TableCell>
                    <TableCell sx={{ whiteSpace: 'nowrap' }}>
                      {w.label}{' '}
                      <Chip size="small" variant="outlined" label={w.hard ? 'hard' : 'soft'} sx={{ ml: 0.5, height: 20 }} />
                    </TableCell>
                    <Num>
                      <Box component="span" sx={{ color: full ? C.error : undefined }}>
                        {money(w.label, w.used)} / {money(w.label, w.limit)}
                      </Box>
                      <Typography component="span" variant="caption" sx={{ color: C.textMuted, ml: 0.75 }}>
                        {pct(w.limit > 0 ? w.used / w.limit : 0)}
                      </Typography>
                    </Num>
                    <Num muted={spanSec === 0}>{spanSec > 0 ? money(w.label, w.ratePerHour) : 'measuring'}</Num>
                    <Num muted={exhaustsSec === 0}>{full ? 'now' : exhaustsSec > 0 ? fmtDur(exhaustsSec) : 'not at this rate'}</Num>
                    <Num muted={w.projectedAtReset === 0}>{w.projectedAtReset > 0 ? money(w.label, w.projectedAtReset) : '—'}</Num>
                    <Num>{w.resetsIn || '—'}</Num>
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

// ── The page body ───────────────────────────────────────────────────────────

export function Dashboard(props: {
  window: TimeWindow
  split: Split
  modelOptions: string[]
  onWindow: (w: TimeWindow) => void
  onSplit: (s: Split) => void
}) {
  const { window, split, modelOptions, onWindow, onSplit } = props
  const vars = windowVars(window)
  const q = useQuery({
    queryKey: ['dashboardRollup', windowKey(window), split.keys.join(','), split.models.join(','), split.groupBy],
    queryFn: () =>
      gqlClient.request(RollupDoc, {
        windowHours: vars.windowHours,
        from: vars.from,
        to: vars.to,
        keys: split.keys.join(','),
        models: split.models.join(','),
        groupBy: split.groupBy,
      }),
    refetchInterval: window.kind === 'absolute' ? false : 30000,
    placeholderData: (prev) => prev,
  })
  const u = q.data?.corrallm?.usageRollup
  const rows = (u?.rows ?? []).map((r) => toRow(r as RawRow))
  const total = u?.total ? toRow(u.total as RawRow) : undefined
  const keyOptions = [...new Set((q.data?.corrallm?.usageByKey?.rows ?? []).map((r) => r.key))].sort()
  const models = [...new Set([...modelOptions, ...rows.map((r) => r.served).filter(Boolean)])].sort()
  const hours = spanMs(window) / 3_600_000

  // Clicking a row narrows the page to it: its key, its model, or both.
  const pick = (r: Row) =>
    onSplit({
      ...split,
      keys: split.groupBy.includes('key') ? [r.key] : split.keys,
      models: split.groupBy.includes('model') ? [r.served] : split.models,
    })

  const empty = (
    <Typography variant="body2" sx={{ color: C.textMuted, p: 1.75 }}>
      {q.isLoading ? 'Loading…' : q.error ? `Could not load: ${String(q.error)}` : `No traffic ${windowPhrase(window)} for this selection.`}
    </Typography>
  )

  return (
    <>
      <Controls
        window={window}
        split={split}
        keyOptions={keyOptions}
        modelOptions={models}
        onWindow={onWindow}
        onSplit={onSplit}
      />
      <Panel title="Metrics" subtitle={`Summed ${windowPhrase(window)}; click a row to filter to it`} flush>
        {rows.length === 0 ? empty : <MetricsTable rows={rows} total={total} groupBy={split.groupBy} onPick={pick} />}
      </Panel>
      <Panel title="Cost" subtitle={`Same rows, ${windowPhrase(window)}`} flush>
        {rows.length === 0 ? (
          empty
        ) : (
          <CostTable
            rows={rows}
            total={total}
            groupBy={split.groupBy}
            hours={hours}
            baselineType={u?.baselineType ?? ''}
            costPerKwh={u?.costPerKwh ?? 0}
            onPick={pick}
          />
        )}
      </Panel>
      <Utilization window={window} />
      <Budgets keys={split.keys} models={split.models} />
    </>
  )
}
