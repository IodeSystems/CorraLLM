import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Box,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  Tooltip,
  Typography,
} from '@mui/material'
import AddIcon from '@mui/icons-material/Add'
import { Panel, Row } from '@/Panel'
import { EntryEditor, openEntry, type EntryEdit } from '@/EntryEditor'
import { ProviderModelDialog } from '@/ProviderModelDialog'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { extractMessage } from '@/format'
import { C } from '@/theme'
import { Loading } from '@/Loading'
import { groupByBox, useSetupData, UNPLACED } from '@/SetupData'

/**
 * What this box serves, and under what name.
 *
 * Three panels that used to be scattered down the Setup scroll with a page of
 * provider configuration between them, though they answer one question each
 * about the same object: the models running HERE, the models taken off somebody
 * else's catalogue, and the lane names a caller can ask for instead of either.
 */

// Blank template for a lane, kept with the panel that creates one: the editor
// is a text box, this is the domain's opinion about what to type in it.
function blankLane(): EntryEdit {
  return {
    kind: 'lane',
    existing: false,
    name: '',
    yaml: `# A lane is an ORDERED fallback list. Requesting the lane allows
# substitution across its members; requesting a model pins that model.
members:
  - some-local-model      # best first
  - some-remote-model     # spilled to when the local one is full
`,
  }
}

const SelectionsDoc = graphql(/* GraphQL */ `
  query Selections {
    corrallm {
      listSelections {
        selections {
          provider
          credential
          model
          upstream
          quality
          serving
          lanes {
            lane
            order
          }
        }
      }
    }
  }
`)

const DeleteModelDoc = graphql(/* GraphQL */ `
  mutation DeleteLocalModel($name: String!) {
    corrallm {
      deleteEntry(kind: "model", name: $name) {
        ok
        message
      }
    }
  }
`)

const UnassignDoc = graphql(/* GraphQL */ `
  mutation UnassignFromList($body: corrallm_UnassignModelInputBodyInput!) {
    corrallm {
      unassignModel(body: $body) {
        ok
        message
      }
    }
  }
`)

/**
 * Everything chosen off a directory, in one list.
 *
 * The per-provider dialog is where you CHOOSE; this is where you see what you
 * chose across all of them and where each one landed. Both write the same rows
 * — there is no separate approvals page any more, because there is nothing to
 * approve: a model is selected or it is not.
 */
function AssignedModels() {
  const qc = useQueryClient()
  const { data } = useQuery({
    queryKey: ['selections'],
    queryFn: () => gqlClient.request(SelectionsDoc),
  })
  const unassign = useMutation({
    mutationFn: (body: { provider: string; credential: string; model: string }) =>
      gqlClient.request(UnassignDoc, { body }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['selections'] })
      qc.invalidateQueries({ queryKey: ['catalog'] })
    },
  })
  const rows = data?.corrallm?.listSelections?.selections ?? []

  return (
    <Panel
      title={`Assigned models (${rows.length})`}
      // Remove is a config change, not a kill: say so where it can be read.
      subtitle="Models chosen off a provider's catalog. Remove takes one out of the catalog this box serves — anything mid-answer on it finishes, and the next request for it is refused."
      flush
    >
      {rows.length === 0 ? (
        <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
          None yet. <strong>Browse</strong> a provider on the Providers tab and add the models you
          want — with a lane and priority if they should join one.
        </Typography>
      ) : (
        rows.map((r) => (
          <Row key={`${r.provider}/${r.credential}/${r.model}`}>
            <Stack
              direction="row"
              spacing={1.5}
              useFlexGap
              sx={{
                alignItems: 'center',
                flexWrap: 'wrap',
              }}
            >
              <Box sx={{ minWidth: 220, flex: 1 }}>
                <Typography variant="body2" sx={{ fontFamily: 'monospace', fontSize: 12.5 }}>
                  {r.model}
                </Typography>
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {r.provider} / {r.credential}
                  {r.upstream ? ` · ${r.upstream}` : ' · placement only'}
                </Typography>
              </Box>
              <Stack
                direction="row"
                spacing={0.75}
                useFlexGap
                sx={{
                  flexWrap: 'wrap',
                }}
              >
                {r.lanes.map((l) => (
                  <Chip key={l.lane} size="small" label={`${l.lane} @ ${l.order}`} />
                ))}
                {r.lanes.length === 0 && (
                  <Tooltip title="Servable by name, but in no lane — nothing routes to it automatically.">
                    <Chip size="small" variant="outlined" label="no lane" />
                  </Tooltip>
                )}
                {Number(r.quality ?? 0) > 0 && (
                  <Chip size="small" variant="outlined" label={`q${r.quality}`} />
                )}
                {!r.serving && (
                  <Tooltip title="On record, but not in the served registry — the provider or its id may have gone away.">
                    <Chip size="small" color="warning" label="not serving" />
                  </Tooltip>
                )}
              </Stack>
              <Button
                size="small"
                color="error"
                disabled={unassign.isPending}
                onClick={() =>
                  unassign.mutate({
                    provider: r.provider,
                    credential: r.credential,
                    model: r.model,
                  })
                }
              >
                Remove
              </Button>
            </Stack>
          </Row>
        ))
      )}
    </Panel>
  )
}

