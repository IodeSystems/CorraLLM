import { useQuery } from '@tanstack/react-query'
import { graphql } from '@/gql'
import type { ProvidersQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'

/**
 * The one query behind Setup, and the shapes derived from it.
 *
 * Setup's panels used to live in a single 1020-line route file, which is why
 * this query and the functions that reshape it had nowhere better to be. They
 * are here so that each tab can ask for the data itself — one `['providers']`
 * key, deduplicated by react-query, so a tab is a component that mounts rather
 * than a prop tree the page has to thread.
 */
export const ProvidersDoc = graphql(/* GraphQL */ `
  query Providers {
    corrallm {
      overview {
        lanes {
          name
          members {
            model
          }
        }
        extensions {
          name
          cmd
          server
          provides
          notes
        }
      }
      listProviders {
        secrets
        local {
          name
          barePrecedence
          notes
          models {
            id
            served
            bare
            type
            server
            hasCmd
          }
        }
        pools {
          extension
          sources
          freeOnly
          minContext
          limit
          models
          lanes {
            lane
            order
          }
        }
        providers {
          extension
          name
          host
          port
          basePath
          manual
          provides
          directory {
            freeOnly
            minContext
          }
          limits {
            req
            usd
            sec
            per
          }
          credentials {
            name
            secretRef
            headerName
            allow
            hasSecret
            limits {
              req
              usd
              sec
              per
            }
          }
        }
      }
    }
  }
`)

type ListProviders = NonNullable<NonNullable<ProvidersQuery['corrallm']>['listProviders']>
export type ProviderRow = ListProviders['providers'][number]
export type PoolRow = ListProviders['pools'][number]
export type LocalProvider = ListProviders['local'][number]
type Overview = NonNullable<NonNullable<ProvidersQuery['corrallm']>['overview']>
export type ExtensionRow = Overview['extensions'][number]
export type LaneRow = Overview['lanes'][number]

/** Everything Setup reads, in one place, fetched once however many tabs ask. */
export function useSetupData() {
  return useQuery({
    queryKey: ['providers'],
    queryFn: () => gqlClient.request(ProvidersDoc),
  })
}

/**
 * Providers live INSIDE extensions — `extensions.free.providers.groq` — and the
 * page used to contradict that by listing extensions, their providers and their
 * pools as three flat panels, so `free` appeared three times and its three
 * providers appeared unattached. One tree instead: the extension is the
 * container, its providers are its members, and a pool is a property of the
 * extension rather than a separate object.
 */
export type Integration = {
  name: string
  ext?: ExtensionRow
  pool?: PoolRow
  provs: ProviderRow[]
}

export function byExtension(
  extensions: readonly ExtensionRow[],
  pools: readonly PoolRow[],
  providers: readonly ProviderRow[],
): Integration[] {
  const groups = new Map<string, Integration>()
  const get = (name: string) => {
    let g = groups.get(name)
    if (!g) {
      g = {
        name,
        ext: extensions.find((x) => x.name === name),
        pool: pools.find((x) => x.extension === name),
        provs: [],
      }
      groups.set(name, g)
    }
    return g
  }
  // Every declared extension gets a group even with no providers: claude and
  // oidio provide models directly, and omitting them would hide half of what
  // is configured.
  for (const x of extensions) get(x.name)
  for (const pr of providers) get(pr.extension).provs.push(pr)
  return [...groups.values()].sort(
    (a, b) => b.provs.length - a.provs.length || a.name.localeCompare(b.name),
  )
}

// A local model with no `server` is a pure proxy — corrallm forwards to it and
// never starts it — so it has no box. It gets its own heading rather than being
// dropped or filed under an arbitrary host, because "which machine runs this"
// having no answer is itself the answer.
export const UNPLACED = '\u0000unplaced'

export function groupByBox<T extends { server?: string | null }>(
  models: readonly T[],
): [string, T[]][] {
  const by = new Map<string, T[]>()
  for (const m of models) {
    const k = m.server || UNPLACED
    by.set(k, [...(by.get(k) ?? []), m])
  }
  // Named boxes first, alphabetically; the unplaced group last.
  return [...by.entries()].sort(([a], [b]) =>
    a === UNPLACED ? 1 : b === UNPLACED ? -1 : a.localeCompare(b),
  )
}
