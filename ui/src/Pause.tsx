import { useState } from 'react'
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
 * anything imports them.
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
 */
export function PauseDialog(props: {
  target: string | null
  warning?: string
  busy: boolean
  onClose: () => void
  onConfirm: (resumeAt: string | null, reason: string) => void
}) {
  const { target, warning, busy, onClose, onConfirm } = props
  const [until, setUntil] = useState('')
  const [reason, setReason] = useState('')

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
          Unloads it and keeps it unloaded — no request, fall-through or preload will
          start it again until it is resumed. Requests naming an affected model fall through
          to the rest of their lane, or fail with 503.
        </Typography>
        <TextField
          fullWidth
          size="small"
          type="datetime-local"
          label="Resume automatically at"
          value={until}
          onChange={(e) => setUntil(e.target.value)}
          slotProps={{ inputLabel: { shrink: true }, htmlInput: { min: nowLocal() } }}
          error={invalid}
          helperText={invalid ? 'Must be in the future' : 'Leave empty to pause until resumed by hand'}
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
