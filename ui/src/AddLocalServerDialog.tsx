import { useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  InputAdornment,
  Stack,
  TextField,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import type { LocalServerProbeQuery } from '@/gql/graphql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'

/**
 * Add THIS machine as a server, from its own probe, after the operator has
 * seen and adjusted it.
 *
 * The probe proposes each pool at the card's or the RAM's full size, less a
 * reserve for what the desktop and other programs use plus a buffer. Writing
 * that without showing it is how a box got a budget that left nothing for the
 * OS, so the numbers come up in a form first.
 */

// Whether a server describes this machine, and what declaring it would write.
export const LocalServerProbeDoc = graphql(/* GraphQL */ `
  query LocalServerProbe {
    corrallm {
      localServerProbe {
        existing
        name
        pools
        devicePool
        poolDetail {
          pool
          totalBytes
          reserveBytes
          reason
          card
          uuid
          bound
          ecc
        }
      }
    }
  }
`)

const AddLocalServerDoc = graphql(/* GraphQL */ `
  mutation AddLocalServer($body: corrallm_LocalServerEditInput) {
    corrallm {
      addLocalServer(body: $body) {
        ok
        message
        server
      }
    }
  }
`)

export type LocalServerProbe = NonNullable<
  NonNullable<LocalServerProbeQuery['corrallm']>['localServerProbe']
>

type Row = { pool: string; total: string; reserve: string }

// Long arrives as a decimal string. Whole GB (10^9), as the config writes it.
const toGB = (bytes: string | number) => String(Math.round(Number(bytes) / 1e9))

// A size field's value as whole GB, or null when it is not one.
const parseGB = (s: string): number | null => {
  const t = s.trim()
  if (!/^\d+$/.test(t)) return null
  return Number(t)
}

export function AddLocalServerDialog(props: {
  open: boolean
  /** The server written, or null when cancelled. */
  onClose: (server: string | null) => void
}) {
  const { open, onClose } = props
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [rows, setRows] = useState<Row[]>([])
  const [err, setErr] = useState('')
  const [saving, setSaving] = useState(false)

  // Probed again on every open: what other programs use changes, and the
  // reserve is proposed from it. The form fills once per open, from a probe
  // taken after it opened, so a late answer cannot overwrite the operator's
  // edits.
  const { data, dataUpdatedAt, refetch } = useQuery({
    queryKey: ['localServerProbe'],
    queryFn: () => gqlClient.request(LocalServerProbeDoc),
    enabled: open,
  })
  const probe: LocalServerProbe | null | undefined = data?.corrallm?.localServerProbe
  const [openedAt, setOpenedAt] = useState(0)
  const [filled, setFilled] = useState(false)
  useEffect(() => {
    setFilled(false)
    setErr('')
    if (!open) {
      setOpenedAt(0)
      return
    }
    setOpenedAt(Date.now())
    void refetch()
  }, [open, refetch])
  useEffect(() => {
    if (!open || filled || !probe || openedAt === 0 || dataUpdatedAt < openedAt) return
    setName(probe.name)
    setRows(
      probe.poolDetail.map((p) => ({
        pool: p.pool,
        total: toGB(p.totalBytes),
        reserve: toGB(p.reserveBytes),
      })),
    )
    setFilled(true)
  }, [open, filled, probe, openedAt, dataUpdatedAt])

  const setRow = (i: number, k: 'total' | 'reserve', v: string) =>
    setRows((rs) => rs.map((r, j) => (j === i ? { ...r, [k]: v } : r)))

  const problems = rows.map((r) => {
    const t = parseGB(r.total)
    const res = parseGB(r.reserve)
    if (t === null || t <= 0) return 'Size must be a whole number of GB above 0 GB'
    if (res === null) return 'Reserve must be a whole number of GB'
    if (res > t) return 'Reserve is more than the size'
    return ''
  })
  const invalid = !filled || !name.trim() || problems.some((p) => p !== '')

  const save = async () => {
    if (!probe || !filled) return
    setErr('')
    setSaving(true)
    const detail = new Map(probe.poolDetail.map((p) => [p.pool, p]))
    try {
      const r = await gqlClient.request(AddLocalServerDoc, {
        body: {
          name: name.trim(),
          pools: Object.fromEntries(rows.map((r) => [r.pool, `${r.total.trim()}GB`])),
          reserve: Object.fromEntries(
            rows.filter((r) => parseGB(r.reserve)! > 0).map((r) => [r.pool, `${r.reserve.trim()}GB`]),
          ),
          devices: Object.fromEntries(
            rows.flatMap((r) => {
              const d = detail.get(r.pool)
              return d?.bound && d.uuid ? [[r.pool, d.uuid]] : []
            }),
          ),
          devicePool: probe.devicePool,
          // ECC is left out: detection keeps running, and an override written
          // now would pin an answer nobody chose.
        },
      })
      const res = r.corrallm?.addLocalServer
      if (!res?.ok || !res.server) {
        setErr(res?.message ?? 'save refused')
        return
      }
      qc.invalidateQueries({ queryKey: ['localModelServers'] })
      qc.invalidateQueries({ queryKey: ['localServerProbe'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
      onClose(res.server)
    } catch (e) {
      setErr(String(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open={open} onClose={() => onClose(null)} maxWidth="sm" fullWidth>
      <DialogTitle>Add this machine</DialogTitle>
      <DialogContent dividers>
        <Stack spacing={2} sx={{ pt: 0.5 }}>
          <Typography variant="caption" sx={{ color: C.textFaint }}>
            Sized from a probe of this machine just now. The reserve is memory corrallm leaves
            free for the desktop and other programs; models are placed in what is left.
          </Typography>
          {!filled && <Typography variant="body2">Probing this machine…</Typography>}
          <TextField
            size="small"
            label="Server name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            error={!name.trim()}
            helperText={!name.trim() ? 'Required' : 'Models name this server to run here'}
          />
          {rows.map((r, i) => {
            const d = probe?.poolDetail.find((p) => p.pool === r.pool)
            const t = parseGB(r.total)
            const res = parseGB(r.reserve)
            const budget = t !== null && res !== null ? t - res : null
            return (
              <Box key={r.pool} sx={{ borderTop: `1px solid ${C.border}`, pt: 1.5 }}>
                <Typography variant="subtitle2">
                  {r.pool}
                  {d?.card ? ` — ${d.card}` : ''}
                </Typography>
                {d?.uuid && (
                  <Typography variant="caption" sx={{ color: C.textFaint, display: 'block' }}>
                    {d.bound ? 'bound to ' : ''}
                    {d.uuid}
                  </Typography>
                )}
                <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap', alignItems: 'center', mt: 1 }}>
                  <TextField
                    size="small"
                    label="Size"
                    value={r.total}
                    onChange={(e) => setRow(i, 'total', e.target.value)}
                    error={t === null || t <= 0}
                    sx={{ width: 120 }}
                    slotProps={{
                      htmlInput: { inputMode: 'numeric' },
                      input: { endAdornment: <InputAdornment position="end">GB</InputAdornment> },
                    }}
                  />
                  <TextField
                    size="small"
                    label="Reserve"
                    value={r.reserve}
                    onChange={(e) => setRow(i, 'reserve', e.target.value)}
                    error={!!problems[i] && t !== null && t > 0}
                    sx={{ width: 120 }}
                    slotProps={{
                      htmlInput: { inputMode: 'numeric' },
                      input: { endAdornment: <InputAdornment position="end">GB</InputAdornment> },
                    }}
                  />
                  <Typography variant="body2">
                    Budget: <b>{budget !== null && budget >= 0 ? `${budget} GB` : '—'}</b>
                  </Typography>
                  <Typography variant="body2" sx={{ color: C.textMuted }}>
                    ECC: {d?.ecc ? 'yes' : 'no'} (detected)
                  </Typography>
                </Box>
                <Typography
                  variant="caption"
                  sx={{ color: problems[i] ? C.error : C.textFaint, display: 'block', mt: 0.5 }}
                >
                  {problems[i] || `Proposed reserve ${d ? toGB(d.reserveBytes) : '?'} GB: ${d?.reason ?? ''}`}
                </Typography>
              </Box>
            )
          })}
          {err && <Alert severity="error">{err}</Alert>}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={() => onClose(null)}>Cancel</Button>
        <Button variant="contained" onClick={save} disabled={invalid || saving}>
          {saving ? 'Saving…' : 'Add server'}
        </Button>
      </DialogActions>
    </Dialog>
  )
}
