import { Box, Chip, Tooltip, Typography } from '@mui/material'
import { Panel, Row } from '@/Panel'
import { C, STATUS } from '@/theme'
import { fmtBytes } from '@/format'

// Memory: what is holding the box's RAM and VRAM right now, and WHO.
//
// Two different truths are shown side by side, deliberately:
//
//   accounted — the scheduler's ledger. Every resident backend reserves its
//               declared `ramUsage` from a pool's budget; this is the number
//               admission and eviction decisions are actually made on.
//   measured  — what the device reports (nvidia-smi / /proc/meminfo). This is
//               what is REALLY resident, including processes corrallm does not
//               own.
//
// They are not the same number and should not be averaged into one bar. A
// backend that outgrew its declared ramUsage, or a stray process squatting on
// the GPU, shows up exactly as a gap between the two — the most useful thing
// this panel can tell you, and it would be erased by picking one.
//
// Form: a stacked horizontal bar per pool/device. The question is "what fills
// this bounded total", which is a composition of a known whole — not a trend
// (no time axis here) and not a comparison of independent magnitudes.

// Non-model segments (other processes, host usage, mid-load reservations) wear a
// neutral grey, never a categorical hue: they are not one of the models, and
// giving them a series color would assert an identity that does not exist. It
// still has to clear the empty track by a wide margin — a segment the eye reads
// as "free" is worse than no segment.
const NEUTRAL = C.textFaint

export type MemSegment = { key: string; bytes: number; color: string; hint?: string }

// A stacked bar over a known total. Segments are separated by 2px of the track
// showing through, so two adjacent hues never read as one block; the ends are
// rounded and anchored to the track's ends.
function StackedBar({ segments, total }: { segments: MemSegment[]; total: number }) {
  const used = segments.reduce((s, x) => s + x.bytes, 0)
  const over = total > 0 && used > total
  return (
    <Box
      sx={{
        display: 'flex',
        gap: '2px',
        height: 14,
        borderRadius: '4px',
        bgcolor: C.raised,
        border: `1px solid ${C.border}`,
        overflow: 'hidden',
      }}
    >
      {segments.map((s) => {
        const pct = total > 0 ? Math.max(0, (s.bytes / total) * 100) : 0
        if (pct <= 0) return null
        return (
          <Tooltip key={s.key} title={`${s.key} · ${fmtBytes(s.bytes)}${s.hint ? ` · ${s.hint}` : ''}`}>
            <Box sx={{ width: `${pct}%`, bgcolor: s.color, minWidth: 2 }} />
          </Tooltip>
        )
      })}
      {/* Over-budget is a STATE, not another series: it takes the reserved
          error color and says so in the legend rather than just running long. */}
      {over && <Box sx={{ flex: 1, bgcolor: STATUS.error }} />}
    </Box>
  )
}

function LegendRow({ segments, free, total }: { segments: MemSegment[]; free: number; total: number }) {
  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1.5, mt: 0.75 }}>
      {segments.map((s) => (
        <Box key={s.key} sx={{ display: 'flex', alignItems: 'center', gap: 0.6 }}>
          <Box sx={{ width: 9, height: 9, borderRadius: 0.3, bgcolor: s.color, flexShrink: 0 }} />
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            {s.key} <span style={{ color: C.text }}>{fmtBytes(s.bytes)}</span>
          </Typography>
        </Box>
      ))}
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.6 }}>
        <Box
          sx={{ width: 9, height: 9, borderRadius: 0.3, bgcolor: C.raised, border: `1px solid ${C.borderStrong}`, flexShrink: 0 }}
        />
        <Typography variant="caption" sx={{ color: C.textFaint }}>
          free {fmtBytes(Math.max(0, free))} of {fmtBytes(total)}
        </Typography>
      </Box>
    </Box>
  )
}

function MemRow(props: {
  label: string
  sublabel?: string
  segments: MemSegment[]
  total: number
  badge?: React.ReactNode
}) {
  const { label, sublabel, segments, total, badge } = props
  const used = segments.reduce((s, x) => s + x.bytes, 0)
  const pct = total > 0 ? Math.round((used / total) * 100) : 0
  return (
    <Row>
      <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, mb: 0.75 }}>
        <Typography variant="subtitle2" sx={{ minWidth: 110 }}>
          {label}
        </Typography>
        {sublabel && (
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            {sublabel}
          </Typography>
        )}
        {badge}
        <Box sx={{ flexGrow: 1 }} />
        <Typography variant="body2" sx={{ fontVariantNumeric: 'tabular-nums' }}>
          {fmtBytes(used)} <span style={{ color: C.textFaint }}>/ {fmtBytes(total)}</span>
        </Typography>
        <Typography
          variant="body2"
          sx={{ minWidth: 42, textAlign: 'right', color: pct > 90 ? STATUS.warn : C.textMuted }}
        >
          {pct}%
        </Typography>
      </Box>
      <StackedBar segments={segments} total={total} />
      <LegendRow segments={segments} free={total - used} total={total} />
    </Row>
  )
}

