import { useEffect, useState } from 'react'

// Admin-token handling for the dashboard. The token is stored in localStorage
// (sent as a Bearer header on GraphQL requests) AND mirrored into a cookie so
// the SSE EventSource — which can't set headers — is authorized too.

const KEY = 'corrallm_token'
const COOKIE = 'corrallm_token'

export function getToken(): string {
  return localStorage.getItem(KEY) ?? ''
}

// Max-Age matters: without it this is a SESSION cookie, which dies when the
// browser closes while localStorage survives. The dashboard then loads signed
// in — Bearer works for every query — but /api/v1/events is cookie-only, so the
// live stream 401s forever with nothing on screen to explain it. The two stores
// have to expire together or not at all.
//
// The server also re-issues this cookie on any Bearer-authenticated /api call,
// so a browser that lost it heals on the next request rather than needing a
// fresh sign-in.
const COOKIE_MAX_AGE = 365 * 24 * 60 * 60

export function setToken(t: string) {
  localStorage.setItem(KEY, t)
  const secure = window.location.protocol === 'https:' ? '; Secure' : ''
  document.cookie = `${COOKIE}=${t}; path=/; Max-Age=${COOKIE_MAX_AGE}; SameSite=Strict${secure}`
}

export function clearToken() {
  localStorage.removeItem(KEY)
  document.cookie = `${COOKIE}=; path=/; Max-Age=0; SameSite=Strict`
}

// is401 detects an unauthorized response from graphql-request (ClientError
// carries response.status) so the app can drop the bad token and re-prompt.
export function is401(err: unknown): boolean {
  const e = err as { response?: { status?: number }; status?: number }
  return e?.response?.status === 401 || e?.status === 401
}

// Whether this instance requires a token at all.
//
// A server started with --insecure has no gate, so prompting for a token would
// be asking for a credential nothing checks — the login screen would be a wall
// in front of an unlocked door. /health says which it is, and is reachable
// unauthenticated precisely because the client may have no token yet.
export type Gate = 'checking' | 'open' | 'needs-token'

export function useAuthGate(): Gate {
  // A token in hand short-circuits: it works in both modes, so there is nothing
  // to ask the server and no reason to make the app wait on a round trip.
  const [gate, setGate] = useState<Gate>(() => (getToken() ? 'open' : 'checking'))

  useEffect(() => {
    if (gate !== 'checking') return
    let alive = true
    fetch('/health')
      .then((r) => r.json())
      .then((d) => alive && setGate(d?.insecure === true ? 'open' : 'needs-token'))
      // Unreachable or unparseable: fall back to asking for a token. Assuming
      // "open" on a failed probe would blank the login screen on a server that
      // does want one, leaving no way in.
      .catch(() => alive && setGate('needs-token'))
    return () => {
      alive = false
    }
  }, [gate])

  return gate
}
