import { createRootRoute, Outlet, Link } from '@tanstack/react-router'
import { AppBar, Box, Toolbar, Typography } from '@mui/material'
import { useLiveEvents } from '@/useLiveEvents'

const NAV = [
  { to: '/', label: 'Overview' },
  { to: '/activity', label: 'Activity' },
  { to: '/usage', label: 'Usage' },
  { to: '/lanes', label: 'Lanes' },
] as const

function RootLayout() {
  useLiveEvents() // push-based refresh for the live views
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
      <Outlet />
    </>
  )
}

export const Route = createRootRoute({ component: RootLayout })
