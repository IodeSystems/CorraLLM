import { useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Box,
  Button,
  ButtonGroup,
  InputAdornment,
  Menu,
  MenuItem,
  TextField,
  Typography,
} from '@mui/material'
import ArrowDropDownIcon from '@mui/icons-material/ArrowDropDown'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'

/**
 * WHAT THE OPERATOR SAID ABOUT A WARNING: hide it for a while, or raise it only
 * past a threshold.
 *
 * Neither changes anything the box does. They change what the Warnings panel
 * says, and nothing else — the tables further down still show every row, so a
 * hidden warning is never a hidden fact.
 */

export type WarningKind =
  | 'queue-clash'
  | 'turned-away'
  | 'unanswered'
  | 'slow-stretch'
  | 'open-callers'

const WarningPrefsDoc = graphql(/* GraphQL */ `
  query WarningPrefs {
    corrallm {
      warningPrefs {
        slowStretchDefault
        prefs {
          kind
          subject
          suppressed
          suppressedUntilMs
          suppressedForever
          threshold
        }
      }
    }
  }
`)

const SuppressWarningDoc = graphql(/* GraphQL */ `
  mutation SuppressWarning($body: corrallm_SuppressWarningInputBodyInput!) {
    corrallm {
      suppressWarning(body: $body) {
        ok
        message
      }
    }
  }
`)

const SetWarningThresholdDoc = graphql(/* GraphQL */ `
  mutation SetWarningThreshold($body: corrallm_SetWarningThresholdInputBodyInput!) {
    corrallm {
      setWarningThreshold(body: $body) {
        ok
        message
      }
    }
  }
`)

export type WarningPref = NonNullable<
  NonNullable<
    NonNullable<Awaited<ReturnType<typeof fetchPrefs>>['corrallm']['warningPrefs']>['prefs']
  >[number]
>

const fetchPrefs = () => gqlClient.request(WarningPrefsDoc)

export function useWarningPrefs() {
  const q = useQuery({
    queryKey: ['warningPrefs'],
    queryFn: fetchPrefs,
    // A suppression lapses on the clock, not on an event, so poll at the
    // pace the warnings themselves do.
    refetchInterval: 15000,
  })
  const prefs = q.data?.corrallm.warningPrefs?.prefs ?? []
  return {
    isLoading: q.isLoading,
    prefs,
    slowStretchDefault: q.data?.corrallm.warningPrefs?.slowStretchDefault ?? 3,
    suppressed: (kind: WarningKind, subject = '') =>
      prefs.some((p) => p.kind === kind && p.subject === subject && p.suppressed),
    /** The operator's threshold, or 0 for the built-in default. */
    threshold: (kind: WarningKind) =>
      prefs.find((p) => p.kind === kind && p.subject === '')?.threshold ?? 0,
  }
}

function useWarningMutations() {
  const qc = useQueryClient()
  const onSuccess = () => {
    void qc.invalidateQueries({ queryKey: ['warningPrefs'] })
    // The slow-stretch multiple is applied server-side, inside the activity
    // queries, so a new threshold has to refetch those too.
    void qc.invalidateQueries({ queryKey: ['activity'] })
  }
  const suppress = useMutation({
    mutationFn: (body: {
      kind: WarningKind
      subject?: string
      minutes?: number
      forever?: boolean
      lift?: boolean
    }) =>
      gqlClient.request(SuppressWarningDoc, {
        body: { ...body, minutes: body.minutes === undefined ? undefined : String(body.minutes) },
      }),
    onSuccess,
  })
  const setThreshold = useMutation({
    mutationFn: (body: { kind: WarningKind; threshold: number }) =>
      gqlClient.request(SetWarningThresholdDoc, { body }),
    onSuccess,
  })
  return { suppress, setThreshold }
}

