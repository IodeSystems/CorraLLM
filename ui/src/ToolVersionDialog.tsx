import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { extractMessage } from '@/format'

/**
 * Holding a tool still — the two levers, in one place because they answer the
 * same question from opposite ends.
 *
 * Upstream ships several llama.cpp builds a day and any of them can regress a
 * model. Before this the only recourse was to stop rebuilding and remember why,
 * which is how a tool ends up held back for weeks by nothing more durable than
 * somebody's memory.
 *
 *   ACTIVATE gets you serving again NOW. A build already in builds/ becomes
 *   current by renaming a symlink — a second, no compiler, per host.
 *
 *   A PIN keeps you there LATER. It is configuration, it applies to every host,
 *   and it survives a rebuild — including a scheduled one, which is the failure
 *   that makes a pin worth having: `rebuild: true` plus a bad upstream means the
 *   regression returns every six hours.
 *
 * Neither implies the other, and doing only one is a common mistake in both
 * directions: activate without pinning and the next check undoes it; pin
 * without activating and the host keeps serving the bad build until something
 * rebuilds. The dialog says so rather than assuming.
 */
const InstalledBuildsDoc = graphql(/* GraphQL */ `
  query InstalledBuilds($tool: String!, $host: String!) {
    corrallm {
      toolInstalledBuilds(tool: $tool, host: $host) {
        builds {
          id
          head
          at
          active
        }
        active
        versioned
        keep
      }
    }
  }
`)

const PinDoc = graphql(/* GraphQL */ `
  mutation ToolPin($tool: String!, $pin: String!) {
    corrallm {
      toolPin(body: { tool: $tool, pin: $pin }) {
        tool
        ref
        pin
        message
      }
    }
  }
`)

const ActivateDoc = graphql(/* GraphQL */ `
  mutation ToolActivate($tool: String!, $host: String!, $id: String!) {
    corrallm {
      toolActivate(body: { tool: $tool, host: $host, id: $id }) {
        active
        previous
        message
      }
    }
  }
`)

const SHA = /^[0-9a-fA-F]{40}$/

function shortSha(s?: string | null): string {
  return s ? s.slice(0, 9) : ''
}

