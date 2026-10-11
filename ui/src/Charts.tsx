import { useEffect, useRef, useState } from 'react'
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

// --- Axis helpers ---------------------------------------------------------
//
// A line with no scale reads as a shape, not a measurement — hence "just look
// like lines". Ticks are picked from ROUND values, not raw min/max fractions:
// "1,240" and "3,720" are read at a glance, "1,187.33" and "3,561.99" are not.
// The plotted scale extends to the rounded ceiling (headroom above the data),
// so the top gridline is always a value worth reading, not the exact peak.
//
// Axis text is never drawn as SVG <text>: the plot is stretched to fill its
// box (preserveAspectRatio="none", same as the polylines above), which would
// warp glyphs non-uniformly with the container's aspect ratio. Labels are
// HTML overlays instead, positioned by fraction of the (fixed-height,
// fluid-width) box — the trick HoverCard already uses for the tooltip.

// niceTicks returns ascending round tick values from 0 to a ceiling >= max,
// stepping by 1/2/5 × 10^n. `count` is a target, not a guarantee — rounding
// can land one tick short or long of it.
function niceTicks(max: number, count: number): number[] {
  if (!(max > 0)) return [0]
  const rawStep = max / Math.max(1, count)
  const mag = 10 ** Math.floor(Math.log10(rawStep))
  const norm = rawStep / mag
  const step = (norm >= 5 ? 10 : norm >= 2 ? 5 : norm >= 1 ? 2 : 1) * mag
  const ceilMax = Math.ceil(max / step) * step
  const out: number[] = []
  for (let v = 0; v <= ceilMax + step / 1e6; v += step) out.push(v)
  return out
}

// Width-aware x-tick count. A narrow panel (mobile, a 3-up grid) gets fewer
// labels — colliding timestamps are worse than sparse ones.
function xTickCountFor(width: number): number {
  if (width <= 0) return 3
  if (width < 360) return 2
  if (width < 520) return 3
  if (width < 720) return 4
  return 5
}

// Evenly spaced bucket indices, always including both ends.
function pickTickIndices(n: number, count: number): number[] {
  if (n <= 1) return [0]
  const c = Math.max(2, Math.min(count, n))
  const seen = new Set<number>()
  for (let k = 0; k < c; k++) seen.add(Math.round((k / (c - 1)) * (n - 1)))
  return [...seen].sort((a, b) => a - b)
}

// Short axis label for a bucket timestamp — a clock time within roughly one
// day's span, a date once the range crosses it. The tooltip (fmtTime) still
// gives the full date+time on hover.
function fmtAxisTime(ms: number, spanMs: number): string {
  const d = new Date(ms)
  return spanMs > 36 * 3_600_000
    ? d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
    : d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
}

// Tracks an element's content width via ResizeObserver — the input to
// xTickCountFor, since the SVG is stretched to its container rather than
// sized by its own viewBox.
function useWidth(ref: React.RefObject<HTMLElement | null>): number {
  const [w, setW] = useState(0)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const update = () => setW(el.getBoundingClientRect().width)
    update()
    const ro = new ResizeObserver(update)
    ro.observe(el)
    return () => ro.disconnect()
  }, [ref])
  return w
}

// YAxisLabels renders round tick values as an HTML column left of the plot,
// high-to-low top-to-bottom. Ticks step evenly, so a plain space-between flex
// column lands each label at its gridline's height without per-label math.
function YAxisLabels({
  ticks,
  fmt,
  height,
}: {
  ticks: number[]
  fmt: (n: number) => string
  height: number
}) {
  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'column',
        justifyContent: ticks.length > 1 ? 'space-between' : 'flex-end',
        height,
        flexShrink: 0,
        pr: 0.75,
      }}
    >
      {[...ticks].reverse().map((t) => (
        <Typography
          key={t}
          variant="caption"
          sx={{
            color: C.textFaint,
            fontSize: 10,
            lineHeight: 1,
            fontVariantNumeric: 'tabular-nums',
            whiteSpace: 'nowrap',
          }}
        >
          {fmt(t)}
        </Typography>
      ))}
    </Box>
  )
}

