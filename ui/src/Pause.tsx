import { useEffect, useState, type ReactNode } from 'react'
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  TextField,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { C } from '@/theme'

/**
 * PAUSE, IN ONE PLACE.
 *
 * Pausing a backend is the reversible way to take it out of service, and it
 * grew up inside /models because that is where the first button needed it. A
 * second screen now needs the same thing, and the way that goes wrong is a
 * second pause built beside this one: two dialogs, two sets of words, and a
 * drift nobody notices until the two disagree about what a pause does.
 *
 * So the dialog and the mutations live here, and every screen that pauses
 * anything imports them — along with the part that matters most, the
 * arithmetic of what one will cost.
 */

export const PauseModelDoc = graphql(/* GraphQL */ `
  mutation PauseModel($model: String!, $resumeAt: String, $reason: String) {
    corrallm {
      pauseModel(body: { model: $model, resumeAt: $resumeAt, reason: $reason }) {
        ok
        message
        target
        affected
        evicted
        draining
      }
    }
  }
`)

export const UnpauseModelDoc = graphql(/* GraphQL */ `
  mutation UnpauseModel($model: String!) {
    corrallm {
      unpauseModel(body: { model: $model }) {
        ok
        message
      }
    }
  }
`)

/**
 * A fallback list as the arithmetic below needs it.
 *
 * `ladder` is what the server actually resolved and is what a request walks;
 * `members` is only what the config wrote down. They differ whenever a pool, a
 * selector or a directory choice contributes a rung — on this box `free`
 * declares one member and resolves to eleven — so a consequence computed from
 * `members` would tell the reader the opposite of the truth.
 *
 * A rung carries `disabled` because the ladder still LISTS a declared rung the
 * setup has taken out — deliberately, so the decision stays visible and has
 * somewhere to be undone — while never walking it. Counting it is the same
 * error as counting `members`, one rung further in.
 */
export type LaneShape = {
  name: string
  ladder: readonly { model: string; disabled?: boolean }[]
  members: readonly { model: string }[]
}

/** One fallback list's fate under a pause. */
export type LaneCost = {
  lane: string
  /** How many rungs the list resolves to right now that would take work. */
  rungs: number
  /** Rungs this pause removes from it. */
  taken: string[]
  /** Rungs that would still serve afterwards, in ladder order. */
  survivors: string[]
}

export type PauseCost = {
  /** Served names this pause takes out — more than one when an extension hosts them. */
  taking: string[]
  lanes: LaneCost[]
  /** At least one list would have nothing left that can serve. */
  strands: boolean
}

/**
 * WHAT THIS PAUSE WILL COST, from the membership in front of us (OB-1).
 *
 * The same click is harmless on one box and an outage on another, and the
 * difference is not visible in the button: a fallback list with four healthy
 * rungs falls through, a list with one turns every caller away. So the
 * answer is computed per press and never written into the copy.
 *
 * `alreadyOut` is the rungs that cannot serve today either — paused, or
 * refusing every request. They are not survivors: counting them would promise a
 * fall-through to a backend that is itself broken, which is the same lie in a
 * politer form.
 *
 * A DISABLED RUNG IS NOT COUNTED AT ALL, which is different from being in
 * `alreadyOut`. Paused and refusing rungs are rungs: they are on the ladder, a
 * request walks past them, and one of them coming back is what the reader is
 * waiting for — so they belong in the denominator and not among the survivors.
 * A rung the setup has taken out is not on the ladder in any sense a request
 * can see; it is listed only so the decision is visible. Leaving it in `rungs`
 * would report "1 of its 2 backends is left" about a list with exactly one.
 */
export function pauseCost(
  taking: readonly string[],
  lanes: readonly LaneShape[],
  alreadyOut: ReadonlySet<string>,
): PauseCost {
  const gone = new Set(taking)
  const out: LaneCost[] = []
  for (const l of lanes) {
    const walked: readonly { model: string; disabled?: boolean }[] =
      l.ladder.length > 0 ? l.ladder : l.members
    const rungs = walked.filter((r) => !r.disabled).map((r) => r.model)
    if (!rungs.some((r) => gone.has(r))) continue
    out.push({
      lane: l.name,
      rungs: rungs.length,
      taken: rungs.filter((r) => gone.has(r)),
      survivors: rungs.filter((r) => !gone.has(r) && !alreadyOut.has(r)),
    })
  }
  return {
    taking: [...taking],
    lanes: out,
    strands: out.some((l) => l.survivors.length === 0),
  }
}

/**
 * The cost, in sentences.
 *
 * Every line here is derived from `cost` — there is no fixed claim about
 * whether a pause is safe, because that is exactly the claim a screen must not
 * make on a box it has not counted.
 */
