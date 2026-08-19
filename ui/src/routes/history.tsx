import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  Typography,
} from '@mui/material'
import { Panel, PageHeader, Row } from '@/Panel'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { extractMessage } from '@/format'

/**
 * Configuration history — what changed, when, and how to get it back.
 *
 * This is the undo. Configuration used to be a file, so "what did this look
 * like yesterday" was answerable with a backup copy; the file is gone now, and
 * a database is not readable in an editor. Leaving this in the CLI would have
 * traded a bad answer for none.
 *
 * Its own page rather than a panel on Hosts or Providers, because a revision
 * spans all of them: one entry can contain a server, a lane and three models.
 * Filing it under any single one of those would be filing it under a part.
 */
const HistoryDoc = graphql(/* GraphQL */ `
  query ConfigHistory {
    corrallm {
      configHistory(limit: 50) {
        revisions {
          id
          at
          note
          bytes
          current
        }
      }
    }
  }
`)

const RevisionDoc = graphql(/* GraphQL */ `
  query ConfigRevision($id: Long!) {
    corrallm {
      configRevision(id: $id) {
        yaml
      }
    }
  }
`)

const ExportDoc = graphql(/* GraphQL */ `
  query ConfigExport {
    corrallm {
      configExport {
        yaml
      }
    }
  }
`)

const RestoreDoc = graphql(/* GraphQL */ `
  mutation ConfigRestore($id: Long!) {
    corrallm {
      configRestore(body: { id: $id }) {
        ok
        message
      }
    }
  }
`)

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  return `${(n / 1024).toFixed(1)} KB`
}

