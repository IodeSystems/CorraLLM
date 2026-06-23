import { GraphQLClient } from 'graphql-request'
import { getToken } from './auth'

// Same-origin via Vite's dev proxy. graphql-request v7 requires an absolute URL
// (it does `new URL(endpoint)`), so anchor the path to the page origin. The
// admin token is attached per-request from storage (header function).
export const gqlClient = new GraphQLClient(`${window.location.origin}/api/graphql`, {
  headers: (): Record<string, string> => {
    const t = getToken()
    return t ? { authorization: `Bearer ${t}` } : {}
  },
})