// XAxisLabels renders bucket timestamps under the plot at the same x
// fractions as the vertical gridlines. The first/last labels anchor to their
// own edge instead of centering, so they stay inside the panel instead of
// overhanging it.
function XAxisLabels({
  indices,
  buckets,
  spanMs,
  xPct,
}: {
  indices: number[]
  buckets: number[]
  spanMs: number
  xPct: (i: number) => number
}) {
  return (
    <Box sx={{ position: 'relative', height: 14, mt: 0.5 }}>
      {indices.map((i, k) => (
        <Typography
          key={i}
          variant="caption"
          sx={{
            position: 'absolute',
            left: `${xPct(i)}%`,
            transform:
              k === 0 ? 'none' : k === indices.length - 1 ? 'translateX(-100%)' : 'translateX(-50%)',
            color: C.textFaint,
            fontSize: 10,
            whiteSpace: 'nowrap',
          }}
        >
          {fmtAxisTime(buckets[i], spanMs)}
        </Typography>
      ))}
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

  const width = useWidth(boxRef)

  const n = series[0]?.values.length ?? 0
  if (n === 0) return null
  const totals = Array.from({ length: n }, (_, i) =>
    series.reduce((s, ser) => s + (ser.values[i] || 0), 0),
  )
  const max = Math.max(0, ...totals)
  // The y-scale climbs to the rounded ceiling, not the raw max, so the top
  // gridline is a round number and the data sits a little short of the edge.
  const yTicks = niceTicks(max, 4)
  const axisMax = yTicks[yTicks.length - 1] || 0
  const x = (i: number) => (n <= 1 ? pad : (i / (n - 1)) * (W - 2 * pad) + pad)
  const y = (v: number) => (axisMax <= 0 ? H - pad : H - pad - (v / axisMax) * (H - 2 * pad))
  const spanMs = buckets && buckets.length > 1 ? buckets[buckets.length - 1] - buckets[0] : 0
  const xIdx =
    buckets && buckets.length === n && n > 1 ? pickTickIndices(n, xTickCountFor(width)) : []

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
      <Box sx={{ display: 'flex', alignItems: 'stretch', gap: 0.75 }}>
        <YAxisLabels ticks={yTicks} fmt={fmtTotal} height={180} />
        <Box sx={{ flex: 1, minWidth: 0 }}>
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
              {/* Gridlines are drawn first (painted under the data) and stay in
                  the same recessive border color as everywhere else — a grid
                  that reads before the lines it measures has it backwards. */}
              {yTicks.map((t) => (
                <line
                  key={`grid-${t}`}
                  x1={pad}
                  x2={W - pad}
                  y1={y(t)}
                  y2={y(t)}
                  stroke={C.border}
                  strokeWidth={1}
                  vectorEffect="non-scaling-stroke"
                />
              ))}
              {xIdx.map((i) => (
                <line
                  key={`vgrid-${i}`}
                  x1={x(i)}
                  x2={x(i)}
                  y1={pad}
                  y2={H - pad}
                  stroke={C.border}
                  strokeWidth={1}
                  vectorEffect="non-scaling-stroke"
                />
              ))}
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
          {xIdx.length > 0 && buckets && (
            <XAxisLabels indices={xIdx} buckets={buckets} spanMs={spanMs} xPct={(i) => (x(i) / W) * 100} />
          )}
        </Box>
      </Box>
      <Legend series={series} collapsible={collapsibleLegend} />
    </Panel>
  )
}

// MetricChart draws one metric over time, one line per key (dependency-free SVG).
//
// `buckets` is optional and new: existing callers that only pass a value
// series keep getting a y-axis with no x labels, same as today's blank x-axis.
// A caller that also has the bucket timestamps (usage's queries already fetch
// them, see usage.tsx's usageSeries.buckets) can pass them through to get a
// real time axis instead of wiring its own.
export function MetricChart({
  title,
  series,
  fmt,
  buckets,
}: {
  title: string
  series: ChartSeries[]
  fmt: (n: number) => string
  buckets?: number[]
}) {
  const W = 600
  const H = 120
  const pad = 4
  const boxRef = useRef<HTMLDivElement | null>(null)
  const width = useWidth(boxRef)
  const n = series[0]?.values.length ?? 0
  const max = Math.max(0, ...series.flatMap((s) => s.values))
  const yTicks = niceTicks(max, 3)
  const axisMax = yTicks[yTicks.length - 1] || 0
  const x = (i: number) => (n <= 1 ? pad : (i / (n - 1)) * (W - 2 * pad) + pad)
  const y = (v: number) => (axisMax <= 0 ? H - pad : H - pad - (v / axisMax) * (H - 2 * pad))
  const spanMs = buckets && buckets.length > 1 ? buckets[buckets.length - 1] - buckets[0] : 0
  const xIdx =
    buckets && buckets.length === n && n > 1 ? pickTickIndices(n, xTickCountFor(width)) : []
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
        <Box sx={{ display: 'flex', alignItems: 'stretch', gap: 0.75 }}>
          <YAxisLabels ticks={yTicks} fmt={fmt} height={130} />
          <Box sx={{ flex: 1, minWidth: 0 }}>
            <Box ref={boxRef}>
              <Box
                component="svg"
                viewBox={`0 0 ${W} ${H}`}
                preserveAspectRatio="none"
                sx={{ width: '100%', height: 130, display: 'block' }}
              >
                {yTicks.map((t) => (
                  <line
                    key={`grid-${t}`}
                    x1={pad}
                    x2={W - pad}
                    y1={y(t)}
                    y2={y(t)}
                    stroke={C.border}
                    strokeWidth={1}
                    vectorEffect="non-scaling-stroke"
                  />
                ))}
                {xIdx.map((i) => (
                  <line
                    key={`vgrid-${i}`}
                    x1={x(i)}
                    x2={x(i)}
                    y1={pad}
                    y2={H - pad}
                    stroke={C.border}
                    strokeWidth={1}
                    vectorEffect="non-scaling-stroke"
                  />
                ))}
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
            </Box>
            {xIdx.length > 0 && buckets && (
              <XAxisLabels indices={xIdx} buckets={buckets} spanMs={spanMs} xPct={(i) => (x(i) / W) * 100} />
            )}
          </Box>
        </Box>
        <Legend series={series} dot />
      </Panel>
    </Box>
  )
}

