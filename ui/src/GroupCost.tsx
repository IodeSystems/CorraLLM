import { useQuery } from '@tanstack/react-query'
import { Box, Typography } from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { fmtInt } from '@/format'

/**
 * WHAT A CALLER'S GROUP COSTS IT, said where the caller is (OB-12).
 *
 * Lenny run 5 recognised his own team's key in the roster, read `batch` and
 * `1`, then had to walk down to the Priority groups table, match the group name
 * by eye and do the division himself to learn that his traffic gets one turn in
 * ten and can be cut off part-way. Nothing on the row said it. A reference table
 * you have to cross-reference by hand is not the screen telling you.
 *
 * Three things this module refuses to do, each because getting it wrong is
 * worse than the defect it fixes:
 *
 *  - It says nothing at all unless ANOTHER GROUP OUTRANKS THIS ONE. A one-group
 *    box, and a caller sitting in the heaviest group, are not paying anything
 *    for their placement, and a dashboard that cries on every wobble teaches
 *    people to stop reading it (see slowdownFaults in routes/index.tsx, which
 *    gates itself for the same reason).
 *  - It never renders as a fault. A caller in the fallback group on an idle box
 *    is a standing arrangement, not a breakage, and it gets muted text. The warm
 *    colour is reserved for the case where something has actually been observed.
 *  - It keeps CAN and HAS apart, in that order and in separate sentences.
 *    "Can be stopped mid-answer" is a setting; "four answers have been stopped
 *    this way on this box" is an event, and only the second one explains a
 *    symptom somebody is already looking at. When there is no event the absence
 *    is written down too — a capability with no history reads as a cause.
 *
 * No new measurement: every field comes from the groups query the Callers page
 * already issues for its Priority groups / Backend load panels, shared through
 * react-query's ['groups'] cache. The key page joins the same cache.
 *
 * Vocabulary (OB-3): group, share, turn, waiting for a slot, stopped part-way.
 * Not lane, not rung, not fairshare, not preempt, not interruptible.
 */

// The live groups query. It lives here rather than in routes/groups.tsx because
// two panels and two pages read it now, and the route file is the consumer that
// changes most often.
export const GroupsDoc = graphql(/* GraphQL */ `
  query Groups {
    corrallm {
      reservations {
        reservations {
          model
          lane
          slots
          expiresAt
        }
      }
      groups {
        preemptions
        lastPreemptedMs
        groups {
          name
          weight
          shareCurrency
          interruptible
          active
          waiting
        }
        backends {
          backend
          capacity
          active
          waiting
          groups {
            group
            active
            waiting
          }
        }
      }
    }
  }
`)

export type GroupCost = {
  /**
   * none      — nothing outranks this group. Render nothing.
   * standing  — something outranks it, and nothing has come of that yet.
   * observed  — something outranks it AND the queue or the preemption ledger
   *             has a number in it. Stronger, and coloured.
   */
  level: 'none' | 'standing' | 'observed'
  /** The compact form for a dense table cell. Complete on its own (OB-10). */
  short: string
  /** True when a higher group can take this one's slot mid-answer. */
  canBeStopped: boolean
  /** The standing arrangement, in one sentence. */
  arrangement: string
  /** What has or has not actually happened. Always present when level ≠ none. */
  evidence: string
}

const NONE: GroupCost = {
  level: 'none',
  short: '',
  canBeStopped: false,
  arrangement: '',
  evidence: '',
}

const sentence = (s: string) => `${s.charAt(0).toUpperCase()}${s.slice(1)}.`

type GroupFacts = {
  name: string
  weight: number
  interruptible: boolean
  waiting: number
}

/**
 * groupCost turns one group's live row into what it costs the callers in it.
 *
 * topWeight/topName describe the heaviest group on the box; preemptions and
 * lastPreemptedMs are box-wide and all-time, which is why the sentence they
 * produce says "on this box" and never "your requests" — the ledger does not
 * record whose they were.
 */
