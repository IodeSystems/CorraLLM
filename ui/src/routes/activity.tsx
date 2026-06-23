import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  Box,
  Chip,
  CircularProgress,
  Paper,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { fmtDuration, fmtInt, fmtTime, fmtUSD } from '@/format'

const ActivityDoc = graphql(/* GraphQL */ `
  query Activity {
    corrallm {
      recentActivity(limit: "100") {
        records {
          ts
          served
          backend
          key
          path
          status
          dwellMs
          promptTokens
          completionTokens
          costUsd
        }
      }
    }
  }
`)

function statusColor(statusStr: string | number): 'success' | 'warning' | 'error' | 'default' {
  const status = typeof statusStr === 'string' ? Number(statusStr) : statusStr
  if (status >= 200 && status < 300) return 'success'
  if (status === 429 || status === 503) return 'warning'
  if (status >= 400) return 'error'
  return 'default'
}

function Activity() {
  const q = useQuery({
    queryKey: ['activity'],
    queryFn: () => gqlClient.request(ActivityDoc),
    refetchInterval: 2000,
  })

  if (q.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }
  if (q.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Typography color="error">{String(q.error)}</Typography>
      </Box>
    )
  }

  const records = q.data?.corrallm.recentActivity?.records ?? []

  return (
    <Box sx={{ p: 3 }}>
      <Typography variant="h6" gutterBottom>
        Recent Activity
      </Typography>
      <TableContainer component={Paper}>
        <Table size="small" stickyHeader>
          <TableHead>
            <TableRow>
              <TableCell>Time</TableCell>
              <TableCell>Served</TableCell>
              <TableCell>Backend</TableCell>
              <TableCell>Key</TableCell>
              <TableCell>Path</TableCell>
              <TableCell align="right">Status</TableCell>
              <TableCell align="right">Dwell</TableCell>
              <TableCell align="right">Prompt</TableCell>
              <TableCell align="right">Completion</TableCell>
              <TableCell align="right">Cost</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {records.length === 0 ? (
              <TableRow>
                <TableCell colSpan={10}>
                  <Typography color="text.secondary">No activity yet.</Typography>
                </TableCell>
              </TableRow>
            ) : (
              records.map((r, i) => (
                <TableRow key={i} hover>
                  <TableCell>{fmtTime(r.ts)}</TableCell>
                  <TableCell>{r.served}</TableCell>
                  <TableCell>{r.backend}</TableCell>
                  <TableCell>{r.key || '—'}</TableCell>
                  <TableCell>{r.path}</TableCell>
                  <TableCell align="right">
                    <Chip size="small" label={r.status} color={statusColor(r.status)} />
                  </TableCell>
                  <TableCell align="right">{fmtDuration(r.dwellMs)}</TableCell>
                  <TableCell align="right">{fmtInt(r.promptTokens)}</TableCell>
                  <TableCell align="right">{fmtInt(r.completionTokens)}</TableCell>
                  <TableCell align="right">{fmtUSD(r.costUsd)}</TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </TableContainer>
    </Box>
  )
}

export const Route = createFileRoute('/activity')({ component: Activity })