const SUPPRESS_FOR = [
  { label: 'for 1 hour', minutes: 60 },
  { label: 'for 1 day', minutes: 24 * 60 },
  { label: 'for 1 week', minutes: 7 * 24 * 60 },
  { label: 'until I show it again', forever: true },
] as const

/**
 * Hide one warning. The button itself is the default — one day: past the rest
 * of the working day, short enough that a problem nobody fixed comes back.
 */
export function SuppressButton({ kind, subject }: { kind: WarningKind; subject?: string }) {
  const { suppress } = useWarningMutations()
  const [anchor, setAnchor] = useState<HTMLElement | null>(null)
  return (
    <>
      <ButtonGroup size="small" variant="text" disabled={suppress.isPending}>
        <Button onClick={() => suppress.mutate({ kind, subject, minutes: 24 * 60 })}>
          {suppress.isPending ? 'Hiding…' : 'Hide for 1 day'}
        </Button>
        <Button aria-label="Hide for another span" onClick={(e) => setAnchor(e.currentTarget)}>
          <ArrowDropDownIcon fontSize="small" />
        </Button>
      </ButtonGroup>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)}>
        {SUPPRESS_FOR.map((o) => (
          <MenuItem
            key={o.label}
            onClick={() => {
              setAnchor(null)
              suppress.mutate(
                'forever' in o ? { kind, subject, forever: true } : { kind, subject, minutes: o.minutes },
              )
            }}
          >
            Hide {o.label}
          </MenuItem>
        ))}
      </Menu>
      {suppress.isError && (
        <Typography variant="caption" sx={{ color: C.warn }}>
          Did not save: {String(suppress.error)}
        </Typography>
      )}
    </>
  )
}

/**
 * What a threshold's number counts, per kind. A bare "3" on screen was the bug
 * this exists for: 3 requests, 3 callers and 3× the average all read the same.
 *
 * The count kinds are per window — "above 5 requests" means in whatever span
 * the page is showing, so the same threshold is stricter on a 1-hour view than
 * on a 7-day one.
 */
const THRESHOLD_UNIT: Record<WarningKind, { field: string; unit: string } | undefined> = {
  'queue-clash': undefined,
  'turned-away': { field: 'Warn only above', unit: 'requests in the window' },
  unanswered: { field: 'Warn only above', unit: 'requests in the window' },
  'open-callers': { field: 'Warn only above', unit: 'callers' },
  'slow-stretch': { field: 'Slow means longer than', unit: '× the average request' },
}

/** What "reset" returns a kind to, said as the value — not as "the default". */
export function resetLabel(kind: WarningKind, slowDefault = 3): string {
  return kind === 'slow-stretch'
    ? `Reset to ${slowDefault}× (default)`
    : 'Reset: warn on the first one (default)'
}

/** A threshold in words, with its unit. 0 on a count kind is the default. */
export function describeThreshold(kind: WarningKind, value: number): string {
  if (kind === 'slow-stretch') return `slow means longer than ${value}× the average request`
  const u = THRESHOLD_UNIT[kind]
  if (!u) return String(value)
  if (value === 0) return `warns on the first one (default)`
  return `warns only above ${value} ${u.unit}`
}

/**
 * Raise the bar a warning must clear. `current` is what is in force now — the
 * operator's value or the default — so the field starts from what the reader
 * is changing.
 */