export type PoolLedger = { server: string; pool: string; budget: number; used: number; reserve: number }
export type ModelUse = { model: string; server: string; pools: { pool: string; bytes: number }[]; measuredBytes: number }
export type DeviceMem = { available: boolean; name: string; totalBytes: number; usedBytes: number; freeBytes: number }

export function MemoryPanel(props: {
  pools: PoolLedger[]
  models: ModelUse[]
  gpu: DeviceMem
  host: DeviceMem
  colorOf: (model: string) => string
}) {
  const { pools, models, gpu, host, colorOf } = props

  // Accounted: one bar per (server, pool), segmented by the models holding it.
  const accounted = pools.map((p) => {
    const segments: MemSegment[] = models
      .filter((m) => m.server === p.server)
      .map((m) => ({
        key: m.model,
        bytes: m.pools.find((x) => x.pool === p.pool)?.bytes ?? 0,
        color: colorOf(m.model),
        hint: 'declared ramUsage',
      }))
      .filter((s) => s.bytes > 0)
      .sort((a, b) => b.bytes - a.bytes)
    // The ledger's own `used` can exceed what the per-model rows explain (a
    // reservation held mid-load). Surface the difference rather than letting the
    // bar silently under-report what the scheduler thinks is spoken for.
    const explained = segments.reduce((s, x) => s + x.bytes, 0)
    if (p.used > explained) {
      segments.push({ key: 'reserving', bytes: p.used - explained, color: NEUTRAL, hint: 'held mid-load' })
    }
    return { ...p, segments }
  })

  // Measured GPU: attribute the device's own number to models by their measured
  // footprint, and label the remainder honestly — it is not corrallm's.
  const gpuSegments: MemSegment[] = models
    .filter((m) => m.measuredBytes > 0)
    .map((m) => ({ key: m.model, bytes: m.measuredBytes, color: colorOf(m.model), hint: 'measured process VRAM' }))
    .sort((a, b) => b.bytes - a.bytes)
  const attributed = gpuSegments.reduce((s, x) => s + x.bytes, 0)
  const otherVram = Math.max(0, gpu.usedBytes - attributed)
  if (gpu.available && otherVram > 0) {
    gpuSegments.push({
      key: 'other processes',
      bytes: otherVram,
      color: NEUTRAL,
      hint: 'on the GPU but not spawned by corrallm',
    })
  }

  const nothing = pools.length === 0 && !gpu.available && !host.available

  return (
    <Panel
      title="Memory"
      subtitle="Who is holding RAM and VRAM right now"
      badge={
        gpu.available ? (
          <Chip size="small" variant="outlined" label={gpu.name} />
        ) : (
          <Chip size="small" variant="outlined" label="no GPU probe" />
        )
      }
      flush
    >
      {nothing ? (
        <Box sx={{ p: 2 }}>
          <Typography variant="body2" sx={{ color: C.textFaint }}>
            No pools declared and no device probe available.
          </Typography>
        </Box>
      ) : (
        <>
          {/* Measured first: it is the ground truth about the box. */}
          {gpu.available && (
            <MemRow
              label="VRAM"
              sublabel="measured on device"
              segments={gpuSegments}
              total={gpu.totalBytes}
            />
          )}
          {host.available && (
            <MemRow
              label="System RAM"
              sublabel="measured on host (MemAvailable)"
              segments={[
                { key: 'in use', bytes: host.usedBytes, color: NEUTRAL, hint: 'all processes on the box' },
              ]}
              total={host.totalBytes}
            />
          )}
          {/* Then the ledger the scheduler actually decides on. */}
          {accounted.map((p) => (
            <MemRow
              key={`${p.server}/${p.pool}`}
              label={p.pool}
              sublabel={
                p.reserve > 0
                  ? `${p.server} · accounted · ${fmtBytes(p.reserve)} held in reserve`
                  : `${p.server} · accounted`
              }
              segments={p.segments}
              total={p.budget}
            />
          ))}
        </>
      )}
    </Panel>
  )
}
