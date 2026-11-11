import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Box,
  Chip,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { Panel, Stat } from '@/Panel'
import { C, seriesColor } from '@/theme'
import { fmtInt, fmtTime } from '@/format'
import { TimeWindowPicker, windowKey, windowPhrase, windowVars, type TimeWindow } from '@/TimeWindow'

const CacheReportDoc = graphql(/* GraphQL */ `
  query CacheReport($served: String!, $minutes: Long, $from: Long, $to: Long) {
    corrallm {
      cacheReport(served: $served, minutes: $minutes, from: $from, to: $to) {
        totals {
          requests
          misses
          promptTokens
          cachedTokens
          tokenHitPct
          lostTokens
          rePrefillSec
          missesPerHour
          windowHours
        }
        classes {
          class
          misses
          lostTokens
          lostShare
          seconds
        }
        gaps {
          bucket
          class
          misses
        }
        reloads {
          id
          ts
          kind
          reason
          instance
          ok
        }
        settings {
          cacheRamMiB
          ctxCheckpoints
          parallel
          slotSimilarity
          memAvailableMiB
          cacheEntries
          cacheBytes
          evictionsPerHour
          idSlotCoverage
        }
        recommendations {
          id
          severity
          title
          evidence
          action
        }
      }
    }
  }
`)

const GAP_BUCKETS = ['first', '<1m', '1-5m', '5-15m', '15-60m', '1-6h', '>6h']

// The miss classes, in classifier order: their colors stay put between windows.
const MISS_CLASSES = [
  'reload',
  'no-history',
  'prefix-diverged',
  'slot-switch',
  'checkpoint-limited',
  'evicted',
  'unknown',
]

const CLASS_HELP: Record<string, string> = {
  reload: 'The backend restarted between the turns (crash, build swap, idle unload, eviction for another model).',
  'no-history': 'The previous turn was seen before a corrallm restart, or had no body: nothing to compare.',
  'prefix-diverged': 'The cache reused everything that still matched: the caller changed an earlier part of the prompt.',
  'slot-switch': 'The turn went to a different slot than the one holding its prefix.',
  'checkpoint-limited': 'Reuse stopped at an earlier checkpoint (hybrid model, --ctx-checkpoints).',
  evicted: 'The prompt-cache entry was pushed out by other traffic.',
  unknown: 'None of the above could be shown.',
}

const flag = (v: number | string, unit = '') => (Number(v) < 0 ? 'not set' : `${fmtInt(v)}${unit}`)

/**
 * WHY THE PROMPT CACHE MISSED (P75), for one model over the page's window.
 *
 * Every number carries its unit. "Lost tokens" are prompt tokens a cache that
 * kept the previous turn would have saved, and were re-prefilled instead.
 * Recommendations are text: the exact flag change and the evidence for it.
 * Nothing on this panel changes a setting.
 */
