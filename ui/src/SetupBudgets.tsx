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
import { fmtInt } from '@/format'
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
 */
function refusalAction(status: number, offer: 'pause' | 'paused' | 'none'): string {
  if (offer === 'pause') return `${refusalFix(status)}. Or stop calling it:`
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
            model
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
 */
export function RefusingBanner() {
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
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)

  // Both keys: the ledger decides whether this banner renders at all, the
  // context decides whether it offers Pause or Resume.
  const done = (text?: string | null, ok?: boolean | null) => {
    setMsg({ ok: !!ok, text: text ?? '' })
    void qc.invalidateQueries({ queryKey: ['quota'] })
    void qc.invalidateQueries({ queryKey: ['refusingContext'] })
    void qc.invalidateQueries({ queryKey: ['overview'] })
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
  const busy = pause.isPending || unpause.isPending

  if (refusing.length === 0) return null

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
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap', mt: 0.5 }}>
                <Button
                  size="small"
                  variant="outlined"
                  color="success"
                  disabled={busy}
                  onClick={() => unpause.mutate(r.backend)}
                >
                  Resume it
                </Button>
                <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                  {/* A paused backend cannot be reached through this box, so a
                      refusal dated after the pause is not a caller — it is
                      something else holding the same key, and saying so is the
                      difference between a stale count and a live one. */}
                  {n(r.lastMs) > n(model.pausedAtMs)
                    ? `Paused ${fmtSince(model.pausedAtMs)}, and it refused again after that — a pause stops requests through this box, not anything else using the same key.`
                    : `Paused ${fmtSince(model.pausedAtMs)}. This box is not calling it; the refusals counted above are from before that.`}
                </Typography>
              </Box>
            ) : model ? (
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap', mt: 0.5 }}>
                <Button
                  size="small"
                  variant="outlined"
                  color="warning"
                  disabled={busy || ctx.isLoading}
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
                >
                  Pause it
                </Button>
                <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                  Stops this box calling it: callers are told the model is paused instead of getting
                  its {status}, and nothing more is spent on a credential that cannot answer. It
                  stays in your configuration, and resuming it is one press here.
                </Typography>
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
                              {w.used}/{w.limit} /{w.label}
                              {w.resetsIn ? ` · resets ${w.resetsIn}` : ''}
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
                      {(be.windows ?? []).length > 0 ? (
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
    </Box>
  )
}