export function ThresholdButton({
  kind,
  current,
  isDefault,
  min = 0,
  minExclusive = false,
  step = 1,
}: {
  kind: WarningKind
  current: number
  isDefault: boolean
  min?: number
  /** The value must be above `min`, not at or above it. */
  minExclusive?: boolean
  step?: number
}) {
  const { setThreshold } = useWarningMutations()
  const [open, setOpen] = useState(false)
  const [value, setValue] = useState(String(current))
  const n = Number(value)
  const valid =
    value.trim() !== '' && Number.isFinite(n) && (minExclusive ? n > min : n >= min)
  if (!open) {
    return (
      <Button
        size="small"
        variant="text"
        onClick={() => {
          setValue(String(current))
          setOpen(true)
        }}
      >
        Threshold: {describeThreshold(kind, current)}
      </Button>
    )
  }
  return (
    <Box sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap' }}>
      <TextField
        size="small"
        type="number"
        label={THRESHOLD_UNIT[kind]?.field}
        value={value}
        onChange={(e) => setValue(e.target.value)}
        slotProps={{
          htmlInput: { min, step },
          input: {
            endAdornment: (
              <InputAdornment position="end">{THRESHOLD_UNIT[kind]?.unit}</InputAdornment>
            ),
          },
        }}
        sx={{ width: 340 }}
      />
      <Button
        size="small"
        variant="outlined"
        disabled={!valid || setThreshold.isPending}
        onClick={() =>
          setThreshold.mutate({ kind, threshold: n }, { onSuccess: () => setOpen(false) })
        }
      >
        Save
      </Button>
      {!isDefault && (
        <Button
          size="small"
          variant="text"
          disabled={setThreshold.isPending}
          onClick={() =>
            setThreshold.mutate({ kind, threshold: 0 }, { onSuccess: () => setOpen(false) })
          }
        >
          {resetLabel(kind)}
        </Button>
      )}
      <Button size="small" variant="text" onClick={() => setOpen(false)}>
        Cancel
      </Button>
      {setThreshold.isError && (
        <Typography variant="caption" sx={{ color: C.warn }}>
          Did not save: {String(setThreshold.error)}
        </Typography>
      )}
    </Box>
  )
}

const KIND_NAME: Record<string, string> = {
  'queue-clash': 'Queue settings contradict',
  'turned-away': 'Told to come back later',
  unanswered: 'Refused or cut short',
  'slow-stretch': 'Much slower than the rest',
  'open-callers': 'Callers still without an answer',
}

/**
 * The warnings that are hidden or tuned, each with the way to undo it. A
 * hidden warning that leaves no trace is a setting nobody remembers making —
 * and a threshold's own controls leave with the line it hides, so without this
 * list a raised threshold could only be undone through the API.
 */
export function HiddenWarnings({ prefs }: { prefs: readonly WarningPref[] }) {
  const { suppress, setThreshold } = useWarningMutations()
  const hidden = prefs.filter((p) => p.suppressed)
  const tuned = prefs.filter((p) => p.threshold > 0)
  if (hidden.length === 0 && tuned.length === 0) return null
  const until = (p: WarningPref) =>
    p.suppressedForever
      ? 'until shown again'
      : `until ${new Date(Number(p.suppressedUntilMs)).toLocaleString([], {
          weekday: 'short',
          hour: '2-digit',
          minute: '2-digit',
        })}`
  const line = (key: string, text: string, action: ReactNode) => (
    <Box key={key} sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap' }}>
      <Typography variant="caption" sx={{ color: C.textMuted }}>
        {text}
      </Typography>
      {action}
    </Box>
  )
  const busy = suppress.isPending || setThreshold.isPending
  return (
    <Box>
      <Typography variant="caption" sx={{ color: C.textFaint }}>
        Held back by a setting here — still in the tables below. All settings:{' '}
        <Link to="/setup" search={{ tab: 'warnings' }}>
          Setup → Warnings
        </Link>
      </Typography>
      {hidden.map((p) =>
        line(
          `hide/${p.kind}/${p.subject}`,
          `${KIND_NAME[p.kind] ?? p.kind}${p.subject ? ` — ${p.subject}` : ''}: hidden ${until(p)}`,
          <Button
            size="small"
            variant="text"
            disabled={busy}
            onClick={() =>
              suppress.mutate({ kind: p.kind as WarningKind, subject: p.subject, lift: true })
            }
          >
            Show again
          </Button>,
        ),
      )}
      {tuned.map((p) =>
        line(
          `tune/${p.kind}`,
          `${KIND_NAME[p.kind] ?? p.kind}: ${describeThreshold(p.kind as WarningKind, p.threshold)}`,
          <Button
            size="small"
            variant="text"
            disabled={busy}
            onClick={() => setThreshold.mutate({ kind: p.kind as WarningKind, threshold: 0 })}
          >
            {resetLabel(p.kind as WarningKind)}
          </Button>,
        ),
      )}
    </Box>
  )
}

