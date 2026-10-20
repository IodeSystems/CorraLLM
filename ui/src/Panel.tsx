import type { ReactNode } from 'react'
import { Box, Paper, Tooltip, Typography } from '@mui/material'
import InfoOutlinedIcon from '@mui/icons-material/InfoOutlined'
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
//   title     the label. Short, plain, sentence case — a LABEL, not prose.
//   subtitle  a second label fragment, muted, in the header. ONE LINE, clipped
//             with an ellipsis when it does not fit. See below.
//   note      prose that belongs to the panel: a band under the header, above
//             the body, wrapping to as many lines as it needs.
//   help      longer explanation, in an info-icon tooltip next to the title.
//             Use it instead of `note` when the text explains and nobody has to
//             read it before acting.
//   badge     a count or status chip pinned next to the title.
//   actions   controls, right-aligned in the header bar.
//   flush     body gets no padding — for a table or list that should meet the
//             panel's own edges. Default (false) pads the body.
//   dense     tighter body padding, for panels holding a single line or two.
//
// THE HEADER BAR IS A LABEL STRIP, AND `subtitle` STILL TRUNCATES ON PURPOSE.
//
// A name, a count and some controls, one line tall, the same height on every
// panel of a page — that is what makes a dashboard scannable, and clipping a
// LABEL costs nothing, because a label's tail only restates its head. A long
// model name losing its last characters is the header working.
//
// A SENTENCE losing its tail is the header lying. The clause that goes first is
// the subject, and the clause that goes last is what happens — so truncation
// takes the consequence and leaves the topic, which reads as complete. Somebody
// who came to find out whether Change cuts off a request in flight read
// "…and a reques…" and closed the lid (OB-2). Nothing on screen said a word was
// missing, and the width that decided it was his window's, not the author's.
//
// So the rule is about what may be PUT there, not about the CSS:
//
//   A subtitle stays in the header only if BOTH hold:
//     1. it is one sentence — no sentence break inside it; and
//     2. it does not say what happens to a request somebody is waiting on.
//   Otherwise it is `note`.
//
// Test 2 is the one that matters and test 1 is the one a machine can check, so
// eslint.config.js enforces test 1 (`corrallm/subtitle-is-a-label`) and this
// comment is the standard for test 2. Widening the bar was rejected: a
// paragraph wrapped into a tinted strip pushes the thing the reader came for
// below the explanation of it (OB-11), and makes every panel's height depend on
// the reader's window.
//
// `note` is where the moved sentences go. It is the band callers.tsx and
// groups.tsx were each hand-rolling out of a Row, and it sits ABOVE the body
// because a consequence is read before the rows it applies to, once for the
// group rather than on each of thirty identical buttons (OB-2). It pads itself,
// so it looks the same on a flush panel and a padded one.
//
// Nested surfaces: use `Row` for items INSIDE a panel. Rows are separated by
// borders, not by their own boxes — a card inside a card is the thing that makes
// dense dashboards muddy.

export function Panel(props: {
  title?: ReactNode
  subtitle?: ReactNode
  note?: ReactNode
  help?: ReactNode
  badge?: ReactNode
  actions?: ReactNode
  flush?: boolean
  dense?: boolean
  children: ReactNode
}) {
  const { title, subtitle, note, help, badge, actions, flush, dense, children } = props
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
            <Typography
              variant="overline"
              sx={{ color: C.text, whiteSpace: 'nowrap', textTransform: 'none', fontSize: 13, letterSpacing: 0 }}
            >
              {title}
            </Typography>
          )}
          {help != null && (
            <Tooltip title={help} arrow placement="bottom-start">
              <InfoOutlinedIcon aria-label="More information" sx={{ fontSize: 16, color: C.textFaint, cursor: 'help' }} />
            </Tooltip>
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
      {note != null && (
        // Outside the body box, so it carries its own padding and reads the
        // same whether the body is flush, dense or padded. Muted and body2 —
        // the size the rest of a panel's prose is set in, not caption: this is
        // the sentence somebody has to read before pressing something.
        <Box
          sx={{
            px: 1.75,
            py: 1.25,
            borderBottom: `1px solid ${C.border}`,
          }}
        >
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            {note}
          </Typography>
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

// Name: a model, key or placement name in a table cell. One line, clipped with an
// ellipsis, full text on hover — names break at every dash otherwise and a row
// becomes five lines tall.
export function Name({ children, max = 260 }: { children: string; max?: number }) {
  return (
    <Tooltip title={children} placement="top-start">
      <Box
        component="span"
        sx={{
          display: 'inline-block',
          maxWidth: max,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
          verticalAlign: 'bottom',
        }}
      >
        {children}
      </Box>
    </Tooltip>
  )
}