export function CachePanel({ name }: { name: string }) {
  const [window, setWindow] = useState<TimeWindow>({ kind: 'relative', minutes: 1440 })
  const vars = windowVars(window)
  const q = useQuery({
    queryKey: ['cacheReport', name, windowKey(window)],
    queryFn: () =>
      gqlClient.request(CacheReportDoc, { served: name, minutes: vars.minutes, from: vars.from, to: vars.to }),
    refetchInterval: window.kind === 'absolute' ? false : 60000,
  })
  const r = q.data?.corrallm.cacheReport
  const t = r?.totals
  const misses = (r?.classes ?? []).filter((c) => MISS_CLASSES.includes(c.class))

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
        <Typography variant="caption" sx={{ color: C.textMuted }}>
          Prompt cache, {windowPhrase(window)}
        </Typography>
        <TimeWindowPicker value={window} onChange={setWindow} />
      </Stack>

      {q.isError && (
        <Typography variant="body2" sx={{ color: C.error }}>
          Could not load the cache report.
        </Typography>
      )}
      {!r || !t ? (
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          {q.isLoading ? 'Loading…' : 'No report.'}
        </Typography>
      ) : Number(t.requests) === 0 ? (
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          No attributed chat turns in this window. Only local backends are attributed.
        </Typography>
      ) : (
        <>
          <Panel title="Prompt cache" help="Per-turn attribution of prompt-cache misses. Text only: nothing here changes a flag.">
            <Stack direction="row" spacing={4} sx={{ flexWrap: 'wrap', rowGap: 1 }}>
              <Stat label="Token hit" value={`${t.tokenHitPct.toFixed(1)}% of prompt tokens`} />
              <Stat label="Lost" value={`${fmtInt(t.lostTokens)} tokens`} title="Re-prefilled by misses" />
              <Stat label="Re-prefill" value={`${fmtInt(Math.round(t.rePrefillSec))} s`} title="Lost tokens ÷ each turn's prefill speed" />
              <Stat label="Misses" value={`${t.missesPerHour.toFixed(1)} per hour`} title={`${fmtInt(t.misses)} misses in ${fmtInt(t.requests)} turns`} />
            </Stack>

            {misses.length > 0 && (
              <Box sx={{ mt: 2 }}>
                <Typography variant="caption" sx={{ color: C.textMuted }}>
                  Share of lost tokens, by class
                </Typography>
                <Box sx={{ display: 'flex', height: 14, borderRadius: 1, overflow: 'hidden', mt: 0.5 }}>
                  {misses.map((c) => (
                    <Tooltip
                      key={c.class}
                      title={`${c.class}: ${c.lostShare.toFixed(1)}% of lost tokens, ${fmtInt(c.misses)} misses, ${fmtInt(c.lostTokens)} tokens`}
                    >
                      <Box
                        sx={{
                          width: `${c.lostShare}%`,
                          bgcolor: seriesColor(MISS_CLASSES.indexOf(c.class)),
                        }}
                      />
                    </Tooltip>
                  ))}
                </Box>
                <Stack direction="row" spacing={1.5} sx={{ flexWrap: 'wrap', rowGap: 0.5, mt: 0.75 }}>
                  {misses.map((c) => (
                    <Tooltip key={c.class} title={CLASS_HELP[c.class] ?? ''}>
                      <Stack direction="row" spacing={0.5} sx={{ alignItems: 'center' }}>
                        <Box sx={{ width: 10, height: 10, borderRadius: 0.5, bgcolor: seriesColor(MISS_CLASSES.indexOf(c.class)) }} />
                        <Typography variant="caption">
                          {c.class} {c.lostShare.toFixed(1)}%
                        </Typography>
                      </Stack>
                    </Tooltip>
                  ))}
                </Stack>
              </Box>
            )}
          </Panel>

          <Panel title="Misses by gap since the previous turn" subtitle="count of missed turns" flush>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Class</TableCell>
                  {GAP_BUCKETS.map((b) => (
                    <TableCell key={b} align="right">
                      {b === 'first' ? 'no prev.' : b}
                    </TableCell>
                  ))}
                  <TableCell align="right">Lost tokens</TableCell>
                  <TableCell align="right">Re-prefill</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {misses.map((c) => (
                  <TableRow key={c.class}>
                    <TableCell>
                      <Tooltip title={CLASS_HELP[c.class] ?? ''}>
                        <span>{c.class}</span>
                      </Tooltip>
                    </TableCell>
                    {GAP_BUCKETS.map((b) => {
                      const n = r.gaps.find((g) => g.bucket === b && g.class === c.class)?.misses ?? 0
                      return (
                        <TableCell key={b} align="right" sx={{ color: Number(n) ? C.text : C.textFaint }}>
                          {fmtInt(n)}
                        </TableCell>
                      )
                    })}
                    <TableCell align="right">{fmtInt(c.lostTokens)} tok</TableCell>
                    <TableCell align="right">{fmtInt(Math.round(c.seconds))} s</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Panel>

          <Panel title="Recommendations" subtitle="text only — nothing is applied">
            <Stack spacing={1.5}>
              {r.recommendations.map((rec) => (
                <Box key={rec.id} sx={{ borderLeft: `3px solid ${rec.severity === 'warn' ? C.warn : C.accent}`, pl: 1.5 }}>
                  <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
                    <Chip size="small" label={rec.id} />
                    <Typography variant="body2" sx={{ fontWeight: 600 }}>
                      {rec.title}
                    </Typography>
                  </Stack>
                  <Typography variant="caption" sx={{ display: 'block', color: C.textMuted, mt: 0.5 }}>
                    Evidence: {rec.evidence}
                  </Typography>
                  <Typography variant="body2" sx={{ fontFamily: 'monospace', mt: 0.5 }}>
                    {rec.action}
                  </Typography>
                </Box>
              ))}
            </Stack>
            <Typography variant="caption" sx={{ display: 'block', color: C.textFaint, mt: 1.5 }}>
              Settings read from the cmd: --cache-ram {flag(r.settings.cacheRamMiB, ' MiB')} · --ctx-checkpoints{' '}
              {flag(r.settings.ctxCheckpoints)} · --parallel {flag(r.settings.parallel)} · MemAvailable{' '}
              {Number(r.settings.memAvailableMiB) < 0 ? 'not measured' : `${(Number(r.settings.memAvailableMiB) / 1024).toFixed(1)} GiB`} ·
              id_slot on {r.settings.idSlotCoverage.toFixed(0)}% of turns · cache entries{' '}
              {Number(r.settings.cacheEntries) < 0 ? 'not exposed (no 0005)' : fmtInt(r.settings.cacheEntries)}
            </Typography>
          </Panel>

          <Panel title="Backend starts and stops" subtitle={`${r.reloads.length} in this window`} flush>
            {r.reloads.length === 0 ? (
              <Typography variant="body2" sx={{ color: C.textMuted, p: 1.5 }}>
                None.
              </Typography>
            ) : (
              <Table size="small">
                <TableHead>
                  <TableRow>
                    <TableCell>When</TableCell>
                    <TableCell>Event</TableCell>
                    <TableCell>Reason</TableCell>
                    <TableCell>Instance</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {[...r.reloads].reverse().map((e) => (
                    <TableRow key={String(e.id)}>
                      <TableCell>{fmtTime(e.ts)}</TableCell>
                      <TableCell sx={{ color: e.ok ? C.text : C.error }}>{e.kind}</TableCell>
                      <TableCell>{e.reason}</TableCell>
                      <TableCell sx={{ fontFamily: 'monospace', color: C.textMuted }}>{e.instance}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </Panel>
        </>
      )}
    </Stack>
  )
}
