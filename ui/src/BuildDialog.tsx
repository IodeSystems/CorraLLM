import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
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
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { C } from '@/theme'
import { extractMessage } from '@/format'

/**
 * The build modal: one build at a time, with its log as it happens.
 *
 * A CUDA build is eight to twenty minutes, so it cannot be a request the
 * browser holds open — a reload would lose it and a proxy would time it out.
 * The daemon runs it as a background job; this polls.
 *
 * There is exactly ONE slot, so this dialog never has to ask which build it is
 * showing: the running one, or the last one that finished. That also means
 * opening it after the fact still answers "did that work?", which is when the
 * question is usually asked.
 */
const BuildStatusDoc = graphql(/* GraphQL */ `
  query BuildStatus($logFrom: Long) {
    corrallm {
      toolBuildStatus(logFrom: $logFrom) {
        current {
          id
          tool
          host
          status
          startedAt
          elapsedSeconds
        }
        last {
          id
          tool
          host
          status
          startedAt
          finishedAt
          elapsedSeconds
          skipped
          version
          stamp
          error
        }
        log
        logTotal
      }
    }
  }
`)

const BuildStartDoc = graphql(/* GraphQL */ `
  mutation BuildStart($tool: String!, $host: String!, $force: Boolean!) {
    corrallm {
      toolBuildStart(body: { tool: $tool, host: $host, force: $force }) {
        job {
          id
          tool
          host
          status
        }
      }
    }
  }
`)

function fmtElapsed(sec: number): string {
  if (sec < 60) return `${sec}s`
  const m = Math.floor(sec / 60)
  const s = sec % 60
  return `${m}m ${String(s).padStart(2, '0')}s`
}

