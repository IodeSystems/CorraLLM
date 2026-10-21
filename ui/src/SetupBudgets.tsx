import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  AlertTitle,
  Box,
  Button,
  Chip,
  LinearProgress,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material'
import { Panel } from '@/Panel'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtInt, fmtMoney, fmtTime } from '@/format'
import { Loading } from '@/Loading'
import {
  PauseDialog,
  PauseCostNote,
  PauseModelDoc,
  UnpauseModelDoc,
  pauseCost,
  type LaneShape,
  type PauseCost,
} from '@/Pause'
import { DisableDialog, disableCost, useSetDisabled } from '@/Disable'
import { useReadOnly } from '@/auth'

// One counted window, e.g. "$0.72 of $5 per day (UTC)" or "3 of 30 requests per minute".
function windowText(label: string, used: number, limit: number): string {
  const [dim, per] = label.split('/')
  const period =
    per === 'calendar-day' ? 'day (UTC)' : per === 'calendar-month' ? 'calendar month (UTC)' : `${per} (rolling)`
  if (dim === 'usd') return `$${used.toFixed(2)} of $${limit} per ${period}`
  if (dim === 'req') return `${Math.round(used)} of ${limit} requests per ${period}`
  return `${used.toFixed(1)} of ${limit} ${dim} per ${period}`
}

/**
 * Free-tier quota ledger (P16): each remote backend's remaining rate-limit
 * budget, learned live from the provider's X-Ratelimit-* headers. The selector
 * routes around a backend that shows unavailable here before it 429s.
 */
const QuotaDoc = graphql(/* GraphQL */ `
  query Quota {
    corrallm {
      quotaLedger {
        refusing {
          backend
          status
          count
          sinceMs
          lastMs
        }
        backends {
          backend
          available
          stale
          coolingInSec
          observedAgoSec
          seen
          windows {
            label
            limit
            used
            resetsIn
          }
          reported {
            name
            value
          }
          requests {
            limit
            remaining
            cap
            effRemaining
            resetsIn
            stale
          }
          tokens {
            limit
            remaining
            cap
            effRemaining
            resetsIn
            stale
          }
        }
      }
    }
  }
`)

type Bucket = {
  limit: string | number
  remaining: string | number
  cap?: string | number | null
  effRemaining?: string | number | null
  resetsIn?: string | null
  stale?: boolean | null
}

const n = (v: unknown): number => Number(v ?? 0) || 0

/**
 * What each provider's own bill says next to what corrallm metered. Metering is
 * an estimate; the bill is the truth, and the spend windows above are raised to
 * it. Read from the last hourly fetch, so it is never a live number.
 */
const BillingDoc = graphql(/* GraphQL */ `
  query ProviderBilling {
    corrallm {
      providerBilling {
        providers {
          provider
          credential
          fetchedMs
          lastOkMs
          error
          settling
          attributable
          billedMonthUsd
          recordedMonthUsd
          billedTodayUsd
          recordedTodayUsd
          models {
            upstream
            served
            billedMonthUsd
            recordedMonthUsd
            billedTodayUsd
            recordedTodayUsd
          }
          days {
            day
            billedUsd
            recordedUsd
          }
        }
      }
    }
  }
`)

/** Billed minus recorded, signed, in dollars. */
function gapText(billed: number, recorded: number): string {
  const d = billed - recorded
  if (Math.abs(d) < 0.005) return '$0.00'
  return `${d > 0 ? '+' : '-'}${fmtMoney(Math.abs(d))}`
}

const strong = { fontWeight: 600 } as const

