import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Typography,
} from '@mui/material'
import { useMutation } from '@tanstack/react-query'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { extractMessage } from '@/format'
import { pauseCost, type LaneShape, type PauseCost } from '@/Pause'
import { C } from '@/theme'

/**
 * TAKING A MODEL OUT OF THE SETUP, IN ONE PLACE — the other half of Pause.tsx.
 *
 * The two live in separate files for the reason 8d64094 split the pause out of
 * the page that grew it: the failure mode of a second screen needing one of
 * these is not a missing feature, it is two of them, worded slightly
 * differently, disagreeing about what the button does.
 *
 * WHY IT IS NOT CALLED "DISABLE" ON SCREEN. `disabled` is the config key, and
 * OB-3 says the config's word is the gloss and never the label. That rule earns
 * its keep here more than anywhere else: the one thing this control must not do
 * is read as a synonym for Pause, and "Disable" next to "Pause" is two switches
 * that both mean off. "Take it out of the setup" cannot be mistaken for a
 * pause, because a pause does not change what your setup IS. The config word
 * appears once, in the dialog, so somebody reading the YAML can join the two
 * vocabularies.
 *
 * The distinction, which every sentence in this file exists to carry:
 *
 *   pause     operational, keyed on the PROCESS. Temporary by intent, and it
 *             nags on purpose because you are meant to come back to it. An
 *             extension's models pause together, because a pause frees a
 *             process and freeing half of one frees nothing.
 *   out of    configuration, keyed on the MODEL. "This is not part of my
 *   the setup setup." Silent by design — no fault, no refusing row, nothing on
 *             Now — and one of an extension's five can go while the other four
 *             keep serving, because it frees nothing and is a statement about
 *             one entry in a file.
 */

/**
 * The write, and why it reads first.
 *
 * upsertModel is a PATCH over the fields the model FORM owns, not over the
 * fields you send: applySpec assigns every one of them from the request, so a
 * body carrying only `name`, `proxy` and `disabled` does not leave `cmd`,
 * `quality`, `notes` and the rest alone — it clears them. A one-field write is
 * therefore not available, and pretending otherwise would silently strip a
 * model's definition on the way to keeping it.
 *
 * So the spec is read at press time and sent back with one field changed. Read
 * at PRESS time and not held in page state, because the gap between opening a
 * screen and pressing a button is long enough for somebody else to have edited
 * the model, and this write would otherwise quietly revert them.
 */
const ModelAuthorDoc = graphql(/* GraphQL */ `
  query ModelAuthor($name: String!) {
    corrallm {
      modelSpec(name: $name) {
        exists
        spec {
          name
          cmd
          server
          proxy
          upstream
          type
          quality
          maxConcurrent
          maxTokens
          persistent
          stickyTtl
          stickyIdleUnload
          stickyEvictCost
          ramUsage
          notes
          disabled
        }
      }
      overview {
        # WHERE THE MODEL IS AUTHORED, which decides where the write has to
        # land. A model of a remote provider lives at
        # extensions.<ext>.providers.<p>.provides.<id>, and the server can only
        # be told that by being handed both names — the model alone does not
        # carry them back. Handed neither, it writes a second, top-level model
        # with the same served name, which the next load refuses as a collision.
        extensions {
          name
          provides
        }
      }
      listProviders {
        providers {
          extension
          name
        }
      }
    }
  }
`)

const SetModelDisabledDoc = graphql(/* GraphQL */ `
  mutation SetModelDisabled(
    $name: String!
    $provider: String
    $extension: String
    $body: corrallm_ModelSpecInput!
  ) {
    corrallm {
      upsertModel(name: $name, provider: $provider, extension: $extension, body: $body) {
        ok
        message
      }
    }
  }
`)

/** Where in the config document a served model was written down. */
type Author = { provider?: string; extension?: string }

/**
 * authorOf works out which door the write goes through.
 *
 * Three shapes, and only two of them can be edited one model at a time:
 *
 *   a provider's model     `providers.local.models.<id>`. The server already
 *                          knows where it came from and writes it back there,
 *                          so neither name is sent.
 *   a remote provider's    `extensions.free.providers.groq.provides.<id>`. Both
 *                          names are required, and the served name carries the
 *                          provider as its prefix.
 *   an extension's own     `extensions.oidio.provides`. One cmd, several
 *                          models, DERIVED from the extension — the server
 *                          refuses to edit the derived copy, because the next
 *                          reload would overwrite it. It throws here rather
 *                          than 409ing after the press.
 *
 * The prefix match is longest-first: two providers in one extension can both
 * prefix a name, and `openrouter-x` must not be read as provider `open`.
 */
