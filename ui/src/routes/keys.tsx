import { createFileRoute, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Chip,
  CircularProgress,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material'
import { Panel, PageHeader } from '@/Panel'
import { KeyLaneActions } from '@/KeyLane'
import { KeyCharts } from '@/KeyCharts'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtAgo, fmtInt, fmtUSD } from '@/format'

/**
 * Keys: who is talking to this box, and whether anybody decided what they get.
 *
 * Weight is the thing that actually schedules a caller, and it lives on the
 * GROUP. A key is only a pointer to one — so this page is really "which lane is
 * each caller in", and the weight column is shown because it is the consequence
 * the operator cares about, not because a key has one.
 *
 * The reason this page exists rather than the Config page covering it: an
 * unassigned key was INVISIBLE. corrallm accepts any key and resolves an unknown
 * one to the fallback lane, so a caller nobody had ever thought about looked
 * exactly like one deliberately placed there. Config can only show you what is
 * written down; this joins that with what has actually been seen, which is the
 * only way to manage keys on a box that mints them freely.
 *
 * Unrecognized rows sort first because they are the ones that need a decision.
 * Requests and Last seen are both shown because either alone misleads: a big
 * total may be entirely historical, and a recent call may be a one-off.
 */
export const KeysDoc = graphql(/* GraphQL */ `
  query Keys {
    corrallm {
      keys(windowHours: "0") {
        keys {
          key
          hash
          group
          weight
          recognized
          requests
          lastSeen
          costUSD
          dwellMS
        }
        unknownAllowed
        unknownGroup
      }
      groups {
        groups {
          name
          weight
        }
      }
    }
  }
`)

function Keys() {
  const [err, setErr] = useState('')

  const q = useQuery({
    queryKey: ['keys'],
    queryFn: () => gqlClient.request(KeysDoc),
    refetchInterval: 30000,
  })

  if (q.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Typography color="error">{String(q.error)}</Typography>
      </Box>
    )
  }

  const data = q.data?.corrallm.keys
  const keys = data?.keys ?? []
  const groups = q.data?.corrallm.groups?.groups ?? []
  const unassigned = keys.filter((k) => !k.recognized).length

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Keys" />

      {err && (
        <Alert severity="error" onClose={() => setErr('')}>
          {err}
        </Alert>
      )}

      {/* The standing policy, stated. It used to be implicit — accept anyone at
          weight 1 — which is a policy wearing the clothes of a fallback, and
          left the reader to infer it from an absence. */}
      <Alert severity={data?.unknownAllowed ? 'info' : 'warning'}>
        {data?.unknownAllowed ? (
          <>
            Unrecognized keys are <b>served</b>, in the <code>{data?.unknownGroup}</code> lane.
            {unassigned > 0 && (
              <>
                {' '}
                {fmtInt(unassigned)} {unassigned === 1 ? 'key has' : 'keys have'} called without
                being assigned one — they are scheduled by default, not by decision.
              </>
            )}
          </>
        ) : (
          <>
            Unrecognized keys are <b>refused</b> (401). Only keys assigned a group below can use
            this box.
          </>
        )}
      </Alert>

      {/* Shape before roster: the table ranks callers by total, which cannot show
          that one of them arrived yesterday and another runs all night. */}
      <KeyCharts />

      <Panel
        title="Caller keys"
        subtitle="Configured lanes, plus keys seen in traffic that nobody has assigned"
        flush
      >
        <TableContainer>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Key</TableCell>
                <TableCell>Hash</TableCell>
                <TableCell>Group</TableCell>
                <TableCell align="right">Weight</TableCell>
                <TableCell align="right">Requests</TableCell>
                <TableCell align="right">Last seen</TableCell>
                <TableCell align="right">Cost</TableCell>
                <TableCell align="right">Actions</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {keys.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={8}>
                    <Typography color="text.secondary">
                      No keys configured, and none seen in traffic yet.
                    </Typography>
                  </TableCell>
                </TableRow>
              ) : (
                keys.map((k) => (
                  <TableRow key={k.hash} hover>
                    <TableCell sx={{ fontFamily: 'monospace' }}>
                      <Link to="/keys/$key" params={{ key: k.key }}>
                        {k.key}
                      </Link>
                    </TableCell>
                    <TableCell sx={{ fontFamily: 'monospace', color: 'text.secondary' }}>
                      {k.hash}
                    </TableCell>
                    <TableCell>
                      {k.recognized ? (
                        <Chip size="small" label={k.group} />
                      ) : (
                        <Tooltip title="Nobody assigned this key. It is being served in the fallback lane because corrallm accepts any key, not because anyone chose this.">
                          <Chip size="small" color="warning" label={`${k.group} (unassigned)`} />
                        </Tooltip>
                      )}
                    </TableCell>
                    <TableCell align="right">{fmtInt(Number(k.weight))}</TableCell>
                    <TableCell align="right">{fmtInt(Number(k.requests))}</TableCell>
                    <TableCell align="right">
                      {/* Relative, with the exact time a hover away: the question
                          this column answers is "is this caller still around". */}
                      <Tooltip title={k.lastSeen || 'never seen in recorded traffic'}>
                        <span
                          style={{
                            color: k.lastSeen ? undefined : 'var(--mui-palette-text-disabled)',
                          }}
                        >
                          {fmtAgo(k.lastSeen)}
                        </span>
                      </Tooltip>
                    </TableCell>
                    <TableCell align="right">{fmtUSD(k.costUSD)}</TableCell>
                    <TableCell align="right">
                      <KeyLaneActions
                        keyName={k.key}
                        group={k.group}
                        recognized={k.recognized}
                        groups={groups}
                        onError={setErr}
                      />
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </TableContainer>
      </Panel>
    </Box>
  )
}

export const Route = createFileRoute('/keys')({ component: Keys })
