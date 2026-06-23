import { createRootRoute, Outlet, Link } from '@tanstack/react-router'
import { AppBar, Box, Toolbar, Typography } from '@mui/material'

const NAV = [
  { to: '/', label: 'Overview' },
  { to: '/activity', label: 'Activity' },
  { to: '/usage', label: 'Usage' },
] as const

export const Route = createRootRoute({
  component: () => (
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
  ),
})
