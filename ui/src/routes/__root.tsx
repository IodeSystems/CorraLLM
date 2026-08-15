import { createRootRoute, Outlet, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  Alert,
  AlertTitle,
  AppBar,
  Box,
  Chip,
  CircularProgress,
  Toolbar,
  Tooltip,
  Typography,
} from '@mui/material'
import { C } from '@/theme'
import { useLiveEvents } from '@/useLiveEvents'
import { useAuthGate } from '@/auth'
import { Login } from '@/Login'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'

// Polled globally, not per-page. A bench run consumes GPU time everyone else is
// also queueing for, so someone watching Activity or Usage slow down needs to
// see the cause without first knowing that a Bench page exists. (It no longer
// LOCKS anyone out — the exclusive lease was removed — but contention still
// shows up as latency, and an unexplained slowdown sends people hunting.)
const CalibrationBannerDoc = graphql(/* GraphQL */ `
  query CalibrationBanner {
    corrallm {
      benchStatus {
        running
        startedAt
        log
      }
    }
  }
`)

function useRunState() {
  const { data } = useQuery({
    queryKey: ['calibrationBanner'],
    queryFn: () => gqlClient.request(CalibrationBannerDoc),
    // Cheap poll: this must appear promptly when a run starts, and disappear
    // promptly when it ends — a stale banner would send someone hunting
    // contention that already cleared.
    refetchInterval: 5000,
  })
  return { bench: data?.corrallm?.benchStatus }
}

/**
 * A persistent indicator in the app bar, visible from EVERY page.
 *
 * The banner below explains the situation, but a banner is easy to scroll past.
 * A spinner in the chrome answers "is something running right now?" at a
 * glance, and clicking it goes to where the live output is.
 */
function RunIndicator() {
  const { bench } = useRunState()
  if (!bench?.running) return null
  const started = Number(bench?.startedAt ?? 0)
  const secs = started > 0 ? Math.max(0, Math.floor(Date.now() / 1000 - started)) : 0
  const mins = Math.floor(secs / 60)
  const elapsed = started > 0 ? (mins > 0 ? `${mins}m ${secs % 60}s` : `${secs}s`) : ''
  const lastLine = (bench?.log ?? []).at(-1) ?? ''
  return (
    <Tooltip title={lastLine || 'A bench run is in progress'}>
      <Chip
        component={Link}
        to="/bench"
        clickable
        color="warning"
        icon={<CircularProgress size={14} color="inherit" />}
        label={elapsed ? `Bench running · ${elapsed}` : 'Bench running'}
        sx={{ ml: 'auto' }}
      />
    </Tooltip>
  )
}

function CalibrationBanner() {
  const { bench } = useRunState()
  if (!bench?.running) return null
  return (
    <Alert severity="info" square sx={{ borderRadius: 0 }}>
      <AlertTitle>Benchmark running</AlertTitle>
      A bench run is competing for slots like any other caller — nothing is being
      evicted and no one is being turned away, but expect added latency and some{' '}
      <b>429 + Retry-After</b> backpressure in Activity until it finishes.{' '}
      <Link to="/bench">Watch it →</Link>
    </Alert>
  )
}

const NAV = [
  { to: '/', label: 'Overview' },
  { to: '/config', label: 'Config' },
  { to: '/activity', label: 'Activity' },
  { to: '/usage', label: 'Usage' },
  { to: '/groups', label: 'Groups' },
  { to: '/keys', label: 'Keys' },
  { to: '/bench', label: 'Bench' },
  { to: '/quota', label: 'Quota' },
  { to: '/providers', label: 'Providers' },
  { to: '/approvals', label: 'Approvals' },
] as const

function RootLayout() {
  useLiveEvents() // push-based refresh for the live views (no-op until signed in)
  const gate = useAuthGate()
  // Render nothing for the one frame the probe takes. A spinner would flash on
  // every load, and a login screen shown then hidden is worse than a blank.
  if (gate === 'checking') return null
  if (gate === 'needs-token') return <Login />
  return (
    <>
      <AppBar position="sticky">
        <Toolbar variant="dense" sx={{ gap: 3, minHeight: 52 }}>
          <Typography variant="h6" sx={{ letterSpacing: '-0.02em' }}>
            corrallm
          </Typography>
          {/* The active tab is marked by an underline bar + full-strength ink;
              inactive tabs sit in muted ink. Bold-vs-normal alone (the old cue)
              is nearly invisible at 14px. */}
          <Box sx={{ display: 'flex', gap: 0.5 }}>
            {NAV.map((n) => (
              <Link
                key={n.to}
                to={n.to}
                activeOptions={{ exact: n.to === '/' }}
                style={{
                  color: C.textMuted,
                  textDecoration: 'none',
                  fontSize: 13.5,
                  fontWeight: 600,
                  padding: '6px 10px',
                  borderRadius: 6,
                  borderBottom: '2px solid transparent',
                }}
                activeProps={{
                  style: { color: C.text, borderBottom: `2px solid ${C.accent}`, borderRadius: 0 },
                }}
              >
                {n.label}
              </Link>
            ))}
          </Box>
          <RunIndicator />
        </Toolbar>
      </AppBar>
      <CalibrationBanner />
      <Outlet />
    </>
  )
}

export const Route = createRootRoute({ component: RootLayout })
