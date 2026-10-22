import { useEffect, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  IconButton,
  MenuItem,
  Switch,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material'
import ContentCopyIcon from '@mui/icons-material/ContentCopy'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { Panel } from '@/Panel'
import { extractMessage, fmtAgo, fmtTime } from '@/format'
import { C } from '@/theme'

/**
 * Setup → Access: who may use this dashboard, and how they sign in.
 *
 * Three panels, in the order a first setup walks them: the users (SSO admits
 * nobody until at least one is listed), the identity provider, and the admin
 * token, which is turned off last, from an SSO session, once SSO is proven.
 *
 * Admin only. The server refuses a viewer every write and this read too; the
 * tab is hidden from viewers rather than shown empty.
 */

const AccessDoc = graphql(/* GraphQL */ `
  query AccessData {
    corrallm {
      listAccess {
        users {
          email
          role
          lastSignInMs
          lastSignInIp
          legacy
        }
        provider {
          configured
          enabled
          issuer
          clientId
          secretRef
          hasSecret
          literalSecret
          redirectUrl
          redirectUri
        }
        secrets
        adminToken
        callerEmail
      }
    }
  }
`)

const UpsertUserDoc = graphql(/* GraphQL */ `
  mutation UpsertUser($body: corrallm_UpsertUserInputBodyInput!) {
    corrallm {
      upsertUser(body: $body) {
        ok
        message
      }
    }
  }
`)

const RemoveUserDoc = graphql(/* GraphQL */ `
  mutation RemoveUser($body: corrallm_UserEmailInputBodyInput!) {
    corrallm {
      removeUser(body: $body) {
        ok
        message
      }
    }
  }
`)

const RevokeUserDoc = graphql(/* GraphQL */ `
  mutation RevokeUserSessions($body: corrallm_UserEmailInputBodyInput!) {
    corrallm {
      revokeUserSessions(body: $body) {
        ok
        message
      }
    }
  }
`)

const SetProviderDoc = graphql(/* GraphQL */ `
  mutation SetIdentityProvider($body: corrallm_SetIdentityProviderInputBodyInput!) {
    corrallm {
      setIdentityProvider(body: $body) {
        ok
        message
      }
    }
  }
`)

const TestProviderDoc = graphql(/* GraphQL */ `
  mutation TestIdentityProvider($body: corrallm_TestIdentityProviderInputBodyInput!) {
    corrallm {
      testIdentityProvider(body: $body) {
        ok
        message
        issuer
        authorizationEndpoint
        tokenEndpoint
        userinfoEndpoint
        jwksUri
        codeChallengeMethods
        tokenAuthMethods
        redirectUri
        checks {
          name
          ok
          detail
        }
      }
    }
  }
`)

const SetAdminTokenDoc = graphql(/* GraphQL */ `
  mutation SetAdminToken($body: corrallm_SetAdminTokenInputBodyInput!) {
    corrallm {
      setAdminToken(body: $body) {
        ok
        message
      }
    }
  }
`)

type Result = { ok: boolean; message: string } | null | undefined

// useAccessAction runs one mutation, refreshes the tab and reports a refusal.
function useAccessAction() {
  const qc = useQueryClient()
  const [err, setErr] = useState('')
  const run = async (fn: () => Promise<Result>): Promise<boolean> => {
    setErr('')
    try {
      const r = await fn()
      if (!r?.ok) {
        setErr(r?.message ?? 'refused')
        return false
      }
      await qc.invalidateQueries({ queryKey: ['access'] })
      await qc.invalidateQueries({ queryKey: ['whoami'] })
      return true
    } catch (e) {
      setErr(extractMessage(e))
      return false
    }
  }
  return { err, setErr, run }
}

function lastSignIn(ms: string | number): string {
  const n = Number(ms)
  return n > 0 ? fmtAgo(new Date(n).toISOString()) : 'never'
}

export function AccessTab() {
  const { data, error, isLoading } = useQuery({
    queryKey: ['access'],
    queryFn: () => gqlClient.request(AccessDoc),
  })
  const a = data?.corrallm?.listAccess
  if (isLoading) return null
  if (error || !a) {
    return <Alert severity="error">{error ? extractMessage(error) : 'Access settings unavailable'}</Alert>
  }
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <UsersPanel users={a.users} ssoOn={a.provider.enabled} />
      <ProviderPanel p={a.provider} secrets={a.secrets} hasUsers={a.users.length > 0} />
      <AdminTokenPanel
        enabled={a.adminToken !== 'disabled'}
        callerEmail={a.callerEmail}
        ssoOn={a.provider.enabled}
        hasAdmin={a.users.some((u) => u.role === 'admin')}
      />
    </Box>
  )
}