function ago(unixSeconds: number): string {
  const secs = Math.max(0, Math.floor(Date.now() / 1000) - unixSeconds)
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`
  return `${Math.floor(secs / 86400)}d ago`
}

export function ToolVersionDialog({
  open,
  onClose,
  tool,
  host,
  ref_,
  pin,
  adopted,
}: {
  open: boolean
  onClose: () => void
  tool?: string
  host?: string
  /** What the tool tracks — a branch or tag. Unchanged by pinning. */
  ref_?: string | null
  /** The commit it is currently held at, if any. */
  pin?: string | null
  /** An adopted install has no builds/ of corrallm's, so there is nothing to activate. */
  adopted?: boolean
}) {
  const qc = useQueryClient()
  const [draft, setDraft] = useState('')
  const [err, setErr] = useState('')
  const [note, setNote] = useState('')

  // Seed from the live pin on every open. Keeping the previous draft would put
  // one tool's sha in another tool's field, which is the worst possible typo
  // here — it is valid, it saves, and it pins the wrong thing.
  useEffect(() => {
    setDraft(pin ?? '')
    setErr('')
    setNote('')
  }, [open, pin])

  const builds = useQuery({
    queryKey: ['installedBuilds', tool ?? '', host ?? ''],
    queryFn: () => gqlClient.request(InstalledBuildsDoc, { tool: tool ?? '', host: host ?? '' }),
    enabled: open && !!tool && !!host && !adopted,
    // A directory read on the host: cheap, but it only changes when something
    // builds or activates, both of which invalidate this explicitly.
    staleTime: 30_000,
    retry: false,
  })
  const bl = builds.data?.corrallm.toolInstalledBuilds
  const list = bl?.builds ?? []

  const done = (message: string) => {
    setErr('')
    setNote(message)
    void qc.invalidateQueries({ queryKey: ['tooling'] })
    void builds.refetch()
  }

  const doPin = useMutation({
    mutationFn: (sha: string) => gqlClient.request(PinDoc, { tool: tool ?? '', pin: sha }),
    onSuccess: (d) => done(d.corrallm.toolPin?.message ?? 'saved'),
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  const doActivate = useMutation({
    mutationFn: (id: string) =>
      gqlClient.request(ActivateDoc, { tool: tool ?? '', host: host ?? '', id }),
    onSuccess: (d) => done(d.corrallm.toolActivate?.message ?? 'switched'),
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  const trimmed = draft.trim()
  const shaOK = trimmed === '' || SHA.test(trimmed)
  const changed = trimmed.toLowerCase() !== (pin ?? '').toLowerCase()
  const busy = doPin.isPending || doActivate.isPending

  return (
    <Dialog open={open} onClose={onClose} maxWidth="sm" fullWidth>
      <DialogTitle>
        <Stack direction="row" spacing={1.5} alignItems="center" flexWrap="wrap" useFlexGap>
          <span>
            Version of {tool} <span style={{ color: C.textFaint }}>on</span> {host}
          </span>
          {pin ? (
            <Chip size="small" color="info" variant="outlined" label={`pinned ${shortSha(pin)}`} />
          ) : (
            <Chip size="small" variant="outlined" label={`tracking ${ref_ ?? '—'}`} />
          )}
        </Stack>
      </DialogTitle>

      <DialogContent dividers>
        {err && (
          <Alert severity="error" sx={{ mb: 2 }}>
            {err}
          </Alert>
        )}
        {note && (
          <Alert severity="success" sx={{ mb: 2 }}>
            {note}
          </Alert>
        )}

        <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
          Pin
        </Typography>
        <Typography variant="caption" sx={{ color: C.textMuted, display: 'block', mb: 1.5 }}>
          Holds <b>every</b> host at one commit, and survives rebuilds — including a scheduled one.
          It does not change what the tool tracks ({ref_ ?? '—'}), and it does not build: the next
          build checks this out, and anything already running keeps the binary it started with.
        </Typography>

        <Stack direction="row" spacing={1} alignItems="flex-start">
          <TextField
            size="small"
            fullWidth
            label="Commit sha"
            placeholder={`full 40-character sha — empty to track ${ref_ ?? 'the ref'} again`}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            error={!shaOK}
            helperText={
              shaOK
                ? ' '
                : // Abbreviated is refused rather than resolved: a host with a
                  // clone could expand it and a host without one could not, so
                  // it would work on the machine you set it from and fail on
                  // the machine you needed it on.
                  'A full 40-character sha. An abbreviated hash cannot be requested from a remote.'
            }
            slotProps={{ htmlInput: { spellCheck: false, style: { fontFamily: 'monospace' } } }}
          />
          <Button
            variant="contained"
            disabled={!shaOK || !changed || busy || !tool}
            onClick={() => doPin.mutate(trimmed.toLowerCase())}
            sx={{ mt: 0.25 }}
          >
            {trimmed === '' ? 'Unpin' : 'Pin'}
          </Button>
        </Stack>

        <Box sx={{ mt: 3 }}>
          <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
            Installed builds on {host}
          </Typography>
          <Typography variant="caption" sx={{ color: C.textMuted, display: 'block', mb: 1.5 }}>
            Switching between these is a symlink rename — seconds, no compiler. It affects this host
            only and takes effect on the next spawn, so reload a model to pick it up.
          </Typography>

          {adopted ? (
            <Typography variant="body2" sx={{ color: C.textMuted }}>
              This install is adopted: corrallm did not build it and keeps no versions of it. A pin
              still applies, but it only takes effect if this host is switched to a managed install.
            </Typography>
          ) : builds.isLoading ? (
            <CircularProgress size={18} />
          ) : builds.error ? (
            <Alert severity="warning">{extractMessage(builds.error)}</Alert>
          ) : !bl?.versioned ? (
            // "Nothing to roll back to yet" and "this layout does not track
            // builds" are different answers, and only one of them is fixed by
            // building again.
            <Typography variant="body2" sx={{ color: C.textMuted }}>
              This prefix predates versioned installs. The next build moves the current bin/ into
              builds/ before installing, so from then on there is something to fall back to.
            </Typography>
          ) : list.length === 0 ? (
            <Typography variant="body2" sx={{ color: C.textMuted }}>
              Nothing built here yet.
            </Typography>
          ) : (
            <Stack spacing={0.5}>
              {list.map((b) => (
                <Stack
                  key={b.id}
                  direction="row"
                  spacing={1}
                  alignItems="center"
                  sx={{
                    px: 1,
                    py: 0.75,
                    border: `1px solid ${b.active ? C.borderStrong : C.border}`,
                    borderRadius: 1,
                    background: b.active ? C.raised : 'transparent',
                  }}
                >
                  <Typography
                    variant="body2"
                    sx={{ fontFamily: 'monospace', fontSize: 12.5, flex: 1 }}
                  >
                    {b.id}
                  </Typography>
                  <Typography variant="caption" sx={{ color: C.textFaint }}>
                    {ago(Number(b.at))}
                  </Typography>
                  {b.active && <Chip size="small" color="success" variant="outlined" label="serving" />}
                  {/* Pinning FROM a build is the safe path to a pin: this
                      commit is known to compile here, because it did. */}
                  {b.head && b.head.length === 40 && (
                    <Tooltip title={`Hold every host at ${shortSha(b.head)} — the commit this build came from`}>
                      <span>
                        <Button
                          size="small"
                          disabled={busy || (pin ?? '').toLowerCase() === b.head.toLowerCase()}
                          onClick={() => {
                            setDraft(b.head ?? '')
                            doPin.mutate((b.head ?? '').toLowerCase())
                          }}
                        >
                          Pin this
                        </Button>
                      </span>
                    </Tooltip>
                  )}
                  <Button
                    size="small"
                    variant={b.active ? 'text' : 'outlined'}
                    disabled={b.active || busy}
                    onClick={() => doActivate.mutate(b.id)}
                  >
                    {b.active ? 'Active' : 'Activate'}
                  </Button>
                </Stack>
              ))}
              {typeof bl?.keep === 'number' && (
                <Typography variant="caption" sx={{ color: C.textFaint, pt: 0.5 }}>
                  The newest {bl.keep} are kept; the serving build is never pruned.
                </Typography>
              )}
            </Stack>
          )}
        </Box>
      </DialogContent>

      <DialogActions>
        <Button onClick={onClose}>Close</Button>
      </DialogActions>
    </Dialog>
  )
}
