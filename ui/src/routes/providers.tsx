import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
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
        providers {
          extension
          name
          host
          port
          basePath
          manual
          provides
          discover {
            freeOnly
            minContext
            limit
            quality
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
            approvalRequired
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
  const secrets = list?.secrets ?? []

  const toInitial = (p: ProviderRow): ProviderInitial => ({
    extension: p.extension,
    name: p.name,
    host: p.host,
    port: String(p.port ?? '443'),
    basePath: p.basePath ?? '',
    manual: p.manual ?? false,
    discover: p.discover
      ? {
          freeOnly: p.discover.freeOnly ?? false,
          minContext: String(p.discover.minContext ?? '0'),
          limit: String(p.discover.limit ?? '0'),
          quality: p.discover.quality ?? 3,
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
                  {/* Where this provider's models come from. The three sources
                      are independent and a provider can have more than one, so
                      these are separate chips rather than one exclusive label. */}
                  {p.discover && (
                    <Tooltip title="Models are enrolled automatically by this filter on every refresh.">
                      <Chip
                        size="small"
                        variant="outlined"
                        label={`filter${p.discover.freeOnly ? ' · free' : ''}${
                          Number(p.discover.limit) > 0 ? ` · top ${p.discover.limit}` : ''
                        }`}
                      />
                    </Tooltip>
                  )}
                  {Number(p.provides ?? 0) > 0 && (
                    <Tooltip title="Models written into the extension YAML by hand. Edit them there, not here.">
                      <Chip size="small" variant="outlined" label={`${p.provides} declared`} />
                    </Tooltip>
                  )}
                  {p.manual && (
                    <Tooltip title="Models are chosen one at a time off this provider's catalogue.">
                      <Chip size="small" variant="outlined" label="hand-picked" />
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
                      label={`${c.name}${c.approvalRequired ? ' · approval' : ''}${
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