export function ModelsTab() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [addLocal, setAddLocal] = useState<{
    provider: string
    extension?: string
    editId?: string
  } | null>(null)
  // Deleting a model is not undoable from here, so it is confirmed by name.
  const [confirmDelete, setConfirmDelete] = useState<{ served: string; hasCmd: boolean } | null>(
    null,
  )
  const [deleteErr, setDeleteErr] = useState('')
  const [editing, setEditing] = useState<EntryEdit | null>(null)

  const del = useMutation({
    mutationFn: (name: string) => gqlClient.request(DeleteModelDoc, { name }),
    onSuccess: (r) => {
      const res = r.corrallm?.deleteEntry
      // The server refuses to delete a lane member and says which lane. That is
      // the useful half of the answer, so it goes on screen rather than being
      // collapsed into a generic failure.
      if (!res?.ok) {
        setDeleteErr(res?.message ?? 'delete refused')
        return
      }
      setConfirmDelete(null)
      setDeleteErr('')
      qc.invalidateQueries({ queryKey: ['providers'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (e) => setDeleteErr(extractMessage(e)),
  })

  const { data, isLoading, error } = useSetupData()

  if (isLoading) return <Loading />
  if (error) return <Alert severity="error">{String(error)}</Alert>

  const local = data?.corrallm?.listProviders?.local ?? []
  // Lanes and extensions moved here from the Config page. Both are about
  // MODELS — a lane is an ordered list of them, an extension is one process
  // serving several — so they belong with the models rather than on a page
  // named after a file.
  const lanes = data?.corrallm?.overview?.lanes ?? []

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      {local.length > 0 && (
        <Panel
          title={`Local (${local.reduce((n, p) => n + p.models.length, 0)} models)`}
          // Delete sat beside a model marked "spawned" with nothing saying what it
          // does to it (Lenny run 4, OB-1/OB-2).
          subtitle="Models this box runs itself. Edit changes how one is started; Delete removes it from the configuration — if it is loaded, it is stopped, and anything mid-answer on it ends there."
          flush
        >
          <Typography variant="body2" sx={{ px: 2, pt: 1.5, color: C.textMuted }}>
            Models that run on this box. Each owns its process, its GPU budget and its port — which
            is why they are declared rather than browsed: the catalogue is whatever you put on the
            disk. Served as <code>&lt;provider&gt;-&lt;id&gt;</code> like any remote.
          </Typography>
          {local.map((p) => (
            <Row key={p.name}>
              <Stack
                direction="row"
                spacing={1.5}
                useFlexGap
                sx={{
                  alignItems: 'baseline',
                  flexWrap: 'wrap',
                }}
              >
                <Box sx={{ minWidth: 140 }}>
                  <Typography variant="subtitle2">{p.name}</Typography>
                  <Typography variant="caption" sx={{ color: C.textFaint }}>
                    {p.models.length} model{p.models.length === 1 ? '' : 's'}
                  </Typography>
                </Box>
                {/* The provider's own notes. A real config field, so unlike a
                    YAML comment it survives the file being rewritten — which is
                    where an explanation about this provider belongs. */}
                {p.notes && (
                  <Tooltip title={<span style={{ whiteSpace: 'pre-wrap' }}>{p.notes}</span>}>
                    <Typography
                      variant="caption"
                      sx={{
                        color: C.textMuted,
                        flexBasis: '100%',
                        whiteSpace: 'pre-wrap',
                        display: '-webkit-box',
                        WebkitLineClamp: 1,
                        WebkitBoxOrient: 'vertical',
                        overflow: 'hidden',
                        mt: 0.5,
                      }}
                    >
                      {p.notes}
                    </Typography>
                  </Tooltip>
                )}
                <Stack
                  direction="row"
                  spacing={0.75}
                  useFlexGap
                  sx={{
                    flexWrap: 'wrap',
                    flex: 1,
                  }}
                >
                  {Number(p.barePrecedence) > 0 ? (
                    <Tooltip
                      title={`An unprefixed name that nothing else claims resolves here (precedence ${p.barePrecedence}). This is what keeps callers working after the prefix rename.`}
                    >
                      <Chip size="small" variant="outlined" label="bare names → here" />
                    </Tooltip>
                  ) : (
                    <Tooltip title="Only the prefixed name resolves; barePrecedence is 0.">
                      <Chip size="small" variant="outlined" label="prefixed only" />
                    </Tooltip>
                  )}
                </Stack>
                <Button
                  size="small"
                  startIcon={<AddIcon />}
                  onClick={() => setAddLocal({ provider: p.name })}
                >
                  Add model
                </Button>
              </Stack>
              {p.models.length === 0 && (
                <Typography variant="caption" sx={{ color: C.textFaint, pl: 0.5 }}>
                  no models yet
                </Typography>
              )}
              {/* Grouped by BOX, because that is the question actually asked of
                  this list: "what runs on box1". A per-row server chip said the
                  same thing scattered across twenty rows, which reads as a flat
                  list of processes with no machine behind them. A model with no
                  server is a pure proxy and groups under its own heading rather
                  than being hidden or faked onto a host. */}
              {groupByBox(p.models).map(([box, boxModels]) => (
                <Box key={box} sx={{ mt: 1 }}>
                  <Stack
                    direction="row"
                    spacing={1}
                    sx={{
                      alignItems: 'baseline',
                      pl: 2,
                      pt: 0.5,
                    }}
                  >
                    <Typography
                      variant="caption"
                      sx={{ color: C.textMuted, fontWeight: 600, letterSpacing: 0.4 }}
                    >
                      {box === UNPLACED ? 'no host (proxy)' : box}
                    </Typography>
                    <Chip size="small" variant="outlined" label={boxModels.length} />
                  </Stack>
                  {boxModels.map((m) => (
                    <Stack
                      key={m.id}
                      direction="row"
                      spacing={1.5}
                      useFlexGap
                      sx={{
                        alignItems: 'center',
                        flexWrap: 'wrap',
                        pl: 2,
                        py: 0.75,
                        borderTop: `1px solid ${C.border}`,
                        mt: 0.75,
                      }}
                    >
                      <Box sx={{ minWidth: 220 }}>
                        <Typography variant="body2" sx={{ fontFamily: 'monospace', fontSize: 12.5 }}>
                          {m.served}
                        </Typography>
                        <Typography variant="caption" sx={{ color: C.textFaint }}>
                          {m.bare ? `also answers to ${m.id}` : 'prefixed name only'}
                        </Typography>
                      </Box>
                      <Stack
                        direction="row"
                        spacing={0.75}
                        useFlexGap
                        sx={{
                          flexWrap: 'wrap',
                          flex: 1,
                        }}
                      >
                        {m.type && <Chip size="small" variant="outlined" label={m.type} />}
                        {/* No server chip: the group heading above already says
                            which box, and repeating it on every row was the flat
                            list's way of answering a question the grouping now
                            answers once. */}
                        <Tooltip
                          title={
                            m.hasCmd
                              ? 'corrallm starts and stops this process'
                              : 'A pure proxy — corrallm forwards to it and never starts or stops it'
                          }
                        >
                          <Chip
                            size="small"
                            variant="outlined"
                            label={m.hasCmd ? 'spawned' : 'proxy'}
                          />
                        </Tooltip>
                      </Stack>
                      <Button
                        size="small"
                        onClick={() => setAddLocal({ provider: p.name, editId: m.id })}
                      >
                        Edit
                      </Button>
                      <Button
                        size="small"
                        onClick={() => navigate({ to: '/m/$name', params: { name: m.served } })}
                      >
                        Inspect
                      </Button>
                      <Button
                        size="small"
                        color="error"
                        onClick={() => {
                          setDeleteErr('')
                          setConfirmDelete({ served: m.served, hasCmd: m.hasCmd })
                        }}
                      >
                        Delete
                      </Button>
                    </Stack>
                  ))}
                </Box>
              ))}
            </Row>
          ))}
        </Panel>
      )}

      <AssignedModels />

      <Panel
        title={`Model lanes (${lanes.length})`}
        subtitle="A name a caller can ask for that stands for several models, tried in order until one can serve. Asking for the lane allows a substitute; asking for a model by name pins it to that one."
        actions={
          <Button size="small" variant="outlined" onClick={() => setEditing(blankLane())}>
            Add lane
          </Button>
        }
        flush
      >
        {lanes.length === 0 ? (
          <Row>
            <Typography variant="body2" sx={{ color: C.textFaint }}>
              No lanes declared.
            </Typography>
          </Row>
        ) : (
          lanes.map((l) => (
            <Row
              key={l.name}
              onClick={() => {
                void openEntry('lane', l.name).then(setEditing)
              }}
            >
              <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                <Typography variant="subtitle2">{l.name}</Typography>
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {l.members.map((mem) => mem.model).join('  →  ')}
                </Typography>
              </Box>
            </Row>
          ))
        )}
      </Panel>

      {addLocal && (
        <ProviderModelDialog
          open
          onClose={() => setAddLocal(null)}
          provider={addLocal.provider}
          extension={addLocal.extension}
          editId={addLocal.editId}
        />
      )}
      <Dialog open={confirmDelete != null} onClose={() => setConfirmDelete(null)}>
        <DialogTitle>Delete {confirmDelete?.served}?</DialogTitle>
        <DialogContent>
          <Typography variant="body2">
            Removes it from the provider&apos;s config. Callers asking for it — by this name or its
            unprefixed one — start getting a 404.
          </Typography>
          {confirmDelete?.hasCmd && (
            <Typography variant="body2" sx={{ mt: 1.5, color: C.warn }}>
              If it is running right now, the process keeps running and keeps its memory: a config
              reload deliberately does not kill backends that are already up. Unload it from its
              model page first if you want the memory back before the next restart.
            </Typography>
          )}
          <Typography variant="caption" sx={{ display: 'block', mt: 1.5, color: C.textFaint }}>
            A lane member cannot be deleted until it is removed from the lane; the server will say
            which lane.
          </Typography>
          {deleteErr && (
            <Alert severity="error" sx={{ mt: 2 }}>
              {deleteErr}
            </Alert>
          )}
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirmDelete(null)}>Cancel</Button>
          <Button
            color="error"
            variant="contained"
            disabled={del.isPending}
            onClick={() => confirmDelete && del.mutate(confirmDelete.served)}
          >
            {del.isPending ? 'Deleting…' : 'Delete'}
          </Button>
        </DialogActions>
      </Dialog>
      <EntryEditor
        editing={editing}
        onChange={setEditing}
        onClose={() => setEditing(null)}
        invalidate={['providers', 'config']}
      />
    </Box>
  )
}
