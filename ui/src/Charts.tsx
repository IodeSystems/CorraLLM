import { useRef, useState } from 'react'
import { Box, Button, Typography } from '@mui/material'
import { Panel } from '@/Panel'
import { C, seriesColor } from '@/theme'
import { fmtTime } from '@/format'

/**
 * The chart primitives, defined once.
 *
 * These lived inside usage.tsx while it was the only page with charts. The keys
 * page needs the same marks, and a second copy is how two charts of the same
 * data start disagreeing about gaps, stacking order, and color assignment.
 *
 * Palette: `SERIES` from the theme, validated against the panel surface (lightness
 * band, chroma floor, adjacent CVD ΔE 8.8, normal-vision separation, contrast —
 * all pass). Re-run the validator before reordering or substituting a hue.
 */

export type ChartSeries = { key: string; color: string; values: number[] }

// OTHER_COLOR is deliberately NOT a palette hue. "Other" is an aggregate, not an
// entity, and giving it a categorical color would claim an identity it does not
// have — the same reason non-model memory segments wear grey on the Overview.
const OTHER_COLOR = C.textFaint
const OTHER_LABEL = 'other'

/**
 * foldSeries ranks entities by total and keeps the top `max`, summing the rest
 * into a single neutral "other" band.
 *
 * Categorical palettes have a fixed number of distinguishable hues; cycling past
 * the end hands two different callers the same color, which is worse than not
 * naming the small ones at all. Keys and models are both unbounded, so they get
 * bounded here rather than in the palette.
 *
 * Color follows RANK WITHIN THE WINDOW, so changing the window can reassign
 * hues. That is a different query, not an interactive filter repainting
 * survivors — the case the fixed-order rule exists to prevent.
 */
export function foldSeries(
  entries: { label: string; values: number[] }[],
  max = 6,
): ChartSeries[] {
  const total = (v: number[]) => v.reduce((a, b) => a + b, 0)
  const ranked = [...entries].sort((a, b) => total(b.values) - total(a.values))
  const head = ranked.slice(0, max)
  const tail = ranked.slice(max)
  const out: ChartSeries[] = head.map((e, i) => ({
    key: e.label,
    color: seriesColor(i),
    values: e.values,
  }))
  if (tail.length > 0) {
    const n = entries[0]?.values.length ?? 0
    const summed = Array.from({ length: n }, (_, i) =>
      tail.reduce((s, e) => s + (e.values[i] || 0), 0),
    )
    out.push({ key: `${OTHER_LABEL} (${tail.length})`, color: OTHER_COLOR, values: summed })
  }
  return out
}

/**
 * Legend swatch + label. Identity is never carried by the mark color alone —
 * every series is named next to its swatch, and the label wears text ink, not
 * the series color.
 *
 * `collapsible` hides it behind a toggle for charts with many bands, where a
 * always-on legend outgrows the plot it explains. The hover tooltip names every
 * band at the cursor, so a collapsed legend never leaves identity color-only.
 */
export function Legend({
  series,
  dot,
  collapsible,
}: {
  series: ChartSeries[]
  dot?: boolean
  collapsible?: boolean
}) {
  const [open, setOpen] = useState(false)
  // One series needs no legend: the panel title already names it, and a swatch
  // beside a lone band explains nothing the title has not said.
  if (series.length < 2) return null
  if (collapsible && !open) {
    return (
      <Button
        size="small"
        onClick={() => setOpen(true)}
        sx={{ mt: 0.5, color: C.textMuted, textTransform: 'none', minWidth: 0, px: 0.5 }}
      >
        show {series.length} series
      </Button>
    )
  }
  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1.5, mt: 1.25, alignItems: 'center' }}>
      {series.map((s) => (
        <Box key={s.key} sx={{ display: 'flex', alignItems: 'center', gap: 0.75 }}>
          <Box
            sx={{
              width: 10,
              height: 10,
              bgcolor: s.color,
              borderRadius: dot ? '50%' : 0.3,
              flexShrink: 0,
            }}
          />
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            {s.key || '(unkeyed)'}
          </Typography>
        </Box>
      ))}
      {collapsible && (
        <Button
          size="small"
          onClick={() => setOpen(false)}
          sx={{ color: C.textFaint, textTransform: 'none', minWidth: 0, px: 0.5 }}
        >
          hide
        </Button>
      )}
    </Box>
  )
}

