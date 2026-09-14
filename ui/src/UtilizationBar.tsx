import type { ReactNode } from 'react'
import { Box, Tooltip, Typography } from '@mui/material'
import { C, STATUS } from '@/theme'
import { fmtBytes } from '@/format'

/**
 * One pool, one bar: a THIN track the full width of the capacity, and a THICK
 * bar over the part of it that is held.
 *
 * The two weights are the whole idea. A stacked bar answers "what is this made
 * of", which needs a legend and several colours and is the job MemoryPanel
 * already does well. This answers a different, faster question — "how much of
 * this machine is spoken for" — and it has to survive being read at a glance in
 * a list of six machines, where a legend cannot follow it. Capacity is the
 * quiet rule; usage is the thing that moved.
 *
 * COLOUR DOES NOT ENCODE A THRESHOLD, deliberately. `ServerView.MadeRoomCount`
 * in internal/api/handlers.go states the reason: a device pool at 96% normally
 * means a model is RESIDENT — the box working as intended — and only becomes a
 * problem when something else wants the space. That is an event, not a level.
 * A bar that turned amber at 90% would report the healthy steady state as a
 * fault on every box that is doing its job, and the operator would learn to
 * ignore it. So the fill is one neutral accent at every level, and the question
 * "does full matter here" is answered by the `note` the caller passes — which
 * is a made-room COUNT, not a percentage.
 *
 * Two states do change the colour, because both mean something is actually
 * wrong:
 *   over capacity   more is held than the budget allows — the ledger is out of
 *                   step with reality, and the overflow is drawn in the error
 *                   colour past the end of the track.
 *   not measured    `available: false` on a device reading is "the probe could
 *                   not answer", NOT "zero in use". Drawing 0% for it would be
 *                   a measurement we did not take. It gets an empty dashed
 *                   track and says so in words.
 */

export function UtilizationBar(props: {
  /** What this bar is measuring — a pool name, a device name. */
  label: ReactNode
  /** A short role beside the label: 'device', 'system', 'device + system'. */
  role?: string
  /** Bytes held. */
  used: number
  /** Bytes that may be held — the scheduler's budget, not the physical total. */
  capacity: number
  /** Declared headroom kept out of `capacity`, if any. */
  reserve?: number
  /** False when nothing could be measured. Anything but an explicit false is available. */
  available?: boolean
  /** Shown in place of the numbers when unavailable — why there is no reading. */
  unavailableHint?: string
  /**
   * The sentence that says whether being full HERE has cost anything. Pass a
   * made-room count, not a threshold. See the note above.
   */
  note?: string
  /** Extra tooltip lines: what is holding it, what the device reports, and so on. */
  lines?: string[]
}) {
  const { label, role, used, capacity, reserve = 0, note, lines = [] } = props
  const available = props.available !== false
  const pct = capacity > 0 ? (used / capacity) * 100 : 0
  const over = capacity > 0 && used > capacity
  const fill = Math.max(0, Math.min(100, pct))
  const free = Math.max(0, capacity - used)

  const tip = (
    <Box>
      <Typography variant="caption" sx={{ display: 'block', color: C.text, fontWeight: 700 }}>
        {label}
        {role ? ` · ${role}` : ''}
      </Typography>
      {available ? (
        <>
          <Typography variant="caption" sx={{ display: 'block' }}>
            {fmtBytes(used)} held of {fmtBytes(capacity)} — {Math.round(pct)}%
          </Typography>
          <Typography variant="caption" sx={{ display: 'block' }}>
            {over
              ? `${fmtBytes(used - capacity)} MORE than the budget allows`
              : `${fmtBytes(free)} free`}
          </Typography>
          {reserve > 0 && (
            <Typography variant="caption" sx={{ display: 'block' }}>
              {fmtBytes(reserve)} more is held in reserve and never offered to a model
            </Typography>
          )}
        </>
      ) : (
        <Typography variant="caption" sx={{ display: 'block' }}>
          {props.unavailableHint ??
            'Nothing could be measured here. Not the same as nothing being in use — no reading was taken either way.'}
        </Typography>
      )}
      {lines.map((l) => (
        <Typography key={l} variant="caption" sx={{ display: 'block', color: C.textMuted }}>
          {l}
        </Typography>
      ))}
      {note && (
        <Typography variant="caption" sx={{ display: 'block', mt: 0.5, color: C.textMuted }}>
          {note}
        </Typography>
      )}
    </Box>
  )

  return (
    <Tooltip title={tip} placement="top">
      <Box sx={{ minWidth: 0, cursor: 'default' }}>
        <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, mb: 0.4 }}>
          <Typography variant="caption" sx={{ color: C.text, minWidth: 0 }}>
            {label}
          </Typography>
          {role && (
            <Typography variant="caption" sx={{ color: C.textFaint }}>
              {role}
            </Typography>
          )}
          <Box sx={{ flexGrow: 1 }} />
          {available ? (
            <>
              <Typography variant="caption" sx={{ fontVariantNumeric: 'tabular-nums' }}>
                {fmtBytes(used)} <span style={{ color: C.textFaint }}>/ {fmtBytes(capacity)}</span>
              </Typography>
              <Typography
                variant="caption"
                sx={{
                  minWidth: 34,
                  textAlign: 'right',
                  fontVariantNumeric: 'tabular-nums',
                  // The percentage takes the error colour ONLY when the ledger
                  // is over its own budget. Not at 90% — see the note above.
                  color: over ? STATUS.error : C.textMuted,
                }}
              >
                {Math.round(pct)}%
              </Typography>
            </>
          ) : (
            <Typography variant="caption" sx={{ color: C.textFaint }}>
              not measured
            </Typography>
          )}
        </Box>

        {/* The bar. A 14px box holds a 4px capacity track centred in it and a
            10px usage bar sitting on the same centre line, so usage reads as
            weight on a rule rather than as two stacked things. */}
        <Box sx={{ position: 'relative', height: 14, display: 'flex', alignItems: 'center' }}>
          <Box
            sx={{
              position: 'absolute',
              inset: '0 0 0 0',
              my: 'auto',
              height: 4,
              borderRadius: '2px',
              bgcolor: available ? C.raised : 'transparent',
              border: `1px ${available ? 'solid' : 'dashed'} ${available ? C.border : C.borderStrong}`,
            }}
          />
          {available && capacity > 0 && (
            <Box
              sx={{
                position: 'absolute',
                left: 0,
                my: 'auto',
                height: 10,
                width: `${fill}%`,
                minWidth: used > 0 ? 3 : 0,
                borderRadius: '3px',
                bgcolor: C.accent,
              }}
            />
          )}
          {/* Overflow hangs off the right end in the error colour: it cannot be
              drawn inside a track it does not fit in, and shortening the track
              to make it fit would hide the only fact worth seeing. */}
          {over && (
            <Box
              sx={{
                position: 'absolute',
                right: 0,
                my: 'auto',
                height: 10,
                width: 10,
                borderRadius: '0 3px 3px 0',
                bgcolor: STATUS.error,
              }}
            />
          )}
        </Box>
      </Box>
    </Tooltip>
  )
}