export function HistoryPage() {
  const qc = useQueryClient()
  const [viewing, setViewing] = useState<string | null>(null)
  const [confirming, setConfirming] = useState<{ id: string; note: string } | null>(null)
  const [err, setErr] = useState('')
  const [notice, setNotice] = useState('')
  const [exported, setExported] = useState(false)

  const q = useQuery({
    queryKey: ['configHistory'],
    queryFn: () => gqlClient.request(HistoryDoc),
  })
  const revisions = q.data?.corrallm.configHistory?.revisions ?? []

  // The body is fetched per revision rather than with the list: fifty
  // revisions of a 68KB config is megabytes to render a list of dates.
  const body = useQuery({
    queryKey: ['configRevision', viewing],
    queryFn: () => gqlClient.request(RevisionDoc, { id: String(viewing) }),
    enabled: !!viewing,
  })

  const doExport = useMutation({
    mutationFn: () => gqlClient.request(ExportDoc),
    onSuccess: (d) => {
      const yaml = d.corrallm.configExport?.yaml ?? ''
      // Downloaded rather than shown: this is a backup, and a backup you have to
      // select out of a <pre> is not one.
      const url = URL.createObjectURL(new Blob([yaml], { type: 'text/yaml' }))
      const a = document.createElement('a')
      a.href = url
      a.download = `corrallm-config-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '')}.yaml`
      a.click()
      URL.revokeObjectURL(url)
      setExported(true)
      setErr('')
    },
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  const restore = useMutation({
    mutationFn: (id: string) => gqlClient.request(RestoreDoc, { id }),
    onSuccess: (d) => {
      setConfirming(null)
      setErr('')
      setNotice(d.corrallm.configRestore?.message ?? 'restored')
      // The restore APPENDS a revision, so the list is stale immediately. So is
      // everything else on the dashboard: the running config just changed.
      void qc.invalidateQueries()
    },
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  return (
    <Box sx={{ p: 3, display: 'flex', flexDirection: 'column', gap: 3 }}>
      <PageHeader title="Config history">
        <Chip size="small" variant="outlined" label={`${revisions.length} revisions`} />
        <Box sx={{ flexGrow: 1 }} />
        <Button size="small" variant="outlined" disabled={doExport.isPending} onClick={() => doExport.mutate()}>
          Export current as YAML
        </Button>
      </PageHeader>

      {err && <Alert severity="error">{err}</Alert>}
      {notice && (
        <Alert severity="success" onClose={() => setNotice('')}>
          {notice}
        </Alert>
      )}
      {exported && !err && (
        <Alert severity="info" onClose={() => setExported(false)}>
          Downloaded. Configuration lives in SQLite, which is not readable in an editor or diffable
          in git — an export is how a backup, a review, or a move to another machine happens.
        </Alert>
      )}

      <Panel
        title="Revisions"
        subtitle="Every save records what the configuration became. Newest first."
        badge={q.isFetching ? <CircularProgress size={14} /> : undefined}
        flush
      >
        {q.error && (
          <Box sx={{ p: 2 }}>
            <Alert severity="error">{String(q.error)}</Alert>
          </Box>
        )}
        {!q.isLoading && revisions.length === 0 && (
          <Typography variant="body2" sx={{ px: 2, py: 1.5, color: C.textMuted }}>
            No revisions yet. One is recorded every time the configuration is saved.
          </Typography>
        )}
        {revisions.map((r) => (
          <Row key={String(r.id)}>
            <Stack direction="row" spacing={1.5} alignItems="baseline" flexWrap="wrap" useFlexGap>
              <Typography variant="subtitle2" sx={{ minWidth: 48, fontFamily: 'monospace' }}>
                {String(r.id)}
              </Typography>
              {r.current && (
                <Chip size="small" color="success" variant="outlined" label="running" />
              )}
              <Typography variant="caption" sx={{ color: C.textFaint, minWidth: 150 }}>
                {new Date(r.at).toLocaleString()}
              </Typography>
              <Typography variant="body2" sx={{ flex: 1, minWidth: 200 }}>
                {r.note || <span style={{ color: C.textFaint }}>(no note)</span>}
              </Typography>
              <Typography variant="caption" sx={{ color: C.textFaint }}>
                {fmtBytes(Number(r.bytes))}
              </Typography>
              <Button size="small" onClick={() => setViewing(String(r.id))}>
                View
              </Button>
              {/* No Restore on the running revision: it would be a no-op that
                  still appends a revision, which is noise in the one place
                  meant to explain what happened. */}
              {!r.current && (
                <Button
                  size="small"
                  onClick={() => setConfirming({ id: String(r.id), note: r.note ?? '' })}
                >
                  Restore
                </Button>
              )}
            </Stack>
          </Row>
        ))}
      </Panel>

      <Dialog open={!!viewing} onClose={() => setViewing(null)} maxWidth="md" fullWidth>
        <DialogTitle>Revision {viewing}</DialogTitle>
        <DialogContent dividers>
          <Box
            component="pre"
            sx={{
              m: 0,
              p: 1,
              maxHeight: 460,
              overflow: 'auto',
              bgcolor: C.canvas,
              border: `1px solid ${C.border}`,
              borderRadius: 1,
              fontSize: 11.5,
              lineHeight: 1.45,
            }}
          >
            {body.isFetching ? 'loading…' : (body.data?.corrallm.configRevision?.yaml ?? '')}
          </Box>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setViewing(null)}>Close</Button>
        </DialogActions>
      </Dialog>

      <Dialog open={!!confirming} onClose={() => setConfirming(null)} maxWidth="sm" fullWidth>
        <DialogTitle>Restore revision {confirming?.id}?</DialogTitle>
        <DialogContent dividers>
          <Typography variant="body2" sx={{ mb: 1.5 }}>
            {confirming?.note}
          </Typography>
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            This replaces the whole configuration and reloads the daemon. It is validated first, so
            a revision that is no longer valid — one naming a host that has since been removed — is
            refused rather than applied.
          </Typography>
          <Typography variant="body2" sx={{ color: C.textMuted, mt: 1.5 }}>
            Nothing is lost: the restore is recorded as a NEW revision, so the configuration running
            right now stays in this list and can be restored back.
          </Typography>
          <Typography variant="body2" sx={{ color: C.textMuted, mt: 1.5 }}>
            Resident models are not evicted — a reload changes what the NEXT spawn uses.
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirming(null)}>Cancel</Button>
          <Button
            variant="contained"
            disabled={restore.isPending}
            onClick={() => confirming && restore.mutate(confirming.id)}
          >
            Restore
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  )
}

export const Route = createFileRoute('/history')({ component: HistoryPage })
