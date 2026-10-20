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
import { AddModelDialog } from '@/AddModelDialog'
import { OperatorNote } from '@/OperatorNote'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { extractMessage } from '@/format'
import { C } from '@/theme'
import { Loading } from '@/Loading'
import { FilterField, matches, Pager, usePaging } from '@/Paging'
import { groupByBox, useSetupData, UNPLACED } from '@/SetupData'

/**
 * What this box serves, and under what name.
 *
 * Three panels that used to be scattered down the Setup scroll with a page of
 * provider configuration between them, though they answer one question each
 * about the same object: the models running HERE, the models taken off somebody
 * else's catalogue, and the fallback lists a caller can ask for instead of
 * either.
 *
 * THE OBJECT IS A FALLBACK LIST. The config file calls it a `lane` and so does
 * the API, and OB-3 says that word is the gloss and never the label: it is the
 * config's word for the thing, not a word anybody arrives already knowing. It
 * survives in the gloss on each label, because an operator reading the YAML
 * needs the two vocabularies joined somewhere — the same trade `placement` took
 * in 0eb90c2.
 */

// Blank template for a fallback list, kept with the panel that creates one: the
// editor is a text box, this is the domain's opinion about what to type in it.
function blankLane(): EntryEdit {
  return {
    kind: 'lane',
    existing: false,
    name: '',
    yaml: `# A fallback list — a "lane" in this file — is an ORDERED list of
# models. Asking for its name allows a substitute from further down;
# asking for a model by name pins the request to that model.
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
 *
 * The list with no ceiling: one row per model per credential per provider, and
 * a provider's catalogue runs to three hundred. Filtered and paged, because
 * "did I already add the free Qwen" is a lookup, not a scroll.
 */
function AssignedModels() {
  const qc = useQueryClient()
  const [q, setQ] = useState('')
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
  const all = data?.corrallm?.listSelections?.selections ?? []
  const filtered = all.filter((r) =>
    matches(q, r.model, r.provider, r.credential, r.upstream, ...r.lanes.map((l) => l.lane)),
  )
  const { rows, pager } = usePaging(filtered, 15)

  return (
    <Panel
      title={`Assigned models (${filtered.length === all.length ? all.length : `${filtered.length} of ${all.length}`})`}
      subtitle="Chosen from a provider's catalog"
      // Remove is a config change, not a kill: say so where it can be read.
      // "where it can be read" was a clipping header until now.
      //
      // The second sentence is what the `chat @ 50` chips on these rows mean.
      // It is said once for the group rather than hidden on a tooltip per row
      // (OB-2, OB-10): a number in a chip is unreadable without it, and a
      // hovered gloss is not written.
      note="Remove takes a model out of the catalog; requests for it are then refused."
      help={
        <>
          Anything mid-answer on a removed model finishes. A chip like <code>chat @ 50</code> shows
          the fallback list the model joined and its position in it; lower is tried first.
        </>
      }
      actions={
        all.length > 8 ? (
          <FilterField value={q} onChange={setQ} placeholder="model, provider, list" />
        ) : undefined
      }
      flush
    >
      {all.length > 0 && filtered.length === 0 && (
        <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
          No assigned model matches “{q}”.
        </Typography>
      )}
      {all.length === 0 ? (
        <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
          None yet. <strong>Browse</strong> a provider on the Providers tab and add the models you
          want — with a fallback list and a position if they should join one.
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
                {/* `placement only` was the other bare OB-3 label on this row.
                    It means the selection carries no upstream id: a discover
                    filter already contributes the model, and this row only says
                    which fallback list it joins — "I did not choose this one,
                    but I do choose where it goes" (internal/config/selection.go). */}
                <Typography variant="caption" sx={{ color: C.textFaint }}>
                  {r.provider} / {r.credential}
                  {r.upstream ? ` · ${r.upstream}` : ' · only says where it goes'}
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
                  <Tooltip title="Servable by name, but in no fallback list — nothing reaches it unless a caller asks for it by name.">
                    <Chip size="small" variant="outlined" label="no fallback list" />
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
      <Pager p={pager} unit="models" />
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
  const [addTo, setAddTo] = useState<string | null>(null)
  // Deleting a model is not undoable from here, so it is confirmed by name.
  const [confirmDelete, setConfirmDelete] = useState<{ served: string; hasCmd: boolean } | null>(
    null,
  )
  const [deleteErr, setDeleteErr] = useState('')
  const [editing, setEditing] = useState<EntryEdit | null>(null)
  const [localQ, setLocalQ] = useState('')

  const del = useMutation({
    mutationFn: (name: string) => gqlClient.request(DeleteModelDoc, { name }),
    onSuccess: (r) => {
      const res = r.corrallm?.deleteEntry
      // The server refuses to delete a model a fallback list still names, and
      // says which list. That is the useful half of the answer, so it goes on
      // screen rather than being collapsed into a generic failure.
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

  const local = data?.corrallm?.listProviders?.local ?? []
  const localTotal = local.reduce((n, p) => n + p.models.length, 0)
  // Filtered, not paged: these rows are GROUPED by the box that runs them, and
  // a page boundary through the groups would cut a heading off its models. The
  // filter narrows the models and drops a provider left with none.
  const localShown = !localQ.trim()
    ? local
    : local
        .map((p) => ({
          ...p,
          models: p.models.filter((m) => matches(localQ, m.served, m.id, m.type, m.server)),
        }))
        .filter((p) => p.models.length > 0)
  const localShownCount = localShown.reduce((n, p) => n + p.models.length, 0)

  // Fallback lists and extensions moved here from the Config page. Both are
  // about MODELS — a list is an ordered set of them, an extension is one
  // process serving several — so they belong with the models rather than on a
  // page named after a file.
  const lanes = data?.corrallm?.overview?.lanes ?? []
  const { rows: lanePage, pager: lanePager } = usePaging(lanes, 10)

  if (isLoading) return <Loading />
  if (error) return <Alert severity="error">{String(error)}</Alert>

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      {local.length > 0 && (
        <Panel
          title={`Local (${localShownCount === localTotal ? localTotal : `${localShownCount} of ${localTotal}`} models)`}
          subtitle="Models this box runs itself"
          // Delete sat beside a model marked "spawned" with nothing saying what it
          // does to it (Lenny run 4, OB-1/OB-2). The header it was moved to clips
          // at the reader's window width, and the clause it takes is "ends there".
          note="Delete removes a model from the configuration and stops it if loaded."
          help="Edit changes how a model is started. Anything mid-answer on a deleted model ends there."

          actions={
            localTotal > 8 ? (
              <FilterField value={localQ} onChange={setLocalQ} placeholder="model, type, host" />
            ) : undefined
          }
          flush
        >
          {localShown.length === 0 && (
            <Typography variant="body2" sx={{ px: 2, py: 1.5, color: C.textFaint }}>
              No local model matches “{localQ}”.
            </Typography>
          )}
          {localShown.map((p) => (
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
                    where an explanation about this provider belongs.

                    This was the last of the one-line-clamp-plus-tooltip notes:
                    a rendered half-sentence with the rest reachable only by
                    hovering (OB-10). Same component as every other note now, so
                    the attribution and the way to read the rest are the same
                    wherever a note appears. */}
                {p.notes && <OperatorNote text={p.notes} />}
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
                  onClick={() => setAddTo(p.name)}
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
                            label={m.hasCmd ? 'managed' : 'proxy'}
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
        title={`Fallback lists (${lanes.length})`}
        subtitle="Models tried in order until one can serve"
        // Which model answers a request is what this decides, so the sentence
        // that says so is the panel's, not the header's (OB-12). The second
        // sentence is the gloss that joins this label to the config file, which
        // is the only place the word `lane` now appears in front of a reader.
        help="Asking for a list by name allows a substitute; asking for a model by name pins the request to that model."
        actions={
          <Button size="small" variant="outlined" onClick={() => setEditing(blankLane())}>
            Add fallback list
          </Button>
        }
        flush
      >
        {lanes.length === 0 ? (
          <Row>
            <Typography variant="body2" sx={{ color: C.textFaint }}>
              None declared.
            </Typography>
          </Row>
        ) : (
          lanePage.map((l) => (
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
        <Pager p={lanePager} unit="fallback lists" />
      </Panel>

      <AddModelDialog
        open={addTo != null}
        onClose={() => setAddTo(null)}
        initialKind="process"
        localProvider={addTo ?? 'local'}
      />
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
            A model cannot be deleted while a fallback list still names it; the server will say
            which list.
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
