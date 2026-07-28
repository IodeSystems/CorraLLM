import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Alert, Box, Chip, CircularProgress, Tooltip, Typography } from '@mui/material'
import { Panel, PageHeader, Row, Stat } from '@/Panel'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { fmtBytes } from '@/format'

/**
 * Config: what corrallm was TOLD, as opposed to what it is currently doing.
 *
 * The Overview answers "what is happening right now" — residency, memory, live
 * requests. This answers "what did I declare", which is the question you have
 * when something is missing or routed somewhere surprising, and the answer is
 * usually in a file rather than in the runtime.
 *
 * Everything here is read-only. Editing config from a dashboard means writing a
 * file a human owns, and corrallm only writes machine-owned includes.
 */
const ConfigDoc = graphql(/* GraphQL */ `
  query Config {
    corrallm {
      overview {
        include
        servers {
          server
          maxConcurrent
          devicePool
          agentEndpoints
          pools {
            pool
            totalBytes
            reserveBytes
          }
        }
        models {
          name
          server
          spawnable
          remote
          quality
          type
          capability
          cmd
          upstream
          target
          maxConcurrent
          persistent
          ttl
        }
        lanes {
          name
          members {
            model
          }
        }
      }
    }
  }
`)

// A model's home: the server it draws capacity from, else the fact that it is
// somebody else's machine. Grouping by this is the question the page answers —
// "what runs where" — and it is also the grouping the Agents section will slot
// into once a server can be bound to a remote agent.
const REMOTE = '(remote — not ours to run)'
const UNBOUND = '(no server)'