const TUNABLE: { kind: WarningKind; minExclusive?: boolean; min?: number; step?: number }[] = [
  { kind: 'turned-away' },
  { kind: 'unanswered' },
  { kind: 'open-callers' },
  { kind: 'slow-stretch', min: 1, minExclusive: true, step: 0.5 },
]

/**
 * Setup → Warnings: every standing order on the Warnings panel, in one place.
 *
 * The panel shows a setting only while it holds a warning back, and shows
 * nothing at all when no warning is up — so a threshold or a hide can be in
 * force with no line anywhere on Traffic saying so. This is where they all are,
 * whether or not anything is being held back right now.
 */
export function WarningSettings() {
  const prefs = useWarningPrefs()
  const { suppress } = useWarningMutations()
  if (prefs.isLoading) {
    return (
      <Typography variant="body2" sx={{ color: C.textFaint }}>
        Asking this box for its warning settings — nothing is listed yet.
      </Typography>
    )
  }
  const hidden = prefs.prefs.filter((p) => p.suppressed)
  const until = (p: WarningPref) =>
    p.suppressedForever
      ? 'until shown again'
      : `until ${new Date(Number(p.suppressedUntilMs)).toLocaleString([], {
          weekday: 'short',
          hour: '2-digit',
          minute: '2-digit',
        })}`
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Typography variant="body2" sx={{ color: C.textMuted }}>
        What the Warnings panel on Traffic holds back. None of this changes what the box does — only
        which lines that panel shows. The tables on Traffic still show every row.
      </Typography>

      <Box>
        <Typography variant="subtitle2">Thresholds</Typography>
        {TUNABLE.map((t) => {
          const custom = prefs.threshold(t.kind)
          const current = t.kind === 'slow-stretch' && custom === 0 ? prefs.slowStretchDefault : custom
          return (
            <Box
              key={t.kind}
              sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap', mt: 0.5 }}
            >
              <Typography variant="body2" sx={{ minWidth: 260 }}>
                {KIND_NAME[t.kind]}
                {custom > 0 && (
                  <Typography component="span" variant="caption" sx={{ color: C.warn, ml: 1 }}>
                    custom
                  </Typography>
                )}
              </Typography>
              <ThresholdButton
                kind={t.kind}
                current={current}
                isDefault={custom === 0}
                min={t.min}
                minExclusive={t.minExclusive}
                step={t.step}
              />
            </Box>
          )
        })}
      </Box>

      <Box>
        <Typography variant="subtitle2">Hidden</Typography>
        {hidden.length === 0 ? (
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            Nothing is hidden. Use <b>Hide for 1 day ▾</b> on a warning to hide it.
          </Typography>
        ) : (
          hidden.map((p) => (
            <Box
              key={`${p.kind}/${p.subject}`}
              sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap', mt: 0.5 }}
            >
              <Typography variant="body2" sx={{ minWidth: 260 }}>
                {KIND_NAME[p.kind] ?? p.kind}
                {p.subject ? ` — ${p.subject}` : ''}
              </Typography>
              <Typography variant="body2" sx={{ color: C.textMuted }}>
                hidden {until(p)}
              </Typography>
              <Button
                size="small"
                variant="text"
                disabled={suppress.isPending}
                onClick={() =>
                  suppress.mutate({ kind: p.kind as WarningKind, subject: p.subject, lift: true })
                }
              >
                Show again
              </Button>
            </Box>
          ))
        )}
      </Box>
    </Box>
  )
}
