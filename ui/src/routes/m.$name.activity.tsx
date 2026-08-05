import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Box, Button, Stack, Typography } from '@mui/material'
import { PageHeader } from '@/Panel'
import { ActivityLog } from '@/ActivityLog'

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
 */
export const Route = createFileRoute('/m/$name/activity')({
  component: ModelActivity,
})

function ModelActivity() {
  const { name } = Route.useParams()
  const navigate = useNavigate()
  return (
    <Box sx={{ p: 3 }}>
      <PageHeader title={`${name} — activity`} />
      <Stack direction="row" spacing={1} sx={{ mb: 2 }}>
        <Button size="small" onClick={() => navigate({ to: '/m/$name', params: { name } })}>
          ← back to model
        </Button>
      </Stack>
      <ActivityLog
        filterModel={name}
        hideModel
        limit={200}
        title="Requests"
        subtitle="Click a row for the captured request and response"
      />
      <Typography variant="caption" sx={{ display: 'block', mt: 2, opacity: 0.7 }}>
        Which PLACEMENT served each request is not recorded yet — with a model
        able to run on more than one box, "backend" no longer identifies where the
        work happened.
      </Typography>
    </Box>
  )
}
