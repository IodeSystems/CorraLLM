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