function BilledVsRecorded() {
  const q = useQuery({
    queryKey: ['providerBilling'],
    queryFn: () => gqlClient.request(BillingDoc),
    refetchInterval: 60000,
  })
  const providers = q.data?.corrallm?.providerBilling?.providers ?? []
  if (providers.length === 0) return null
  return (
    <>
      {providers.map((p) => (
        <Panel
          key={`${p.provider}/${p.credential}`}
          title={`Billed by ${p.provider} vs recorded`}
          subtitle={`This UTC month, credential ${p.credential}`}
          note={`Last read from the provider ${fmtTime(p.lastOkMs)}. Metering is an estimate; spend caps are raised to the bill.`}
          flush
        >
          {p.error && (
            <Alert severity="warning" sx={{ m: 1 }}>
              The last attempt to read the bill failed ({fmtTime(p.fetchedMs)}): {p.error}
            </Alert>
          )}
          {p.settling && (
            <Typography variant="caption" sx={{ display: 'block', px: 2, pt: 1, color: 'text.secondary' }}>
              The provider is still settling its latest hours, so the newest billed dollars can rise.
            </Typography>
          )}
          {!p.attributable && (
            <Typography variant="caption" sx={{ display: 'block', px: 2, pt: 1, color: 'text.secondary' }}>
              This provider has several credentials. Recorded dollars cannot be split by account, so they show $0.
            </Typography>
          )}
          <TableContainer>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Model</TableCell>
                  <TableCell align="right">Billed, month (USD)</TableCell>
                  <TableCell align="right">Recorded, month (USD)</TableCell>
                  <TableCell align="right">Billed minus recorded, month (USD)</TableCell>
                  <TableCell align="right">Billed, today (USD)</TableCell>
                  <TableCell align="right">Recorded, today (USD)</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {p.models.map((m) => (
                  <TableRow key={m.upstream} hover>
                    <TableCell>
                      <Tooltip title={m.served ? `Served as ${m.served}` : 'No configured model maps to this id'}>
                        <span style={{ fontFamily: 'monospace' }}>
                          {m.upstream.replace(/^accounts\/[^/]+\/models\//, '')}
                        </span>
                      </Tooltip>
                    </TableCell>
                    <TableCell align="right">{fmtMoney(m.billedMonthUsd)}</TableCell>
                    <TableCell align="right">{fmtMoney(m.recordedMonthUsd)}</TableCell>
                    <TableCell align="right">{gapText(m.billedMonthUsd, m.recordedMonthUsd)}</TableCell>
                    <TableCell align="right">{fmtMoney(m.billedTodayUsd)}</TableCell>
                    <TableCell align="right">{fmtMoney(m.recordedTodayUsd)}</TableCell>
                  </TableRow>
                ))}
                <TableRow>
                  <TableCell sx={strong}>All models</TableCell>
                  <TableCell align="right" sx={strong}>{fmtMoney(p.billedMonthUsd)}</TableCell>
                  <TableCell align="right" sx={strong}>{fmtMoney(p.recordedMonthUsd)}</TableCell>
                  <TableCell align="right" sx={strong}>{gapText(p.billedMonthUsd, p.recordedMonthUsd)}</TableCell>
                  <TableCell align="right" sx={strong}>{fmtMoney(p.billedTodayUsd)}</TableCell>
                  <TableCell align="right" sx={strong}>{fmtMoney(p.recordedTodayUsd)}</TableCell>
                </TableRow>
              </TableBody>
            </Table>
          </TableContainer>
          {p.days.length > 0 && (
            <TableContainer>
              <Table size="small">
                <TableHead>
                  <TableRow>
                    <TableCell>Day (UTC)</TableCell>
                    <TableCell align="right">Billed (USD)</TableCell>
                    <TableCell align="right">Recorded (USD)</TableCell>
                    <TableCell align="right">Billed minus recorded (USD)</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {p.days.map((d) => (
                    <TableRow key={d.day} hover>
                      <TableCell>{d.day}</TableCell>
                      <TableCell align="right">{fmtMoney(d.billedUsd)}</TableCell>
                      <TableCell align="right">{fmtMoney(d.recordedUsd)}</TableCell>
                      <TableCell align="right">{gapText(d.billedUsd, d.recordedUsd)}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          )}
        </Panel>
      ))}
    </>
  )
}

/**
 * One ledger, two readers at different speeds. The budgets tab watches it tick;
 * the page banner only needs to know whether something is broken, and polling a
 * whole ledger every four seconds to keep an alert warm is not worth it.
 */
function useLedger(refetchInterval: number) {
  return useQuery({
    queryKey: ['quota'],
    queryFn: () => gqlClient.request(QuotaDoc),
    refetchInterval,
  })
}

/** A budget bar: effective (capped) remaining over the ceiling that governs it. */
function BucketCell({ b }: { b: Bucket }) {
  const capped = n(b.cap) > 0 && n(b.cap) < n(b.limit)
  // The number availability is judged on: effective remaining when capped, else
  // the provider's own remaining.
  const rem = capped ? n(b.effRemaining) : n(b.remaining)
  const ceil = capped ? n(b.cap) : n(b.limit)
  const pct = ceil > 0 ? Math.max(0, Math.min(100, (rem / ceil) * 100)) : 0
  const color = pct > 40 ? 'success' : pct > 10 ? 'warning' : 'error'
  return (
    <Box sx={{ minWidth: 160 }}>
      <Box sx={{ display: 'flex', justifyContent: 'space-between', mb: 0.3 }}>
        <Typography variant="caption">
          {fmtInt(rem)} / {fmtInt(ceil)}
          {capped && (
            <Tooltip
              title={`Self-capped at ${fmtInt(n(b.cap))} of the provider's ${fmtInt(n(b.limit))}`}
            >
              <Chip size="small" label="cap" sx={{ ml: 0.5, height: 16 }} />
            </Tooltip>
          )}
        </Typography>
        <Typography
          variant="caption"
          sx={{
            color: 'text.secondary',
          }}
        >
          {b.stale ? 'window rolled' : b.resetsIn ? `resets ${b.resetsIn}` : ''}
        </Typography>
      </Box>
      <LinearProgress variant="determinate" value={pct} color={color} />
    </Box>
  )
}

/**
 * What a hard refusal MEANS, in the operator's words rather than the status
 * code's. 401/402/403 are three different problems with three different fixes,
 * and "refused with 402" names the wire, not the situation (OB-3, OB-6).
 */
function refusalMeaning(status: number): string {
  if (status === 402) return 'the account is unpaid'
  if (status === 401) return 'the key is not accepted'
  if (status === 403) return 'the key is not permitted to use it'
  return `it answers ${status}`
}

/** The half of the fix that happens at the provider, not here. */
function refusalFix(status: number): string {
  if (status === 402) return 'Fund the account'
  if (status === 401) return 'Replace the credential'
  if (status === 403) return 'Check what the credential is allowed to reach'
  return 'Check the provider'
}

/**
 * What to do about it — a fault the reader cannot act on is not a fault (OB-6).
 *
 * The second half used to read "or take this backend out of its fallback list
 * on the Models tab": an instruction to go and edit configuration by hand, in
 * another room, which is what the owner was looking at when he asked "possibly
 * disable backend as an option?". It stood for two days.
 *
 * It is now a button, so the sentence hands off to it instead of describing an
 * errand. The old wording survives for the one case the button cannot cover —
 * a refusing backend that is not a model configured here, which nothing on this
 * box can pause. And once it IS paused, the second half is already done: the
 * sentence stops offering it and the row underneath says so.
 *
 * TWO WAYS TO STOP CALLING IT, so the sentence now says there are two rather
 * than leading into a single button. "For now, or for good" is the whole fork
 * in five words, and the reader meets it before either control — which is the
 * only way a person picks correctly without being told to go and read about it.
 */
function refusalAction(status: number, offer: 'pause' | 'paused' | 'none'): string {
  if (offer === 'pause') return `${refusalFix(status)}. Or stop calling it — for now, or for good:`
  if (offer === 'paused') return `${refusalFix(status)}.`
  return `${refusalFix(status)}, or take this backend out of its fallback list on the Models tab.`
}

/**
 * How long it has been broken, which is the number that decides whether this is
 * news or a standing fact. A date alone makes the reader do the subtraction.
 */
function fmtSince(msStr: string | number): string {
  const ms = typeof msStr === 'string' ? Number(msStr) : msStr
  if (!Number.isFinite(ms) || ms <= 0) return 'an unknown time'
  const secs = Math.max(0, (Date.now() - ms) / 1000)
  const when = new Date(ms).toLocaleString()
  const ago = (v: number, unit: string) => `${v} ${unit}${v === 1 ? '' : 's'} ago (${when})`
  if (secs < 3600) return ago(Math.max(1, Math.floor(secs / 60)), 'minute')
  if (secs < 86400) return ago(Math.floor(secs / 3600), 'hour')
  return ago(Math.floor(secs / 86400), 'day')
}

/**
 * What the banner needs to know beyond "it is refusing": whether the thing
 * refusing is a model this box can pause, what else goes with it, and which
 * fallback lists would lose a rung.
 *
 * A separate query rather than fields on the ledger, and `enabled` on there
 * being something wrong, because the banner renders on every Setup tab: the
 * normal case is nothing refusing, and the normal case should cost nothing.
 */
const RefusingContextDoc = graphql(/* GraphQL */ `
  query RefusingContext {
    corrallm {
      overview {
        models {
          name
          # THE PROCESS, which is what a pause is actually keyed on. An
          # extension hosts several models in one process, so a pause aimed at
          # any of them takes all of them; every other model is its own process
          # and takes only itself. Asking procKey gets both cases right without
          # the UI having to know which is which.
          procKey
          paused
          pausedAtMs
        }
        lanes {
          name
          members {
            model
          }
          ladder {
            # A rung the setup has taken out is still LISTED here so the
            # decision stays visible, and is never walked. pauseCost drops it
            # rather than counting it as cover this list does not have.
            model
            disabled
          }
        }
      }
    }
  }
`)

/** What one press of Pause is aimed at, assembled before the press (OB-1). */
type PauseTarget = {
  name: string
  warning?: string
  reason: string
  resumeHelp: string
  cost: PauseCost
}

/**
 * A backend that refuses EVERY request is a fault, not a reading, so it is not
 * allowed to live inside a tab. Setup renders this above the tab bar, where it
 * is visible whichever tab is open — the alert was previously buried partway
 * down a scroll, and putting a scroll behind a tab would have buried it twice.
 *
 * IT NOW CARRIES THE ACTION. It described one instead: "take this backend out
 * of its fallback list on the Models tab" — a config edit, in another room, to
 * be done by hand, offered to somebody who had just been told the box was
 * broken. On this box it said that for two days about a Groq key answering 403.
 * A banner that can only issue errands is the dead end OB-8 forbids, and the
 * owner read it exactly that way: "possibly disable backend as an option?"
 *
 * Pause is that option and already existed. It is the right one rather than the
 * config edit because it is reversible, takes effect at once, leaves the
 * configuration alone, and shows up as paused on Now and Models — where an edit
 * deletes the rung, records nothing about why, and has to be undone from memory.
 *
 * AND NOW THE OTHER OPTION, WHICH IS THE ONE THE OWNER ACTUALLY ASKED FOR.
 * "Possibly disable backend" was answered with a pause because a pause was what
 * existed, and it is the wrong answer for this row: a pause nags by design so
 * you come back to it, and nobody is coming back to a Groq key that has
 * answered 403 since September. What was missing is the statement "this is not
 * part of my setup", which belongs in the config document and is silent.
 *
 * THE TWO CONTROLS ARE SIDE BY SIDE ON PURPOSE, AND THIS IS THE RISK IN IT. Two
 * buttons that both mean "off", distinguished by a sentence nobody reads, would
 * be worse than one button. Three things keep them apart, in the order a person
 * meets them: the fork is named before either of them ("for now, or for good"),
 * the labels do not share a verb ("Pause it" against "Take it out of the
 * setup"), and each caption opens with what KIND of thing it is rather than
 * with what it does. The dialogs then differ in shape — one asks when and why,
 * the other asks nothing — so the difference survives into the second screen.
 *
 * WHY THE CONTROL LIVES HERE AND NOT ON /models. This is the one place in the
 * product that already knows a backend is finished with: the fault, how long it
 * has stood, and how many times. A reader deciding whether something is over is
 * standing here, holding the evidence, and moving the control to the page that
 * lists models would put the decision three clicks from its reason. /models
 * carries the WAY BACK instead — which has to be permanent, where this banner
 * disappears the moment the decision is made.
 */
export function RefusingBanner() {
  const readOnly = useReadOnly()
  const qc = useQueryClient()
  const q = useLedger(20000)
  const refusing = q.data?.corrallm?.quotaLedger?.refusing ?? []

  const ctx = useQuery({
    queryKey: ['refusingContext'],
    queryFn: () => gqlClient.request(RefusingContextDoc),
    enabled: refusing.length > 0,
    refetchInterval: 20000,
  })

  const [target, setTarget] = useState<PauseTarget | null>(null)
  // What one press of "Take it out of the setup" is aimed at, counted before
  // the press. Separate state from `target` so the two dialogs cannot both be
  // open, and so neither one inherits the other's prefill.
  const [outTarget, setOutTarget] = useState<{ name: string; cost: PauseCost } | null>(null)
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)

  // Both keys: the ledger decides whether this banner renders at all, the
  // context decides whether it offers Pause or Resume.
  //
  // `providers` joins them for the config write: Setup's own model lists read
  // from that key, and a model that just left the setup must not still be drawn
  // as part of it one tab over.
  const done = (text?: string | null, ok?: boolean | null) => {
    setMsg({ ok: !!ok, text: text ?? '' })
    void qc.invalidateQueries({ queryKey: ['quota'] })
    void qc.invalidateQueries({ queryKey: ['refusingContext'] })
    void qc.invalidateQueries({ queryKey: ['overview'] })
    void qc.invalidateQueries({ queryKey: ['providers'] })
  }
  const fail = (e: unknown) => setMsg({ ok: false, text: String(e) })

  const pause = useMutation({
    mutationFn: (v: { model: string; resumeAt: string | null; reason: string }) =>
      gqlClient.request(PauseModelDoc, v),
    onSuccess: (d) => done(d.corrallm.pauseModel?.message, d.corrallm.pauseModel?.ok),
    onError: fail,
  })
  const unpause = useMutation({
    mutationFn: (model: string) => gqlClient.request(UnpauseModelDoc, { model }),
    onSuccess: (d) => done(d.corrallm.unpauseModel?.message, d.corrallm.unpauseModel?.ok),
    onError: fail,
  })
  const setDisabled = useSetDisabled({
    onDone: (text, ok) => done(text, ok),
    onFail: (text) => setMsg({ ok: false, text }),
  })
  const busy = pause.isPending || unpause.isPending || setDisabled.isPending

  // THE CONFIRMATION HAS TO OUTLIVE THE ROWS IT WAS RAISED ON.
  //
  // A backend that leaves the setup also leaves the refusing ledger — that is
  // the feature — so the last refusing row disappears on success and this whole
  // banner unmounts with it. The reader pressed a control and watched the panel
  // it was in vanish, with nothing on screen saying what had happened or where
  // the model went. For a change whose entire risk is "silent becomes lost",
  // that is the worst possible acknowledgement.
  //
  // So an empty ledger with a message still renders the message. An empty
  // ledger with nothing to say renders nothing, as before.
  if (refusing.length === 0) {
    if (!msg) return null
    return (
      <Alert severity={msg.ok ? 'success' : 'error'} onClose={() => setMsg(null)}>
        {msg.text}
      </Alert>
    )
  }

  const ov = ctx.data?.corrallm?.overview
  const models = ov?.models ?? []
  const lanes: LaneShape[] = (ov?.lanes ?? []).map((l) => ({
    name: l.name,
    ladder: l.ladder,
    members: l.members,
  }))
  // Nothing already out of service counts as somewhere to fall through to —
  // including the other backends on this very list.
  const alreadyOut = new Set<string>([
    ...models.filter((m) => m.paused).map((m) => m.name),
    ...refusing.map((r) => r.backend),
  ])

  // A BACKEND YOU PAUSED IS NOT A FAULT, IT IS A DECISION YOU MADE.
  //
  // The ledger keeps its row after a pause on purpose — ClearBackendRefusal
  // only fires on a SUCCESSFUL response, and a paused backend is never called,
  // so nothing can ever clear it. That is right for the record and wrong for
  // this banner, which was rendering severity="error" and the present tense
  // "is refusing every request" over a backend the reader had just switched
  // off, with its own "Paused" row directly underneath. The owner's words:
  // "it still shows errors, and it shows that it is paused - kinda pointless".
  //
  // He is right, and the rule is OB-6's: a fault the reader cannot act on is
  // not a fault. There is nothing left to do about a backend you have already
  // stopped calling, so it stops being red and starts being past tense.
  const live = refusing.filter((r) => !models.find((m) => m.name === r.backend)?.paused)
  const allQuiet = live.length === 0

  return (
    <Alert severity={allQuiet ? 'info' : 'error'}>
      <AlertTitle>
        {allQuiet
          ? refusing.length === 1
            ? 'A backend is paused after refusing requests'
            : `${refusing.length} backends are paused after refusing requests`
          : live.length === 1
            ? 'A backend is refusing every request'
            : `${live.length} backends are refusing every request`}
      </AlertTitle>
      {msg && (
        <Alert severity={msg.ok ? 'success' : 'error'} sx={{ mb: 1 }} onClose={() => setMsg(null)}>
          {msg.text}
        </Alert>
      )}
      {refusing.map((r) => {
        const status = n(r.status)
        const model = models.find((m) => m.name === r.backend)
        // Everything on the same process, which is exactly what the server's
        // pause takes out. Matching on the integration that lists a model would
        // have been wrong here: `free` lists groq-gpt-oss-120b and is a remote
        // integration, not a process, so a pause aimed at that model takes
        // nothing else with it.
        const siblings = model
          ? models.filter((m) => m.procKey === model.procKey).map((m) => m.name)
          : [r.backend]
        const host = siblings.length > 1 ? model?.procKey.replace(/^extension:/, '') : undefined

        return (
          <Box key={r.backend} sx={{ mb: 1.5 }}>
            <Typography variant="body2">
              <b style={{ fontFamily: 'monospace' }}>{r.backend}</b> —{' '}
              {model?.paused ? 'it refused' : refusalMeaning(status)}
              {model?.paused ? ' until you paused it' : ''} since {fmtSince(r.sinceMs)},{' '}
              {fmtInt(r.count)} {n(r.count) === 1 ? 'time' : 'times'}
              {/* The last refusal, not the first, is what says whether this is
                  news. One 403 two days ago and nothing since reads very
                  differently from one a minute ago, and the row was showing
                  only `since`, which made a single old event look ongoing. */}
              {model?.paused && n(r.lastMs) > 0 ? `, last ${fmtSince(r.lastMs)}` : ''}.{' '}
              {refusalAction(status, !model ? 'none' : model.paused ? 'paused' : 'pause')}
            </Typography>

            {model?.paused ? (
              <Box sx={{ mt: 0.5, display: 'flex', flexDirection: 'column', gap: 0.75 }}>
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                  <Button
                    size="small"
                    variant="outlined"
                    color="success"
                    disabled={busy || readOnly}
                    onClick={() => unpause.mutate(r.backend)}
                    sx={{ minWidth: 190, justifyContent: 'center' }}
                  >
                    Resume it
                  </Button>
                  <Typography
                    variant="caption"
                    sx={{ color: 'text.secondary', flex: 1, minWidth: 280 }}
                  >
                    {/* A paused backend cannot be reached through this box, so a
                        refusal dated after the pause is not a caller — it is
                        something else holding the same key, and saying so is the
                        difference between a stale count and a live one. */}
                    {n(r.lastMs) > n(model.pausedAtMs)
                      ? `Paused ${fmtSince(model.pausedAtMs)}, and it refused again after that — a pause stops requests through this box, not anything else using the same key.`
                      : `Paused ${fmtSince(model.pausedAtMs)}. This box is not calling it; the refusals counted above are from before that.`}
                  </Typography>
                </Box>
                {/* THE SECOND OFFER IS BIGGEST HERE, not on the un-paused row.
                    A pause that has stood for days over a credential nobody is
                    going to fix is exactly the state this feature exists to end,
                    and this row is where a person is looking at one. Offering
                    only Resume left them with a control that puts a dead
                    backend back and nothing that agrees with them. */}
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                  <Button
                    size="small"
                    variant="outlined"
                    disabled={busy || ctx.isLoading || readOnly}
                    onClick={() =>
                      setOutTarget({ name: r.backend, cost: disableCost(r.backend, lanes, alreadyOut) })
                    }
                    sx={{ minWidth: 190, justifyContent: 'center' }}
                  >
                    Take it out of the setup
                  </Button>
                  <Typography
                    variant="caption"
                    sx={{ color: 'text.secondary', flex: 1, minWidth: 280 }}
                  >
                    If it is not coming back: writes the decision into your configuration and stops
                    reporting it, here and on Now. The pause is kept underneath — put the model back
                    and you get it back exactly as it is now, still paused.
                  </Typography>
                </Box>
              </Box>
            ) : model ? (
              <Box sx={{ mt: 0.5, display: 'flex', flexDirection: 'column', gap: 0.75 }}>
                {/* TWO CONTROLS, AND THE CAPTIONS OPEN WITH WHAT KIND OF THING
                    EACH ONE IS. "Temporary and reversible" against "A decision
                    about what your setup contains" is the whole choice in the
                    first four words of each, which is as far as a person reads
                    while deciding which button to press. What each one DOES to
                    callers comes second, because it is the answer to a question
                    they have not asked yet. */}
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                  <Button
                    size="small"
                    variant="outlined"
                    color="warning"
                    disabled={busy || ctx.isLoading || readOnly}
                    onClick={() =>
                      setTarget({
                        name: r.backend,
                        warning: host
                          ? `${r.backend} shares the ${host} process with its siblings, so pausing it takes all of them out of service: ${siblings.join(', ')}.`
                          : undefined,
                        // The fault, already known, so nobody has to retype it —
                        // and so the reason shows up on Now as the sentence that
                        // explains a paused model somebody else may not have
                        // paused.
                        reason: `refusing every request — ${status} since ${new Date(n(r.sinceMs)).toLocaleString()}`,
                        // LEFT INDEFINITE ON PURPOSE. A 401/402/403 does not heal
                        // on a clock; it lifts when a person changes something at
                        // the provider. A resume time would put the same dead
                        // credential back in the rotation at an hour chosen for no
                        // reason, and refill this very banner.
                        resumeHelp: `Empty on purpose: a ${status} clears when somebody fixes the account or the key, not on a clock. It stays listed as paused on Now until it is resumed.`,
                        cost: pauseCost(siblings, lanes, alreadyOut),
                      })
                    }
                    sx={{ minWidth: 190, justifyContent: 'center' }}
                  >
                    Pause it
                  </Button>
                  <Typography
                    variant="caption"
                    sx={{ color: 'text.secondary', flex: 1, minWidth: 280 }}
                  >
                    Temporary and reversible. Stops this box calling it — callers are told the model
                    is paused instead of getting its {status} — and it stays listed on Now until
                    somebody resumes it, so it is not forgotten.
                  </Typography>
                </Box>
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
                  <Button
                    size="small"
                    variant="outlined"
                    disabled={busy || ctx.isLoading || readOnly}
                    onClick={() =>
                      setOutTarget({ name: r.backend, cost: disableCost(r.backend, lanes, alreadyOut) })
                    }
                    sx={{ minWidth: 190, justifyContent: 'center' }}
                  >
                    Take it out of the setup
                  </Button>
                  <Typography
                    variant="caption"
                    sx={{ color: 'text.secondary', flex: 1, minWidth: 280 }}
                  >
                    A decision about what your setup contains. It leaves every fallback list and
                    stops being reported anywhere, including here. The definition is kept — Models
                    lists it under &ldquo;Out of this setup&rdquo;, which is where you put it back.
                  </Typography>
                </Box>
              </Box>
            ) : null}
          </Box>
        )
      })}

      <PauseDialog
        target={target?.name ?? null}
        warning={target?.warning}
        consequence={target ? <PauseCostNote cost={target.cost} /> : undefined}
        defaultReason={target?.reason}
        resumeHelp={target?.resumeHelp}
        busy={busy}
        onClose={() => setTarget(null)}
        onConfirm={(resumeAt, reason) => {
          if (target) pause.mutate({ model: target.name, resumeAt, reason })
          setTarget(null)
        }}
      />

      <DisableDialog
        target={outTarget?.name ?? null}
        cost={outTarget?.cost ?? null}
        busy={busy}
        onClose={() => setOutTarget(null)}
        onConfirm={() => {
          if (outTarget) setDisabled.mutate({ model: outTarget.name, disabled: true })
          setOutTarget(null)
        }}
      />
    </Alert>
  )
}

/**
 * The upstream-budget tab of Setup, and a rename.
 *
 * It was a top-level entry called "Quota", which is the word a person uses for
 * "am I about to run out". Three Lenny runs went looking there for exactly that
 * and found a ledger of remote providers' rate limits — twice after having just
 * seen a GPU at 96% two screens earlier. The page was honest; its NAME answered
 * a different question than the one it was filed under.
 *
 * So: "Budgets", under Setup, beside the providers it describes. What the box
 * itself is about to run out of is answered on Now and Machines.
 *
 * Being a tab also means it stops polling when nobody is reading it: this is the
 * only four-second query on the page, and it used to run whether or not the
 * reader had scrolled anywhere near it.
 */
export function ProviderBudgets() {
  const q = useLedger(4000)
  if (q.isLoading) return <Loading />
  const backends = q.data?.corrallm?.quotaLedger?.backends ?? []

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Typography
        variant="body2"
        sx={{
          color: 'text.secondary',
        }}
      >
        Per-backend budget learned live from each provider's rate-limit headers. A backend goes{' '}
        <b>unavailable</b> when exhausted or cooling from a 429, and a request that can fall back to
        another backend does.
        Counts are a snapshot from the last call — <i>observed N ago</i> — not a live tick.
      </Typography>

      {backends.length === 0 ? (
        <Alert severity="info">
          No remote free backend has been called yet. The ledger learns a backend's budget from its
          first response, so it stays empty until traffic flows.
        </Alert>
      ) : (
        <Panel title="Backends" subtitle="Budget per provider window" flush>
          <TableContainer>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Backend</TableCell>
                  <TableCell>Status</TableCell>
                  <TableCell>Requests</TableCell>
                  <TableCell>Tokens</TableCell>
                  <TableCell align="right">Seen</TableCell>
                  <TableCell align="right">Observed</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {backends.map((be) => (
                  <TableRow key={be.backend} hover>
                    <TableCell sx={{ fontFamily: 'monospace' }}>{be.backend}</TableCell>
                    <TableCell>
                      {be.available ? (
                        <Chip size="small" color="success" label="available" />
                      ) : be.stale ? (
                        <Tooltip title="Model churned out of the provider's free roster (P16e)">
                          <Chip size="small" color="warning" variant="outlined" label="stale" />
                        </Tooltip>
                      ) : n(be.coolingInSec) > 0 ? (
                        <Tooltip title="Cooling down after a 429">
                          <Chip size="small" color="error" label={`cooling ${be.coolingInSec}s`} />
                        </Tooltip>
                      ) : (
                        <Chip size="small" color="warning" label="exhausted" />
                      )}
                    </TableCell>
                    <TableCell>
                      {(be.windows ?? []).length > 0 ? (
                        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.3 }}>
                          {(be.windows ?? []).map((w) => (
                            <Typography key={w.label} variant="caption">
                              {windowText(w.label, Number(w.used), Number(w.limit))}
                              {w.resetsIn ? ` · ${w.label.includes('calendar') ? 'resets' : 'drains'} in ${w.resetsIn}` : ''}
                            </Typography>
                          ))}
                          <Typography
                            variant="caption"
                            sx={{
                              color: 'text.secondary',
                            }}
                          >
                            counter-mode
                          </Typography>
                        </Box>
                      ) : (
                        <BucketCell b={be.requests} />
                      )}
                    </TableCell>
                    <TableCell>
                      {(be.reported ?? []).length > 0 ? (
                        <Tooltip title="What the provider reports about its own limits, as sent. Shown, not enforced.">
                          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.3 }}>
                            {(be.reported ?? []).map((h) => (
                              <Typography key={h.name} variant="caption" sx={{ fontFamily: 'monospace' }}>
                                {h.name.replace(/^x-ratelimit-/, '')}: {h.value}
                              </Typography>
                            ))}
                          </Box>
                        </Tooltip>
                      ) : (be.windows ?? []).length > 0 ? (
                        <Typography
                          variant="caption"
                          sx={{
                            color: 'text.secondary',
                          }}
                        >
                          —
                        </Typography>
                      ) : (
                        <BucketCell b={be.tokens} />
                      )}
                    </TableCell>
                    <TableCell align="right">{fmtInt(n(be.seen))}</TableCell>
                    <TableCell align="right">
                      <Typography
                        variant="caption"
                        sx={{
                          color: 'text.secondary',
                        }}
                      >
                        {n(be.observedAgoSec)}s ago
                      </Typography>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableContainer>
        </Panel>
      )}
      <BilledVsRecorded />
    </Box>
  )
}
