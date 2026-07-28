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
//
// STRUCTURE: server → pools → readings. This nesting is the point.
//
// The flat version listed "VRAM", "System RAM", "gpu0" and "system" as four
// sibling rows, which read as four separate resources. They are two: gpu0 IS
// the VRAM and system IS the host RAM, each shown once accounted and once
// measured. Pairing the two readings under the pool they both describe is what
// makes the accounted-vs-measured gap legible instead of looking like a
// duplicate row — and the server grouping is what keeps it legible once a
// second box exists and there are two of everything.
//
// A unified-memory host (Apple silicon) has ONE pool that is both its device
// and its system memory. That is not a special case to paper over: its
// devicePool names that pool, both readings land on it, and the row says
// "device + system" rather than pretending there is a GPU pool hiding somewhere.

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

// Reading is ONE view of a pool — accounted or measured. Two of them stack
// under a single pool heading, indented, so the pair reads as one resource seen
// twice rather than two resources.
function Reading(props: { kind: string; hint: string; segments: MemSegment[]; total: number }) {
  const { kind, hint, segments, total } = props
  const used = segments.reduce((s, x) => s + x.bytes, 0)
  const pct = total > 0 ? Math.round((used / total) * 100) : 0
  return (
    <Box sx={{ mt: 0.75 }}>
      <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, mb: 0.5 }}>
        <Typography variant="caption" sx={{ minWidth: 78, color: C.textMuted }}>
          {kind}
        </Typography>
        <Typography variant="caption" sx={{ color: C.textFaint }}>
          {hint}
        </Typography>
        <Box sx={{ flexGrow: 1 }} />
        <Typography variant="caption" sx={{ fontVariantNumeric: 'tabular-nums' }}>
          {fmtBytes(used)} <span style={{ color: C.textFaint }}>/ {fmtBytes(total)}</span>
        </Typography>
        <Typography
          variant="caption"
          sx={{ minWidth: 38, textAlign: 'right', color: pct > 90 ? STATUS.warn : C.textMuted }}
        >
          {pct}%
        </Typography>
      </Box>
      <StackedBar segments={segments} total={total} />
      <LegendRow segments={segments} free={total - used} total={total} />
    </Box>
  )
}

// PoolRow is one pool of one server, with every reading available for it.
//
// The device NAME belongs here, not on the server header: it identifies the
// device this pool is, and a box with two cards has two differently-named
// devices under one server.
function PoolRow(props: { pool: string; role: string; device?: string; children: React.ReactNode }) {
  const { pool, role, device, children } = props
  return (
    <Row>
      <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1 }}>
        <Typography variant="subtitle2">{pool}</Typography>
        <Chip size="small" variant="outlined" label={role} />
        {device && (
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            {device}
          </Typography>
        )}
      </Box>
      <Box sx={{ pl: 1.5, borderLeft: `1px solid ${C.border}`, ml: 0.25 }}>{children}</Box>
    </Row>
  )
}

export type PoolLedger = { server: string; pool: string; budget: number; used: number; reserve: number }
export type ModelUse = { model: string; server: string; pools: { pool: string; bytes: number }[]; measuredBytes: number }
export type DeviceMem = { available: boolean; name: string; totalBytes: number; usedBytes: number; freeBytes: number }
export type ServerShape = { server: string; devicePool: string }

export function MemoryPanel(props: {
  pools: PoolLedger[]
  models: ModelUse[]
  servers: ServerShape[]
  gpu: DeviceMem
  host: DeviceMem
  colorOf: (model: string) => string
}) {
  const { pools, models, servers, gpu, host, colorOf } = props

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

  // The device probes read THIS machine. Attributing them to a server is only
  // honest while exactly one server is local, which is the case until a server
  // can be bound to a remote agent. With several, say nothing rather than pick
  // one — a measured bar under the wrong box is worse than no measured bar.
  const soleServer = servers.length === 1 ? servers[0] : undefined
  const devicePoolOf = (server: string) =>
    servers.find((s) => s.server === server)?.devicePool ?? 'gpu0'

  const byServer = new Map<string, (typeof accounted)[number][]>()
  for (const p of accounted) {
    byServer.set(p.server, [...(byServer.get(p.server) ?? []), p])
  }
  // Device pool first: it is the scarce one, and the one eviction fights over.
  const boxes = [...byServer.entries()]
    .map(([server, ps]) => {
      const dp = devicePoolOf(server)
      return {
        server,
        pools: [...ps].sort(
          (a, b) =>
            Number(b.pool === dp) - Number(a.pool === dp) || a.pool.localeCompare(b.pool),
        ),
      }
    })
    .sort((a, b) => a.server.localeCompare(b.server))

  const nothing = pools.length === 0 && !gpu.available && !host.available

  return (
    <Panel
      title="Memory"
      subtitle="Who is holding RAM and VRAM right now"
      // No device name here: it names ONE device, and this panel covers every
      // box. It sits on the pool row it describes. A missing probe is still
      // panel-scope, because then there is no device reading anywhere below.
      badge={!gpu.available && <Chip size="small" variant="outlined" label="no GPU probe" />}
      flush
    >
      {nothing ? (
        <Box sx={{ p: 2 }}>
          <Typography variant="body2" sx={{ color: C.textFaint }}>
            No pools declared and no device probe available.
          </Typography>
        </Box>
      ) : (
        boxes.map((box) => {
          const dp = devicePoolOf(box.server)
          const local = soleServer?.server === box.server
          return (
            <Box key={box.server}>
              <Box
                sx={{
                  px: 2,
                  py: 1,
                  display: 'flex',
                  alignItems: 'center',
                  gap: 1,
                  bgcolor: C.raised,
                  borderTop: `1px solid ${C.border}`,
                  borderBottom: `1px solid ${C.border}`,
                }}
              >
                <Typography variant="subtitle2">{box.server}</Typography>
              </Box>

              {box.pools.map((p) => {
                const isDevice = p.pool === dp
                // A unified-memory box's single pool IS both, and the label says
                // so rather than implying a GPU pool that does not exist.
                const isUnified = isDevice && box.pools.length === 1
                const role = isUnified ? 'device + system' : isDevice ? 'device' : 'system'
                return (
                  <PoolRow
                    key={`${box.server}/${p.pool}`}
                    pool={p.pool}
                    role={role}
                    device={local && isDevice && gpu.available ? gpu.name : undefined}
                  >
                    <Reading
                      kind="accounted"
                      hint={
                        p.reserve > 0
                          ? `scheduler ledger · ${fmtBytes(p.reserve)} held in reserve`
                          : 'scheduler ledger'
                      }
                      segments={p.segments}
                      total={p.budget}
                    />
                    {/* The measured reading of the SAME pool, nested under it —
                        this pairing is what stops it reading as a second pool. */}
                    {local && isDevice && gpu.available && (
                      <Reading
                        kind="measured"
                        hint="on device"

                        segments={gpuSegments}
                        total={gpu.totalBytes}
                      />
                    )}
                    {local && (!isDevice || isUnified) && p.pool === 'system' && host.available && (
                      <Reading
                        kind="measured"
                        hint="on host · MemAvailable"
                        segments={[
                          {
                            key: 'in use',
                            bytes: host.usedBytes,
                            color: NEUTRAL,
                            hint: 'all processes on the box, not just corrallm',
                          },
                        ]}
                        total={host.totalBytes}
                      />
                    )}
                  </PoolRow>
                )
              })}
            </Box>
          )
        })
      )}
    </Panel>
  )
}