// MacdChart draws a series that swings both sides of zero: a MACD line, its
// signal line, and their difference as bars (dependency-free SVG).
//
// MetricChart's axis starts at 0, so it cannot show this. The axis here is
// symmetric about zero so "above" and "below" read at the same scale, and the
// zero line is drawn stronger than the grid: crossing it is the event.
export function MacdChart({
  title,
  macd,
  signal,
  fmt,
  buckets,
  note,
}: {
  title: string
  macd: number[]
  signal: number[]
  fmt: (n: number) => string
  buckets?: number[]
  note?: string
}) {
  const W = 600
  const H = 120
  const pad = 4
  const boxRef = useRef<HTMLDivElement | null>(null)
  const width = useWidth(boxRef)
  const n = macd.length
  const hist = macd.map((v, i) => v - (signal[i] ?? 0))
  const mag = Math.max(0, ...macd.map(Math.abs), ...signal.map(Math.abs), ...hist.map(Math.abs))
  const half = niceTicks(mag, 2)
  const axisMax = half[half.length - 1] || 0
  const yTicks = [...half.slice(1).map((t) => -t).reverse(), ...half]
  const x = (i: number) => (n <= 1 ? pad : (i / (n - 1)) * (W - 2 * pad) + pad)
  const y = (v: number) => (axisMax <= 0 ? H / 2 : H / 2 - (v / axisMax) * (H / 2 - pad))
  const barW = n > 0 ? Math.max(1, ((W - 2 * pad) / n) * 0.7) : 1
  const spanMs = buckets && buckets.length > 1 ? buckets[buckets.length - 1] - buckets[0] : 0
  const xIdx =
    buckets && buckets.length === n && n > 1 ? pickTickIndices(n, xTickCountFor(width)) : []
  const line = (vs: number[]) => vs.map((v, i) => `${x(i)},${y(v)}`).join(' ')
  const series: ChartSeries[] = [
    { key: 'MACD (fast − slow)', color: seriesColor(0), values: macd },
    { key: 'signal', color: seriesColor(1), values: signal },
  ]
  const last = n > 0 ? macd[n - 1] : 0
  return (
    <Box sx={{ flex: '1 1 380px', minWidth: 320 }}>
      <Panel
        title={title}
        note={note}
        actions={
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            now {last >= 0 ? '+' : ''}
            {fmt(last)}
          </Typography>
        }
        dense
      >
        <Box sx={{ display: 'flex', alignItems: 'stretch', gap: 0.75 }}>
          <YAxisLabels ticks={yTicks} fmt={fmt} height={130} />
          <Box sx={{ flex: 1, minWidth: 0 }}>
            <Box ref={boxRef}>
              <Box
                component="svg"
                viewBox={`0 0 ${W} ${H}`}
                preserveAspectRatio="none"
                sx={{ width: '100%', height: 130, display: 'block' }}
              >
                {yTicks.map((t) => (
                  <line
                    key={`grid-${t}`}
                    x1={pad}
                    x2={W - pad}
                    y1={y(t)}
                    y2={y(t)}
                    stroke={t === 0 ? C.textFaint : C.border}
                    strokeWidth={1}
                    vectorEffect="non-scaling-stroke"
                  />
                ))}
                {xIdx.map((i) => (
                  <line
                    key={`vgrid-${i}`}
                    x1={x(i)}
                    x2={x(i)}
                    y1={pad}
                    y2={H - pad}
                    stroke={C.border}
                    strokeWidth={1}
                    vectorEffect="non-scaling-stroke"
                  />
                ))}
                {hist.map((v, i) => (
                  <rect
                    key={`h-${i}`}
                    x={x(i) - barW / 2}
                    width={barW}
                    y={Math.min(y(v), y(0))}
                    height={Math.abs(y(v) - y(0))}
                    fill={v >= 0 ? C.ok : C.error}
                    opacity={0.45}
                  />
                ))}
                {series.map((s) => (
                  <polyline
                    key={s.key}
                    fill="none"
                    stroke={s.color}
                    strokeWidth={2}
                    strokeLinejoin="round"
                    vectorEffect="non-scaling-stroke"
                    points={line(s.values)}
                  />
                ))}
              </Box>
            </Box>
            {xIdx.length > 0 && buckets && (
              <XAxisLabels indices={xIdx} buckets={buckets} spanMs={spanMs} xPct={(i) => (x(i) / W) * 100} />
            )}
          </Box>
        </Box>
        <Legend series={series} dot />
      </Panel>
    </Box>
  )
}