function authorOf(
  name: string,
  extensions: readonly { name: string; provides: readonly string[] }[],
  providers: readonly { extension: string; name: string }[],
): Author {
  const ext = extensions.find((e) => e.provides.includes(name))
  if (!ext) return {}
  const prov = providers
    .filter((p) => p.extension === ext.name && name.startsWith(`${p.name}-`))
    .sort((a, b) => b.name.length - a.name.length)[0]
  if (!prov) {
    throw new Error(
      `${name} is provided by the ${ext.name} extension itself, which serves several models from one definition. ` +
        `Edit the extension to change what it provides.`,
    )
  }
  return { provider: prov.name, extension: ext.name }
}

/**
 * useSetDisabled returns the mutation both directions share.
 *
 * One hook for in and out, because enabling has to work through exactly the
 * door disabling went through — a one-way control is a trap, and a second
 * mutation built for the way back is how the two ends stop agreeing about where
 * the model lives.
 */
export function useSetDisabled(opts: {
  onDone: (message: string, ok: boolean) => void
  onFail: (message: string) => void
}) {
  return useMutation({
    mutationFn: async (v: { model: string; disabled: boolean }) => {
      const ctx = await gqlClient.request(ModelAuthorDoc, { name: v.model })
      const got = ctx.corrallm?.modelSpec
      const s = got?.spec
      if (!got?.exists || !s) {
        throw new Error(`${v.model} is not in the configuration any more — reload the page.`)
      }
      const author = authorOf(
        v.model,
        ctx.corrallm?.overview?.extensions ?? [],
        ctx.corrallm?.listProviders?.providers ?? [],
      )
      const r = await gqlClient.request(SetModelDisabledDoc, {
        name: v.model,
        provider: author.provider ?? null,
        extension: author.extension ?? null,
        // Every field the form owns, read back a moment ago, with one changed.
        // See ModelAuthorDoc: sending a subset clears the remainder.
        body: {
          name: s.name,
          cmd: s.cmd,
          server: s.server,
          proxy: s.proxy,
          upstream: s.upstream,
          type: s.type,
          quality: s.quality,
          maxConcurrent: s.maxConcurrent,
          maxTokens: s.maxTokens,
          persistent: s.persistent,
          stickyTtl: s.stickyTtl,
          stickyIdleUnload: s.stickyIdleUnload,
          stickyEvictCost: s.stickyEvictCost,
          ramUsage: s.ramUsage,
          notes: s.notes,
          disabled: v.disabled,
        },
      })
      return r.corrallm?.upsertModel
    },
    onSuccess: (res, v) => {
      if (!res?.ok) {
        opts.onFail(res?.message ?? 'the change was refused')
        return
      }
      // The server's own message is "saved <name>", which is true and says
      // nothing about what the reader just decided.
      opts.onDone(
        v.disabled
          ? `${v.model} is out of the setup. Models lists it under “Out of this setup”, which is where you put it back.`
          : `${v.model} is back in the setup, exactly as it was left.`,
        true,
      )
    },
    onError: (e: unknown) => opts.onFail(extractMessage(e)),
  })
}

/**
 * WHAT THIS WILL COST — the same ladder arithmetic as a pause, aimed at one
 * model instead of one process.
 *
 * The function exists to hold that difference rather than to do new work.
 * pauseCost takes a LIST of served names because a pause keys on procKey and an
 * extension hosts several; this takes one name, always, and an extension's
 * other four models keep serving. Calling pauseCost with a siblings array here
 * would have been the single most plausible mistake in this feature, and the
 * signature now makes it unspellable.
 *
 * `alreadyOut` is still paused + refusing. Rungs that are ALREADY out of the
 * setup need no mention: pauseCost drops them from the ladder outright, because
 * a request never sees them.
 */
export function disableCost(
  model: string,
  lanes: readonly LaneShape[],
  alreadyOut: ReadonlySet<string>,
): PauseCost {
  return pauseCost([model], lanes, alreadyOut)
}

