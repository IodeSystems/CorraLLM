import type { ReactNode } from 'react'
import { Box, Paper, Typography } from '@mui/material'
import { C } from '@/theme'

// What a PANEL is, defined once.
//
// A panel is a bordered surface with a tinted header bar. That is the whole
// contract, and everything on a page is either a panel or lives inside one —
// which is what stops a page from reading as one undifferentiated sheet. The
// header bar is not decoration: it is the boundary marker. A bare <Typography>
// heading floating above a white box (the old pattern) gives the eye nothing to
// anchor the box's extent to.
//
//   title     the label. Rendered small/uppercase/tracked — a LABEL, not prose.
//   subtitle  one line of explanation, muted, in the header.
//   badge     a count or status chip pinned next to the title.
//   actions   controls, right-aligned in the header bar.
//   flush     body gets no padding — for a table or list that should meet the
//             panel's own edges. Default (false) pads the body.
//   dense     tighter body padding, for panels holding a single line or two.
//
// Nested surfaces: use `Row` for items INSIDE a panel. Rows are separated by
// borders, not by their own boxes — a card inside a card is the thing that makes
// dense dashboards muddy.

export function Panel(props: {
  title?: ReactNode
  subtitle?: ReactNode
  badge?: ReactNode
  actions?: ReactNode
  flush?: boolean
  dense?: boolean
  children: ReactNode
}) {
  const { title, subtitle, badge, actions, flush, dense, children } = props
  const hasHeader = title != null || actions != null || badge != null
  return (
    <Paper elevation={0} sx={{ overflow: 'hidden' }}>
      {hasHeader && (
        <Box
          sx={{
            display: 'flex',
            alignItems: 'center',
            gap: 1,
            px: 1.75,
            py: 1,
            bgcolor: C.raised,
            borderBottom: `1px solid ${C.border}`,
            minHeight: 40,
          }}
        >
          {title != null && (
            <Typography variant="overline" sx={{ color: C.text, whiteSpace: 'nowrap' }}>
              {title}
            </Typography>
          )}
          {badge}
          {subtitle != null && (
            <Typography
              variant="caption"
              sx={{ color: C.textMuted, ml: 0.5, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
            >
              {subtitle}
            </Typography>
          )}
          {actions != null && (
            <Box sx={{ ml: 'auto', display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap' }}>
              {actions}
            </Box>
          )}
        </Box>
      )}
      <Box sx={{ p: flush ? 0 : dense ? 1.25 : 2 }}>{children}</Box>
    </Paper>
  )
}

// Row is an item inside a panel: separated from its neighbors by a hairline, not
// by its own border/elevation. Keeps a list of models or lanes scannable without
// stacking surfaces.
export function Row(props: { children: ReactNode; onClick?: () => void }) {
  const { children, onClick } = props
  return (
    <Box
      onClick={onClick}
      sx={{
        px: 1.75,
        py: 1.25,
        borderBottom: `1px solid ${C.border}`,
        '&:last-of-type': { borderBottom: 0 },
        ...(onClick ? { cursor: 'pointer', '&:hover': { bgcolor: C.raised } } : {}),
      }}
    >
      {children}
    </Box>
  )
}

// Stat is the small "label over value" unit used for capacity/quality/slots —
// the replacement for one-row tables, which cost a header, a border box, and a
// scroll container to show four numbers.
export function Stat(props: { label: ReactNode; value: ReactNode; title?: string }) {
  return (
    <Box sx={{ minWidth: 64 }} title={props.title}>
      <Typography
        variant="overline"
        sx={{ color: C.textFaint, display: 'block', fontSize: 10, lineHeight: 1.4 }}
      >
        {props.label}
      </Typography>
      <Typography variant="body2" sx={{ fontVariantNumeric: 'tabular-nums' }}>
        {props.value}
      </Typography>
    </Box>
  )
}

// PageHeader: the one h6 on a page, with room for page-level controls. Pages get
// exactly one; every other heading is a panel title.
export function PageHeader(props: { title: ReactNode; children?: ReactNode }) {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5, flexWrap: 'wrap' }}>
      <Typography variant="h6">{props.title}</Typography>
      {props.children}
    </Box>
  )
}