export function PauseCostNote({ cost }: { cost: PauseCost }) {
  return (
    <>
      {cost.lanes.map((l) =>
        l.survivors.length > 0 ? (
          <Typography key={l.lane} variant="body2" sx={{ mb: 1 }}>
            <b>{l.lane}</b> keeps serving — {l.survivors.length} of its {l.rungs} backends{' '}
            {l.survivors.length === 1 ? 'is' : 'are'} left, and <b>{l.survivors[0]}</b> is tried
            first.
          </Typography>
        ) : (
          <Alert key={l.lane} severity="error" sx={{ mb: 1 }}>
            <b>{l.lane}</b> stops serving.{' '}
            {l.taken.length === l.rungs
              ? l.rungs === 1
                ? 'It has one backend and this is it.'
                : 'This takes every backend on it.'
              : `Its other ${l.rungs - l.taken.length} ${
                  l.rungs - l.taken.length === 1 ? 'backend is' : 'backends are'
                } already paused or refusing.`}{' '}
            Every request for <b>{l.lane}</b> is answered 429 &ldquo;retry after 5 minutes&rdquo; until this is resumed.
          </Alert>
        ),
      )}
      {cost.lanes.length === 0 && (
        <Typography variant="body2" sx={{ mb: 1 }}>
          No fallback list contains it, so nothing falls through to anything else.
        </Typography>
      )}
      <Typography variant="body2" sx={{ mb: 2, color: C.textMuted }}>
        Anything that asks for{' '}
        {cost.taking.length === 1 ? <b>{cost.taking[0]}</b> : 'one of them'} by name, rather than
        through a fallback list, is answered 429 &ldquo;retry after 5 minutes&rdquo; (or the time left, if
        sooner) on every request until the pause lifts.
      </Typography>
    </>
  )
}

/**
 * PauseDialog collects the optional resume time and reason for a pause.
 *
 * The input is `datetime-local`, whose value is a LOCAL wall-clock string with
 * no zone ("2026-08-02T09:00"). The server takes RFC3339 and enforces
 * "in the future", so the conversion has to happen here, through a Date — which
 * is exactly right: the operator means 9am where they are, not 9am UTC.
 *
 * `min` is now, not midnight tonight: "after today" in the sense that matters is
 * "not in the past", and forbidding a pause that lifts in two hours because it
 * is still today would be the wrong reading.
 *
 * `warning` carries the blast radius when it is wider than the thing named —
 * pausing one model of an extension pauses all of them, and that must be on the
 * screen BEFORE the click, not in the result message after it.
 *
 * `consequence` is what CALLERS get, counted from the live fallback lists. It
 * replaces a fixed sentence that hedged — "fall through to the rest of their
 * lane, or fail with 503" — which is both outcomes at once and therefore no
 * answer to the only question the reader has.
 *
 * `defaultReason` and `resumeHelp` exist for a caller that already knows more
 * about why than the operator should have to retype. Both are prefill: the
 * reason field stays editable and the resume time stays empty, because a
 * default resume time is a promise about when a fault will be fixed and no
 * screen is in a position to make one.
 */
export function PauseDialog(props: {
  target: string | null
  warning?: string
  consequence?: ReactNode
  defaultReason?: string
  resumeHelp?: string
  busy: boolean
  onClose: () => void
  onConfirm: (resumeAt: string | null, reason: string) => void
}) {
  const { target, warning, consequence, defaultReason, resumeHelp, busy, onClose, onConfirm } =
    props
  const [until, setUntil] = useState('')
  const [reason, setReason] = useState('')

  // Seed on OPEN, not on mount. The dialog is mounted closed for the life of
  // the page it sits on, so a useState initialiser would capture the first
  // target's prefill and hand it to every target after it.
  useEffect(() => {
    if (!target) return
    setUntil('')
    setReason(defaultReason ?? '')
  }, [target, defaultReason])

  // Local-time "now" in the format the input wants, sliced to minutes.
  const nowLocal = () => {
    const d = new Date()
    d.setMinutes(d.getMinutes() - d.getTimezoneOffset())
    return d.toISOString().slice(0, 16)
  }

  const close = () => {
    setUntil('')
    setReason('')
    onClose()
  }

  const parsed = until ? new Date(until) : null
  const invalid = !!parsed && !(parsed.getTime() > Date.now())

  return (
    <Dialog open={!!target} onClose={close} maxWidth="xs" fullWidth>
      <DialogTitle>Pause {target}</DialogTitle>
      <DialogContent>
        {warning && (
          <Alert severity="warning" sx={{ mb: 2 }}>
            {warning}
          </Alert>
        )}
        <Typography variant="body2" sx={{ mb: 2, color: C.textMuted }}>
          Unloads it and keeps it unloaded — no request, fall-through or preload will start it again
          until it is resumed. Nothing is removed from your configuration.
        </Typography>
        {consequence ?? (
          <Typography variant="body2" sx={{ mb: 2, color: C.textMuted }}>
            Requests naming an affected model fall through to the rest of their fallback list, or
            fail with 503.
          </Typography>
        )}
        <TextField
          fullWidth
          size="small"
          type="datetime-local"
          label="Resume automatically at"
          value={until}
          onChange={(e) => setUntil(e.target.value)}
          slotProps={{ inputLabel: { shrink: true }, htmlInput: { min: nowLocal() } }}
          error={invalid}
          helperText={
            invalid
              ? 'Must be in the future'
              : (resumeHelp ?? 'Leave empty to pause until resumed by hand')
          }
          sx={{ mb: 2 }}
        />
        <TextField
          fullWidth
          size="small"
          label="Reason (optional)"
          placeholder="e.g. freeing the GPU for the 70B eval"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
      </DialogContent>
      <DialogActions>
        <Button onClick={close}>Cancel</Button>
        <Button
          variant="contained"
          color="warning"
          disabled={busy || invalid}
          onClick={() => {
            onConfirm(parsed ? parsed.toISOString() : null, reason.trim())
            close()
          }}
        >
          Pause
        </Button>
      </DialogActions>
    </Dialog>
  )
}