type User = { email: string; role: string; lastSignInMs: string | number; lastSignInIp: string; legacy: boolean }

function UsersPanel({ users, ssoOn }: { users: User[]; ssoOn: boolean }) {
  const { err, setErr, run } = useAccessAction()
  const [adding, setAdding] = useState(false)
  const [confirm, setConfirm] = useState<{ kind: 'remove' | 'revoke'; email: string } | null>(null)

  const setRole = (email: string, role: string) =>
    run(async () => (await gqlClient.request(UpsertUserDoc, { body: { email, role } })).corrallm?.upsertUser)

  return (
    <Panel
      title={`Users (${users.length})`}
      subtitle="Who may sign in with SSO"
      help="Admins can change everything. Viewers see the dashboard but every change is refused. The admin token is always an admin."
      actions={
        <Button size="small" variant="outlined" onClick={() => setAdding(true)}>
          Add user
        </Button>
      }
      flush
    >
      {err && !confirm && (
        <Alert severity="error" onClose={() => setErr('')} sx={{ m: 1 }}>
          {err}
        </Alert>
      )}
      {users.length === 0 ? (
        <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
          No users yet. SSO admits only the emails listed here, so it needs at least one before it can be
          enabled. Add yourself as an admin first.
        </Typography>
      ) : (
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Email</TableCell>
              <TableCell>Role</TableCell>
              <TableCell>Last sign-in</TableCell>
              <TableCell align="right">Actions</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {users.map((u) => (
              <TableRow key={u.email}>
                <TableCell>
                  {u.email}
                  {u.legacy && (
                    <Tooltip title="From the legacy auth.oidc.allowedEmails list. Saved into the users list on the next change here.">
                      <Typography component="span" variant="caption" sx={{ ml: 1, color: C.textMuted }}>
                        legacy
                      </Typography>
                    </Tooltip>
                  )}
                </TableCell>
                <TableCell>
                  <TextField
                    select
                    size="small"
                    variant="standard"
                    value={u.role}
                    onChange={(e) => void setRole(u.email, e.target.value)}
                  >
                    <MenuItem value="admin">admin</MenuItem>
                    <MenuItem value="viewer">viewer</MenuItem>
                  </TextField>
                </TableCell>
                <TableCell>
                  <Tooltip title={Number(u.lastSignInMs) > 0 ? `${fmtTime(u.lastSignInMs)} from ${u.lastSignInIp || 'unknown address'}` : ''}>
                    <span>{lastSignIn(u.lastSignInMs)}</span>
                  </Tooltip>
                </TableCell>
                <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
                  <Button size="small" onClick={() => setConfirm({ kind: 'revoke', email: u.email })}>
                    Revoke sessions
                  </Button>
                  <Button size="small" color="error" onClick={() => setConfirm({ kind: 'remove', email: u.email })}>
                    Remove
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {!ssoOn && users.length > 0 && (
        <Typography variant="caption" sx={{ display: 'block', px: 2, py: 1, color: C.textMuted }}>
          SSO is off, so nobody signs in with these yet. Enable the identity provider below.
        </Typography>
      )}
      <AddUserDialog open={adding} onClose={() => setAdding(false)} />
      <Confirm
        open={!!confirm}
        title={confirm?.kind === 'remove' ? `Remove ${confirm?.email}?` : `Sign ${confirm?.email} out everywhere?`}
        confirmLabel={confirm?.kind === 'remove' ? 'Remove' : 'Revoke sessions'}
        error={err}
        onClose={() => {
          setConfirm(null)
          setErr('')
        }}
        onConfirm={async () => {
          if (!confirm) return
          const body = { email: confirm.email }
          const ok = await run(async () =>
            confirm.kind === 'remove'
              ? (await gqlClient.request(RemoveUserDoc, { body })).corrallm?.removeUser
              : (await gqlClient.request(RevokeUserDoc, { body })).corrallm?.revokeUserSessions,
          )
          if (ok) setConfirm(null)
        }}
      >
        {confirm?.kind === 'remove'
          ? 'They lose access on their next request, and their current sessions are revoked.'
          : 'Every session they hold stops working on its next request. They can sign in again.'}
      </Confirm>
    </Panel>
  )
}

function AddUserDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { err, setErr, run } = useAccessAction()
  const [email, setEmail] = useState('')
  const [role, setRole] = useState('viewer')
  useEffect(() => {
    if (open) {
      setEmail('')
      setRole('viewer')
      setErr('')
    }
  }, [open, setErr])
  const valid = /^[^\s@]+@[^\s@]+$/.test(email.trim())
  const save = async () => {
    const ok = await run(async () => (await gqlClient.request(UpsertUserDoc, { body: { email: email.trim(), role } })).corrallm?.upsertUser)
    if (ok) onClose()
  }
  return (
    <Dialog open={open} onClose={onClose} maxWidth="xs" fullWidth>
      <DialogTitle>Add user</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2, pt: '8px !important' }}>
        <TextField
          size="small"
          label="Email"
          autoFocus
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          helperText="The address the identity provider reports as verified"
        />
        <TextField select size="small" label="Role" value={role} onChange={(e) => setRole(e.target.value)}>
          <MenuItem value="admin">admin: can change everything</MenuItem>
          <MenuItem value="viewer">viewer: read-only</MenuItem>
        </TextField>
        {err && <Alert severity="error">{err}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" disabled={!valid} onClick={() => void save()}>
          Add
        </Button>
      </DialogActions>
    </Dialog>
  )
}

type Provider = {
  configured: boolean
  enabled: boolean
  issuer: string
  clientId: string
  secretRef: string
  hasSecret: boolean
  literalSecret: boolean
  redirectUrl: string
  redirectUri: string
}

type KeyMode = 'none' | 'stored' | 'new'

type TestResult = {
  ok: boolean
  message: string
  issuer: string
  authorizationEndpoint: string
  tokenEndpoint: string
  userinfoEndpoint: string
  jwksUri: string
  codeChallengeMethods: string[]
  tokenAuthMethods: string[]
  redirectUri: string
  checks: { name: string; ok: boolean; detail: string }[]
}

function ProviderPanel({ p, secrets, hasUsers }: { p: Provider; secrets: string[]; hasUsers: boolean }) {
  const { err, setErr, run } = useAccessAction()
  const [issuer, setIssuer] = useState(p.issuer)
  const [clientId, setClientId] = useState(p.clientId)
  const [redirectUrl, setRedirectUrl] = useState(p.redirectUrl)
  const [enabled, setEnabled] = useState(p.enabled)
  const [keyMode, setKeyMode] = useState<KeyMode>(p.secretRef ? 'stored' : 'none')
  const [secretRef, setSecretRef] = useState(p.secretRef)
  const [secretValue, setSecretValue] = useState('')
  const [saved, setSaved] = useState('')
  const [test, setTest] = useState<TestResult | null>(null)
  const [copied, setCopied] = useState(false)

  const testRun = useMutation({
    mutationFn: () =>
      gqlClient.request(TestProviderDoc, {
        body: {
          issuer: issuer.trim(),
          clientId: clientId.trim(),
          redirectUrl: redirectUrl.trim(),
          public: keyMode === 'none',
        },
      }),
    onSuccess: (r) => setTest(r.corrallm?.testIdentityProvider ?? null),
    onError: (e) => setErr(extractMessage(e)),
  })

  const newKeyOk = /^[A-Za-z_][A-Za-z0-9_]*$/.test(secretRef) && secretValue !== ''
  const valid =
    issuer.trim() !== '' &&
    clientId.trim() !== '' &&
    (keyMode === 'none' || (keyMode === 'stored' && secrets.includes(secretRef)) || (keyMode === 'new' && newKeyOk))

  const save = async () => {
    setSaved('')
    const ok = await run(
      async () =>
        (
          await gqlClient.request(SetProviderDoc, {
            body: {
              issuer: issuer.trim(),
              clientId: clientId.trim(),
              enabled,
              redirectUrl: redirectUrl.trim(),
              secretRef: keyMode === 'none' ? '' : secretRef.trim(),
              secretValue: keyMode === 'new' ? secretValue : '',
            },
          })
        ).corrallm?.setIdentityProvider,
    )
    if (ok) {
      setSecretValue('')
      if (keyMode === 'new') setKeyMode('stored')
      setSaved('Saved. Applies to the next sign-in.')
    }
  }

  // What sign-in will send: the override, else this page's own address — the
  // server builds it from the request the same way (auth.RedirectFor).
  const callback = redirectUrl.trim() || (p.redirectUrl ? `${window.location.origin}/auth/callback` : p.redirectUri)
  const copy = () => {
    void navigator.clipboard?.writeText(callback).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <Panel
      title="Identity provider"
      subtitle={p.configured ? (p.enabled ? 'SSO on' : 'SSO off') : 'Not set up'}
      help="One OpenID Connect provider. corrallm finds its endpoints at <issuer>/.well-known/openid-configuration and signs people in with the authorization code flow and PKCE."
      note={
        <>
          Zitadel: create a <strong>Web</strong> application with authentication method <strong>PKCE</strong>, and
          register the redirect URI below. A confidential client (Basic or Post) also works; then store its secret
          below.
        </>
      }
    >
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, maxWidth: 640 }}>
        <TextField
          size="small"
          label="Issuer"
          placeholder="https://id.example.com"
          value={issuer}
          onChange={(e) => setIssuer(e.target.value)}
          helperText="https; the URL the provider names itself by"
        />
        <TextField size="small" label="Client ID" value={clientId} onChange={(e) => setClientId(e.target.value)} />

        <Box>
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            Redirect URI (register this in your provider)
          </Typography>
          {callback ? (
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
              <Box component="code" sx={{ px: 0.5, bgcolor: C.canvas, borderRadius: 0.5, wordBreak: 'break-all' }}>
                {callback}
              </Box>
              <Tooltip title={copied ? 'Copied' : 'Copy'}>
                <IconButton size="small" onClick={copy} aria-label="Copy redirect URI">
                  <ContentCopyIcon fontSize="inherit" />
                </IconButton>
              </Tooltip>
            </Box>
          ) : (
            <Typography variant="body2" sx={{ color: C.textFaint }}>
              Unknown: set a callback override below.
            </Typography>
          )}
          <Typography variant="caption" sx={{ color: C.textMuted, display: 'block', mt: 0.5 }}>
            Built from the address you opened this page at. People who sign in at a different address
            send a different callback; register each, or set one override.
          </Typography>
        </Box>
        <TextField
          size="small"
          label="Callback override (optional)"
          placeholder="https://llm.example.com/auth/callback"
          value={redirectUrl}
          onChange={(e) => setRedirectUrl(e.target.value)}
          helperText="Leave empty to use the address the browser signs in from"
        />

        <Box>
          <Typography variant="body2" sx={{ mb: 1 }}>
            Client secret (only for confidential clients)
          </Typography>
          {p.literalSecret && (
            <Alert severity="warning" sx={{ mb: 1 }}>
              The config holds this secret written out, not as a stored key. Saving here drops it unless you store it
              as a key.
            </Alert>
          )}
          <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
            <TextField
              select
              size="small"
              label="Secret"
              sx={{ width: 220 }}
              value={keyMode}
              onChange={(e) => setKeyMode(e.target.value as KeyMode)}
            >
              <MenuItem value="none">None (PKCE only)</MenuItem>
              <MenuItem value="stored">Use a stored key</MenuItem>
              <MenuItem value="new">Store a new key</MenuItem>
            </TextField>
            {keyMode === 'stored' && (
              <TextField
                select
                size="small"
                label="Stored key"
                sx={{ width: 280 }}
                value={secrets.includes(secretRef) ? secretRef : ''}
                onChange={(e) => setSecretRef(e.target.value)}
                helperText={secrets.length ? 'Names only; values are never shown' : 'None stored yet'}
              >
                {secrets.map((s) => (
                  <MenuItem key={s} value={s}>
                    {s}
                  </MenuItem>
                ))}
              </TextField>
            )}
            {keyMode === 'new' && (
              <>
                <TextField
                  size="small"
                  label="Key name"
                  sx={{ width: 240 }}
                  placeholder="OIDC_CLIENT_SECRET"
                  value={secretRef}
                  onChange={(e) => setSecretRef(e.target.value.toUpperCase())}
                  helperText="Stored outside the config"
                />
                <TextField
                  size="small"
                  type="password"
                  label="Key value"
                  autoComplete="off"
                  sx={{ flex: 1, minWidth: 220 }}
                  value={secretValue}
                  onChange={(e) => setSecretValue(e.target.value)}
                  helperText="Write-only; never displayed again"
                />
              </>
            )}
          </Box>
          {p.secretRef && !p.hasSecret && keyMode === 'stored' && secretRef === p.secretRef && (
            <Typography variant="caption" sx={{ color: C.warn }}>
              {p.secretRef} is not in the credential store.
            </Typography>
          )}
        </Box>

        <FormControlLabel
          control={<Switch checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />}
          label={enabled ? 'Sign-in enabled' : 'Sign-in disabled'}
        />
        {enabled && !hasUsers && (
          <Typography variant="caption" sx={{ color: C.warn }}>
            Add at least one user above first; SSO with no users admits nobody.
          </Typography>
        )}

        <Box sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap' }}>
          <Button variant="contained" size="small" disabled={!valid} onClick={() => void save()}>
            Save
          </Button>
          <Button
            variant="outlined"
            size="small"
            disabled={!issuer.trim() || testRun.isPending}
            onClick={() => {
              setTest(null)
              testRun.mutate()
            }}
          >
            {testRun.isPending ? 'Testing…' : 'Test'}
          </Button>
          {saved && (
            <Typography variant="body2" sx={{ color: C.textMuted }}>
              {saved}
            </Typography>
          )}
        </Box>
        {err && (
          <Alert severity="error" onClose={() => setErr('')}>
            {err}
          </Alert>
        )}
        {test && <TestResultView r={test} />}
      </Box>
    </Panel>
  )
}

function TestResultView({ r }: { r: TestResult }) {
  const rows: [string, string][] = [
    ['Issuer', r.issuer],
    ['Authorization', r.authorizationEndpoint],
    ['Token', r.tokenEndpoint],
    ['Userinfo', r.userinfoEndpoint],
    ['JWKS', r.jwksUri],
    ['PKCE methods', r.codeChallengeMethods.join(', ')],
    ['Client auth methods', r.tokenAuthMethods.join(', ')],
  ]
  return (
    <Alert severity={r.ok ? 'success' : 'error'}>
      <Typography variant="body2" sx={{ mb: r.issuer ? 1 : 0 }}>
        {r.message}
      </Typography>
      {r.checks.length > 0 && (
        <Box component="ul" sx={{ m: 0, mb: 1, pl: 2.5, fontSize: 13 }}>
          {r.checks.map((c) => (
            <li key={c.name}>
              <b>{c.ok ? '✓' : '✗'} {c.name}</b> — {c.detail}
            </li>
          ))}
        </Box>
      )}
      {r.issuer && (
        <Box component="dl" sx={{ m: 0, display: 'grid', gridTemplateColumns: 'auto 1fr', columnGap: 2, fontSize: 13 }}>
          {rows.map(([k, v]) => (
            <Box key={k} sx={{ display: 'contents' }}>
              <Box component="dt" sx={{ color: C.textMuted }}>
                {k}
              </Box>
              <Box component="dd" sx={{ m: 0, wordBreak: 'break-all' }}>
                {v || '—'}
              </Box>
            </Box>
          ))}
        </Box>
      )}
    </Alert>
  )
}

const RECOVER_CMD = 'corrallm auth admin-token enable && corrallm service reload'

function AdminTokenPanel({
  enabled,
  callerEmail,
  ssoOn,
  hasAdmin,
}: {
  enabled: boolean
  callerEmail: string
  ssoOn: boolean
  hasAdmin: boolean
}) {
  const { err, setErr, run } = useAccessAction()
  const [confirming, setConfirming] = useState(false)
  const set = (on: boolean) =>
    run(async () => (await gqlClient.request(SetAdminTokenDoc, { body: { enabled: on } })).corrallm?.setAdminToken)

  // Why Disable is unavailable, in the order a person would fix it.
  const blocker = !ssoOn
    ? 'Enable the identity provider first.'
    : !hasAdmin
      ? 'Make at least one user an admin first.'
      : !callerEmail
        ? 'Sign in with SSO as an admin to disable it. That sign-in is what proves SSO works; a request made with the admin token cannot turn the token off.'
        : ''

  return (
    <Panel
      title="Admin token"
      subtitle={enabled ? 'Accepted' : 'Disabled'}
      help="The admin token (admin.token in corrallm's home directory) signs in as an admin. Once SSO works, turning it off leaves SSO as the only way into the dashboard. The file stays on disk."
    >
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5, maxWidth: 640 }}>
        {enabled ? (
          <>
            <Typography variant="body2">
              The dashboard and /api accept the admin token. Disable it once SSO admins can sign in.
            </Typography>
            {blocker && (
              <Typography variant="body2" sx={{ color: C.textMuted }}>
                {blocker}
              </Typography>
            )}
            <Box>
              <Button size="small" variant="outlined" color="error" disabled={!!blocker} onClick={() => setConfirming(true)}>
                Disable admin token
              </Button>
            </Box>
          </>
        ) : (
          <>
            <Typography variant="body2">
              The admin token is refused; SSO is the only way in. If SSO stops working, run this on the server:
            </Typography>
            <Box component="code" sx={{ px: 0.5, bgcolor: C.canvas, borderRadius: 0.5, wordBreak: 'break-all' }}>
              {RECOVER_CMD}
            </Box>
            <Box>
              <Button size="small" variant="outlined" onClick={() => void set(true)}>
                Enable admin token
              </Button>
            </Box>
          </>
        )}
        {err && !confirming && (
          <Alert severity="error" onClose={() => setErr('')}>
            {err}
          </Alert>
        )}
      </Box>
      <Confirm
        open={confirming}
        title="Disable the admin token?"
        confirmLabel="Disable"
        error={err}
        onClose={() => {
          setConfirming(false)
          setErr('')
        }}
        onConfirm={async () => {
          if (await set(false)) setConfirming(false)
        }}
      >
        <Typography variant="body2" sx={{ mb: 1 }}>
          After this, only SSO users can sign in. Scripts and tools that send the admin token get 401.
        </Typography>
        <Typography variant="body2" sx={{ mb: 1 }}>
          If SSO stops working, turn the token back on from a shell on the server:
        </Typography>
        <Box component="code" sx={{ display: 'block', p: 1, bgcolor: C.canvas, borderRadius: 0.5, wordBreak: 'break-all' }}>
          {RECOVER_CMD}
        </Box>
      </Confirm>
    </Panel>
  )
}

function Confirm({
  open,
  title,
  confirmLabel,
  onClose,
  onConfirm,
  error,
  children,
}: {
  open: boolean
  title: string
  confirmLabel: string
  onClose: () => void
  onConfirm: () => void | Promise<void>
  error?: string
  children: ReactNode
}) {
  const [busy, setBusy] = useState(false)
  return (
    <Dialog open={open} onClose={onClose} maxWidth="sm" fullWidth>
      <DialogTitle>{title}</DialogTitle>
      <DialogContent>
        {children}
        {error && (
          <Alert severity="error" sx={{ mt: 2 }}>
            {error}
          </Alert>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Cancel</Button>
        <Button
          variant="contained"
          color="error"
          disabled={busy}
          onClick={async () => {
            setBusy(true)
            try {
              await onConfirm()
            } finally {
              setBusy(false)
            }
          }}
        >
          {confirmLabel}
        </Button>
      </DialogActions>
    </Dialog>
  )
}
