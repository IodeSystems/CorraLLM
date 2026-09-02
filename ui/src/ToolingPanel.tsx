import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Stack,
  Tooltip,
  Typography,
} from '@mui/material'
import { BuildDialog } from '@/BuildDialog'
import { ToolVersionDialog } from '@/ToolVersionDialog'
import { Panel, Row } from '@/Panel'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'

/**
 * Tooling: the PROGRAMS that run the models, per host.
 *
 * corrallm knew what models it ran and nothing about what ran them — llama.cpp
 * was a path inside a cmd string. This is the answer to "what version is where,
 * and is it stale", which is otherwise an ssh and a --version by hand.
 *
 * It is its own query rather than part of the page's, because a survey ASKS
 * every host: a fork and an exec per tool locally, an HTTP round trip per tool
 * remotely, plus a git ls-remote each. Folding that into the page query would
 * make host capacity wait on a sleeping laptop to render. Here it can spin on
 * its own while everything else is already on screen.
 */
const ToolingDoc = graphql(/* GraphQL */ `
  query Tooling {
    corrallm {
      toolStates(drift: true) {
        tools {
          tool
          host
          declared
          adopted
          present
          path
          version
          versionSource
          commit
          behind
          remoteHead
          driftError
          error
          ref
          pin
          ahead
          pinUnsupported
        }
      }
    }
  }
`)

