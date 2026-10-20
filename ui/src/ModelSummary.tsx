import { useState } from 'react'
import { Box, Stack, Tooltip, Typography } from '@mui/material'
import { useModelStats } from '@/ModelStats'
import { TimeWindowPicker, windowPhrase, type TimeWindow } from '@/TimeWindow'
import { C } from '@/theme'

/** What the summary needs of one placement: the launch, read from its cmd. */
export type LaunchFacts = {
  name: string
  server: string
  modelFile: string
  quant: string
  cacheTypeK: string
  cacheTypeV: string
  ctxSize: number
  llamaServer: boolean
}

// GraphQL Long fields arrive as strings.
const int = (n: number | string) => Math.round(Number(n)).toLocaleString()

/** "q8_0", or "f16 (default)" when llama-server was not told, or "—". */
function kv(t: string, llama: boolean): string {
  if (t) return t
  return llama ? 'f16 (default)' : '—'
}

function Fact({ label, value, hint }: { label: string; value: string; hint?: string }) {
  const body = (
    <Box sx={{ minWidth: 120 }}>
      <Typography variant="caption" sx={{ color: C.textMuted, display: 'block' }}>
        {label}
      </Typography>
      <Typography variant="body2" sx={{ fontFamily: 'monospace' }}>
        {value}
      </Typography>
    </Box>
  )
  return hint ? <Tooltip title={hint}>{body}</Tooltip> : body
}

/**
 * The model page's summary: how it is configured to run (weights quant, KV
 * cache types, context) and how it has actually run (requests, prefill and
 * decode speed, context per request) over a window.
 *
 * Configuration is read from the cmd, so it shows before the model ever loads.
 * The loaded context (nCtx) is what the backend actually got, which can be less
 * than configured. Every number carries its unit: a bare "3" has been misread.
 */
export function ModelSummary({
  name,
  placements,
  loadedCtx,
}: {
  name: string
  placements: readonly LaunchFacts[]
  loadedCtx?: number
}) {
  const [window, setWindow] = useState<TimeWindow>({ kind: 'relative', minutes: 1440 })
  const stats = useModelStats(window)
  const row = stats.data?.corrallm.usageRollup?.rows.find((r) => r.served === name)
  const many = placements.length > 1

  return (
    <Box sx={{ border: `1px solid ${C.border}`, borderRadius: 1, p: 1.5, mb: 2 }}>
      {placements.map((p) => (
        <Stack key={p.name} direction="row" spacing={3} sx={{ flexWrap: 'wrap', rowGap: 1, mb: 1 }}>
          {many && <Fact label="Placement" value={`${p.name} on ${p.server}`} />}
          <Fact
            label="Weights quant"
            value={p.quant || '—'}
            hint={p.modelFile ? `Read from ${p.modelFile}` : 'The cmd names no weights file'}
          />
          <Fact
            label="KV cache (K / V)"
            value={`${kv(p.cacheTypeK, p.llamaServer)} / ${kv(p.cacheTypeV, p.llamaServer)}`}
            hint="--cache-type-k / --cache-type-v from the cmd"
          />
          <Fact
            label="Context, configured"
            value={p.ctxSize ? `${int(p.ctxSize)} tokens` : 'model default'}
            hint="-c / --ctx-size from the cmd. Without it llama-server uses the model's trained context."
          />
          {!many && loadedCtx ? (
            <Fact
              label="Context, loaded"
              value={`${int(loadedCtx)} tokens`}
              hint="n_ctx the running backend reported at load"
            />
          ) : null}
        </Stack>
      ))}

      <Stack direction="row" spacing={1} sx={{ alignItems: 'center', mt: 1, mb: 0.5 }}>
        <Typography variant="caption" sx={{ color: C.textMuted }}>
          Served, {windowPhrase(window)}
        </Typography>
        <TimeWindowPicker value={window} onChange={setWindow} />
      </Stack>
      {!row ? (
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          {stats.isLoading ? 'Loading…' : 'No requests in this window.'}
        </Typography>
      ) : (
        <Stack direction="row" spacing={3} sx={{ flexWrap: 'wrap', rowGap: 1 }}>
          <Fact label="Requests" value={`${int(row.requests)} requests`} />
          <Fact
            label="Prefill, fused / raw"
            value={
              Number(row.prefillN) > 0
                ? `${int(row.prefillFusedTps)} / ${int(row.prefillRawTps)} tok/s`
                : '—'
            }
            hint={
              Number(row.prefillN) > 0
                ? `Fused counts the cached prefix; raw only the tokens processed. Token-weighted over ${int(row.prefillN)} requests, ${row.prefillSecAvg.toFixed(2)} s prefill per request. Raw by new tokens: ${row.prefillBuckets.map((b) => `${b.label} ${Number(b.n) > 0 ? int(b.rawTps) : '—'}`).join(', ')} tok/s.`
                : 'No request reported a prefill speed'
            }
          />
          <Fact
            label="Decode (tg), avg"
            value={Number(row.tgN) > 0 ? `${row.tgAvg.toFixed(1)} tok/s` : '—'}
            hint={
              Number(row.tgN) > 0
                ? `Per request over ${int(row.tgN)} requests. p10 (slow tail) ${row.tgP10.toFixed(1)} tok/s, p90 ${row.tgP90.toFixed(1)} tok/s, p99 ${row.tgP99.toFixed(1)} tok/s.`
                : 'No request reported a decode speed'
            }
          />
          <Fact
            label="Context per request (avg / p90 / p99)"
            value={
              Number(row.ctxRequests) > 0
                ? `${int(row.ctxRequestAvg)} / ${int(Number(row.ctxRequestP90))} / ${int(Number(row.ctxRequestP99))} tokens`
                : '—'
            }
            hint={`Prompt + completion tokens per served chat turn (this turn's thinking included), over ${int(Number(row.ctxRequests))} turns that named a conversation.`}
          />
        </Stack>
      )}
      {row && Number(row.ctxRequests) > 0 && (
        <Typography variant="caption" sx={{ color: C.textMuted, display: 'block', mt: 1, fontFamily: 'monospace' }}>
          avg {int(row.ctxRequestAvg)} tokens = prompt {int(row.ctxPromptAvg)} (cached {int(row.ctxCachedAvg)})
          {Number(row.ctxThinkingN) > 0 ? (
            <>
              {' '}+ thinking {int(row.ctxThinkingAvg)} + answer {int(row.ctxAnswerAvg)}
              {Number(row.ctxThinkingN) < Number(row.ctxRequests) &&
                ` (thinking measured on ${int(row.ctxThinkingN)} of ${int(row.ctxRequests)} turns)`}
            </>
          ) : (
            ' + completion (thinking not measured in this window)'
          )}
        </Typography>
      )}
    </Box>
  )
}