function ConfigPage() {
  const q = useQuery({
    queryKey: ['config'],
    queryFn: () => gqlClient.request(ConfigDoc),
    refetchInterval: 30000,
  })

  if (q.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress size={20} />
      </Box>
    )
  }
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Alert severity="error">{String(q.error)}</Alert>
      </Box>
    )
  }

  const ov = q.data?.corrallm.overview
  const servers = ov?.servers ?? []
  const models = ov?.models ?? []
  const lanes = ov?.lanes ?? []
  const includes = ov?.include ?? []

  const homeOf = (m: (typeof models)[number]) =>
    m.remote ? REMOTE : m.server ? m.server : UNBOUND

  const byHome = new Map<string, typeof models>()
  for (const m of models) {
    const k = homeOf(m)
    byHome.set(k, [...(byHome.get(k) ?? []), m])
  }
  // Declared servers first (in order), then remote, then anything unbound —
  // roughly most-ours to least-ours.
  const homes = [
    ...servers.map((s) => s.server).filter((s) => byHome.has(s)),
    ...[REMOTE, UNBOUND].filter((k) => byHome.has(k)),
  ]

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Config">
        <Chip size="small" variant="outlined" label={`${models.length} models`} />
        <Chip size="small" variant="outlined" label={`${servers.length} servers`} />
      </PageHeader>

      <Panel
        title="Hosts"
        subtitle="Declared capacity. A budget the scheduler admits against — not a probe."
        flush
      >
        {servers.map((s) => (
          <Row key={s.server}>
            <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
              <Typography variant="subtitle2">{s.server}</Typography>
              {/* No "local" chip: every server IS local until a server can be
                  bound to an agent, so it would carry no information now and be
                  wrong the moment one can. The Agents panel says where that
                  distinction will appear. */}
              {Number(s.maxConcurrent) > 0 && (
                <Chip size="small" variant="outlined" label={`max ${s.maxConcurrent}`} />
              )}
            </Box>
            <Box sx={{ display: 'flex', gap: 3, mt: 0.75, flexWrap: 'wrap' }}>
              {s.pools.map((p) => (
                <Stat
                  key={p.pool}
                  label={
                    p.pool === s.devicePool ? `${p.pool} · device` : p.pool
                  }
                  value={
                    Number(p.reserveBytes) > 0
                      ? `${fmtBytes(Number(p.totalBytes))} − ${fmtBytes(Number(p.reserveBytes))} reserved`
                      : fmtBytes(Number(p.totalBytes))
                  }
                  title={
                    p.pool === s.devicePool
                      ? 'A measured footprint is charged against this pool. Unified-memory hosts point it at their single system pool.'
                      : undefined
                  }
                />
              ))}
            </Box>
          </Row>
        ))}
      </Panel>

      {/* Agents: deliberately honest. Agent mode is not built, so this section
          says so plainly rather than showing an enrollment command that would
          fail. It exists now so the shape of the page is settled and there is
          one obvious place for a second machine to appear. */}
      <Panel
        title="Agents"
        subtitle="Other machines this daemon spawns and evicts on"
        badge={<Chip size="small" variant="outlined" color="warning" label="not yet available" />}
        flush
      >
        {servers
          .filter((s) => (s.agentEndpoints ?? []).length > 0)
          .map((s) => (
            <Row key={s.server}>
              <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                <Typography variant="subtitle2">{s.server}</Typography>
                <Chip size="small" variant="outlined" color="warning" label="declared · cannot spawn yet" />
              </Box>
              <Typography variant="caption" sx={{ color: C.textFaint, display: 'block', mt: 0.5 }}>
                Addresses, in preference order. Several are normal — a LAN address, a VPN address
                and an external one can all be valid at once; which works depends on where this
                daemon is sitting.
              </Typography>
              <Box sx={{ mt: 0.5 }}>
                {(s.agentEndpoints ?? []).map((e) => (
                  <Typography key={e} variant="body2" sx={{ fontFamily: 'monospace', fontSize: 12 }}>
                    {e}
                  </Typography>
                ))}
              </Box>
            </Row>
          ))}
        <Row>
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            No agents enrolled — and none can be yet. <b>Agent mode is not implemented.</b> There
            is no <code>corrallm agent</code> command, no enrollment endpoint, and no installer, so
            there is nothing to paste into a second machine's terminal today.
          </Typography>
          <Typography variant="body2" sx={{ color: C.textMuted, mt: 1.5 }}>
            What already works, and is what an agent will be built on:
          </Typography>
          <Box component="ul" sx={{ color: C.textMuted, mt: 0.5, mb: 0, pl: 3 }}>
            <li>
              <Typography variant="body2">
                A server declares its capacity as pools, and a unified-memory host names its single
                pool as the <code>devicePool</code> — so an Apple-silicon box is already expressible.
              </Typography>
            </li>
            <li>
              <Typography variant="body2">
                Backend process control sits behind a host interface, so a remote implementation
                slots in beside the local one.
              </Typography>
            </li>
            <li>
              <Typography variant="body2">
                Config reloads on SIGHUP, and <code>include:</code> merges a machine-owned file — so
                an agent's models can be written to disk and served without a restart.
              </Typography>
            </li>
          </Box>
          <Typography variant="body2" sx={{ color: C.textFaint, mt: 1.5 }}>
            Still to build: the agent binding on a server (an agent has several addresses — LAN,
            external, VPN — so it is a list, not one host), the <code>corrallm agent</code> command,
            the remote host client, per-host capacity probing on macOS, and what happens to
            residency when an agent goes away mid-flight.
          </Typography>
        </Row>
      </Panel>

      {homes.map((home) => {
        const ms = byHome.get(home) ?? []
        return (
          <Panel
            key={home}
            title={home}
            subtitle={
              home === REMOTE
                ? 'Forwarded to a host we do not run. No process, no residency.'
                : home === UNBOUND
                  ? 'Declared without a server.'
                  : 'Spawned and evicted here'
            }
            badge={<Chip size="small" variant="outlined" label={`${ms.length}`} />}
            flush
          >
            {ms
              .slice()
              .sort((a, b) => Number(b.quality) - Number(a.quality) || a.name.localeCompare(b.name))
              .map((m) => (
                <Row key={m.name}>
                  <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                    <Typography variant="subtitle2">{m.name}</Typography>
                    {/* The alias. corrallm routes on the served name; the backend
                        knows it by this one. */}
                    {m.upstream && m.upstream !== m.name && (
                      <Tooltip title="The id the BACKEND knows this model by. corrallm routes on the served name and rewrites it on the way out.">
                        <Chip size="small" variant="outlined" label={`→ ${m.upstream}`} />
                      </Tooltip>
                    )}
                    <Chip size="small" color="info" variant="outlined" label={m.capability} />
                    {m.persistent && <Chip size="small" variant="outlined" label="pinned" />}
                    {m.ttl && <Chip size="small" variant="outlined" label={`ttl ${m.ttl}`} />}
                  </Box>
                  <Box sx={{ display: 'flex', gap: 3, mt: 0.75, flexWrap: 'wrap' }}>
                    <Stat label="Quality" value={m.quality} />
                    <Stat label="Type" value={m.type} />
                    <Stat label="Slots" value={m.maxConcurrent} />
                    <Stat label="Target" value={m.target || '—'} />
                  </Box>
                  {m.cmd && (
                    <Box
                      component="pre"
                      sx={{
                        mt: 0.75,
                        mb: 0,
                        p: 1,
                        bgcolor: C.raised,
                        border: `1px solid ${C.border}`,
                        borderRadius: 1,
                        fontSize: 11,
                        color: C.textMuted,
                        overflowX: 'auto',
                        whiteSpace: 'pre',
                      }}
                    >
                      {m.cmd}
                    </Box>
                  )}
                </Row>
              ))}
          </Panel>
        )
      })}

      <Panel
        title="Lanes"
        subtitle="Named fallback lists. Requesting a lane allows substitution; requesting a model pins it."
        flush
      >
        {lanes.length === 0 ? (
          <Row>
            <Typography variant="body2" sx={{ color: C.textFaint }}>
              No lanes declared.
            </Typography>
          </Row>
        ) : (
          lanes.map((l) => (
            <Row key={l.name}>
              <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                <Typography variant="subtitle2">{l.name}</Typography>
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {l.members.map((mem) => mem.model).join('  →  ')}
                </Typography>
              </Box>
            </Row>
          ))
        )}
      </Panel>

      <Panel
        title="Included files"
        subtitle="Merged into the top-level config, weakest first; the hand-written file always wins"
        flush
      >
        {includes.length === 0 ? (
          <Row>
            <Typography variant="body2" sx={{ color: C.textFaint }}>
              None. Everything is declared in the one config file.
            </Typography>
          </Row>
        ) : (
          includes.map((f) => (
            <Row key={f}>
              <Typography variant="body2" sx={{ fontFamily: 'monospace' }}>
                {f}
              </Typography>
            </Row>
          ))
        )}
      </Panel>
    </Box>
  )
}

export const Route = createFileRoute('/config')({ component: ConfigPage })
