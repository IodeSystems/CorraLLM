import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { CssBaseline, ThemeProvider, createTheme } from '@mui/material'
import { QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createRouter } from '@tanstack/react-router'
import { routeTree } from './routeTree.gen'
import { clearToken, is401 } from './auth'

const theme = createTheme()
// On a 401 the admin token is stale/missing — drop it and reload to the login
// screen (no token → gate shows login → no queries → no loop).
const queryClient = new QueryClient({
  queryCache: new QueryCache({
    onError: (err) => {
      if (is401(err)) {
        clearToken()
        window.location.reload()
      }
    },
  }),
})
const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ThemeProvider theme={theme}>
      <CssBaseline />
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </ThemeProvider>
  </StrictMode>,
)