export function groupCost(
  g: GroupFacts,
  topWeight: number,
  topName: string,
  preemptions: number,
  lastPreemptedMs: number,
): GroupCost {
  // THE WHOLE GATE. Weight is a ratio against the other groups, so a group that
  // nothing outranks costs its callers nothing, whatever its number says — and
  // a group cannot be stopped to free a slot for a group that does not outrank
  // it either, so the interruptible flag is meaningless without this too.
  if (!(topWeight > 0 && g.weight > 0 && g.weight < topWeight)) return NONE

  const ratio = Math.round(topWeight / g.weight)
  const canBeStopped = g.interruptible
  const waitingNow = g.waiting > 0
  const stoppedEver = canBeStopped && preemptions > 0

  const short = canBeStopped
    ? `1 turn for every ${ratio} when busy · can be stopped mid-answer`
    : `1 turn for every ${ratio} when busy`

  const arrangement =
    `While the box is busy this group gets one turn for every ${ratio} that ${topName} gets` +
    (canBeStopped
      ? ', and an answer already running here can be stopped part-way to free the slot for a group above it.'
      : ', though an answer already running here always finishes.')

  // CAN, then HAS — and the absence of HAS is written down rather than left to
  // be inferred from a missing sentence.
  const parts: string[] = []
  if (waitingNow) {
    parts.push(
      `${fmtInt(g.waiting)} request${g.waiting === 1 ? '' : 's'} in this group ${g.waiting === 1 ? 'is' : 'are'} waiting for a slot right now`,
    )
  }
  if (canBeStopped) {
    parts.push(
      preemptions > 0
        ? `${fmtInt(preemptions)} answer${preemptions === 1 ? ' has' : 's have'} been stopped this way on this box, most recently ${new Date(lastPreemptedMs).toLocaleString()}`
        : 'no answer has ever been stopped this way on this box',
    )
  }
  if (!waitingNow) parts.unshift('nothing in this group is waiting for a slot right now')

  return {
    level: waitingNow || stoppedEver ? 'observed' : 'standing',
    canBeStopped,
    short: waitingNow
      ? `${short} · ${fmtInt(g.waiting)} waiting in this group now`
      : stoppedEver
        ? `${short} · ${fmtInt(preemptions)} stopped this way on this box`
        : short,
    arrangement,
    // It renders as its own sentence under the arrangement, so it reads as one.
    evidence: sentence(parts.join('; ')),
  }
}

/**
 * useGroupCosts reads the shared ['groups'] cache and returns a lookup by group
 * name. On /callers the Priority groups panel has already issued this query, so
 * this costs no request; on a key page it is the one query that answers "and
 * what does that group buy me".
 */
export function useGroupCosts(): {
  costOf: (group: string) => GroupCost
  /** Requests ever stopped mid-answer on this box, all-time. Not per-group. */
  preemptions: number
  lastPreemptedMs: number
} {
  const q = useQuery({
    queryKey: ['groups'],
    queryFn: () => gqlClient.request(GroupsDoc),
    refetchInterval: 15000,
  })
  const live = q.data?.corrallm.groups
  const groups = live?.groups ?? []
  const topWeight = groups.reduce((m, g) => Math.max(m, Number(g.weight ?? 0)), 0)
  const topName = groups.find((g) => Number(g.weight) === topWeight)?.name ?? ''
  const preemptions = Number(live?.preemptions ?? 0)
  const lastPreemptedMs = Number(live?.lastPreemptedMs ?? 0)

  return {
    preemptions,
    lastPreemptedMs,
    costOf: (name: string) => {
      const g = groups.find((x) => x.name === name)
      if (!g) return NONE
      return groupCost(
        {
          name: g.name,
          weight: Number(g.weight),
          interruptible: !!g.interruptible,
          waiting: Number(g.waiting),
        },
        topWeight,
        topName,
        preemptions,
        lastPreemptedMs,
      )
    },
  }
}

/**
 * The box's preemption ledger as a sentence. Box-wide and all-time, so it never
 * says whose requests they were — the ledger does not record that, and guessing
 * would be worse than the vagueness.
 *
 * The zero case is a sentence too, not a silence: "can be stopped mid-answer" is
 * a setting, and a setting with no history reads as the cause of whatever the
 * reader is already worried about.
 */
export function preemptionHistory(preemptions: number, lastPreemptedMs: number): string {
  return preemptions === 0
    ? 'No answer has ever been stopped this way on this box.'
    : `${fmtInt(preemptions)} answer${preemptions === 1 ? ' has' : 's have'} been stopped this way on this box, most recently ${new Date(lastPreemptedMs).toLocaleString()}.`
}

const TONE = { standing: C.textMuted, observed: C.warn, none: C.textMuted } as const

/**
 * The roster-cell form: one muted line under the group chip. Short enough for a
 * table column, complete enough that nobody has to hover it (OB-10).
 */
export function GroupCostTag({ cost }: { cost: GroupCost }) {
  if (cost.level === 'none') return null
  return (
    <Typography
      variant="caption"
      sx={{ display: 'block', color: TONE[cost.level], mt: 0.25, maxWidth: 340 }}
    >
      {cost.short}
    </Typography>
  )
}

/**
 * The full form, for a page about one caller: the arrangement, then what has
 * actually come of it. Deliberately not an Alert — a standing arrangement drawn
 * as a fault is the bug on the other side of this one.
 */
export function GroupCostNote({ group, cost }: { group: string; cost: GroupCost }) {
  if (cost.level === 'none') return null
  return (
    <Box
      sx={{
        borderLeft: `2px solid ${TONE[cost.level]}`,
        pl: 1.5,
        py: 0.25,
      }}
    >
      <Typography variant="body2" sx={{ color: C.text }}>
        <b>
          This caller is in <code>{group}</code>, and other groups outrank it.
        </b>{' '}
        {cost.arrangement}
      </Typography>
      <Typography variant="body2" sx={{ color: TONE[cost.level], mt: 0.5 }}>
        {cost.evidence}
      </Typography>
    </Box>
  )
}
