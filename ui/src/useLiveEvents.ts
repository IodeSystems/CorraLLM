import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'

// Subscribes to the server's SSE event stream and invalidates the live query
// caches on each event, so the observability views update on push. EventSource
// auto-reconnects, so a dropped connection self-heals; the views also keep a
// slow fallback poll in case an event is missed. Mount once (at the root).
export function useLiveEvents() {
  const qc = useQueryClient()
  useEffect(() => {
    const es = new EventSource(`${window.location.origin}/api/v1/events`)
    const invalidate = () => {
      void qc.invalidateQueries({ queryKey: ['activity'] })
      void qc.invalidateQueries({ queryKey: ['usage'] })
      void qc.invalidateQueries({ queryKey: ['lanes'] })
      void qc.invalidateQueries({ queryKey: ['overview'] })
    }
    es.addEventListener('activity', invalidate)
    es.addEventListener('changed', invalidate)
    return () => es.close()
  }, [qc])
}
