import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Box, Button, Chip, Stack } from '@mui/material'
import { PageHeader } from '@/Panel'
import { ActivityLog } from '@/ActivityLog'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'

/**
 * /m/<name>/activity — this model's requests, in the same table as everywhere
 * else.
 *
 * Literally the same component the global activity page uses, scoped by model
 * and with the model column dropped: the page title already says which model it
 * is, so repeating it on every row is noise. Reusing the table rather than
 * writing a narrower one is deliberate — two tables that "look the same" drift,
 * one gaining a column or a formatter the other never gets, and then the
 * per-model view quietly becomes the worse one.
 *
 * ?placement=<name> narrows further. That is the question the placement work
 * was for: not "how does this model behave" but "how does it behave ON THAT
 * BOX". A model served from two machines has two latency distributions, and a
 * figure averaged across both describes neither.
 */
const PlacementsDoc = graphql(/* GraphQL */ `
  query ModelPlacements {
    corrallm {
      overview {
        models {
          name
          placements {
            name
            server
          }
        }
      }
    }
  }
`)

export const Route = createFileRoute('/m/$name/activity')({
  validateSearch: (s: Record<string, unknown>): { placement?: string } => ({
    placement: s.placement ? String(s.placement) : undefined,
  }),
  component: ModelActivity,
})

function ModelActivity() {
  const { name } = Route.useParams()
  const { placement } = Route.useSearch()
  const navigate = useNavigate()

  const q = useQuery({
    queryKey: ['modelPlacements'],
    queryFn: () => gqlClient.request(PlacementsDoc),
    staleTime: 30000,
  })
  const places =
    (q.data?.corrallm?.overview?.models ?? []).find((m) => m?.name === name)?.placements ?? []

  const go = (p?: string) =>
    navigate({ to: '/m/$name/activity', params: { name }, search: p ? { placement: p } : {} })

  return (
    <Box sx={{ p: 3 }}>
      <PageHeader title={`${name} — activity`} />
      <Stack direction="row" spacing={1} sx={{ mb: 2, flexWrap: 'wrap', alignItems: 'center' }}>
        <Button size="small" onClick={() => navigate({ to: '/m/$name', params: { name } })}>
          ← back to model
        </Button>
        {/* Only offered when there IS a choice. One placement means the filter
            can only ever say what the page already says. */}
        {places.length > 1 && (
          <>
            <Chip
              size="small"
              label="all placements"
              color={placement ? 'default' : 'primary'}
              variant={placement ? 'outlined' : 'filled'}
              onClick={() => go(undefined)}
            />
            {places.map((p) => (
              <Chip
                key={p?.name}
                size="small"
                label={p?.name}
                color={placement === p?.name ? 'primary' : 'default'}
                variant={placement === p?.name ? 'filled' : 'outlined'}
                onClick={() => go(p?.name ?? undefined)}
              />
            ))}
          </>
        )}
      </Stack>
      <ActivityLog
        filterModel={name}
        filterPlacement={placement}
        hideModel
        limit={200}
        title={placement ? `Requests on ${placement}` : 'Requests'}
        subtitle="Click a row for the captured request and response"
      />
    </Box>
  )
}
