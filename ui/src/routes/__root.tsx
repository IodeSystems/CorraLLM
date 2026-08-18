import { createRootRoute, Outlet, Link, useMatchRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  AlertTitle,
  AppBar,
  Box,
  Chip,
  CircularProgress,
  Drawer,
  IconButton,
  List,
  ListItemButton,
  ListItemIcon,
  ListItemText,
  Toolbar,
  Tooltip,
  Typography,
  useMediaQuery,
} from '@mui/material'
import type { SvgIconComponent } from '@mui/icons-material'
import BarChartOutlined from '@mui/icons-material/BarChartOutlined'
import CloudOutlined from '@mui/icons-material/CloudOutlined'
import DashboardOutlined from '@mui/icons-material/DashboardOutlined'
import DataUsageOutlined from '@mui/icons-material/DataUsageOutlined'
import GroupsOutlined from '@mui/icons-material/GroupsOutlined'
import MenuIcon from '@mui/icons-material/Menu'
import SettingsOutlined from '@mui/icons-material/SettingsOutlined'
import SpeedOutlined from '@mui/icons-material/SpeedOutlined'
import TimelineOutlined from '@mui/icons-material/TimelineOutlined'
import VpnKeyOutlined from '@mui/icons-material/VpnKeyOutlined'
import { C, theme } from '@/theme'
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

const NAV: { to: string; label: string; icon: SvgIconComponent }[] = [
  { to: '/', label: 'Overview', icon: DashboardOutlined },
  { to: '/hosts', label: 'Hosts', icon: SettingsOutlined },
  { to: '/activity', label: 'Activity', icon: TimelineOutlined },
  { to: '/usage', label: 'Usage', icon: BarChartOutlined },
  { to: '/groups', label: 'Groups', icon: GroupsOutlined },
  { to: '/keys', label: 'Keys', icon: VpnKeyOutlined },
  { to: '/bench', label: 'Bench', icon: SpeedOutlined },
  { to: '/quota', label: 'Quota', icon: DataUsageOutlined },
  // No Approvals entry: models are chosen on the provider that offers them
  // (Providers → Browse), so a separate page for deciding about them was a
  // second place to look for one thing.
  { to: '/providers', label: 'Providers', icon: CloudOutlined },
]

const BAR_H = 52
const RAIL_W = 56 // icon-only rail: one 24px glyph plus breathing room
const OPEN_W = 200

/**
 * The nav itself, shared by the desktop rail and the mobile overlay.
 *
 * `labels` is what differs: the rail hides them (tooltip instead), the overlay
 * and the expanded rail show them. Both render the SAME list so a page can't be
 * reachable on one form factor and missing on the other.
 */
function NavList({ labels, onNavigate }: { labels: boolean; onNavigate?: () => void }) {
  const matchRoute = useMatchRoute()
  return (
    <List sx={{ py: 1 }}>
      {NAV.map((n) => {
        // Sub-routes count as their parent: /keys/$key should light up Keys.
        // '/' would fuzzy-match everything, so it stays exact.
        const active = !!matchRoute({ to: n.to, fuzzy: n.to !== '/' })
        const Icon = n.icon
        const item = (
          <ListItemButton
            component={Link}
            to={n.to}
            onClick={onNavigate}
            sx={{
              minHeight: 42,
              px: 1.75,
              justifyContent: labels ? 'flex-start' : 'center',
              // The active cue is a left bar + full-strength ink. In an
              // icon-only rail there is no text weight to carry it, so the bar
              // does the work — visible whether or not the label is showing.
              borderLeft: `3px solid ${active ? C.accent : 'transparent'}`,
              bgcolor: active ? C.raised : 'transparent',
              color: active ? C.text : C.textMuted,
              '&:hover': { bgcolor: C.raised, color: C.text },
            }}
          >
            <ListItemIcon
              sx={{ color: 'inherit', minWidth: 0, mr: labels ? 1.75 : 0, justifyContent: 'center' }}
            >
              <Icon fontSize="small" />
            </ListItemIcon>
            {labels && (
              <ListItemText
                primary={n.label}
                slotProps={{ primary: { fontSize: 13.5, fontWeight: 600 } }}
              />
            )}
          </ListItemButton>
        )
        return labels ? (
          <Box key={n.to}>{item}</Box>
        ) : (
          <Tooltip key={n.to} title={n.label} placement="right">
            {item}
          </Tooltip>
        )
      })}
    </List>
  )
}

