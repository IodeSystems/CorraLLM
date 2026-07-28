import { createTheme, alpha } from '@mui/material/styles'

// corrallm's dark ops theme.
//
// The problem it fixes: MUI's default light theme paints every surface #fff on
// #fff, so a page of cards, tables, and section headings has no edges — a dozen
// panels read as one undifferentiated sheet. Here a SURFACE is defined by three
// things at once (a step up in lightness, a real 1px border, and a tinted header
// bar), so a panel is legible as an object even before you read its title.
//
// Layers, darkest → lightest:
//   canvas   the page behind everything
//   surface  a panel
//   raised   a panel's header bar, the app bar, and rows inside a panel
//
// Elevation/shadow is deliberately NOT the primary separator: shadows disappear
// on dark backgrounds. Borders do the work.

export const C = {
  canvas: '#0f1115',
  surface: '#171a21',
  raised: '#1c2029',
  border: '#262b36',
  borderStrong: '#333a49',
  text: '#e6e8ee',
  textMuted: '#9aa3b2',
  textFaint: '#6b7484',
  accent: '#4f9cf9',
  ok: '#34d399',
  warn: '#fbbf24',
  error: '#f87171',
} as const

// Categorical series palette — fixed order, never cycled past its length.
// Validated (dataviz six checks) against the panel surface #171a21 in dark mode:
// lightness band, chroma floor, adjacent-pair CVD separation (worst ΔE 8.8),
// normal-vision floor (8.8+), and ≥3:1 contrast all PASS. Do not reorder or
// substitute values without re-running the validator — the order IS the check.
export const SERIES = [
  '#3b82f6', // blue
  '#ec4899', // pink
  '#16a34a', // green
  '#8b5cf6', // violet
  '#ea580c', // orange
  '#0d9488', // teal
] as const

// seriesColor assigns by index in fixed order. Beyond the palette it wraps —
// callers with unbounded series (per-key usage) accept that; charts that can
// bound their series should.
export const seriesColor = (i: number) => SERIES[i % SERIES.length]

// Status colors are RESERVED: they mean state, never "series 4". Each is paired
// with a text label everywhere it is used, so state is never color-alone.
export const STATUS = {
  ok: C.ok,
  warn: C.warn,
  error: C.error,
  info: C.accent,
  idle: C.textFaint,
} as const

export const theme = createTheme({
  palette: {
    mode: 'dark',
    background: { default: C.canvas, paper: C.surface },
    divider: C.border,
    text: { primary: C.text, secondary: C.textMuted, disabled: C.textFaint },
    primary: { main: C.accent },
    success: { main: C.ok },
    warning: { main: C.warn },
    error: { main: C.error },
    info: { main: C.accent },
  },
  shape: { borderRadius: 8 },
  typography: {
    fontFamily:
      '"Inter", -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif',
    // Section/panel titles are small, uppercase, and tracked — a header should
    // read as a LABEL for the box under it, not as body text that happens to be
    // bold. Panel applies this via `overline`.
    overline: {
      fontSize: 11,
      fontWeight: 700,
      letterSpacing: '0.08em',
      lineHeight: 1.6,
      textTransform: 'uppercase',
    },
    h6: { fontSize: 18, fontWeight: 650, letterSpacing: '-0.01em' },
    subtitle1: { fontSize: 14, fontWeight: 650 },
    subtitle2: { fontSize: 13, fontWeight: 650 },
    body2: { fontSize: 13 },
    caption: { fontSize: 12, color: C.textMuted },
    button: { textTransform: 'none', fontWeight: 600 },
  },
  components: {
    MuiCssBaseline: {
      styleOverrides: {
        body: { backgroundColor: C.canvas, color: C.text },
        // Numbers in tables must align on the decimal — proportional digits make
        // a column of durations impossible to scan.
        'table td, table th': { fontVariantNumeric: 'tabular-nums' },
        '::-webkit-scrollbar': { width: 10, height: 10 },
        '::-webkit-scrollbar-thumb': { background: C.borderStrong, borderRadius: 8 },
        '::-webkit-scrollbar-track': { background: 'transparent' },
      },
    },
    MuiPaper: {
      styleOverrides: {
        // MUI's dark mode fakes elevation with a white overlay gradient, which
        // is exactly the "white sauce" that flattens everything. Kill it; the
        // border defines the object.
        root: { backgroundImage: 'none', border: `1px solid ${C.border}` },
      },
    },
    MuiAppBar: {
      defaultProps: { elevation: 0, color: 'transparent' },
      styleOverrides: {
        root: {
          backgroundColor: C.raised,
          borderWidth: 0,
          borderBottom: `1px solid ${C.border}`,
        },
      },
    },
    MuiCard: { defaultProps: { elevation: 0 } },
    MuiTableCell: {
      styleOverrides: {
        root: { borderBottom: `1px solid ${C.border}`, paddingTop: 8, paddingBottom: 8 },
        head: {
          // NOT the raised tint: a flush table sits directly under a panel's
          // header bar, and two stacked tinted bars read as one confused block.
          // The column head keeps the body surface and separates itself with a
          // stronger rule instead.
          backgroundColor: C.surface,
          borderBottom: `1px solid ${C.borderStrong}`,
          color: C.textMuted,
          fontSize: 11,
          fontWeight: 700,
          letterSpacing: '0.06em',
          textTransform: 'uppercase',
          whiteSpace: 'nowrap',
        },
      },
    },
    MuiTableRow: {
      styleOverrides: {
        root: {
          '&:last-child td': { borderBottom: 0 },
          '&.MuiTableRow-hover:hover': { backgroundColor: alpha(C.accent, 0.07) },
        },
      },
    },
    MuiChip: {
      styleOverrides: {
        root: { fontWeight: 600 },
        sizeSmall: { height: 21, fontSize: 11.5 },
        outlined: { borderColor: C.borderStrong },
      },
    },
    MuiButton: {
      defaultProps: { disableElevation: true },
      styleOverrides: {
        outlined: { borderColor: C.borderStrong },
        sizeSmall: { paddingTop: 2, paddingBottom: 2 },
      },
    },
    MuiTooltip: {
      styleOverrides: {
        tooltip: {
          backgroundColor: C.raised,
          border: `1px solid ${C.borderStrong}`,
          color: C.text,
          fontSize: 12,
        },
        arrow: { color: C.raised },
      },
    },
    MuiAlert: {
      styleOverrides: {
        root: { border: `1px solid ${C.border}` },
        standardWarning: { backgroundColor: alpha(C.warn, 0.12), color: C.text },
        standardError: { backgroundColor: alpha(C.error, 0.12), color: C.text },
        standardSuccess: { backgroundColor: alpha(C.ok, 0.12), color: C.text },
        standardInfo: { backgroundColor: alpha(C.accent, 0.12), color: C.text },
      },
    },
    MuiDialog: { styleOverrides: { paper: { backgroundColor: C.surface } } },
    MuiDialogTitle: { styleOverrides: { root: { fontSize: 16, fontWeight: 650 } } },
    MuiLinearProgress: {
      styleOverrides: { root: { backgroundColor: C.border, borderRadius: 4 } },
    },
    MuiOutlinedInput: {
      styleOverrides: {
        root: { backgroundColor: C.raised },
        notchedOutline: { borderColor: C.borderStrong },
      },
    },
    MuiTab: { styleOverrides: { root: { textTransform: 'none', fontWeight: 600 } } },
    MuiDivider: { styleOverrides: { root: { borderColor: C.border } } },
  },
})
