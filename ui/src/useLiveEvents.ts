import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { getToken } from './auth'

// Coalesce bursts: under heavy load the server emits an event per request, so
// invalidating on every one is a refetch stampede (esp. the heavy `overview`
// query). Collapse all events within COALESCE_MS into a single trailing
// invalidation — at most one refetch pass per window, while staying near-live.
const COALESCE_MS = 300

// Subscribes to the server's SSE event stream and invalidates the live query
// caches (throttled) so the observability views update on push. EventSource
// auto-reconnects, so a dropped connection self-heals; the views also keep a
// slow fallback poll in case an event is missed. Mount once (at the root).
export function useLiveEvents() {
  const qc = useQueryClient()
  useEffect(() => {
    if (!getToken()) return // not signed in — the cookie wouldn't authorize the stream
    const es = new EventSource(`${window.location.origin}/api/v1/events`)
    let timer: ReturnType<typeof setTimeout> | null = null

    const flush = () => {
      timer = null
      for (const key of ['activity', 'usage', 'groups', 'overview']) {
        void qc.invalidateQueries({ queryKey: [key] })
      }
    }
    // Trailing throttle: the first event schedules a flush; further events in the
    // window fold into it (timer already armed). Under sustained traffic this
    // refetches at most once per COALESCE_MS instead of once per request.
    const schedule = () => {
      if (timer == null) timer = setTimeout(flush, COALESCE_MS)
    }

    es.addEventListener('activity', schedule)
    es.addEventListener('changed', schedule)
    return () => {
      es.close()
      if (timer != null) clearTimeout(timer)
    }
  }, [qc])
}