export function BuildDialog({
  open,
  onClose,
  tool,
  host,
}: {
  open: boolean
  onClose: () => void
  tool?: string
  host?: string
}) {
  const qc = useQueryClient()
  const [err, setErr] = useState('')
  // Absolute index of the last log line already held. The server hands back
  // logTotal; sending it as logFrom asks only for what is new, so a
  // twenty-thousand-line build is not re-sent every second.
  const [logFrom, setLogFrom] = useState(0)
  const [lines, setLines] = useState<string[]>([])
  const logRef = useRef<HTMLDivElement | null>(null)
  const pinnedRef = useRef(true)

  const q = useQuery({
    queryKey: ['buildStatus'],
    queryFn: () => gqlClient.request(BuildStatusDoc, { logFrom: String(logFrom) }),
    enabled: open,
    // Poll only while something is running. A finished build is static, and
    // polling it forever would be a request per second for no new information.
    refetchInterval: (query) =>
      query.state.data?.corrallm.toolBuildStatus?.current ? 1000 : false,
  })

  const st = q.data?.corrallm.toolBuildStatus
  const current = st?.current
  const last = st?.last
  // The job on screen: the running one, else the last. Same rule the server
  // uses to choose whose log to send, so the title and the log always agree.
  const shown = current ?? last

  // Accumulate the incremental log.
  useEffect(() => {
    if (!st) return
    if (st.log.length > 0) setLines((prev) => [...prev, ...st.log])
    const total = Number(st.logTotal ?? 0)
    if (Number.isFinite(total) && total !== logFrom) setLogFrom(total)
  }, [st, logFrom])

  // Reset when the dialog opens, so a second look does not show the previous
  // job's lines above this one's.
  useEffect(() => {
    if (open) {
      setLines([])
      setLogFrom(0)
      setErr('')
      pinnedRef.current = true
    }
  }, [open])

  // Follow the tail, but only while the operator has not scrolled up to read
  // something. Yanking them back to the bottom every second is why log panes
  // are unusable during a build.
  useEffect(() => {
    const el = logRef.current
    if (!el || !pinnedRef.current) return
    el.scrollTop = el.scrollHeight
  }, [lines])

  const start = useMutation({
    mutationFn: (v: { force: boolean }) =>
      gqlClient.request(BuildStartDoc, { tool: tool ?? '', host: host ?? '', force: v.force }),
    onSuccess: () => {
      setErr('')
      setLines([])
      setLogFrom(0)
      void q.refetch()
    },
    onError: (e: unknown) => setErr(extractMessage(e)),
  })

  const running = !!current
  const canStart = !!tool && !!host && !running

  const statusChip = (s?: string, skipped?: boolean) => {
    if (s === 'running') return <Chip size="small" color="info" label="running" />
    if (s === 'ok')
      return (
        <Chip
          size="small"
          color="success"
          variant="outlined"
          label={skipped ? 'already current' : 'built'}
        />
      )
    if (s === 'failed') return <Chip size="small" color="error" variant="outlined" label="failed" />
    return null
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      maxWidth="md"
      fullWidth
      // Closing must not cancel: the build runs on the daemon, not here.
      // Dismissing the modal is how you go and do something else for ten
      // minutes, and it would be a nasty surprise if that killed the compile.
    >
      <DialogTitle>
        <Stack direction="row" spacing={1.5} alignItems="center" flexWrap="wrap" useFlexGap>
          <span>
            Build {tool ?? shown?.tool} <span style={{ color: C.textFaint }}>on</span>{' '}
            {host ?? shown?.host}
          </span>
          {statusChip(shown?.status ?? undefined, last?.skipped ?? undefined)}
          {shown && (
            <Typography variant="caption" sx={{ color: C.textMuted }}>
              {fmtElapsed(Number(shown.elapsedSeconds))}
              {current ? ' elapsed' : ' total'}
            </Typography>
          )}
          {running && <CircularProgress size={14} />}
        </Stack>
      </DialogTitle>

      <DialogContent dividers>
        {err && (
          <Alert severity="error" sx={{ mb: 1.5 }}>
            {err}
          </Alert>
        )}

        {!running && !shown && (
          <Typography variant="body2" sx={{ color: C.textMuted, mb: 1 }}>
            Aligns the managed checkout to the tool&apos;s pinned ref, applies any patches, compiles
            and installs it, then records a build stamp. Ten to twenty minutes for a first CUDA
            build; far less afterwards, since llama.cpp uses ccache when it finds it.
          </Typography>
        )}

        {last && !running && last.status === 'ok' && (
          <Alert severity="success" sx={{ mb: 1.5 }}>
            {last.skipped
              ? 'Already current — the stamp matched (same commit, same patches, same CUDA archs), so nothing was compiled.'
              : `Built ${last.version ?? ''} in ${fmtElapsed(Number(last.elapsedSeconds))}.`}
            {last.stamp && (
              <Box
                sx={{ mt: 0.5, fontFamily: 'monospace', fontSize: 11.5, color: C.textMuted }}
              >
                {last.stamp}
              </Box>
            )}
          </Alert>
        )}
        {last && !running && last.status === 'failed' && (
          <Alert severity="error" sx={{ mb: 1.5 }}>
            {last.error}
          </Alert>
        )}

        <Box
          ref={logRef}
          onScroll={(e) => {
            const el = e.currentTarget
            // Within a couple of lines of the bottom counts as "following".
            pinnedRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
          }}
          sx={{
            height: 380,
            overflow: 'auto',
            bgcolor: C.canvas,
            border: `1px solid ${C.border}`,
            borderRadius: 1,
            p: 1,
            fontFamily: 'monospace',
            fontSize: 11.5,
            lineHeight: 1.45,
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
          }}
        >
          {lines.length === 0 ? (
            <Typography variant="caption" sx={{ color: C.textFaint }}>
              {running ? 'waiting for output…' : 'no output'}
            </Typography>
          ) : (
            lines.map((l, i) => (
              <div key={i} style={{ color: /error|Error|FAILED/.test(l) ? C.error : undefined }}>
                {l}
              </div>
            ))
          )}
        </Box>
      </DialogContent>

      <DialogActions>
        <Typography variant="caption" sx={{ color: C.textFaint, mr: 'auto', pl: 1 }}>
          {running
            ? 'Runs on the daemon — closing this does not stop it.'
            : 'One build at a time: a build takes every core on the box.'}
        </Typography>
        <Button onClick={onClose}>Close</Button>
        <Button
          disabled={!canStart || start.isPending}
          onClick={() => {
            qc.invalidateQueries({ queryKey: ['tooling'] })
            start.mutate({ force: false })
          }}
          variant="contained"
        >
          {last && !running ? 'Build again' : 'Build'}
        </Button>
        <Button
          disabled={!canStart || start.isPending}
          onClick={() => start.mutate({ force: true })}
          // Force exists because the stamp is deliberately conservative: same
          // commit, same patches, same archs means no work. This is the escape
          // hatch when you believe the tree and disbelieve the stamp.
          title="Rebuild even when the stamp already matches"
        >
          Force
        </Button>
      </DialogActions>
    </Dialog>
  )
}