export function ToolingPanel() {
  const qc = useQueryClient()
  // Which row's build the modal is for. The build itself is a single global
  // slot on the daemon; this only decides what a fresh Build click targets.
  const [building, setBuilding] = useState<{ tool: string; host: string } | null>(null)
  // Which row's version dialog is open. Keyed by (tool, host) like the build
  // slot, but unlike a build there is nothing global about it — a pin is
  // config and an activation is one host's symlink.
  const [versioning, setVersioning] = useState<{
    tool: string
    host: string
    ref?: string | null
    pin?: string | null
    adopted: boolean
  } | null>(null)

  const q = useQuery({
    queryKey: ['tooling'],
    queryFn: () => gqlClient.request(ToolingDoc),
    // Nothing here changes on its own except upstream drift, and that costs a
    // network round trip per tool. Refetching on focus would re-survey every
    // host every time this tab is looked at.
    refetchOnWindowFocus: false,
    staleTime: 60_000,
  })

  const tools = q.data?.corrallm.toolStates?.tools ?? []

  return (
    <Panel
      title="Tooling"
      subtitle="The programs that run the models. A version per host, and whether it is behind its pin."
      badge={
        q.isFetching ? (
          <CircularProgress size={14} />
        ) : (
          <Chip size="small" variant="outlined" label={`${tools.length}`} />
        )
      }
      flush
    >
      {q.error && (
        <Box sx={{ p: 2 }}>
          <Alert severity="error">{String(q.error)}</Alert>
        </Box>
      )}

      {!q.isLoading && !q.error && tools.length === 0 && (
        <Typography variant="body2" sx={{ px: 2, py: 1.5, color: C.textMuted }}>
          No tools declared. A <code>tools:</code> entry names a program (llama.cpp, ninfer), where
          it comes from, and which hosts have it — after which a model&apos;s cmd can say{' '}
          <code>{'${tool:llama.cpp}/llama-server'}</code> instead of an absolute path that differs
          per machine.
        </Typography>
      )}

      {tools.map((t) => {
        // Three states that a single "version" column would blur, and the
        // blurring is what makes a dashboard lie:
        //   - could not ASK (unreachable host, agent too old)   -> error
        //   - asked, nothing installed                          -> absent
        //   - installed, but it cannot say what it is           -> unidentified
        const unidentified = t.present && !t.version
        return (
          <Row key={`${t.tool}:${t.host}`}>
            <Stack
              direction="row"
              spacing={1.5}
              useFlexGap
              sx={{
                alignItems: "baseline",
                flexWrap: "wrap",
                width: '100%'
              }}>
              <Box sx={{ minWidth: 150 }}>
                <Typography variant="subtitle2">{t.tool}</Typography>
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {t.host}
                </Typography>
              </Box>

              <Box sx={{ minWidth: 230 }}>
                {t.error ? (
                  <Tooltip title="corrallm could not ASK this host. Not the same as the tool being absent — nothing is known either way.">
                    <Chip size="small" color="warning" variant="outlined" label="cannot ask" />
                  </Tooltip>
                ) : !t.present ? (
                  <Tooltip title={t.path ? `Nothing at ${t.path}` : 'Not installed on this host'}>
                    <Chip size="small" variant="outlined" label="absent" />
                  </Tooltip>
                ) : unidentified ? (
                  <Tooltip title="Installed, and there is no way to say which build. ninfer has no --version at all, so a copy corrallm did not build cannot be identified — rather than show a made-up version, this says so.">
                    <Chip size="small" color="warning" variant="outlined" label="version unknown" />
                  </Tooltip>
                ) : (
                  <Stack
                    direction="row"
                    spacing={0.75}
                    useFlexGap
                    sx={{
                      alignItems: "center",
                      flexWrap: "wrap"
                    }}>
                    <Typography variant="body2" sx={{ fontFamily: 'monospace', fontSize: 12.5 }}>
                      {t.version}
                    </Typography>
                    <Tooltip
                      title={
                        t.versionSource === 'binary'
                          ? 'The binary reported this itself.'
                          : 'From the build stamp corrallm wrote — this tool cannot report its own version.'
                      }
                    >
                      <Chip size="small" variant="outlined" label={t.versionSource} />
                    </Tooltip>
                  </Stack>
                )}
              </Box>

              <Stack
                direction="row"
                spacing={0.75}
                useFlexGap
                sx={{
                  flexWrap: "wrap",
                  flex: 1
                }}>
                {t.adopted && (
                  <Tooltip title="corrallm does not own this install and will never write to it — no build, no dependency install. Drop installedAt to manage it here.">
                    <Chip size="small" variant="outlined" label="adopted" />
                  </Tooltip>
                )}
                {/* A pin changes what "behind" MEANS, so it is rendered first
                    and the drift chips read against it. While pinned, behind
                    is "not yet at the pin" — a build is owed — and being far
                    behind upstream is the intended state, not a warning. */}
                {t.pin && (
                  <Tooltip
                    title={
                      t.ahead
                        ? `Held at ${t.pin}. ${t.ref} has moved on to ${t.remoteHead ?? 'a newer commit'} — this tool stays put until the pin is cleared.`
                        : `Held at ${t.pin} (${t.ref} is there too).`
                    }
                  >
                    <Chip
                      size="small"
                      color="info"
                      variant="outlined"
                      label={`pinned ${t.pin.slice(0, 9)}`}
                    />
                  </Tooltip>
                )}
                {/* The window between deploying a primary that understands pins
                    and a host whose agent has not self-updated yet. Its drift
                    answer was computed against the branch, so it is reported as
                    unknown rather than as a number that means something else. */}
                {t.pinUnsupported && (
                  <Tooltip title="This host's agent predates tool pins, so it cannot say whether it is at the pin — and it is refused a build until it updates, because it would check out the tracked branch instead. It self-updates from the primary within a heartbeat or two.">
                    <Chip size="small" color="warning" variant="outlined" label="agent predates pins" />
                  </Tooltip>
                )}
                {t.behind && (
                  <Tooltip
                    title={
                      t.pin
                        ? `Installed ${t.commit ?? 'build'} is not the pinned ${t.pin.slice(0, 9)} — rebuild, or activate a build already here.`
                        : `Upstream is at ${t.remoteHead ?? 'a newer commit'}`
                    }
                  >
                    <Chip
                      size="small"
                      color="warning"
                      variant="outlined"
                      label={t.pin ? 'not at pin' : 'behind'}
                    />
                  </Tooltip>
                )}
                {!t.behind && t.present && !t.driftError && !t.pinUnsupported && (t.remoteHead || t.pin) && (
                  <Chip
                    size="small"
                    color="success"
                    variant="outlined"
                    label={t.pin ? 'at pin' : 'current'}
                  />
                )}
                {t.driftError && (
                  <Tooltip title={t.driftError}>
                    <Chip size="small" variant="outlined" label="drift unknown" />
                  </Tooltip>
                )}
              </Stack>

              <Box sx={{ flexBasis: '100%' }}>
                {t.error ? (
                  <Typography variant="caption" sx={{ color: C.textMuted }}>
                    {t.error}
                  </Typography>
                ) : (
                  <Typography
                    variant="caption"
                    sx={{ color: C.textFaint, fontFamily: 'monospace', fontSize: 11.5 }}
                  >
                    {t.path}
                  </Typography>
                )}
              </Box>

              {/* Only where a build is actually possible. An adopted entry would
                  be refused by the server anyway, and saying so after the click
                  is worse than not offering it. */}
              {/* Offered even on an adopted entry and an unreachable host: a pin
                  is CONFIGURATION. Refusing to show it because a laptop is
                  asleep would mean the one thing you can always do — record
                  which commit this tool should be at — is unavailable exactly
                  when a bad build has made the question urgent. */}
              <Tooltip title="Hold this tool at a commit, or switch this host to a build it already has.">
                <Button
                  size="small"
                  onClick={() =>
                    setVersioning({
                      tool: t.tool,
                      host: t.host,
                      ref: t.ref,
                      pin: t.pin,
                      adopted: !!t.adopted,
                    })
                  }
                >
                  Version
                </Button>
              </Tooltip>

              {!t.adopted && !t.error && (
                <Tooltip
                  title={
                    t.present
                      ? 'Pull the pinned ref and rebuild. Minutes of full-machine compile; it runs on the daemon and keeps going if you close the dialog.'
                      : 'Clone and build it on this host.'
                  }
                >
                  <Button size="small" onClick={() => setBuilding({ tool: t.tool, host: t.host })}>
                    {t.present ? 'Rebuild' : 'Build'}
                  </Button>
                </Tooltip>
              )}
            </Stack>
          </Row>
        );
      })}
      <ToolVersionDialog
        open={!!versioning}
        tool={versioning?.tool}
        host={versioning?.host}
        ref_={versioning?.ref}
        pin={versioning?.pin}
        adopted={versioning?.adopted}
        onClose={() => setVersioning(null)}
      />
      <BuildDialog
        open={!!building}
        tool={building?.tool}
        host={building?.host}
        onClose={() => {
          setBuilding(null)
          // A finished build changes the version and the drift answer, so the
          // table behind the dialog is stale the moment it closes — and so is
          // the rollback list, which just gained an entry.
          void qc.invalidateQueries({ queryKey: ['tooling'] })
          void qc.invalidateQueries({ queryKey: ['installedBuilds'] })
        }}
      />
    </Panel>
  );
}
