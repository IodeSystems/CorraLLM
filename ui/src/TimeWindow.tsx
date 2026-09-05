import { useState } from 'react'
import { Box, Button, Chip, Popover, Stack, TextField, Typography } from '@mui/material'
import { C } from '@/theme'

/**
 * THE PAGE'S TIME AXIS — one control, obeyed by every panel under it.
 *
 * Traffic used to be two pages of panels, each carrying its own trailing
 * constant: 60 minutes for utilization, promises and journeys, 24 hours for the
 * service profiles and every rollup, a 6h/24h/7d toggle of its own on the key
 * charts, and "the newest 100 rows, whenever they were" for the log. Two
 * consequences, both found by a Lenny run (plan/p30-information-architecture.md):
 *
 *   1. The product could answer "the last hour" and "the last day" and could NOT
 *      answer "this morning", which is the question a person actually arrives
 *      with when the assistant started failing at 09:00.
 *   2. Two numbers about the same caller, on the same screen, disagreed — because
 *      they were measured over different spans and nothing said so.
 *
 * So the window is a property of the PAGE. A relative window keeps ticking (the
 * panels stay live and their own refetch keeps working); an absolute one is
 * frozen, which is what makes a number quotable — "43% at 09:00" stops being true
 * the moment the window slides.
 */
export type TimeWindow =
  | { kind: 'relative'; minutes: number }
  | { kind: 'absolute'; fromMS: number; toMS: number }

export const PRESETS: { label: string; minutes: number }[] = [
  { label: 'Last hour', minutes: 60 },
  { label: 'Last 6 hours', minutes: 360 },
  { label: 'Last 24 hours', minutes: 1440 },
  { label: 'Last 7 days', minutes: 10080 },
]

export const DEFAULT_WINDOW: TimeWindow = { kind: 'relative', minutes: 60 }

/**
 * The variables every windowed query sends.
 *
 * `minutes`/`windowHours` are ALWAYS sent, even for an absolute window: the
 * server ignores them when from/to are present (from/to win), and several of its
 * inputs declare `minimum: 1`, so sending 0 would be rejected by validation
 * rather than treated as "no window". The absolute pair is the meaningful half.
 */
export function windowVars(w: TimeWindow): {
  minutes: string
  windowHours: string
  from: string
  to: string
} {
  if (w.kind === 'absolute') {
    return {
      minutes: '1440',
      windowHours: '24',
      from: String(w.fromMS),
      to: String(w.toMS),
    }
  }
  return {
    minutes: String(w.minutes),
    windowHours: String(Math.max(1, Math.round(w.minutes / 60))),
    from: '0',
    to: '0',
  }
}

/** A cache key that changes exactly when the window does. */
export function windowKey(w: TimeWindow): string {
  return w.kind === 'absolute' ? `abs:${w.fromMS}-${w.toMS}` : `rel:${w.minutes}`
}

/**
 * How the window reads in a CHIP: short, no preposition.
 */
export function windowLabel(w: TimeWindow): string {
  if (w.kind === 'absolute') {
    const f = new Date(w.fromMS)
    const t = new Date(w.toMS)
    const sameDay = f.toDateString() === t.toDateString()
    const time = (d: Date) => d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    return sameDay
      ? `${f.toLocaleDateString()} ${time(f)}–${time(t)}`
      : `${f.toLocaleString()} – ${t.toLocaleString()}`
  }
  const p = PRESETS.find((x) => x.minutes === w.minutes)
  if (p) return p.label.toLowerCase()
  return w.minutes >= 1440 ? `last ${Math.round(w.minutes / 1440)}d` : `last ${w.minutes} min`
}

/**
 * How the window reads inside a SENTENCE — "Models asked for in the last hour",
 * "Nobody was turned away between 09:00 and 10:00". Panels say this rather than
 * each printing its own "last 60 minutes", which is how they came to disagree
 * about the same caller in the first place.
 */
export function windowPhrase(w: TimeWindow): string {
  if (w.kind === 'absolute') {
    const f = new Date(w.fromMS)
    const t = new Date(w.toMS)
    const time = (d: Date) => d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    return f.toDateString() === t.toDateString()
      ? `between ${time(f)} and ${time(t)} on ${f.toLocaleDateString()}`
      : `between ${f.toLocaleString()} and ${t.toLocaleString()}`
  }
  return `in the ${windowLabel(w).replace(/^last /, 'last ')}`
}

// datetime-local wants "YYYY-MM-DDTHH:mm" in LOCAL time, and toISOString gives
// UTC — an hour or several out for most of the world, silently.
const forInput = (ms: number) => {
  const d = new Date(ms)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export function TimeWindowPicker({
  value,
  onChange,
}: {
  value: TimeWindow
  onChange: (w: TimeWindow) => void
}) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null)
  const now = Date.now()
  const openFrom = value.kind === 'absolute' ? value.fromMS : now - value.minutes * 60_000
  const openTo = value.kind === 'absolute' ? value.toMS : now
  const [from, setFrom] = useState(forInput(openFrom))
  const [to, setTo] = useState(forInput(openTo))

  const apply = () => {
    const f = new Date(from).getTime()
    const t = new Date(to).getTime()
    if (!Number.isFinite(f) || !Number.isFinite(t) || t <= f) return
    onChange({ kind: 'absolute', fromMS: f, toMS: t })
    setAnchor(null)
  }

  return (
    <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap' }} useFlexGap>
      {PRESETS.map((p) => (
        <Chip
          key={p.minutes}
          size="small"
          label={p.label}
          onClick={() => onChange({ kind: 'relative', minutes: p.minutes })}
          variant={value.kind === 'relative' && value.minutes === p.minutes ? 'filled' : 'outlined'}
          color={value.kind === 'relative' && value.minutes === p.minutes ? 'primary' : 'default'}
        />
      ))}
      <Chip
        size="small"
        label={value.kind === 'absolute' ? windowLabel(value) : 'Pick a time…'}
        onClick={(e) => setAnchor(e.currentTarget)}
        variant={value.kind === 'absolute' ? 'filled' : 'outlined'}
        color={value.kind === 'absolute' ? 'primary' : 'default'}
      />
      {/* Live vs frozen, said rather than implied: a page that stopped updating
          must say so (OB-9). */}
      <Typography variant="caption" sx={{ color: C.textFaint }}>
        {value.kind === 'absolute'
          ? 'a fixed span — these numbers will not change'
          : 'updating as requests arrive'}
      </Typography>

      <Popover
        open={!!anchor}
        anchorEl={anchor}
        onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
      >
        <Box sx={{ p: 2, display: 'flex', flexDirection: 'column', gap: 1.5, minWidth: 280 }}>
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            Everything on this page will show this span, and nothing else.
          </Typography>
          <TextField
            type="datetime-local"
            size="small"
            label="From"
            value={from}
            onChange={(e) => setFrom(e.target.value)}
            slotProps={{ inputLabel: { shrink: true } }}
          />
          <TextField
            type="datetime-local"
            size="small"
            label="To"
            value={to}
            onChange={(e) => setTo(e.target.value)}
            slotProps={{ inputLabel: { shrink: true } }}
          />
          <Stack direction="row" spacing={1} sx={{ justifyContent: 'flex-end' }}>
            <Button size="small" onClick={() => setAnchor(null)}>
              Cancel
            </Button>
            <Button size="small" variant="contained" onClick={apply}>
              Show this span
            </Button>
          </Stack>
        </Box>
      </Popover>
    </Stack>
  )
}
