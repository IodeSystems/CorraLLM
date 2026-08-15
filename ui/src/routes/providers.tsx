import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Divider,
  Stack,
  Switch,
  FormControlLabel,
  TextField,
  Typography,
} from '@mui/material'
import { Panel, PageHeader } from '@/Panel'
import { graphql } from '@/gql'
import type { Corrallm_ProviderSpecInput } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'

/**
 * Providers (P21): an upstream endpoint and the accounts held against it.
 *
 * Secrets are NEVER shown. A credential carries a secretRef — the name of an
 * entry in ~/.corrallm/credentials, referenced from config as ${NAME} — and the
 * value is written through a separate write-only endpoint. That is what keeps
 * the config document safe to read, back up and share, which is the whole
 * reason the store is a separate file.
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

const UpsertProviderDoc = graphql(/* GraphQL */ `
  mutation UpsertProvider($body: corrallm_ProviderSpecInput!) {
    corrallm {
      upsertProvider(body: $body) {
        ok
        message
      }
    }
  }
`)

const SetSecretDoc = graphql(/* GraphQL */ `
  mutation SetSecret($body: corrallm_SetSecretInputBodyInput!) {
    corrallm {
      setSecret(body: $body) {
        ok
        message
      }
    }
  }
`)

type Draft = {
  extension: string
  name: string
  host: string
  port: string
  basePath: string
  secretRef: string
  secretValue: string
  approvalRequired: boolean
  monthlyUSD: string
  rpm: string
  freeOnly: boolean
  minContext: string
  limit: string
}

const BLANK: Draft = {
  extension: 'free',
  name: '',
  host: '',
  port: '443',
  basePath: '',
  secretRef: '',
  secretValue: '',
  approvalRequired: true,
  monthlyUSD: '',
  rpm: '',
  freeOnly: true,
  minContext: '8192',
  limit: '12',
}

