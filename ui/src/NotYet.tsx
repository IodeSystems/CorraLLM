import { Tooltip } from '@mui/material'
import { C } from '@/theme'

/**
 * A NUMBER NOBODY HAS REPORTED YET, said in words instead of drawn as a zero.
 *
 * Three things in this product already exist to stop one mistake: printing a
 * figure nobody measured. `cacheReports`' "ZERO MEANS UNKNOWN, NOT 0%"
 * (internal/api/handlers.go), the dashed empty track `available: false` gets in
 * UtilizationBar, and ModelStats' `n/r` where a backend reported no rate. A tile
 * that falls back to `?? 0` while its query is still in flight makes the same
 * mistake, and makes it at the one moment a dashboard is certain to be read —
 * first paint, on a box somebody thinks is misbehaving. A Lenny run took
 * "Requests 0 · Processed 0 · Generated 0 · Cost $0" off a box running flat out:
 * "I'd have told my team nothing is happening, and I'd have been wrong."
 *
 * NOT the same token as `n/r`, deliberately. `n/r` is settled: the backend
 * answered and has no such figure, and a remote provider never will. This is
 * "we have not been told yet", which may be a number a second from now. Drawing
 * two different reasons with the same glyph is what OB-9 forbids — the same call
 * ModelStats makes between its em-dash and "not recorded".
 */
export const NOT_YET_HINT =
  'Nothing has been counted here yet — this box has not returned a figure. Not a measured zero.'

export function NotYet() {
  return (
    <Tooltip title={NOT_YET_HINT}>
      <span style={{ color: C.textFaint }}>not in yet</span>
    </Tooltip>
  )
}
