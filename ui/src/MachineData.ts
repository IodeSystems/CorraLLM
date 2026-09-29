import { useQuery } from '@tanstack/react-query'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { seriesColor } from '@/theme'

/** One pool of one server as the scheduler accounts for it. */
export type PoolLedger = {
  server: string
  pool: string
  budget: number
  used: number
  reserve: number
}
/** A resident backend's reservation, plus what the device says it really costs. */
export type ModelUse = {
  model: string
  server: string
  pools: { pool: string; bytes: number }[]
  measuredBytes: number
}
/** A measured device reading. `available: false` is "no reading", never "zero". */
export type DeviceMem = {
  available: boolean
  name: string
  totalBytes: number
  usedBytes: number
  freeBytes: number
}
/** A device reading plus the pool that budgets it ('' when nothing claims it). */
export type GpuMem = DeviceMem & { uuid: string; pool: string }
export type ServerShape = {
  server: string
  devicePool: string
  /**
   * Whether being full has COST anything here in the last day.
   *
   * A full bar was drawn exactly like an empty one, and the percentage alone
   * cannot settle whether it matters — because usually it does not. A device
   * pool at 96% normally means a model is RESIDENT, which is the box working
   * as intended. It becomes a problem when something else wants the space, and
   * that is an event, not a level (Lenny run 9: "I left it alone, but I did so
   * guessing, not knowing").
   */
  madeRoomCount?: number
  madeRoomEvicted?: string
  madeRoomFor?: string
}

/**
 * The one query behind /machines and /machines/<name>, and the derivations both
 * pages read it through.
 *
 * It lives here rather than on the list page because the two pages ask the SAME
 * question of the server and must not disagree about the answer. Sharing the
 * document and the `['config']` query key also means clicking a machine renders
 * from cache instead of re-fetching a survey of every host — the list already
 * paid for it.
 *
 * The derivations below are the part that was worth extracting. Joining declared
 * capacity (overview.servers) to the live ledger (residency.servers) to a
 * measured device reading (residency.gpus) is three-way and easy to get subtly
 * wrong; there is now exactly one copy of it.
 */
const ConfigDoc = graphql(/* GraphQL */ `
  query Config {
    corrallm {
      overview {
        servers {
          server
          maxConcurrent
          devicePool
          agentEndpoints
          agentStatus
          agentLastSeen
          noProcessMemory
          ephemeral
          notes
          pools {
            pool
            totalBytes
            reserveBytes
          }
        }
        models {
          name
          server
          remote
        }
        extensions {
          name
          cmd
          server
          provides
          notes
          state
          paused
          pauseReason
          pinned
          inFlight
          draining
        }
      }
      residency {
        servers {
          server
          pools {
            pool
            budget
            used
          }
          madeRoomCount
          madeRoomLastMs
          madeRoomEvicted
          madeRoomFor
        }
        gpus {
          available
          name
          uuid
          pool
          totalBytes
          usedBytes
          freeBytes
        }
        host {
          available
          name
          totalBytes
          usedBytes
          freeBytes
        }
        models {
          name
          modelName
          server
          state
          refs
          remote
          persistent
          lastUsedMs
          footprintMiB
          usage {
            pool
            bytes
          }
        }
      }
    }
  }
`)

export function useMachines() {
  return useQuery({
    queryKey: ['config'],
    queryFn: () => gqlClient.request(ConfigDoc),
    refetchInterval: 30000,
  })
}

export type MachinePool = {
  pool: string
  /** 'device' | 'system' | 'device + system' — what this pool IS on this box. */
  role: string
  /** What the scheduler may fill: the declared total minus the reserve. */
  budget: number
  /** What it has spoken for right now. */
  used: number
  /** Declared headroom, held out of the budget. */
  reserve: number
  /** The declared physical total. */
  total: number
  /** The measured card backing this pool, when one can honestly be paired to it. */
  device?: GpuMem
}

export type MachineResident = {
  name: string
  modelName: string
  state: string
  refs: number
  remote: boolean
  persistent: boolean
  lastUsedMs: number
  usage: { pool: string; bytes: number }[]
}

export type MachineExtension = {
  name: string
  state: string
  provides: string[]
  paused: boolean
  pauseReason: string
  pinned: boolean
  inFlight: number
  draining: boolean
  /** A remote integration has no local process, so it holds nothing here. */
  local: boolean
}

export type Machine = {
  server: string
  devicePool: string
  maxConcurrent: number
  agentEndpoints: string[]
  /** up | down | unknown | away | local. */
  agentStatus: string
  agentLastSeen: number
  noProcessMemory: boolean
  /** Comes and goes (a laptop): not reporting in is 'away', not a fault. */
  ephemeral: boolean
  notes: string
  pools: MachinePool[]
  resident: MachineResident[]
  extensions: MachineExtension[]
  /** Declared models whose home this machine is, resident or not. */
  declaredModels: number
  madeRoomCount: number
  madeRoomLastMs: number
  madeRoomEvicted: string
  madeRoomFor: string
}

