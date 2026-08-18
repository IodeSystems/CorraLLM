import { useQuery } from '@tanstack/react-query'
import { Alert, Box, Chip, CircularProgress, Stack, Tooltip, Typography } from '@mui/material'
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
        }
      }
    }
  }
`)

// A build is minutes long, so there is no button for it: a request the browser
// holds open for a quarter of an hour is the wrong shape, and a reload would
// lose it. The command is shown instead, which is honest about where the work
// happens and is copy-pasteable.
function buildHint(tool: string, host: string) {
  return `corrallm tools build ${tool} --server ${host}`
}

export function ToolingPanel() {
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
              alignItems="baseline"
              flexWrap="wrap"
              useFlexGap
              sx={{ width: '100%' }}
            >
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
                  <Stack direction="row" spacing={0.75} alignItems="center" flexWrap="wrap" useFlexGap>
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

              <Stack direction="row" spacing={0.75} flexWrap="wrap" useFlexGap sx={{ flex: 1 }}>
                {t.adopted && (
                  <Tooltip title="corrallm does not own this install and will never write to it — no build, no dependency install. Drop installedAt to manage it here.">
                    <Chip size="small" variant="outlined" label="adopted" />
                  </Tooltip>
                )}
                {t.behind && (
                  <Tooltip title={`Upstream is at ${t.remoteHead ?? 'a newer commit'}`}>
                    <Chip size="small" color="warning" variant="outlined" label="behind" />
                  </Tooltip>
                )}
                {!t.behind && t.present && !t.driftError && t.remoteHead && (
                  <Chip size="small" color="success" variant="outlined" label="current" />
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
                  refuse it, and saying so after the click is worse than not
                  offering it. */}
              {!t.adopted && !t.error && (t.behind || !t.present) && (
                <Box sx={{ flexBasis: '100%' }}>
                  <Tooltip title="A CUDA build is minutes of full-machine compile that replaces a binary models may be spawning, so it stays a deliberate command rather than a button.">
                    <Typography
                      variant="caption"
                      sx={{ color: C.textMuted, fontFamily: 'monospace', fontSize: 11.5 }}
                    >
                      $ {buildHint(t.tool, t.host)}
                    </Typography>
                  </Tooltip>
                </Box>
              )}
            </Stack>
          </Row>
        )
      })}
    </Panel>
  )
}
