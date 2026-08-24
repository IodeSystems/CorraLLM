import { Box, CircularProgress } from '@mui/material'

// The one loading indicator, centered.
//
// Every page and panel used to spell this itself as a spinner in a padded Box,
// which left it jammed against the top-left corner of an otherwise empty page —
// it read as a stray element rather than as "this is loading". Twelve copies
// also meant twelve slightly different paddings and sizes.
//
// Centered on BOTH axes against a minimum height, so the spinner sits where the
// eye already is — the middle of the space the content is about to fill — and
// so the page does not visibly jump when that content arrives.
//
//   size    spinner diameter; the default suits a page, `size={20}` a panel.
//   minHeight  the space to center within. The default is a share of the
//           VIEWPORT, not a fixed band: a page-level loader centered inside 240
//           fixed pixels still sits near the top of a tall empty window, which
//           is the complaint this component was written for. Panels pass a
//           small fixed height instead, because a panel body is not the page.
export function Loading(props: { size?: number; minHeight?: number | string }) {
  return (
    <Box
      sx={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        minHeight: props.minHeight ?? '50vh',
        p: 3,
      }}
    >
      <CircularProgress size={props.size} />
    </Box>
  )
}