type Data = NonNullable<ReturnType<typeof useMachines>['data']>

/**
 * The device probe reads THIS machine, so a measured card can only honestly be
 * attributed to a server while exactly one server is local. With several, say
 * nothing rather than pick one — a measured bar under the wrong box is worse
 * than no measured bar. (MemoryPanel made this call first; it is the same rule,
 * now in one place, and MemoryPanel reads it from here.)
 */
export function localServer(servers: { server: string }[]): string | undefined {
  return servers.length === 1 ? servers[0].server : undefined
}

/**
 * The measured reading that belongs to (server, pool), or undefined.
 *
 * A card that declares which pool backs it wins outright. Falling back to "the
 * only card" is right only when there IS one — the assumption that broke when a
 * second GPU arrived and every reading silently described the new card.
 */
export function pairDevice(opts: {
  gpus: GpuMem[]
  servers: { server: string; devicePool: string }[]
  server: string
  pool: string
}): GpuMem | undefined {
  const { gpus, servers, server, pool } = opts
  if (localServer(servers) !== server) return undefined
  const bound = gpus.find((g) => g.pool && g.pool === pool)
  if (bound) return bound
  const devicePool = servers.find((s) => s.server === server)?.devicePool ?? 'gpu0'
  if (pool !== devicePool) return undefined
  return gpus.length === 1 && gpus[0].available ? gpus[0] : undefined
}

const num = (v: string | number | null | undefined) => Number(v ?? 0)

const dev = (d?: {
  available: boolean
  name: string
  totalBytes: string
  usedBytes: string
  freeBytes: string
}): DeviceMem => ({
  available: !!d?.available,
  name: d?.name ?? '',
  totalBytes: num(d?.totalBytes),
  usedBytes: num(d?.usedBytes),
  freeBytes: num(d?.freeBytes),
})

/**
 * Everything both pages need, derived once.
 *
 * `colorOf` is assigned over the full sorted model list, never over the subset
 * in one bar: a colour must not change when a model loads or unloads, or every
 * bar repaints and the eye reads a change that did not happen.
 */
