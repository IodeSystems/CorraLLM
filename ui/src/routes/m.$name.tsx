import { createFileRoute } from '@tanstack/react-router'
import { ModelConsole } from '@/routes/model'

/**
 * /m/<name> — a model's page, addressed by PATH rather than by query string.
 *
 * `/model?name=x` treated the model as a parameter to a generic page. It is the
 * subject of the page, and the URL should say so: it makes the address
 * guessable, gives sub-pages somewhere to live (/m/<name>/activity), and reads
 * as a resource rather than a search.
 *
 * The old route still resolves — links live in bookmarks, notes and chat
 * history, and breaking them buys nothing.
 */
export const Route = createFileRoute('/m/$name')({
  validateSearch: (s: Record<string, unknown>): { tab?: string; replay?: string } => ({
    tab: s.tab ? String(s.tab) : undefined,
    replay: s.replay ? String(s.replay) : undefined,
  }),
  component: ModelByPath,
})

function ModelByPath() {
  const { name } = Route.useParams()
  const { tab, replay } = Route.useSearch()
  return <ModelConsole name={name} tab={tab} replay={replay} />
}
