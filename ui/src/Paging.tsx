import { useState } from 'react'
import { Box, Button, TextField, Typography } from '@mui/material'
import SearchIcon from '@mui/icons-material/Search'
import { C } from '@/theme'

// Reading a long list in pieces.
//
// Every list on a settings page is short until the box has been run for a year,
// and then it is not: a catalogue selection per model per provider, a revision
// per save. A list that only ever grows is a list nobody scrolls to the bottom
// of, so the ones that grow get a filter above them and a page footer below.
//
// Client-side on purpose. These lists arrive whole in one query already — the
// server has no offset for selections or lanes — so paging here costs one
// slice() and buys the reader a screen they can finish. Where the SERVER is the
// one holding back rows (configHistory's limit: 50), the footer says so rather
// than implying the last page is the end of the record.

export type PagerState = {
  page: number
  pages: number
  from: number
  total: number
  pageSize: number
  setPage: (n: number) => void
}

/**
 * Slice `rows` into pages of `pageSize`.
 *
 * The page index is CLAMPED rather than reset: a filter that narrows the list
 * under the reader — or a row deleted off the last page — would otherwise leave
 * them looking at an empty panel with no hint that the content moved up.
 */
export function usePaging<T>(rows: readonly T[], pageSize: number): { rows: T[]; pager: PagerState } {
  const [page, setPage] = useState(0)
  const pages = Math.max(1, Math.ceil(rows.length / pageSize))
  const at = Math.min(Math.max(0, page), pages - 1)
  const from = at * pageSize
  return {
    rows: rows.slice(from, from + pageSize),
    pager: { page: at, pages, from, total: rows.length, pageSize, setPage },
  }
}

/**
 * The footer: where you are, how much there is, and the two controls.
 *
 * Renders nothing when everything fits — a pager under a four-row list is
 * furniture that says "there is more" when there is not.
 */
export function Pager(props: { p: PagerState; unit: string; capped?: boolean }) {
  const { p, unit, capped } = props
  if (p.total <= p.pageSize) return null
  const to = Math.min(p.from + p.pageSize, p.total)
  return (
    <Box
      sx={{
        display: 'flex',
        alignItems: 'center',
        gap: 1,
        px: 1.75,
        py: 1,
        borderTop: `1px solid ${C.border}`,
      }}
    >
      <Typography variant="caption" sx={{ color: C.textMuted, fontVariantNumeric: 'tabular-nums' }}>
        {p.from + 1}–{to} of {p.total} {unit}
        {/* The server's limit, not the end of the list. Said here because a
            last page that is silently truncated reads as "that is all of it". */}
        {capped && ' (most recent only)'}
      </Typography>
      <Box sx={{ ml: 'auto', display: 'flex', gap: 0.5 }}>
        <Button size="small" disabled={p.page === 0} onClick={() => p.setPage(p.page - 1)}>
          Prev
        </Button>
        <Button
          size="small"
          disabled={p.page >= p.pages - 1}
          onClick={() => p.setPage(p.page + 1)}
        >
          Next
        </Button>
      </Box>
    </Box>
  )
}

/**
 * The filter above a list. Narrow, unlabelled, placeholder-only: it goes in a
 * Panel's action bar, which is 40px tall and already holds the count.
 */
export function FilterField(props: {
  value: string
  onChange: (v: string) => void
  placeholder: string
  width?: number
}) {
  return (
    <TextField
      size="small"
      value={props.value}
      onChange={(e) => props.onChange(e.target.value)}
      placeholder={props.placeholder}
      sx={{ width: props.width ?? 200, '& .MuiInputBase-input': { fontSize: 12.5, py: 0.5 } }}
      slotProps={{
        input: {
          startAdornment: <SearchIcon sx={{ fontSize: 15, mr: 0.75, color: C.textFaint }} />,
        },
      }}
    />
  )
}

/** Case-insensitive substring match over several fields of a row. */
export function matches(q: string, ...fields: (string | null | undefined)[]): boolean {
  const needle = q.trim().toLowerCase()
  if (!needle) return true
  return fields.some((f) => (f ?? '').toLowerCase().includes(needle))
}
