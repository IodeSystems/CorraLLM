import { useQuery } from '@tanstack/react-query'
import {
  Box,
  Chip,
  CircularProgress,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { Panel } from '@/Panel'
import { C } from '@/theme'
import { fmtDuration, fmtInt } from '@/format'

/**
 * What each caller's work actually costs, and how predictable it is.
 *
 * The scheduler carries ONE dwell EWMA per backend, so every caller of a model
 * is predicted by the same scalar. This is what that scalar averages over — and
 * on a single model, callers routinely differ several-fold in mean and several
 * more in variability. That spread is exactly the information a position×mean
 * estimate discards, which is why the Retry-After we hand out runs short.
 *
 * CV (stddev/mean) is the shape number. Near 0, every request costs the same and
 * a mean is a fair predictor. Above 1, the mean is tail-dominated: queueing
 * behind one of these costs far more than the average suggests. The ×wait column
 * is the (1+CV²)/2 term from Pollaczek–Khinchine — the multiple by which this
 * caller's variability alone inflates the wait of whoever is behind them.
 *
 * Read-only. Nothing here feeds admission or the backoff a caller receives.
 */

const ProfilesDoc = graphql(/* GraphQL */ `
  query ServiceProfiles($minutes: Long!) {
    corrallm {
      serviceProfiles(minutes: $minutes) {
        minutes
        rows {
          served
          key
          n
          meanMs
          stdMs
          maxMs
          cv
          shareMs
          shrunkMeanMs
          shrunkCv
          modelMeanMs
          modelCv
          varianceFactor
        }
      }
    }
  }
`)

// Above 1 the mean stops being a usable predictor; call that out.
function cvColor(cv: number): string | undefined {
  if (cv >= 2) return C.warn
  if (cv >= 1) return C.text
  return undefined
}

export function ServiceProfiles({ minutes = 1440 }: { minutes?: number }) {
  const q = useQuery({
    queryKey: ['activity', 'serviceProfiles', minutes],
    queryFn: () => gqlClient.request(ProfilesDoc, { minutes: String(minutes) }),
    refetchInterval: 60000, // a distribution does not move in seconds
  })

  const rows = q.data?.corrallm.serviceProfiles?.rows ?? []

  const body = q.isLoading ? (
    <Box sx={{ p: 2 }}>
      <CircularProgress />
    </Box>
  ) : q.error ? (
    <Box sx={{ p: 2 }}>
      <Typography color="error">{String(q.error)}</Typography>
    </Box>
  ) : rows.length === 0 ? (
    <Box sx={{ p: 2 }}>
      <Typography color="text.secondary">Nothing served in the last {minutes} minutes.</Typography>
    </Box>
  ) : (
    <TableContainer>
      <Table size="small" stickyHeader>
        <TableHead>
          <TableRow>
            <TableCell>Model</TableCell>
            <TableCell>Caller</TableCell>
            <TableCell align="right">n</TableCell>
            <TableCell align="right">
              <Tooltip title="Mean time this caller's requests occupy a slot. Queue time and cold-load are excluded — those are consequences of contention, not properties of the work.">
                <span>Mean</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Coefficient of variation (stddev/mean). Near 0, every request costs the same and a mean predicts well. Above 1, the mean is tail-dominated.">
                <span>CV</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">Max</TableCell>
            <TableCell align="right">
              <Tooltip title="(1+CV²)/2 — the multiple by which this caller's variability alone inflates the wait of whoever queues behind them, versus a mean-only estimate.">
                <span>×wait</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Blended toward the model's overall profile by sample count (pseudo-count 30), so a handful of requests cannot let one outlier set policy. Converges to the caller's own numbers as evidence accumulates.">
                <span>Blended</span>
              </Tooltip>
            </TableCell>
            <TableCell align="right">
              <Tooltip title="Total slot time this caller consumed — who the model has actually been working for.">
                <span>Slot time</span>
              </Tooltip>
            </TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((r, i) => {
            const n = Number(r.n)
            const cv = Number(r.cv)
            const shrunkCv = Number(r.shrunkCv)
            // Under the pseudo-count the caller's own numbers are the minority
            // of the blend; say so rather than presenting them as established.
            const thin = n < 30
            return (
              <TableRow key={`${r.served}/${r.key}/${i}`} hover>
                <TableCell>{r.served}</TableCell>
                <TableCell>{r.key || <span style={{ color: C.textFaint }}>unkeyed</span>}</TableCell>
                <TableCell align="right">
                  <span style={thin ? { color: C.textFaint } : undefined}>{fmtInt(r.n)}</span>
                </TableCell>
                <TableCell align="right">{fmtDuration(r.meanMs)}</TableCell>
                <TableCell align="right">
                  <span style={{ color: cvColor(cv) }}>{cv.toFixed(2)}</span>
                </TableCell>
                <TableCell align="right">{fmtDuration(r.maxMs)}</TableCell>
                <TableCell align="right">
                  <span style={{ color: Number(r.varianceFactor) >= 2 ? C.warn : undefined }}>
                    {Number(r.varianceFactor).toFixed(2)}×
                  </span>
                </TableCell>
                <TableCell align="right">
                  <Tooltip
                    title={`model prior: ${fmtDuration(r.modelMeanMs)} / CV ${Number(r.modelCv).toFixed(2)}`}
                  >
                    <span style={thin ? { color: C.textFaint } : undefined}>
                      {fmtDuration(r.shrunkMeanMs)} / {shrunkCv.toFixed(2)}
                    </span>
                  </Tooltip>
                </TableCell>
                <TableCell align="right">{fmtDuration(r.shareMs)}</TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </TableContainer>
  )

  return (
    <Panel
      title="Caller service profiles"
      subtitle={`How long each caller's work holds a slot, and how predictable it is — last ${Math.round(minutes / 60)}h`}
      badge={<Chip size="small" variant="outlined" label={`${rows.length} callers`} />}
      flush
    >
      {body}
    </Panel>
  )
}
