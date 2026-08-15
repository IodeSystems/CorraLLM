import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  MenuItem,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  TextField,
  Tooltip,
  Typography,
  useMediaQuery,
} from '@mui/material'
import { theme } from '@/theme'
import { graphql } from '@/gql'
import type {
  Corrallm_AssignModelInputBodyInput,
  Corrallm_UnassignModelInputBodyInput,
} from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { fmtInt } from '@/format'

/**
 * One provider's directory: what it offers on this account, and which of it you
 * want.
 *
 * This is the primary way models get in. A discover filter is the bulk
 * shortcut, but against four hundred OpenRouter models a filter is a guess and
 * everything it rejects is dropped with no record it existed.
 *
 * There is no approval step and no queue. Ticking a row and pressing Add writes
 * a SELECTION carrying the provider's own model id — which is what makes the
 * model exist, since nothing else knows the id to put on the wire — plus the
 * lane and priority you chose. Remove deletes that row. Those two operations
 * are the entire vocabulary.
 */
const CatalogDoc = graphql(/* GraphQL */ `
  query BrowseCatalog($provider: String!, $credential: String) {
    corrallm {
      browseCatalog(provider: $provider, credential: $credential) {
        provider
        credential
        url
        hasFilter
        error
        entries {
          id
          servedName
          name
          contextLength
          free
          promptUsd
          completionUsd
          inputModality
          outputModality
          assigned
          lanes {
            lane
            order
          }
          enrolled
          passesFilter
          declared
          conflictsWith
        }
      }
      overview {
        lanes {
          name
        }
      }
    }
  }
`)

const AssignDoc = graphql(/* GraphQL */ `
  mutation AssignModel($body: corrallm_AssignModelInputBodyInput!) {
    corrallm {
      assignModel(body: $body) {
        ok
        message
      }
    }
  }
`)

const UnassignDoc = graphql(/* GraphQL */ `
  mutation UnassignModel($body: corrallm_UnassignModelInputBodyInput!) {
    corrallm {
      unassignModel(body: $body) {
        ok
        message
      }
    }
  }
`)

// Prices arrive as dollars per TOKEN, which is unreadable at a glance — every
// value is 0.0000004. Per-million is the unit vendors quote and people compare.
//
// A NEGATIVE price is OpenRouter's marker on its router pseudo-models
// ("openrouter/auto"), whose real cost depends on which model the router picks.
// Rendering the arithmetic gives "$-1000000.00", so it reads as unknown — which
// is the truth.
function perMillion(usdPerToken: number): string {
  if (!usdPerToken || usdPerToken < 0) return '—'
  const m = usdPerToken * 1_000_000
  return m < 1 ? `$${m.toFixed(2)}` : `$${m.toFixed(m < 10 ? 2 : 0)}`
}

