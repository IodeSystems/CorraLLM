import { useState } from 'react'
import {
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { Panel, Row } from '@/Panel'
import { NotYet } from '@/NotYet'
import { C } from '@/theme'
import { fmtInt, fmtKwh, fmtMoney } from '@/format'

/**
 * WHAT RUNNING IT HERE IS WORTH — and what that comparison does not cover.
 *
 * The owner: "if like a comparison cost". The box bills electricity; the
 * alternative bills tokens. Both numbers exist, nothing put them beside each
 * other, and an operator paying for a 5090 had no figure for what it earns.
 *
 * THREE THINGS THIS PANEL REFUSES TO DO, each because the number would stop
 * being checkable:
 *
 *   It does not hide the baseline. "Would have cost $27" is meaningless without
 *   "on what" — an opus figure and a haiku figure differ threefold over the same
 *   traffic. The type is in the header, not a tooltip (OB-10).
 *
 *   It does not sum everything it was handed. Audio bills per MiB and images
 *   bill per image; neither has a token term, so neither can be priced against a
 *   per-token rate table. Traffic already on a paid provider is excluded too:
 *   "what it would have cost on a paid API" is not a question about traffic that
 *   was on one. What is left out is stated (OB-4 — a figure without its window
 *   is not a figure, and scope is a window), with the request count, so a reader
 *   can see how much of the hour the claim speaks for.
 *
 *   It does NOT exclude free-tier traffic, and that is a change from the first
 *   version. A declared free tier's cost is $0 — not a missing coefficient, the
 *   actual price — so unlike an unattributed off-box row there is nothing junk to
 *   compare against, and it is the traffic the question is most worth asking
 *   about: real work, billed to nobody. It is counted in its own figure rather
 *   than folded in, because a return earned by this box's hardware and one earned
 *   by a provider's generosity are different claims about the machine.
 *
 *   It does not present the saving as the whole story. The multiple is the
 *   headline because that is what somebody arrives asking; the three classes are
 *   under it because that is where the answer is checkable — and because cache
 *   reads are the class that only exists on the paid side of the comparison.
 *   Locally a cached prefix is free (KV cache, no recompute); on a paid API it is
 *   billed at a reduced but real rate, and on this box's traffic that is a
 *   double-digit share of the difference.
 */

/** One rollup row, as much of it as the comparison reads. */
export type ComparisonRow = {
  served: string
  /** Which kind of money — the comparison covers two of them, and says which. */
  billing: string
  requests: number
  cachedTokens: number
  promptTokens: number
  completionTokens: number
  energyUsd: number
  baselineUsd: number
  baselineCachedUsd: number
  baselineProcessedUsd: number
  baselineGeneratedUsd: number
  baselineExcluded: string
  /** The paid type THIS row was priced at — its model's costEquivalent, else the default. */
  baselineType: string
  /** Heat the row's local work gave off, BTU (metered token energy x 3412.14). */
  energyBtu: number
  /** energyBtu over the window's length, BTU/h. */
  heatBtuPerHour: number
  /** What that heat did to the room's climate bill: + cooling cost, − heating saved. */
  thermalUsd: number
}

/** Measured draw for the window (usageRollup.power), as much as the heat figure reads. */
export type PowerHeat = {
  measured: boolean
  error: string
  energyBtu: number
  heatBtuPerHour: number
  thermalUsd: number
  pools: readonly { avgW: number }[]
  boxes: readonly { avgW: number }[]
}

/** The room settings the climate figure was computed from (config thermal). */
export type ThermalSetting = {
  mode: string
  roomF: number
  outdoorF: number
  coolTargetF: number
  heatTargetF: number
  coolBlocked: boolean
  coolFactorNow: number
  coolFanCfm: number
  outdoorSource: string
  outdoorNowF: number
  outdoorMinF: number
  outdoorMaxF: number
  // Long on the wire (Go int), which arrives as a string.
  hoursPriced: number | string
  hoursBlocked: number | string
  hoursNoWeather: number | string
  roomSource: string
  roomMinF: number
  roomMaxF: number
  hoursCooling: number | string
  hoursHeating: number | string
  hoursNeutral: number | string
}

/** The reasons the server can give, in the words a reader gets. */
const EXCLUSION_WORDS: Record<string, string> = {
  audio: 'speech, billed by size rather than by token',
  image: 'image generation, billed per picture — and unpriced on this box, so it already reads $0',
  paid: 'already served by a paid provider',
  'off-box':
    'served by a machine we do not run, with nothing in the config saying what it charges — so their local figure is not one',
  'unknown-model': 'served by a model that is no longer in the config',
  'no-tokens': 'turned away, refused or failed before producing a token, so there is nothing to price',
}

export function PaidComparison({
  when,
  baselineType,
  costPerKwh,
  thermal = null,
  power = null,
  rows,
  pending = false,
}: {
  /** The window, in words — said once, on the panel (OB-4). */
  when: string
  /** The commandCosts type the server priced against. Empty ⇒ no comparison. */
  baselineType: string
  /** $/kWh the electricity figure was computed at. 0 ⇒ no rate configured. */
  costPerKwh: number
  /** Room settings; null when none are configured (heat stays in BTU). */
  thermal?: ThermalSetting | null
  /** Measured draw; when measured, heat is the whole box at the wall, not token work. */
  power?: PowerHeat | null
  rows: ComparisonRow[]
  /** Nothing has been counted yet — distinct from a counted zero (NotYet.tsx). */
  pending?: boolean
}) {
  // The per-model breakdown lives in a dialog: the panel states the sum, the
  // dialog says which model contributed what at which paid model's rates.
  const [breakdownOpen, setBreakdownOpen] = useState(false)
  const covered = rows.filter((r) => !r.baselineExcluded)
  const excluded = rows.filter((r) => r.baselineExcluded && r.requests > 0)
  const sum = (f: (r: ComparisonRow) => number) => covered.reduce((a, r) => a + f(r), 0)

  const wouldCost = sum((r) => r.baselineUsd)
  const cachedUsd = sum((r) => r.baselineCachedUsd)
  const processedUsd = sum((r) => r.baselineProcessedUsd)
  const generatedUsd = sum((r) => r.baselineGeneratedUsd)
  const actual = sum((r) => r.energyUsd)
  const coveredReqs = sum((r) => r.requests)
  const excludedReqs = excluded.reduce((a, r) => a + r.requests, 0)
  const kwh = costPerKwh > 0 ? actual / costPerKwh : 0

  // FREE-TIER TRAFFIC IS COVERED, AND NAMED. It is the traffic this question is
  // most worth asking about — real work, billed to nobody — and it is also the
  // one part of "here" that is not electricity, so folding it silently into a
  // figure labelled "in electricity" would make the label wrong. Counted
  // separately so the composition of both numbers stays visible.
  const freeRows = covered.filter((r) => r.billing === 'free')
  const freeCost = freeRows.reduce((a, r) => a + r.baselineUsd, 0)
  const freeReqs = freeRows.reduce((a, r) => a + r.requests, 0)
  const localReqs = coveredReqs - freeReqs

  // Group the excluded by reason so the sentence names kinds, not thirty model
  // names. A reason the server sends that this UI has no words for is passed
  // through verbatim rather than dropped — a silently missing exclusion is the
  // failure this whole panel exists to avoid.
  const byReason = new Map<string, number>()
  for (const r of excluded) {
    byReason.set(r.baselineExcluded, (byReason.get(r.baselineExcluded) ?? 0) + r.requests)
  }
  const reasons = [...byReason.entries()].sort((a, b) => b[1] - a[1])
  // One reason needs no count of its own — the total was just stated, and
  // "103 more are NOT in these figures: 103 served by …" reads as two numbers.
  const reasonList =
    reasons.length === 1
      ? [EXCLUSION_WORDS[reasons[0][0]] ?? reasons[0][0]]
      : reasons.map(([why, n]) => `${fmtInt(n)} ${EXCLUSION_WORDS[why] ?? why}`)

  if (!baselineType) {
    return (
      <Panel title="What it would have cost on a paid API" dense>
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          No paid backend type is configured, so there is no rate table to compare against.
          Declaring one under <code>commandCosts</code> — a <code>cachedCostFactor</code>,{' '}
          <code>processCostFactor</code> and <code>generateCostFactor</code> in $/token — is what
          this reads.
        </Typography>
      </Panel>
    )
  }

  const multiple = actual > 0 ? wouldCost / actual : 0

  // WHOSE RATES. A model can declare the paid model it stands in for
  // (costEquivalent), so one panel can price rows at different tables. Name the
  // one when there is one; name all of them when there are several, rather
  // than calling a mixed sum "haiku rates".
  // PER MODEL, AT ITS OWN EQUIVALENT. Each local model is priced as the paid
  // model it stands in for (costEquivalent), so the headline is a sum of
  // per-model figures. The panel says "paid-API rates" — true however many
  // models there are — and the Breakdown dialog says which model at which rates.
  const perModel = covered.filter((r) => r.requests > 0).sort((a, b) => b.baselineUsd - a.baselineUsd)
  // MEASURED, when config power reads the wall: everything the box drew ends
  // up in the room, idle and CPU included. Otherwise the token-work estimate.
  const wall = power?.measured ? power : null
  const btu = wall ? Number(wall.energyBtu) : sum((r) => r.energyBtu)
  const btuPerHour = wall ? Number(wall.heatBtuPerHour) : sum((r) => r.heatBtuPerHour)
  const climateUsd = wall ? Number(wall.thermalUsd) : sum((r) => r.thermalUsd)
  const gpuW = wall ? wall.pools.reduce((n, p) => n + Number(p.avgW), 0) : 0
  // Hour by hour when a location is set: the outdoor range and how many hours
  // the fan could not cool say more than one temperature can.
  const hourly = thermal?.outdoorSource === 'open-meteo'
  const hourlyNote = hourly
    ? `; outdoor ${Math.round(thermal!.outdoorMinF)}–${Math.round(thermal!.outdoorMaxF)}°F hourly, ${Number(thermal!.hoursPriced)} h priced` +
      (Number(thermal!.hoursBlocked) > 0 ? `, ${Number(thermal!.hoursBlocked)} h too warm outside for the fan` : '') +
      (Number(thermal!.hoursNoWeather) > 0 ? `, ${Number(thermal!.hoursNoWeather)} h without weather` : '')
    : ''
  // ROOM HOUR BY HOUR (a house sensor): a window can hold cooling, heating and
  // neutral hours at once, so the figure is the NET of all of them, and the
  // sub-line says how the hours split.
  const roomHourly = thermal?.roomSource === 'home-assistant'
  const climate = !thermal
    ? null
    : roomHourly
      ? {
          value:
            climateUsd > 0 ? `+${fmtMoney(climateUsd)}` : climateUsd < 0 ? `${fmtMoney(-climateUsd)} saved` : fmtMoney(0),
          sub:
            `room ${Math.round(thermal.roomMinF)}–${Math.round(thermal.roomMaxF)}°F hourly: ` +
            `${Number(thermal.hoursCooling)} h cooling, ${Number(thermal.hoursHeating)} h heating, ${Number(thermal.hoursNeutral)} h neither` +
            (hourly ? `; outdoor ${Math.round(thermal.outdoorMinF)}–${Math.round(thermal.outdoorMaxF)}°F` : '') +
            (Number(thermal.hoursBlocked) > 0 ? `, ${Number(thermal.hoursBlocked)} h too warm outside for the fan` : ''),
        }
    : hourly && thermal.mode === 'cooling'
      ? {
          value: `+${fmtMoney(climateUsd)}`,
          sub: `extra cooling — room ${thermal.roomF}°F, above ${thermal.coolTargetF}°F${hourlyNote}`,
        }
    : thermal.mode === 'cooling'
      ? thermal.coolBlocked
        ? {
            value: 'fan can’t cool',
            sub: `room ${thermal.roomF}°F, outside ${thermal.outdoorF}°F — warmer outside, so this heat raises the room instead of a bill`,
          }
        : {
            value: `+${fmtMoney(climateUsd)}`,
            sub:
              `extra cooling — room ${thermal.roomF}°F, above ${thermal.coolTargetF}°F` +
              (thermal.coolFanCfm > 0
                ? `; fan at a ${(thermal.roomF - thermal.outdoorF).toFixed(0)}°F gap, $${thermal.coolFactorNow.toFixed(3)}/therm`
                : `; $${thermal.coolFactorNow.toFixed(2)}/therm`),
          }
      : thermal.mode === 'heating'
        ? { value: `${fmtMoney(-climateUsd)} saved`, sub: `heating displaced — room ${thermal.roomF}°F, below ${thermal.heatTargetF}°F${hourlyNote}` }
        : { value: fmtMoney(0), sub: `room ${thermal.roomF}°F is between ${thermal.heatTargetF}°F and ${thermal.coolTargetF}°F — neither system runs` }

  return (
    <Panel
      title="What it would have cost on a paid API"
      actions={
        perModel.length > 0 ? (
          <Button size="small" onClick={() => setBreakdownOpen(true)} sx={{ textTransform: 'none' }}>
            Breakdown by model
          </Button>
        ) : undefined
      }
      // A LABEL: the baseline's name and the window, no sentence break, and
      // nothing about a request in flight. It is the header's whole job to say
      // what the numbers below are of.
      subtitle={`priced per model at its cost equivalent · ${when}`}
      note={
        // The scope sentence names WHOSE hardware, because the covered set is no
        // longer one thing: our own boxes, plus free-tier traffic that cost
        // nothing to anybody. "Run on our own hardware" was true when it was
        // written and would now be false of some of the requests it counts.
        (freeReqs > 0
          ? `Priced from the tokens of ${fmtInt(coveredReqs)} request(s) — ${fmtInt(localReqs)} on our own hardware, ${fmtInt(freeReqs)} given away by a provider's free tier.`
          : `Priced from the tokens of ${fmtInt(coveredReqs)} request(s) run on our own hardware.`) +
        (excludedReqs > 0
          ? ` ${fmtInt(excludedReqs)} more are NOT in these figures — ${reasonList.join('; ')}.`
          : ' That is every request in the window.')
      }
      dense
    >
      <Row>
        <Box sx={{ display: 'flex', gap: 4, flexWrap: 'wrap', py: 0.5 }}>
          <Figure
            label="On a paid API"
            value={pending ? <NotYet /> : fmtMoney(wouldCost)}
            sub="at paid-API rates"
          />
          <Figure
            label="Here, in electricity"
            value={pending ? <NotYet /> : fmtMoney(actual)}
            sub={costPerKwh > 0 ? `${fmtKwh(kwh)} at $${costPerKwh}/kWh` : 'no $/kWh configured'}
          />
          <Figure
            label="Difference"
            value={
              pending ? (
                <NotYet />
              ) : multiple > 0 ? (
                `${multiple < 10 ? multiple.toFixed(1) : Math.round(multiple)}×`
              ) : wouldCost > 0 ? (
                // Nothing was spent, so there is no multiple — dividing by zero
                // is not "0×", and a dash here would hide a real figure. The
                // money is the answer.
                fmtMoney(wouldCost)
              ) : (
                '—'
              )
            }
            sub={
              multiple > 0
                ? `${fmtMoney(wouldCost - actual)} not spent`
                : wouldCost > 0
                  ? 'not spent — none of this traffic was billed to anybody'
                  : 'nothing metered to compare'
            }
          />
          {/* WHERE THE FREE PART OF IT CAME FROM. Without this, a reader cannot
              tell a figure earned by this box's own hardware from one earned by
              a provider giving tokens away — and only the first is a return on
              the machine. Shown only when there is free traffic: a permanent
              "$0.00 / 0 requests" teaches nobody anything. */}
          {/* HEAT. Every watt-hour a local model draws ends up in the room: free
              heating in winter, extra load on the cooling in summer. From metered
              token energy only, so the box's real output is higher. */}
          <Figure
            label="Heat given off"
            value={pending ? <NotYet /> : `${fmtInt(Math.round(btu))} BTU`}
            sub={
              btuPerHour > 0
                ? wall
                  ? `≈ ${fmtInt(Math.round(btuPerHour))} BTU/h (${fmtInt(Math.round(btuPerHour / 3.41214))} W) measured at the wall; GPUs ${fmtInt(Math.round(gpuW))} W of it`
                  : `≈ ${fmtInt(Math.round(btuPerHour))} BTU/h (${fmtInt(Math.round(btuPerHour / 3.41214))} W) from token work; idle draw not counted`
                : wall
                  ? 'measured at the wall'
                  : 'from token work; idle draw not counted'
            }
          />
          {/* WHAT THE HEAT DID TO THE ROOM'S BILL, from Setup → Room. */}
          {climate && <Figure label="Room climate" value={pending ? <NotYet /> : climate.value} sub={climate.sub} />}
          {freeReqs > 0 && (
            <Figure
              label="Of that, free tier"
              value={pending ? <NotYet /> : fmtMoney(freeCost)}
              sub={`${fmtInt(freeReqs)} request(s) another provider served at no charge`}
            />
          )}
        </Box>
      </Row>
      <Dialog open={breakdownOpen} onClose={() => setBreakdownOpen(false)} maxWidth="md" fullWidth>
        <DialogTitle>What it would have cost on a paid API, by model · {when}</DialogTitle>
        <DialogContent>
          <Typography variant="body2" sx={{ color: C.textMuted, mb: 1 }}>
            Each model is priced at the rates of the paid model it stands in for (its cost
            equivalent, or the default when it declares none). Heat is metered token energy ×
            3,412 BTU/kWh; idle draw is not counted.
          </Typography>
          <Table size="small" sx={{ my: 0.5 }}>
            <TableHead>
              <TableRow>
                <TableCell>Model</TableCell>
                <TableCell>Priced as</TableCell>
                <TableCell align="right">On a paid API</TableCell>
                <TableCell align="right">Electricity here</TableCell>
                <TableCell align="right">Heat, BTU</TableCell>
                {thermal && <TableCell align="right">Room climate</TableCell>}
              </TableRow>
            </TableHead>
            <TableBody>
              {perModel.map((r) => (
                <TableRow key={r.served}>
                  <TableCell>{r.served}</TableCell>
                  <TableCell>{r.baselineType || baselineType}</TableCell>
                  <TableCell align="right">{fmtMoney(r.baselineUsd)}</TableCell>
                  <TableCell align="right">{fmtMoney(r.energyUsd)}</TableCell>
                  <TableCell align="right">{fmtInt(Math.round(r.energyBtu))}</TableCell>
                  {thermal && (
                    <TableCell align="right">
                      {r.thermalUsd > 0
                        ? `+${fmtMoney(r.thermalUsd)} cooling`
                        : r.thermalUsd < 0
                          ? `${fmtMoney(-r.thermalUsd)} saved`
                          : fmtMoney(0)}
                    </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setBreakdownOpen(false)}>Close</Button>
        </DialogActions>
      </Dialog>
      {/* The breakdown, in the three classes tokens are always measured in. It
          is below the headline rather than beside it because the headline is what
          somebody came for (OB-11) — and it is here at all because a single
          "would have cost" is a claim, while three rates against three token
          counts is an arithmetic anybody can redo. */}
      <Row>
        <Box sx={{ display: 'flex', gap: 4, flexWrap: 'wrap', py: 0.5 }}>
          <Figure
            label="Cache reads"
            value={pending ? <NotYet /> : fmtMoney(cachedUsd)}
            sub={`${fmtInt(sum((r) => r.cachedTokens))} tokens — free here, billed there`}
          />
          <Figure
            label="Processed"
            value={pending ? <NotYet /> : fmtMoney(processedUsd)}
            sub={`${fmtInt(sum((r) => r.promptTokens - r.cachedTokens))} tokens read`}
          />
          <Figure
            label="Generated"
            value={pending ? <NotYet /> : fmtMoney(generatedUsd)}
            sub={`${fmtInt(sum((r) => r.completionTokens))} tokens written`}
          />
        </Box>
      </Row>
    </Panel>
  )
}

function Figure({
  label,
  value,
  sub,
}: {
  label: string
  value: React.ReactNode
  sub?: string
}) {
  return (
    <Box sx={{ minWidth: 150 }}>
      <Typography variant="caption" sx={{ color: C.textFaint, display: 'block' }}>
        {label}
      </Typography>
      <Typography variant="h6" sx={{ lineHeight: 1.2 }}>
        {value}
      </Typography>
      {sub && (
        <Typography variant="caption" sx={{ color: C.textMuted }}>
          {sub}
        </Typography>
      )}
    </Box>
  )
}
