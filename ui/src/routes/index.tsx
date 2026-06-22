import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  Box,
  Card,
  CardContent,
  CircularProgress,
  List,
  ListItem,
  ListItemText,
  ListSubheader,
  Typography,
} from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'

const HomeDoc = graphql(/* GraphQL */ `
  query Home {
    corrallm {
      health {
        status
        version
      }
      configSummary {
        servers
        models
        priorityGroups
      }
    }
  }
`)

function Home() {
  const home = useQuery({
    queryKey: ['home'],
    queryFn: () => gqlClient.request(HomeDoc),
  })

  if (home.isLoading) {
    return (
      <Box sx={{ p: 3 }}>
        <CircularProgress />
      </Box>
    )
  }

  if (home.error) {
    return (
      <Box sx={{ p: 3 }}>
        <Typography color="error">{String(home.error)}</Typography>
      </Box>
    )
  }

  const { health, configSummary } = home.data!.corrallm

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Card>
        <CardContent>
          <Typography variant="h6">Health</Typography>
          <Typography>status: {health?.status ?? '—'}</Typography>
          <Typography>version: {health?.version ?? '—'}</Typography>
        </CardContent>
      </Card>

      <Card>
        <CardContent>
          <Typography variant="h6" gutterBottom>
            Config Summary
          </Typography>
          <Section title="Servers" items={configSummary?.servers ?? []} />
          <Section title="Models" items={configSummary?.models ?? []} />
          <Section title="Priority Groups" items={configSummary?.priorityGroups ?? []} />
        </CardContent>
      </Card>
    </Box>
  )
}

function Section({ title, items }: { title: string; items: readonly string[] }) {
  return (
    <List dense subheader={<ListSubheader disableGutters>{title}</ListSubheader>}>
      {items.length === 0 ? (
        <ListItem>
          <ListItemText secondary="(none)" />
        </ListItem>
      ) : (
        items.map((item) => (
          <ListItem key={item}>
            <ListItemText primary={item} />
          </ListItem>
        ))
      )}
    </List>
  )
}

export const Route = createFileRoute('/')({ component: Home })
