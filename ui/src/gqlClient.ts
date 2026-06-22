import { GraphQLClient } from 'graphql-request'

// Same-origin via Vite's dev proxy. graphql-request v7 requires an absolute URL
// (it does `new URL(endpoint)`), so anchor the path to the page origin.
export const gqlClient = new GraphQLClient(`${window.location.origin}/api/graphql`)
