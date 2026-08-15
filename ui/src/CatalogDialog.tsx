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
import type { Corrallm_DecideApprovalInputBodyInput } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { fmtInt } from '@/format'

/**
 * Browse one credential's catalogue and enrol models from it.
 *
 * This is the answer to "go to the provider and pick the models I want", which
 * discovery cannot be: discovery is a FILTER, and everything it rejects is
 * dropped with no record it existed. Reaching one excluded model used to mean
 * loosening the filter and admitting a hundred others with it.
 *
 * Picking here writes an APPROVAL carrying the provider's own model id, which
 * is what makes the model exist at all — nothing else knows the id to put on
 * the wire, and the served name cannot be turned back into it.
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
          state
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

const DecideDoc = graphql(/* GraphQL */ `
  mutation DecideCatalogPick($body: corrallm_DecideApprovalInputBodyInput!) {
    corrallm {
      decideApproval(body: $body) {
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
  const [undecidedOnly, setUndecidedOnly] = useState(false)
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

  const decide = useMutation({
    mutationFn: (body: Corrallm_DecideApprovalInputBodyInput) =>
      gqlClient.request(DecideDoc, { body }),
  })

  const cat = data?.corrallm?.browseCatalog
  const lanes = (data?.corrallm?.overview?.lanes ?? []).map((l) => l.name)
  const entries = useMemo(() => cat?.entries ?? [], [cat])

  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return entries.filter((e) => {
      if (freeOnly && !e.free) return false
      if (undecidedOnly && (e.state || e.enrolled)) return false
      if (!needle) return true
      return (
        e.id.toLowerCase().includes(needle) ||
        e.name.toLowerCase().includes(needle) ||
        e.servedName.toLowerCase().includes(needle)
      )
    })
  }, [entries, q, freeOnly, undecidedOnly])

  const toggle = (id: string) =>
    setSel((s) => {
      const n = new Set(s)
      if (!n.delete(id)) n.add(id)
      return n
    })

  const enrol = async (state: 'approved' | 'rejected') => {
    setMsg('')
    const picks = entries.filter((e) => sel.has(e.id))
    let failed = 0
    // Sequential rather than parallel: each decision reloads the approval set
    // and rebuilds the served registry, and firing thirty of those at once
    // makes the last writer's view the only one that survives.
    for (const p of picks) {
      const r = await decide.mutateAsync({
        provider,
        credential,
        model: p.servedName,
        state,
        upstream: p.id,
        lanes:
          state === 'approved' && lane ? [{ lane, order: String(Number(order || 0)) }] : [],
        quality: 0,
      })
      if (!r.corrallm?.decideApproval?.ok) failed++
    }
    setSel(new Set())
    setMsg(
      failed
        ? `${picks.length - failed} of ${picks.length} saved; ${failed} refused`
        : `${picks.length} ${state}`,
    )
    qc.invalidateQueries({ queryKey: ['approvals'] })
    qc.invalidateQueries({ queryKey: ['providers'] })
    refetch()
  }

  return (
    <Dialog open={open} onClose={onClose} maxWidth="lg" fullWidth fullScreen={!wide}>
      <DialogTitle sx={{ pb: 1 }}>
        {provider} catalogue
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
                  checked={undecidedOnly}
                  onChange={(e) => setUndecidedOnly(e.target.checked)}
                />
              }
              label={<Typography variant="body2">Undecided only</Typography>}
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
                          {e.state === 'approved' && (
                            <Chip size="small" color="success" variant="outlined" label="approved" />
                          )}
                          {e.state === 'rejected' && (
                            <Chip size="small" color="error" variant="outlined" label="rejected" />
                          )}
                          {!e.state && e.enrolled && (
                            <Tooltip title="Admitted by this provider's discover filter — it already serves, with no decision recorded.">
                              <Chip size="small" variant="outlined" label="serving" />
                            </Tooltip>
                          )}
                          {!e.state && !e.enrolled && e.passesFilter && (
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
        <Button
          color="error"
          disabled={sel.size === 0 || decide.isPending}
          onClick={() => enrol('rejected')}
        >
          Reject {sel.size || ''}
        </Button>
        <Button
          variant="contained"
          disabled={sel.size === 0 || decide.isPending}
          onClick={() => enrol('approved')}
        >
          {decide.isPending ? 'Saving…' : `Add ${sel.size || ''}`}
        </Button>
      </DialogActions>
    </Dialog>
  )
}
