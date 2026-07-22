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
import { useLiveEvents } from '@/useLiveEvents'
import { getToken } from '@/auth'
import { Login } from '@/Login'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'

// Polled globally, not per-page. A held lease turns EVERY request into a 429,
// so someone staring at Activity or Usage during a throughput collapse needs to
// see the cause without first knowing that a Bench page exists. Surfacing it
// only where it is started would explain the outage exclusively to the person
// who already knew.
const CalibrationBannerDoc = graphql(/* GraphQL */ `
  query CalibrationBanner {
    corrallm {
      calibrationStatus {
        active
        reason
        remainingSeconds
      }
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
    // promptly when it ends — a stale "locked" banner would send someone
    // hunting a lockout that already ended.
    refetchInterval: 5000,
  })
  return {
    lease: data?.corrallm?.calibrationStatus,
    bench: data?.corrallm?.benchStatus,
  }
}

/**
 * A persistent indicator in the app bar, visible from EVERY page.
 *
 * The banner below explains the situation, but a banner is easy to scroll past
 * and its wording leads with "calibration lease", which does not read as "a
 * benchmark is running" to someone who did not start it. A spinner in the
 * chrome answers "is something running right now?" at a glance, and clicking it
 * goes to where the live output is.
 */
function RunIndicator() {
  const { lease, bench } = useRunState()
  const running = !!bench?.running || !!lease?.active
  if (!running) return null
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
  const { lease: st, bench } = useRunState()
  if (!st?.active && !bench?.running) return null
  // remainingSeconds arrives as a string (codegen maps int64 -> string for a
  // uniform id contract), so coerce before arithmetic.
  const mins = Math.max(1, Math.ceil(Number(st?.remainingSeconds ?? 0) / 60))
  return (
    <Alert severity="warning" square sx={{ borderRadius: 0 }}>
      <AlertTitle>
        Benchmark running — all other traffic is being turned away
      </AlertTitle>
      Every caller except the bench run is receiving <b>429 + Retry-After</b>, and models
      are being evicted for cold measurements. Expect zero throughput and a wall of 429s
      in Activity until this clears.{' '}
      {st?.reason ? <>Reason: <b>{st.reason}</b>. </> : null}
      Self-expires in ~{mins} min. <Link to="/bench">Watch it →</Link>
    </Alert>
  )
}

const NAV = [
  { to: '/', label: 'Overview' },
  { to: '/activity', label: 'Activity' },
  { to: '/usage', label: 'Usage' },
  { to: '/groups', label: 'Groups' },
  { to: '/bench', label: 'Bench' },
  { to: '/quota', label: 'Quota' },
] as const

function RootLayout() {
  useLiveEvents() // push-based refresh for the live views (no-op until signed in)
  if (!getToken()) return <Login />
  return (
    <>
      <AppBar position="static">
        <Toolbar sx={{ gap: 3 }}>
          <Typography variant="h6">corrallm</Typography>
          <Box sx={{ display: 'flex', gap: 2 }}>
            {NAV.map((n) => (
              <Link
                key={n.to}
                to={n.to}
                activeOptions={{ exact: n.to === '/' }}
                style={{ color: 'inherit', textDecoration: 'none' }}
                activeProps={{ style: { textDecoration: 'underline', fontWeight: 700 } }}
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
