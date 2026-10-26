import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Alert, Box, Button, FormControlLabel, IconButton, Stack, Switch, TextField, Typography } from '@mui/material'
import DeleteOutline from '@mui/icons-material/DeleteOutlined'
import AddIcon from '@mui/icons-material/Add'
import { graphql } from '@/gql'
import type { RequestRulesQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { Panel, Row } from '@/Panel'
import { Loading } from '@/Loading'
import { extractMessage } from '@/format'

/**
 * Settings → Requests: who may put an access request on the Requests page.
 *
 * Moved off the Requests page (stooges pass 10-25): it is a setting changed
 * once, and between the keys and the decisions it read as part of the decision.
 * Filing grants nothing — an admin still approves each request.
 */

const RequestRulesDoc = graphql(/* GraphQL */ `
  query RequestRules {
    corrallm {
      listRequests {
        rules {
          name
          cidrs
          token
          users
        }
        defaultRules
      }
    }
  }
`)

const RulesDoc = graphql(/* GraphQL */ `
  mutation SetRequestRules($body: corrallm_RequestRulesInputBodyInput!) {
    corrallm {
      setRequestRules(body: $body) {
        ok
        message
      }
    }
  }
`)

type RulesData = NonNullable<NonNullable<RequestRulesQuery['corrallm']>['listRequests']>

export function RequestRulesTab() {
  const q = useQuery({ queryKey: ['request-rules'], queryFn: () => gqlClient.request(RequestRulesDoc) })
  const d = q.data?.corrallm?.listRequests
  if (q.error) return <Alert severity="error">{extractMessage(q.error)}</Alert>
  if (!d) return <Loading />
  return <RulesForm d={d} />
}

type RuleForm = { name: string; cidrs: string; token: string; users: string }

const splitList = (s: string) =>
  s
    .split(/[,\s]+/)
    .map((x) => x.trim())
    .filter(Boolean)

function RulesForm({ d }: { d: RulesData }) {
  const qc = useQueryClient()
  const [useDefault, setUseDefault] = useState(d.defaultRules)
  const [rules, setRules] = useState<RuleForm[]>(
    d.rules.map((r) => ({ name: r.name ?? '', cidrs: (r.cidrs ?? []).join(', '), token: r.token ?? '', users: (r.users ?? []).join(', ') })),
  )
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)
  const save = useMutation({
    mutationFn: () =>
      gqlClient.request(RulesDoc, {
        body: {
          useDefault,
          rules: rules.map((r) => ({ name: r.name.trim(), cidrs: splitList(r.cidrs), token: r.token.trim(), users: splitList(r.users) })),
        },
      }),
    onSuccess: () => {
      setMsg({ ok: true, text: 'Saved.' })
      void qc.invalidateQueries({ queryKey: ['request-rules'] })
    },
    onError: (e) => setMsg({ ok: false, text: extractMessage(e) }),
  })
  const setRule = (i: number, p: Partial<RuleForm>) => setRules(rules.map((r, j) => (j === i ? { ...r, ...p } : r)))
  return (
    <Panel title="Who may ask for access" note="Asking grants nothing: you still approve each request on the Requests page.">
      <Row>
        <Stack spacing={1.5}>
          {msg && <Alert severity={msg.ok ? 'success' : 'error'}>{msg.text}</Alert>}
          <FormControlLabel
            control={<Switch checked={useDefault} onChange={(e) => setUseDefault(e.target.checked)} />}
            label="Default: direct connections from this machine and private networks (not through the public endpoint)"
          />
          {!useDefault && (
            <>
              <Typography variant="body2" color="text.secondary">
                A request is accepted when any rule matches. Within a rule, every field you fill must match. No rules = nobody may ask.
              </Typography>
              {rules.map((r, i) => (
                <Stack key={i} direction="row" spacing={1} useFlexGap sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
                  <TextField label="Name" size="small" value={r.name} onChange={(e) => setRule(i, { name: e.target.value })} sx={{ width: 140 }} />
                  <TextField
                    label="Subnets"
                    placeholder="192.168.1.0/24, 127.0.0.0/8"
                    helperText="Direct connections only"
                    size="small"
                    value={r.cidrs}
                    onChange={(e) => setRule(i, { cidrs: e.target.value })}
                    sx={{ width: 240 }}
                  />
                  <TextField
                    label="Shared token"
                    placeholder="${ACCESS_REQUEST_TOKEN}"
                    helperText="Sent as X-Corrallm-Request-Token"
                    size="small"
                    value={r.token}
                    onChange={(e) => setRule(i, { token: e.target.value })}
                    sx={{ width: 240 }}
                  />
                  <TextField
                    label="Dashboard users"
                    placeholder="carl@example.com"
                    helperText="SSO emails, also under Access"
                    size="small"
                    value={r.users}
                    onChange={(e) => setRule(i, { users: e.target.value })}
                    sx={{ width: 240 }}
                  />
                  <IconButton aria-label="Remove rule" onClick={() => setRules(rules.filter((_, j) => j !== i))}>
                    <DeleteOutline fontSize="small" />
                  </IconButton>
                </Stack>
              ))}
              <Box>
                <Button size="small" startIcon={<AddIcon />} onClick={() => setRules([...rules, { name: '', cidrs: '', token: '', users: '' }])}>
                  Add rule
                </Button>
              </Box>
              {rules.some((r) => r.token && !r.token.trim().startsWith('${')) && (
                <Alert severity="warning">
                  A literal token is stored in the config, where config history keeps it. Prefer a credential-store reference like
                  {' ${ACCESS_REQUEST_TOKEN}'} (corrallm config secret set; read at start-up).
                </Alert>
              )}
            </>
          )}
          <Box>
            <Button variant="contained" size="small" onClick={() => save.mutate()} disabled={save.isPending}>
              Save rules
            </Button>
          </Box>
        </Stack>
      </Row>
    </Panel>
  )
}

