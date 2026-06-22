import { createRootRoute, Outlet } from '@tanstack/react-router'
import { AppBar, Toolbar, Typography } from '@mui/material'

export const Route = createRootRoute({
  component: () => (
    <>
      <AppBar position="static">
        <Toolbar>
          <Typography variant="h6">corrallm</Typography>
        </Toolbar>
      </AppBar>
      <Outlet />
    </>
  ),
})