export function CatalogDialog(props: {
  open: boolean
  onClose: () => void
  provider: string
  credential: string
}) {
  const { open, onClose, provider, credential } = props
  const qc = useQueryClient()
  // A catalogue is a wide table over hundreds of rows. On a phone a centred
  // dialog leaves it a few hundred pixels to work in, so it takes the screen;
  // and the two price columns fold into the model cell rather than being
  // truncated off the right edge where nothing hints they exist.
  const wide = useMediaQuery(theme.breakpoints.up('md'))
  const [q, setQ] = useState('')
  const [freeOnly, setFreeOnly] = useState(false)
  const [unassignedOnly, setUnassignedOnly] = useState(false)
  const [sel, setSel] = useState<Set<string>>(new Set())
  const [lane, setLane] = useState('')
  const [order, setOrder] = useState('50')
  const [msg, setMsg] = useState('')

  const { data, isFetching, error, refetch } = useQuery({
    queryKey: ['catalog', provider, credential],
    queryFn: () => gqlClient.request(CatalogDoc, { provider, credential }),
    enabled: open,
    // A remote catalogue is a slow fetch over someone else's network; holding
    // it for the length of a session beats re-paying on every reopen.
    staleTime: 5 * 60 * 1000,
  })

  const assign = useMutation({
    mutationFn: (body: Corrallm_AssignModelInputBodyInput) =>
      gqlClient.request(AssignDoc, { body }),
  })
  const unassign = useMutation({
    mutationFn: (body: Corrallm_UnassignModelInputBodyInput) =>
      gqlClient.request(UnassignDoc, { body }),
  })

  const cat = data?.corrallm?.browseCatalog
  const lanes = (data?.corrallm?.overview?.lanes ?? []).map((l) => l.name)
  const entries = useMemo(() => cat?.entries ?? [], [cat])

  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return entries.filter((e) => {
      if (freeOnly && !e.free) return false
      if (unassignedOnly && e.assigned) return false
      if (!needle) return true
      return (
        e.id.toLowerCase().includes(needle) ||
        e.name.toLowerCase().includes(needle) ||
        e.servedName.toLowerCase().includes(needle)
      )
    })
  }, [entries, q, freeOnly, unassignedOnly])

  const toggle = (id: string) =>
    setSel((s) => {
      const n = new Set(s)
      if (!n.delete(id)) n.add(id)
      return n
    })

  const apply = async (action: 'add' | 'remove') => {
    setMsg('')
    const rows = entries.filter((e) => sel.has(e.id))
    let failed = 0
    // Sequential rather than parallel: each write reloads the selection set and
    // rebuilds the served registry, and firing thirty of those at once makes
    // the last writer's view the only one that survives.
    for (const p of rows) {
      const ok =
        action === 'add'
          ? (
              await assign.mutateAsync({
                provider,
                credential,
                model: p.servedName,
                // The provider's own id, always sent on an add: it is the only
                // record of what goes on the wire.
                upstream: p.id,
                lanes: lane ? [{ lane, order: String(Number(order || 0)) }] : [],
                quality: 0,
              })
            ).corrallm?.assignModel?.ok
          : (
              await unassign.mutateAsync({
                provider,
                credential,
                model: p.servedName,
              })
            ).corrallm?.unassignModel?.ok
      if (!ok) failed++
    }
    setSel(new Set())
    setMsg(
      failed
        ? `${rows.length - failed} of ${rows.length} saved; ${failed} refused`
        : `${rows.length} ${action === 'add' ? 'added' : 'removed'}`,
    )
    qc.invalidateQueries({ queryKey: ['selections'] })
    qc.invalidateQueries({ queryKey: ['providers'] })
    refetch()
  }

  // Only rows that are actually assigned can be removed, so the Remove button
  // reports what it would really do rather than counting ticks that are no-ops.
  const selectedAssigned = entries.filter((e) => sel.has(e.id) && e.assigned).length

  return (
    <Dialog open={open} onClose={onClose} maxWidth="lg" fullWidth fullScreen={!wide}>
      <DialogTitle sx={{ pb: 1 }}>
        {provider} directory
        <Typography variant="caption" sx={{ display: 'block', color: C.textFaint }}>
          as seen by <strong>{credential}</strong>
          {cat?.url ? ` · ${cat.url}` : ''}
        </Typography>
      </DialogTitle>
      <DialogContent dividers sx={{ p: 0 }}>
        <Box sx={{ p: 2, pb: 1.5 }}>
          <Stack direction="row" spacing={2} alignItems="center" flexWrap="wrap" useFlexGap>
            <TextField
              size="small"
              autoFocus
              label="Search"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="qwen, gpt, :free…"
              sx={{ minWidth: 220 }}
            />
            <FormControlLabel
              control={
                <Checkbox
                  size="small"
                  checked={freeOnly}
                  onChange={(e) => setFreeOnly(e.target.checked)}
                />
              }
              label={<Typography variant="body2">Free only</Typography>}
            />
            <FormControlLabel
              control={
                <Checkbox
                  size="small"
                  checked={unassignedOnly}
                  onChange={(e) => setUnassignedOnly(e.target.checked)}
                />
              }
              label={<Typography variant="body2">Not added yet</Typography>}
            />
            <Typography variant="caption" sx={{ color: C.textFaint, ml: 'auto' }}>
              {shown.length} of {entries.length}
            </Typography>
          </Stack>
        </Box>

        {isFetching && <CircularProgress size={20} sx={{ m: 2 }} />}
        {error && <Alert severity="error">{String(error)}</Alert>}
        {cat?.error && (
          <Alert severity="warning" sx={{ mx: 2, mb: 2 }}>
            {cat.error}
            <Typography variant="caption" sx={{ display: 'block', mt: 0.5 }}>
              Tried <code>{cat.url}</code> — a wrong base path shows up here as a 404 from a host
              that plainly exists.
            </Typography>
          </Alert>
        )}

        {!isFetching && !cat?.error && (
          <TableContainer sx={{ maxHeight: wide ? '52vh' : 'none' }}>
            <Table size="small" stickyHeader>
              <TableHead>
                <TableRow>
                  <TableCell padding="checkbox" />
                  <TableCell>Model</TableCell>
                  <TableCell align="right">Context</TableCell>
                  {wide && <TableCell align="right">In / M</TableCell>}
                  {wide && <TableCell align="right">Out / M</TableCell>}
                  <TableCell>Status</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {shown.map((e) => {
                  const locked = e.declared
                  return (
                    <TableRow key={e.id} hover selected={sel.has(e.id)}>
                      <TableCell padding="checkbox">
                        <Tooltip
                          title={locked ? 'Declared in config by hand — already yours' : ''}
                        >
                          <span>
                            <Checkbox
                              size="small"
                              disabled={locked}
                              checked={sel.has(e.id)}
                              onChange={() => toggle(e.id)}
                            />
                          </span>
                        </Tooltip>
                      </TableCell>
                      <TableCell>
                        <Typography
                          variant="body2"
                          sx={{ fontFamily: 'monospace', fontSize: 12.5 }}
                        >
                          {e.id}
                        </Typography>
                        <Typography variant="caption" sx={{ color: C.textFaint }}>
                          → {e.servedName}
                          {e.inputModality && e.inputModality !== 'text'
                            ? ` · ${e.inputModality}`
                            : ''}
                          {!wide && !e.free
                            ? ` · ${perMillion(e.promptUsd)} / ${perMillion(e.completionUsd)} per M`
                            : ''}
                        </Typography>
                      </TableCell>
                      <TableCell align="right">
                        {e.contextLength && e.contextLength !== '0'
                          ? fmtInt(Number(e.contextLength))
                          : '—'}
                      </TableCell>
                      {wide && (
                        <TableCell align="right">
                          {e.free ? '—' : perMillion(e.promptUsd)}
                        </TableCell>
                      )}
                      {wide && (
                        <TableCell align="right">
                          {e.free ? '—' : perMillion(e.completionUsd)}
                        </TableCell>
                      )}
                      <TableCell>
                        <Stack direction="row" spacing={0.5} flexWrap="wrap" useFlexGap>
                          {e.free && <Chip size="small" color="success" label="free" />}
                          {e.declared && <Chip size="small" variant="outlined" label="declared" />}
                          {e.assigned && (
                            <Tooltip title="You added this one. Tick it and press Remove to take it out of service.">
                              <Chip
                                size="small"
                                color="success"
                                label={
                                  e.lanes.length
                                    ? `added · ${e.lanes.map((l) => `${l.lane}@${l.order}`).join(', ')}`
                                    : 'added'
                                }
                              />
                            </Tooltip>
                          )}
                          {!e.assigned && e.enrolled && (
                            <Tooltip title="Admitted by this provider's discover filter, not by you. It already serves; adding it pins it so a filter change cannot drop it.">
                              <Chip size="small" variant="outlined" label="from filter" />
                            </Tooltip>
                          )}
                          {!e.assigned && !e.enrolled && e.passesFilter && (
                            <Tooltip title="The filter admits this row, but a cap or a pending refresh means it is not serving yet.">
                              <Chip size="small" variant="outlined" label="in filter" />
                            </Tooltip>
                          )}
                          {e.conflictsWith && (
                            <Tooltip
                              title={`${e.servedName} currently serves ${e.conflictsWith}. Served names drop ':free' and rewrite '/', so both ids collapse to one name — adding this REPOINTS it.`}
                            >
                              <Chip size="small" color="warning" label="name taken" />
                            </Tooltip>
                          )}
                        </Stack>
                      </TableCell>
                    </TableRow>
                  )
                })}
                {shown.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={wide ? 6 : 4}>
                      <Typography variant="body2" sx={{ color: C.textFaint, py: 2 }}>
                        Nothing matches.
                      </Typography>
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </TableContainer>
        )}
      </DialogContent>
      <DialogActions sx={{ gap: 1, flexWrap: 'wrap' }}>
        {msg && (
          <Typography variant="caption" sx={{ mr: 'auto', ml: 1 }}>
            {msg}
          </Typography>
        )}
        <TextField
          select
          size="small"
          label="Lane"
          value={lane}
          onChange={(e) => setLane(e.target.value)}
          sx={{ minWidth: 130 }}
        >
          <MenuItem value="">(none)</MenuItem>
          {lanes.map((l) => (
            <MenuItem key={l} value={l}>
              {l}
            </MenuItem>
          ))}
        </TextField>
        <Tooltip title="Position in that lane's ladder; lower is tried first.">
          <TextField
            size="small"
            label="Order"
            value={order}
            onChange={(e) => setOrder(e.target.value)}
            sx={{ width: 90 }}
          />
        </Tooltip>
        <Button onClick={onClose}>Close</Button>
        <Tooltip title="Take these out of service. For a model you added this removes it; one the filter contributes comes back on the next refresh.">
          <span>
            <Button
              color="error"
              disabled={selectedAssigned === 0 || unassign.isPending}
              onClick={() => apply('remove')}
            >
              Remove {selectedAssigned || ''}
            </Button>
          </span>
        </Tooltip>
        <Button
          variant="contained"
          disabled={sel.size === 0 || assign.isPending}
          onClick={() => apply('add')}
        >
          {assign.isPending ? 'Saving…' : `Add ${sel.size || ''}`}
        </Button>
      </DialogActions>
    </Dialog>
  )
}
