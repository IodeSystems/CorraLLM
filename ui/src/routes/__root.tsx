import { createRootRoute, Outlet, Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Alert, AlertTitle, AppBar, Box, Toolbar, Typography } from '@mui/material'
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
    }
  }
`)

function CalibrationBanner() {
  const { data } = useQuery({
    queryKey: ['calibrationBanner'],
    queryFn: () => gqlClient.request(CalibrationBannerDoc),
    // Cheap poll: this must appear promptly when a lease starts, and disappear
    // promptly when it expires — a stale "locked" banner would send someone
    // hunting a lockout that already ended.
    refetchInterval: 5000,
  })
  const st = data?.corrallm?.calibrationStatus
  if (!st?.active) return null
  // remainingSeconds arrives as a string (codegen maps int64 -> string for a
  // uniform id contract), so coerce before arithmetic.
  const mins = Math.max(1, Math.ceil(Number(st.remainingSeconds ?? 0) / 60))
  return (
    <Alert severity="warning" square sx={{ borderRadius: 0 }}>
      <AlertTitle>
        Calibration lease held — all other traffic is being turned away
      </AlertTitle>
      Every caller except the bench run is receiving <b>429 + Retry-After</b>, and models
      are being evicted for cold measurements. Expect zero throughput and a wall of 429s
      in Activity until this clears.{' '}
      {st.reason ? <>Reason: <b>{st.reason}</b>. </> : null}
      Self-expires in ~{mins} min.
    </Alert>
  )
}

const NAV = [
  { to: '/', label: 'Overview' },
  { to: '/activity', label: 'Activity' },
  { to: '/usage', label: 'Usage' },
  { to: '/groups', label: 'Groups' },
  { to: '/bench', label: 'Bench' },
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
        </Toolbar>
      </AppBar>
      <CalibrationBanner />
      <Outlet />
    </>
  )
}

export const Route = createRootRoute({ component: RootLayout })