function ProvidersPage() {
  const qc = useQueryClient()
  const [d, setD] = useState<Draft>(BLANK)
  const [msg, setMsg] = useState<string>('')

  const { data, isLoading, error } = useQuery({
    queryKey: ['providers'],
    queryFn: () => gqlClient.request(ProvidersDoc),
  })

  const setSecret = useMutation({
    mutationFn: (body: { name: string; value: string }) =>
      gqlClient.request(SetSecretDoc, { body }),
  })
  const upsert = useMutation({
    mutationFn: (body: Corrallm_ProviderSpecInput) =>
      gqlClient.request(UpsertProviderDoc, { body }),
    onSuccess: (r) => {
      setMsg(r.corrallm?.upsertProvider?.message ?? '')
      qc.invalidateQueries({ queryKey: ['providers'] })
    },
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
  const set = (patch: Partial<Draft>) => setD((s) => ({ ...s, ...patch }))

  const save = async () => {
    setMsg('')
    // The secret goes to its own endpoint FIRST, so the config never carries a
    // reference to something that does not resolve yet.
    if (d.secretRef && d.secretValue) {
      await setSecret.mutateAsync({ name: d.secretRef, value: d.secretValue })
    }
    const credLimits = d.rpm ? [{ req: Number(d.rpm), per: 'minute' }] : []
    upsert.mutate({
      extension: d.extension,
      name: d.name,
      host: d.host,
      port: String(Number(d.port || 443)),
      basePath: d.basePath,
      limits: d.monthlyUSD ? [{ usd: Number(d.monthlyUSD), per: 'month' }] : [],
      discover: {
        freeOnly: d.freeOnly,
        inputModality: 'text',
        outputModality: 'text',
        minContext: String(Number(d.minContext || 0)),
        limit: String(Number(d.limit || 0)),
        quality: 3,
      },
      credentials: d.secretRef
        ? [
            {
              name: 'main',
              secretRef: d.secretRef,
              approvalRequired: d.approvalRequired,
              limits: credLimits,
            },
          ]
        : [],
    })
    set({ secretValue: '' }) // never keep it in component state after the write
  }

  return (
    <Box sx={{ p: 2, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <PageHeader title="Providers">
        <Typography variant="body2" sx={{ opacity: 0.75 }}>
          An upstream endpoint and the accounts held against it. Secrets live in the credential
          store and are referenced as <code>${'{NAME}'}</code> — this page can set one, never show
          one.
        </Typography>
      </PageHeader>

      <Panel title="Add a provider">
        <Stack spacing={2} sx={{ maxWidth: 900 }}>
          <Stack direction="row" spacing={2} flexWrap="wrap">
            <TextField
              size="small"
              label="Extension"
              value={d.extension}
              onChange={(e) => set({ extension: e.target.value })}
              helperText="Existing extension that groups it"
            />
            <TextField
              size="small"
              label="Name"
              value={d.name}
              onChange={(e) => set({ name: e.target.value })}
              helperText="Served models become <name>-<id>"
            />
            <TextField
              size="small"
              label="Host"
              value={d.host}
              onChange={(e) => set({ host: e.target.value })}
              placeholder="openrouter.ai"
            />
            <TextField
              size="small"
              label="Port"
              value={d.port}
              onChange={(e) => set({ port: e.target.value })}
              sx={{ width: 90 }}
            />
            <TextField
              size="small"
              label="Base path"
              value={d.basePath}
              onChange={(e) => set({ basePath: e.target.value })}
              placeholder="/api"
              sx={{ width: 120 }}
            />
          </Stack>

          <Divider textAlign="left">
            <Typography variant="caption">Account</Typography>
          </Divider>
          <Stack direction="row" spacing={2} flexWrap="wrap" alignItems="flex-start">
            <TextField
              size="small"
              label="Secret name"
              value={d.secretRef}
              onChange={(e) => set({ secretRef: e.target.value.toUpperCase() })}
              placeholder="OPENROUTER_KEY_WORK"
              helperText="Referenced as ${NAME}; stored outside config"
            />
            <TextField
              size="small"
              type="password"
              label="Secret value"
              value={d.secretValue}
              onChange={(e) => set({ secretValue: e.target.value })}
              helperText="Write-only — never displayed again"
            />
            <TextField
              size="small"
              label="Requests / minute"
              value={d.rpm}
              onChange={(e) => set({ rpm: e.target.value })}
              sx={{ width: 150 }}
            />
            <TextField
              size="small"
              label="Provider $ / month"
              value={d.monthlyUSD}
              onChange={(e) => set({ monthlyUSD: e.target.value })}
              helperText="Across ALL its accounts"
              sx={{ width: 170 }}
            />
          </Stack>
          <FormControlLabel
            control={
              <Switch
                checked={d.approvalRequired}
                onChange={(e) => set({ approvalRequired: e.target.checked })}
              />
            }
            label={
              <Typography variant="body2">
                Require approval before a discovered model serves — recommended on any account that
                can spend money
              </Typography>
            }
          />

          <Divider textAlign="left">
            <Typography variant="caption">Discovery</Typography>
          </Divider>
          <Stack direction="row" spacing={2} flexWrap="wrap" alignItems="center">
            <FormControlLabel
              control={
                <Switch checked={d.freeOnly} onChange={(e) => set({ freeOnly: e.target.checked })} />
              }
              label={<Typography variant="body2">Free models only</Typography>}
            />
            <TextField
              size="small"
              label="Min context"
              value={d.minContext}
              onChange={(e) => set({ minContext: e.target.value })}
              sx={{ width: 130 }}
            />
            <TextField
              size="small"
              label="Max models"
              value={d.limit}
              onChange={(e) => set({ limit: e.target.value })}
              sx={{ width: 130 }}
            />
          </Stack>

          <Box>
            <Button variant="contained" onClick={save} disabled={!d.name || !d.host}>
              Save provider
            </Button>
            {msg && (
              <Typography variant="caption" sx={{ ml: 2 }}>
                {msg}
              </Typography>
            )}
          </Box>
        </Stack>
      </Panel>

      <Panel title={`Configured (${providers.length})`}>
        <Stack spacing={1.5}>
          {providers.map((p) => (
            <Box key={`${p.extension}/${p.name}`}>
              <Typography variant="subtitle2">
                {p.extension} / {p.name}{' '}
                <Typography component="span" variant="caption" sx={{ opacity: 0.7 }}>
                  {p.host}:{p.port}
                  {p.basePath}
                </Typography>
              </Typography>
              <Stack direction="row" spacing={1} sx={{ mt: 0.5 }} flexWrap="wrap">
                {(p.limits ?? []).map((l, i) => (
                  <Chip
                    key={i}
                    size="small"
                    variant="outlined"
                    label={`provider ${l.usd ? `$${l.usd}` : l.req}/${l.per}`}
                  />
                ))}
                {(p.credentials ?? []).length === 0 && (
                  <Chip size="small" variant="outlined" label="no explicit accounts" />
                )}
                {(p.credentials ?? []).map((c) => (
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
            </Box>
          ))}
        </Stack>
      </Panel>

      <Panel title={`Credential store (${secrets.length})`}>
        <Typography variant="body2" sx={{ opacity: 0.75, mb: 1 }}>
          Names only. No endpoint returns a value — that is what keeps the config document safe to
          read and share.
        </Typography>
        <Stack direction="row" spacing={1} flexWrap="wrap">
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
    </Box>
  )
}

export const Route = createFileRoute('/providers')({ component: ProvidersPage })