// Tooltip contents for one bucket: every non-zero band, largest first, plus the
// stack total. Bands at zero are omitted — a list of "0" rows buries the ones
// that matter.
function HoverCard({
  series,
  i,
  at,
  fmt,
  x,
}: {
  series: ChartSeries[]
  i: number
  at?: number
  fmt: (n: number) => string
  x: number
}) {
  const rows = series
    .map((s) => ({ key: s.key, color: s.color, v: s.values[i] || 0 }))
    .filter((r) => r.v > 0)
    .sort((a, b) => b.v - a.v)
  const total = rows.reduce((s, r) => s + r.v, 0)
  return (
    <Box
      sx={{
        position: 'absolute',
        top: 4,
        // Flip to the cursor's left past the midpoint so the card never runs off.
        left: x > 50 ? undefined : `calc(${x}% + 12px)`,
        right: x > 50 ? `calc(${100 - x}% + 12px)` : undefined,
        zIndex: 2,
        pointerEvents: 'none',
        bgcolor: C.raised,
        border: `1px solid ${C.border}`,
        borderRadius: 1,
        px: 1,
        py: 0.75,
        minWidth: 150,
        maxWidth: 260,
      }}
    >
      {at != null && (
        <Typography variant="caption" sx={{ color: C.textMuted, display: 'block', mb: 0.5 }}>
          {fmtTime(at)}
        </Typography>
      )}
      {rows.length === 0 ? (
        <Typography variant="caption" sx={{ color: C.textFaint }}>
          nothing in this bucket
        </Typography>
      ) : (
        rows.map((r) => (
          <Box
            key={r.key}
            sx={{ display: 'flex', alignItems: 'center', gap: 0.75, lineHeight: 1.5 }}
          >
            <Box sx={{ width: 8, height: 8, bgcolor: r.color, borderRadius: 0.3, flexShrink: 0 }} />
            <Typography
              variant="caption"
              sx={{ color: C.textMuted, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis' }}
              noWrap
            >
              {r.key || '(unkeyed)'}
            </Typography>
            <Typography variant="caption" sx={{ fontVariantNumeric: 'tabular-nums' }}>
              {fmt(r.v)}
            </Typography>
          </Box>
        ))
      )}
      {rows.length > 1 && (
        <Box
          sx={{
            display: 'flex',
            gap: 1,
            mt: 0.5,
            pt: 0.5,
            borderTop: `1px solid ${C.border}`,
            justifyContent: 'space-between',
          }}
        >
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            total
          </Typography>
          <Typography variant="caption" sx={{ fontVariantNumeric: 'tabular-nums' }}>
            {fmt(total)}
          </Typography>
        </Box>
      )}
    </Box>
  )
}

/**
 * StackedArea draws bands summed bottom-to-top, with a crosshair + tooltip on
 * hover.
 *
 * The hover layer is what makes a dense stack readable: bands below the top one
 * are read as thicknesses, which the eye cannot measure, so the exact values
 * have to be obtainable some other way. It is also what lets the legend collapse
 * without identity becoming color-only.
 */
export function StackedArea({
  title,
  series,
  fmtTotal,
  buckets,
  collapsibleLegend,
  actions,
}: {
  title: string
  series: ChartSeries[]
  fmtTotal: (n: number) => string
  buckets?: number[]
  collapsibleLegend?: boolean
  actions?: React.ReactNode
}) {
  const W = 600
  const H = 160
  const pad = 4
  const boxRef = useRef<HTMLDivElement | null>(null)
  const [hover, setHover] = useState<{ i: number; x: number } | null>(null)

  const n = series[0]?.values.length ?? 0
  if (n === 0) return null
  const totals = Array.from({ length: n }, (_, i) =>
    series.reduce((s, ser) => s + (ser.values[i] || 0), 0),
  )
  const max = Math.max(0, ...totals)
  const x = (i: number) => (n <= 1 ? pad : (i / (n - 1)) * (W - 2 * pad) + pad)
  const y = (v: number) => (max <= 0 ? H - pad : H - pad - (v / max) * (H - 2 * pad))

  const cum = new Array<number>(n).fill(0)
  const bands = series.map((ser) => {
    const bottom = cum.slice()
    const top = cum.map((c, i) => c + (ser.values[i] || 0))
    for (let i = 0; i < n; i++) cum[i] = top[i]
    const topPts = top.map((v, i) => `${x(i)},${y(v)}`)
    const pts = [...topPts, ...bottom.map((v, i) => `${x(i)},${y(v)}`).reverse()].join(' ')
    return { key: ser.key, color: ser.color, pts, top: topPts.join(' ') }
  })

  // The SVG is stretched (preserveAspectRatio none), so the bucket under the
  // cursor comes from the fraction across the CONTAINER, not from SVG units.
  const onMove = (e: React.MouseEvent) => {
    const el = boxRef.current
    if (!el || n === 0) return
    const r = el.getBoundingClientRect()
    if (r.width <= 0) return
    const frac = Math.min(1, Math.max(0, (e.clientX - r.left) / r.width))
    setHover({ i: Math.round(frac * (n - 1)), x: frac * 100 })
  }

  return (
    <Panel
      title={title}
      actions={
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          {actions}
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            peak {fmtTotal(max)}
          </Typography>
        </Box>
      }
      dense
    >
      <Box
        ref={boxRef}
        sx={{ position: 'relative' }}
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
      >
        <Box
          component="svg"
          viewBox={`0 0 ${W} ${H}`}
          preserveAspectRatio="none"
          sx={{ width: '100%', height: 180, display: 'block' }}
        >
          {/* Each band is stroked in the SURFACE color, not its own: a 2px gap
              between adjacent fills is what keeps two similar hues from reading as
              one shape where they touch. The top edge is then drawn in the series
              color so the band still has an identity line. */}
          {bands.map((b) => (
            <polygon
              key={b.key}
              points={b.pts}
              fill={b.color}
              fillOpacity={0.45}
              stroke={C.surface}
              strokeWidth={2}
              vectorEffect="non-scaling-stroke"
            />
          ))}
          {bands.map((b) => (
            <polyline
              key={`${b.key}-edge`}
              points={b.top}
              fill="none"
              stroke={b.color}
              strokeWidth={2}
              vectorEffect="non-scaling-stroke"
            />
          ))}
          {hover && (
            <line
              x1={x(hover.i)}
              x2={x(hover.i)}
              y1={pad}
              y2={H - pad}
              stroke={C.textMuted}
              strokeWidth={1}
              vectorEffect="non-scaling-stroke"
            />
          )}
        </Box>
        {hover && (
          <HoverCard
            series={series}
            i={hover.i}
            at={buckets?.[hover.i]}
            fmt={fmtTotal}
            x={hover.x}
          />
        )}
      </Box>
      <Legend series={series} collapsible={collapsibleLegend} />
    </Panel>
  )
}

// MetricChart draws one metric over time, one line per key (dependency-free SVG).
export function MetricChart({
  title,
  series,
  fmt,
}: {
  title: string
  series: ChartSeries[]
  fmt: (n: number) => string
}) {
  const W = 600
  const H = 120
  const pad = 4
  const n = series[0]?.values.length ?? 0
  const max = Math.max(0, ...series.flatMap((s) => s.values))
  const x = (i: number) => (n <= 1 ? pad : (i / (n - 1)) * (W - 2 * pad) + pad)
  const y = (v: number) => (max <= 0 ? H - pad : H - pad - (v / max) * (H - 2 * pad))
  return (
    <Box sx={{ flex: '1 1 380px', minWidth: 320 }}>
      <Panel
        title={title}
        actions={
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            peak {fmt(max)}
          </Typography>
        }
        dense
      >
        <Box
          component="svg"
          viewBox={`0 0 ${W} ${H}`}
          preserveAspectRatio="none"
          sx={{ width: '100%', height: 130, display: 'block' }}
        >
          {series.map((s) => (
            <polyline
              key={s.key}
              fill="none"
              stroke={s.color}
              strokeWidth={2}
              strokeLinejoin="round"
              vectorEffect="non-scaling-stroke"
              points={s.values.map((v, i) => `${x(i)},${y(v)}`).join(' ')}
            />
          ))}
        </Box>
        <Legend series={series} dot />
      </Panel>
    </Box>
  )
}