function RootLayout() {
  useLiveEvents() // push-based refresh for the live views (no-op until signed in)
  const gate = useAuthGate()
  const desktop = useMediaQuery(theme.breakpoints.up('md'))
  // Two independent bits of state, because the two drawers mean different
  // things: on mobile the drawer is an overlay that must start CLOSED, on
  // desktop the rail is always present and only its width toggles.
  const [mobileOpen, setMobileOpen] = useState(false)
  const [railOpen, setRailOpen] = useState(false)
  const railW = railOpen ? OPEN_W : RAIL_W

  // Render nothing for the one frame the probe takes. A spinner would flash on
  // every load, and a login screen shown then hidden is worse than a blank.
  if (gate === 'checking') return null
  if (gate === 'needs-token') return <Login />
  return (
    <>
      <AppBar position="sticky" sx={{ zIndex: (t) => t.zIndex.drawer + 1 }}>
        <Toolbar variant="dense" sx={{ gap: 1.5, minHeight: BAR_H }}>
          <IconButton
            size="small"
            edge="start"
            aria-label="Toggle navigation"
            onClick={() => (desktop ? setRailOpen((v) => !v) : setMobileOpen((v) => !v))}
          >
            <MenuIcon fontSize="small" />
          </IconButton>
          <Typography variant="h6" sx={{ letterSpacing: '-0.02em' }}>
            corrallm
          </Typography>
          <RunIndicator />
        </Toolbar>
      </AppBar>

      {/* Desktop: a permanent rail that sits under the app bar. Icons by
          default; the hamburger widens it to show labels. */}
      <Drawer
        variant="permanent"
        sx={{
          display: { xs: 'none', md: 'block' },
          '& .MuiDrawer-paper': {
            width: railW,
            top: BAR_H,
            height: `calc(100vh - ${BAR_H}px)`,
            bgcolor: C.surface,
            border: 0,
            borderRight: `1px solid ${C.border}`,
            overflowX: 'hidden',
            transition: 'width 160ms ease',
          },
        }}
      >
        <NavList labels={railOpen} />
      </Drawer>

      {/* Mobile: closed by default, pulled out over the content. Tapping an
          item navigates AND closes — leaving the scrim up after a tap reads as
          a stuck menu. */}
      <Drawer
        variant="temporary"
        open={mobileOpen}
        onClose={() => setMobileOpen(false)}
        ModalProps={{ keepMounted: true }}
        sx={{
          display: { xs: 'block', md: 'none' },
          '& .MuiDrawer-paper': {
            width: OPEN_W,
            bgcolor: C.surface,
            border: 0,
            borderRight: `1px solid ${C.border}`,
          },
        }}
      >
        <Toolbar variant="dense" sx={{ minHeight: BAR_H }}>
          <Typography variant="h6" sx={{ letterSpacing: '-0.02em' }}>
            corrallm
          </Typography>
        </Toolbar>
        <NavList labels onNavigate={() => setMobileOpen(false)} />
      </Drawer>

      <Box
        sx={{
          ml: { xs: 0, md: `${railW}px` },
          transition: 'margin-left 160ms ease',
          minWidth: 0,
        }}
      >
        <CalibrationBanner />
        <Outlet />
      </Box>
    </>
  )
}

export const Route = createRootRoute({ component: RootLayout })