export function derive(data?: Data) {
  const ov = data?.corrallm.overview
  const res = data?.corrallm.residency
  const declared = ov?.servers ?? []
  const models = ov?.models ?? []
  const extensions = ov?.extensions ?? []

  const colorIndex = new Map(
    models
      .map((m) => m.name)
      .sort()
      .map((n, i) => [n, i]),
  )
  const colorOf = (name: string) => seriesColor(colorIndex.get(name) ?? 0)

  const gpus: GpuMem[] = (res?.gpus ?? []).map((g) => ({
    ...dev(g),
    uuid: g.uuid ?? '',
    pool: g.pool ?? '',
  }))
  const host = dev(res?.host)

  // The flat shapes MemoryPanel already speaks. Kept flat because that panel
  // groups by server itself and scoping it to one machine is then a filter.
  const reserveByPool = new Map(
    declared.flatMap((s) => s.pools.map((p) => [`${s.server}/${p.pool}`, num(p.reserveBytes)])),
  )
  const totalByPool = new Map(
    declared.flatMap((s) => s.pools.map((p) => [`${s.server}/${p.pool}`, num(p.totalBytes)])),
  )
  const memPools: PoolLedger[] = (res?.servers ?? []).flatMap((s) =>
    s.pools.map((p) => ({
      server: s.server,
      pool: p.pool,
      budget: num(p.budget),
      used: num(p.used),
      reserve: reserveByPool.get(`${s.server}/${p.pool}`) ?? 0,
    })),
  )
  const memModels: ModelUse[] = (res?.models ?? []).map((m) => ({
    model: m.modelName,
    server: m.server,
    pools: m.usage.map((u) => ({ pool: u.pool, bytes: num(u.bytes) })),
    measuredBytes: num(m.footprintMiB) * 1024 * 1024,
  }))
  const liveByServer = new Map((res?.servers ?? []).map((s) => [s.server, s]))
  const memServers: ServerShape[] = declared.map((s) => {
    const live = liveByServer.get(s.server)
    return {
      server: s.server,
      devicePool: s.devicePool,
      madeRoomCount: num(live?.madeRoomCount),
      madeRoomEvicted: live?.madeRoomEvicted ?? '',
      madeRoomFor: live?.madeRoomFor ?? '',
    }
  })

  const machines: Machine[] = declared.map((s) => {
    const live = liveByServer.get(s.server)
    const livePools = new Map((live?.pools ?? []).map((p) => [p.pool, p]))
    const pools: MachinePool[] = s.pools
      .map((p) => {
        const l = livePools.get(p.pool)
        const device = pairDevice({ gpus, servers: declared, server: s.server, pool: p.pool })
        const isDevice = !!device || p.pool === s.devicePool
        return {
          pool: p.pool,
          // A unified-memory box (Apple silicon) has ONE pool that is both its
          // device and its system memory. Saying so beats implying a GPU pool
          // that does not exist.
          role: isDevice && s.pools.length === 1 ? 'device + system' : isDevice ? 'device' : 'system',
          budget: l ? num(l.budget) : Math.max(0, num(p.totalBytes) - num(p.reserveBytes)),
          used: num(l?.used),
          reserve: num(p.reserveBytes),
          total: totalByPool.get(`${s.server}/${p.pool}`) ?? num(p.totalBytes),
          device,
        }
      })
      // The device pool first: it is the scarce one, and the one eviction
      // fights over.
      .sort(
        (a, b) =>
          Number(b.pool === s.devicePool) - Number(a.pool === s.devicePool) ||
          a.pool.localeCompare(b.pool),
      )

    return {
      server: s.server,
      devicePool: s.devicePool,
      maxConcurrent: num(s.maxConcurrent),
      agentEndpoints: s.agentEndpoints ?? [],
      agentStatus: s.agentStatus,
      agentLastSeen: num(s.agentLastSeen),
      noProcessMemory: !!s.noProcessMemory,
      ephemeral: !!s.ephemeral,
      notes: s.notes ?? '',
      pools,
      // A remote model is served by a host corrallm does not run: it holds no
      // residency and must never be counted as loaded here.
      resident: (res?.models ?? [])
        .filter((m) => m.server === s.server && !m.remote)
        .map((m) => ({
          name: m.name,
          modelName: m.modelName,
          state: m.state,
          refs: num(m.refs),
          remote: !!m.remote,
          persistent: !!m.persistent,
          lastUsedMs: num(m.lastUsedMs),
          usage: m.usage.map((u) => ({ pool: u.pool, bytes: num(u.bytes) })),
        })),
      extensions: extensions
        .filter((e) => e.server === s.server)
        .map((e) => ({
          name: e.name,
          state: e.state,
          provides: e.provides ?? [],
          paused: !!e.paused,
          pauseReason: e.pauseReason ?? '',
          pinned: !!e.pinned,
          inFlight: num(e.inFlight),
          draining: !!e.draining,
          local: !!e.cmd,
        })),
      declaredModels: models.filter((m) => m.server === s.server && !m.remote).length,
      madeRoomCount: num(live?.madeRoomCount),
      madeRoomLastMs: num(live?.madeRoomLastMs),
      madeRoomEvicted: live?.madeRoomEvicted ?? '',
      madeRoomFor: live?.madeRoomFor ?? '',
    }
  })

  // Cards present in the machine that no pool budgets. The scheduler cannot see
  // them at all, which is the state a freshly installed GPU is in — and a GPU
  // the scheduler cannot see looks exactly like a GPU that is not installed.
  // They belong to no machine row, so the list surfaces them itself.
  const unclaimed = gpus.filter((g) => g.available && !g.pool)

  return {
    machines,
    gpus,
    host,
    unclaimed,
    anyGpu: gpus.some((g) => g.available),
    colorOf,
    memPools,
    memModels,
    memServers,
    modelCount: models.length,
  }
}

/**
 * Whether being full on this box has COST anything, in the only terms that can
 * settle it: what it cost. A percentage cannot — see UtilizationBar's note and
 * ServerView.MadeRoomCount in internal/api/handlers.go.
 */
export function madeRoomNote(m: Machine): string {
  if (m.madeRoomCount <= 0) {
    return 'Nothing has been unloaded here today to fit something else, so however full this reads, it has cost nothing.'
  }
  const traded =
    m.madeRoomEvicted && m.madeRoomFor
      ? ` Last unloaded ${m.madeRoomEvicted} to fit ${m.madeRoomFor}.`
      : ''
  return `Made room ${m.madeRoomCount} ${m.madeRoomCount === 1 ? 'time' : 'times'} today — a load here had to unload something first.${traded}`
}

/** What each agent state means for whether anything can run there. */
export const AGENT_STATUS_HINT: Record<string, string> = {
  up: 'heartbeating; models can be spawned here',
  down: 'stopped reporting in — new spawns are refused here, but its config and any running backends are left alone',
  // Last-seen lives in memory, so "unknown" is "not since corrallm started",
  // not "never" — after a restart a machine that reported in all week reads so.
  unknown: 'configured but has not reported in since corrallm started; a spawn will be attempted and will say why if it fails',
  away: 'marked as coming and going, and not here now — its models fall through to the next choice until it reports in',
  local: 'this machine',
}

export function agentColor(status: string): 'success' | 'error' | 'warning' | 'default' {
  if (status === 'up') return 'success'
  if (status === 'down') return 'error'
  if (status === 'local' || status === 'away') return 'default'
  return 'warning'
}

/** The states a resident backend can be in, in the dashboard's shared colours. */
export function residentColor(
  state: string,
): 'success' | 'info' | 'warning' | 'error' | 'default' {
  switch (state) {
    case 'ready':
      return 'success'
    case 'loading':
      return 'info'
    case 'evicting':
    case 'draining':
      return 'warning'
    case 'failed':
      return 'error'
    default:
      return 'default'
  }
}