/**
 * The cost, in sentences — and every one of them differs from the pause's.
 *
 * A pause's survivor line reads "10 of its 11 backends are left", because the
 * rung is still on the ladder and is coming back. This one drops the
 * denominator: the rung leaves, and "of its 11" would describe a list that no
 * longer exists the moment the button is pressed.
 *
 * The last line is the sharpest thing on the screen for telling the two apart,
 * and it is a fact rather than a characterisation: a caller naming a paused
 * model gets 503, which says the thing exists and may come back and invites a
 * retry. A caller naming a model that is out of the setup gets 404 AND the
 * reason, because there is nothing to wait for and "unknown model" would send
 * somebody hunting for a typo in a name that is spelled correctly.
 */
export function DisableCostNote({ cost }: { cost: PauseCost }) {
  const model = cost.taking[0] ?? ''
  return (
    <>
      {cost.lanes.map((l) =>
        l.survivors.length > 0 ? (
          <Typography key={l.lane} variant="body2" sx={{ mb: 1 }}>
            <b>{l.lane}</b> keeps serving — {l.survivors.length}{' '}
            {l.survivors.length === 1 ? 'backend is' : 'backends are'} left in it, and{' '}
            <b>{l.survivors[0]}</b> is tried first.
          </Typography>
        ) : (
          <Alert key={l.lane} severity="error" sx={{ mb: 1 }}>
            <b>{l.lane}</b> stops serving.{' '}
            {l.rungs === l.taken.length
              ? 'It has one backend and this is it.'
              : `Its other ${l.rungs - l.taken.length} ${
                  l.rungs - l.taken.length === 1 ? 'backend is' : 'backends are'
                } already paused or refusing.`}{' '}
            Every request for <b>{l.lane}</b> is turned down until something in it can serve again.
          </Alert>
        ),
      )}
      {cost.lanes.length === 0 && (
        <Typography variant="body2" sx={{ mb: 1 }}>
          No fallback list contains it, so no list loses a rung.
        </Typography>
      )}
      <Typography variant="body2" sx={{ mb: 2, color: C.textMuted }}>
        Anything that asks for <b>{model}</b> by name is answered <b>404</b> —{' '}
        <i>model &ldquo;{model}&rdquo; is disabled in this configuration</i> — and not the 503 a
        pause gives, because there is nothing to wait for.
      </Typography>
    </>
  )
}

/**
 * The confirmation, and it deliberately does not look like PauseDialog.
 *
 * PauseDialog collects two things: when to come back, and why. This collects
 * nothing. That is not an omission to be tidied up later — it is the difference
 * on screen. A pause has an end and a reason somebody has to be told, so the
 * reason is prefilled onto Now where a colleague reads it. A decision about what
 * your setup contains is already recorded, with who made it and when, in the
 * config's own revision history; asking for it a second time in a text box
 * would put the operator's explanation somewhere it cannot be read back.
 *
 * So the reader meets two dialogs that are shaped differently before they have
 * read a word of either, which is the point (OB-1).
 */
export function DisableDialog(props: {
  target: string | null
  cost: PauseCost | null
  busy: boolean
  onClose: () => void
  onConfirm: () => void
}) {
  const { target, cost, busy, onClose, onConfirm } = props
  return (
    <Dialog open={!!target} onClose={onClose} maxWidth="sm" fullWidth>
      <DialogTitle>Take {target} out of the setup?</DialogTitle>
      <DialogContent>
        <Typography variant="body2" sx={{ mb: 2, color: C.textMuted }}>
          A change to your configuration, not a pause. Nothing resolves to it, nothing preloads it,
          and it leaves every fallback list — until somebody puts it back. Its definition is kept,
          so putting it back restores it as it is now. The config file calls this{' '}
          <code>disabled</code>.
        </Typography>
        {cost && <DisableCostNote cost={cost} />}
        {/* SILENT IS THE POINT, AND SILENT IS ALSO THE RISK. Everything that
            names this backend today — this banner, the fault list on Now — goes
            quiet the moment the button is pressed, including the control that
            was pressed. Saying where it went is the difference between quiet
            and lost, and it has to be said BEFORE the press, because afterwards
            there is nothing left on this screen to say it (OB-8). */}
        <Alert severity="info">
          It stops being reported: no fault card on Now, and no row on the budgets banner — a
          decision you have already made is not something the box should keep raising. You will find
          it on <b>Models</b>, under <b>Out of this setup</b>, which is also where you put it back.
        </Alert>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" disabled={busy} onClick={onConfirm}>
          Take it out of the setup
        </Button>
      </DialogActions>
    </Dialog>
  )
}
