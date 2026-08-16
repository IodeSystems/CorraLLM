import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
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
import AddIcon from '@mui/icons-material/Add'
import SearchIcon from '@mui/icons-material/Search'
import { Panel, PageHeader, Row } from '@/Panel'
import { ProviderDialog } from '@/ProviderDialog'
import type { ProviderInitial } from '@/ProviderDialog'
import { CatalogDialog } from '@/CatalogDialog'
import { graphql } from '@/gql'
import type { ProvidersQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'

/**
 * Providers (P21): an upstream endpoint and the accounts held against it.
 *
 * The page is a LIST plus two dialogs, not a form. Adding a provider asks for
 * things nobody knows by heart — host, port, the path prefix in front of /v1 —
 * so that lives in a modal with a typeahead over known endpoints; and choosing
 * which of a provider's three hundred models to serve is a search problem, so
 * that lives in a modal of its own, opened FROM the provider that holds the
 * catalogue. Neither belongs inline under a heading.
 *
 * Secrets are NEVER shown. A credential carries a secretRef — the name of an
 * entry in ~/.corrallm/credentials, referenced from config as ${NAME} — and the
 * value is written through a separate write-only endpoint. That is what keeps
 * the config document safe to read, back up and share.
 */
const ProvidersDoc = graphql(/* GraphQL */ `
  query Providers {
    corrallm {
      listProviders {
        secrets
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

type ProviderRow = NonNullable<
  NonNullable<ProvidersQuery['corrallm']>['listProviders']
>['providers'][number]

const SelectionsDoc = graphql(/* GraphQL */ `
  query Selections {
    corrallm {
      listSelections {
        selections {
          provider
          credential
          model
          upstream
          quality
          serving
          lanes {
            lane
            order
          }
        }
      }
    }
  }
`)

const UnassignDoc = graphql(/* GraphQL */ `
  mutation UnassignFromList($body: corrallm_UnassignModelInputBodyInput!) {
    corrallm {
      unassignModel(body: $body) {
        ok
        message
      }
    }
  }
`)

/**
 * Everything chosen off a directory, in one list.
 *
 * The per-provider dialog is where you CHOOSE; this is where you see what you
 * chose across all of them and where each one landed. Both write the same rows
 * — there is no separate approvals page any more, because there is nothing to
 * approve: a model is selected or it is not.
 */
function AssignedModels() {
  const qc = useQueryClient()
  const { data } = useQuery({
    queryKey: ['selections'],
    queryFn: () => gqlClient.request(SelectionsDoc),
  })
  const unassign = useMutation({
    mutationFn: (body: { provider: string; credential: string; model: string }) =>
      gqlClient.request(UnassignDoc, { body }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['selections'] })
      qc.invalidateQueries({ queryKey: ['catalog'] })
    },
  })
  const rows = data?.corrallm?.listSelections?.selections ?? []

  return (
    <Panel title={`Assigned models (${rows.length})`} flush>
      {rows.length === 0 ? (
        <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
          None yet. <strong>Browse</strong> a provider above and add the models you want — with a
          lane and priority if they should join one.
        </Typography>
      ) : (
        rows.map((r) => (
          <Row key={`${r.provider}/${r.credential}/${r.model}`}>
            <Stack direction="row" spacing={1.5} alignItems="center" flexWrap="wrap" useFlexGap>
              <Box sx={{ minWidth: 220, flex: 1 }}>
                <Typography variant="body2" sx={{ fontFamily: 'monospace', fontSize: 12.5 }}>
                  {r.model}
                </Typography>
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {r.provider} / {r.credential}
                  {r.upstream ? ` · ${r.upstream}` : ' · placement only'}
                </Typography>
              </Box>
              <Stack direction="row" spacing={0.75} flexWrap="wrap" useFlexGap>
                {r.lanes.map((l) => (
                  <Chip key={l.lane} size="small" label={`${l.lane} @ ${l.order}`} />
                ))}
                {r.lanes.length === 0 && (
                  <Tooltip title="Servable by name, but in no lane — nothing routes to it automatically.">
                    <Chip size="small" variant="outlined" label="no lane" />
                  </Tooltip>
                )}
                {Number(r.quality ?? 0) > 0 && (
                  <Chip size="small" variant="outlined" label={`q${r.quality}`} />
                )}
                {!r.serving && (
                  <Tooltip title="On record, but not in the served registry — the provider or its id may have gone away.">
                    <Chip size="small" color="warning" label="not serving" />
                  </Tooltip>
                )}
              </Stack>
              <Button
                size="small"
                color="error"
                disabled={unassign.isPending}
                onClick={() =>
                  unassign.mutate({
                    provider: r.provider,
                    credential: r.credential,
                    model: r.model,
                  })
                }
              >
                Remove
              </Button>
            </Stack>
          </Row>
        ))
      )}
    </Panel>
  )
}

function ProvidersPage() {
  const [addOpen, setAddOpen] = useState(false)
  const [edit, setEdit] = useState<ProviderInitial | null>(null)
  const [browse, setBrowse] = useState<{ provider: string; credential: string } | null>(null)

  const { data, isLoading, error } = useQuery({
    queryKey: ['providers'],
    queryFn: () => gqlClient.request(ProvidersDoc),
  })

  if (isLoading) return <CircularProgress sx={{ m: 4 }} />
  if (error)
    return (
      <Alert severity="error" sx={{ m: 2 }}>
        {String(error)}
      </Alert>
    )

  const list = data?.corrallm?.listProviders
  const providers = list?.providers ?? []
  const pools = list?.pools ?? []
  const secrets = list?.secrets ?? []

  const toInitial = (p: ProviderRow): ProviderInitial => ({
    extension: p.extension,
    name: p.name,
    host: p.host,
    port: String(p.port ?? '443'),
    basePath: p.basePath ?? '',
    manual: p.manual ?? false,
    directory: p.directory
      ? {
          freeOnly: p.directory.freeOnly ?? false,
          minContext: String(p.directory.minContext ?? '0'),
        }
      : null,
  })

  return (
    <Box sx={{ p: 2, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <PageHeader title="Providers">
        <Button
          size="small"
          variant="contained"
          startIcon={<AddIcon />}
          onClick={() => setAddOpen(true)}
        >
          Add provider
        </Button>
        <Typography variant="body2" sx={{ opacity: 0.75 }}>
          An upstream endpoint and the accounts held against it. Secrets live in the credential
          store and are referenced as <code>${'{NAME}'}</code> — this page can set one, never show
          one.
        </Typography>
      </PageHeader>

      <Panel title={`Configured (${providers.length})`} flush>
        {providers.length === 0 && (
          <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
            None yet. <strong>Add provider</strong> starts from a table of known OpenAI-compatible
            endpoints, or takes a custom one.
          </Typography>
        )}
        {providers.map((p) => {
          // A provider with no explicit credentials still has one — the
          // implicit "default" carrying the provider's own headers — and the
          // catalogue is fetched per credential, so that is what Browse uses.
          const creds = p.credentials ?? []
          const browseCred = creds[0]?.name ?? 'default'
          return (
            <Row key={`${p.extension}/${p.name}`}>
              <Stack direction="row" spacing={1.5} alignItems="center" flexWrap="wrap" useFlexGap>
                <Box sx={{ minWidth: 200 }}>
                  <Typography variant="subtitle2">
                    {p.name}{' '}
                    <Typography component="span" variant="caption" sx={{ color: C.textFaint }}>
                      {p.extension}
                    </Typography>
                  </Typography>
                  <Typography variant="caption" sx={{ color: C.textMuted }}>
                    {p.host}:{p.port}
                    {p.basePath}
                  </Typography>
                </Box>

                <Stack direction="row" spacing={0.75} flexWrap="wrap" useFlexGap sx={{ flex: 1 }}>
                  {/* Where this provider's models come from. A directory filter
                      is NOT one of them — it only changes what Browse opens
                      pre-filtered to. */}
                  {Number(p.provides ?? 0) > 0 && (
                    <Tooltip title="Models written into the extension YAML by hand. Edit them there, not here.">
                      <Chip size="small" variant="outlined" label={`${p.provides} declared`} />
                    </Tooltip>
                  )}
                  {p.manual && (
                    <Tooltip title="Models are chosen one at a time off this provider's directory.">
                      <Chip size="small" variant="outlined" label="hand-picked" />
                    </Tooltip>
                  )}
                  {p.directory?.freeOnly && (
                    <Tooltip title="Browse opens pre-filtered to free models. A browsing default only — it imports nothing.">
                      <Chip size="small" variant="outlined" label="browse: free" />
                    </Tooltip>
                  )}
                  {(p.limits ?? []).map((l, i) => (
                    <Chip
                      key={i}
                      size="small"
                      variant="outlined"
                      label={`provider ${l.usd ? `$${l.usd}` : l.req}/${l.per}`}
                    />
                  ))}
                  {creds.length === 0 && (
                    <Tooltip title="No credentials block: the provider's own proxy headers are the one implicit account.">
                      <Chip size="small" variant="outlined" label="implicit account" />
                    </Tooltip>
                  )}
                  {creds.map((c) => (
                    <Chip
                      key={c.name}
                      size="small"
                      color={c.hasSecret ? 'success' : 'warning'}
                      variant="outlined"
                      label={`${c.name}${
                        c.hasSecret ? '' : ` · ${c.secretRef || 'no secret'} missing`
                      }`}
                    />
                  ))}
                </Stack>

                <Stack direction="row" spacing={1}>
                  <Button
                    size="small"
                    startIcon={<SearchIcon />}
                    onClick={() => setBrowse({ provider: p.name, credential: browseCred })}
                  >
                    Browse
                  </Button>
                  <Button size="small" onClick={() => setEdit(toInitial(p))}>
                    Edit
                  </Button>
                </Stack>
              </Stack>
            </Row>
          )
        })}
      </Panel>

      {pools.length > 0 && (
        <Panel title={`Pools (${pools.length})`} flush>
          <Typography variant="body2" sx={{ px: 2, pt: 1.5, color: C.textMuted }}>
            An extension that satisfies the provider contract by pooling its members&apos;
            catalogues. It holds no endpoint and no key of its own — each model is reached with
            the key of whichever member serves it, and membership is re-derived on every refresh
            so a provider withdrawing a model does not take the pool down.
          </Typography>
          {pools.map((p) => (
            <Row key={p.extension}>
              <Stack direction="row" spacing={1.5} alignItems="center" flexWrap="wrap" useFlexGap>
                <Box sx={{ minWidth: 180 }}>
                  <Typography variant="subtitle2">{p.extension}</Typography>
                  <Typography variant="caption" sx={{ color: C.textMuted }}>
                    over {p.sources.join(', ') || 'no members'}
                  </Typography>
                </Box>
                <Stack direction="row" spacing={0.75} flexWrap="wrap" useFlexGap sx={{ flex: 1 }}>
                  <Chip
                    size="small"
                    color={Number(p.models) > 0 ? 'success' : 'warning'}
                    label={`${p.models} model${Number(p.models) === 1 ? '' : 's'}`}
                  />
                  {p.freeOnly && <Chip size="small" variant="outlined" label="free only" />}
                  {Number(p.minContext) > 0 && (
                    <Chip size="small" variant="outlined" label={`ctx ≥ ${p.minContext}`} />
                  )}
                  {Number(p.limit) > 0 && (
                    <Tooltip title="Cap on the pool as a whole, largest window first — not per member, so one verbose provider cannot crowd out the rest.">
                      <Chip size="small" variant="outlined" label={`top ${p.limit}`} />
                    </Tooltip>
                  )}
                  {p.lanes.map((l) => (
                    <Tooltip
                      key={l.lane}
                      title="Every model in the pool joins this lane at this priority. Ask for the lane to get the pool."
                    >
                      <Chip size="small" label={`${l.lane} @ ${l.order}`} />
                    </Tooltip>
                  ))}
                </Stack>
              </Stack>
            </Row>
          ))}
        </Panel>
      )}

      <AssignedModels />

      <Panel title={`Credential store (${secrets.length})`}>
        <Typography variant="body2" sx={{ opacity: 0.75, mb: 1 }}>
          Names only. No endpoint returns a value — that is what keeps the config document safe to
          read and share.
        </Typography>
        <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
          {secrets.map((s) => (
            <Chip key={s} size="small" label={s} />
          ))}
          {secrets.length === 0 && (
            <Typography variant="caption" sx={{ opacity: 0.7 }}>
              empty
            </Typography>
          )}
        </Stack>
      </Panel>

      <ProviderDialog open={addOpen} onClose={() => setAddOpen(false)} secrets={secrets} />
      <ProviderDialog
        open={edit != null}
        onClose={() => setEdit(null)}
        initial={edit ?? undefined}
        secrets={secrets}
      />
      {browse && (
        <CatalogDialog
          open
          onClose={() => setBrowse(null)}
          provider={browse.provider}
          credential={browse.credential}
        />
      )}
    </Box>
  )
}

export const Route = createFileRoute('/providers')({ component: ProvidersPage })
