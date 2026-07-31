import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { CssBaseline, ThemeProvider } from '@mui/material'
import { QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createRouter } from '@tanstack/react-router'
import { routeTree } from './routeTree.gen'
import { clearToken, is401 } from './auth'
import { theme } from './theme'

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
  defaultOptions: {
    queries: {
      // Never retry a 401. react-query retries three times with backoff by
      // default, and onError — the hook that drops the token and returns to
      // the login screen — only fires once retries are exhausted. So a bad or
      // expired token did not show the login screen; it showed a spinner for
      // several seconds first, on every query at once, and only then bounced.
      // That is the "loads, takes my token, then hangs" report.
      //
      // A 401 is a verdict, not a hiccup: retrying cannot change the answer.
      // Everything else keeps the default retry, which is there for the
      // genuinely transient failures worth a second attempt.
      retry: (failureCount, err) => !is401(err) && failureCount < 3,
    },
  },
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
