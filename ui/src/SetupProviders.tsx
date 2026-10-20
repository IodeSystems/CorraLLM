import { useState } from 'react'
import { Alert, Box, Button, Chip, Stack, Tooltip, Typography } from '@mui/material'
import AddIcon from '@mui/icons-material/Add'
import SearchIcon from '@mui/icons-material/Search'
import { Panel, Row } from '@/Panel'
import { EntryEditor, openEntry, type EntryEdit } from '@/EntryEditor'
import { ProviderDialog } from '@/ProviderDialog'
import { toLimit, type ProviderInitial } from '@/ProviderDialog'
import { CatalogDialog } from '@/CatalogDialog'
import { ProviderModelDialog } from '@/ProviderModelDialog'
import { OperatorNote } from '@/OperatorNote'
import { C } from '@/theme'
import { Loading } from '@/Loading'
import { FilterField, matches } from '@/Paging'
import { byExtension, useSetupData, type ProviderRow } from '@/SetupData'

/**
 * Providers (P21): an upstream endpoint and the accounts held against it.
 *
 * The tab is a LIST plus two dialogs, not a form. Adding a provider asks for
 * things nobody knows by heart — host, port, the path prefix in front of /v1 —
 * so that lives in a modal with a typeahead over known endpoints; and choosing
 * which of a provider's three hundred models to serve is a search problem, so
 * that lives in a modal of its own, opened FROM the provider that holds the
 * catalogue. Neither belongs inline under a heading.
 *
 * Secrets are NEVER shown. A credential carries a secretRef — the name of an
 * entry in ~/.corrallm/credentials, referenced from config as ${NAME} — and the
 * value is written through a separate write-only endpoint. That is what keeps
 * the config document safe to read, back up and share.
 */

// Blank template for an extension. It lives with the panel that creates one
// rather than in the editor, because the editor is a text box and this is the
// domain's opinion about what to type in it.
function blankExtension(): EntryEdit {
  return {
    kind: 'extension',
    existing: false,
    name: '',
    yaml: `# One process serving SEVERAL models. They load, unload and are
# accounted for together, because they are the same bytes.
# cmd: "exec my-server --addr :5806"
# server: box1
# ramUsage: { system: 3GB }     # counted ONCE, not per provided model
proxy: 5806
provides:
  - name: my-model
    type: chat
`,
  }
}

export function ProvidersTab(props: {
  // The page header's "Add provider" opens this dialog, so the one piece of
  // state two places set lives above both of them. '' means "no group chosen
  // yet"; a name means opened FROM that group.
  addTo: string | null
  onAddTo: (v: string | null) => void
}) {
  const { addTo, onAddTo } = props
  const [edit, setEdit] = useState<ProviderInitial | null>(null)
  const [browse, setBrowse] = useState<{ provider: string; credential: string } | null>(null)
  const [addLocal, setAddLocal] = useState<{ provider: string; extension?: string } | null>(null)
  const [editing, setEditing] = useState<EntryEdit | null>(null)
  const [q, setQ] = useState('')

  const { data, isLoading, error } = useSetupData()

  if (isLoading) return <Loading />
  if (error) return <Alert severity="error">{String(error)}</Alert>

  const list = data?.corrallm?.listProviders
  const providers = list?.providers ?? []
  const pools = list?.pools ?? []
  const ov = data?.corrallm?.overview
  const extensions = ov?.extensions ?? []
  const secrets = list?.secrets ?? []

  const all = byExtension(extensions, pools, providers)
  // Filtered, not paged. This is a TREE — an extension and the providers under
  // it — and a page boundary drawn through it would separate a provider from
  // the heading that says what it belongs to. A match on either half keeps the
  // whole group, for the same reason.
  const groups = all.filter(
    (g) =>
      matches(q, g.name) ||
      g.provs.some((p) => matches(q, p.name, p.host, ...(p.credentials ?? []).map((c) => c.name))),
  )

  const toInitial = (p: ProviderRow): ProviderInitial => ({
    extension: p.extension,
    name: p.name,
    host: p.host,
    port: String(p.port ?? '443'),
    basePath: p.basePath ?? '',
    manual: p.manual ?? false,
    directory: p.directory
      ? {
          freeOnly: p.directory.freeOnly ?? false,
          minContext: String(p.directory.minContext ?? '0'),
        }
      : null,
    limits: (p.limits ?? []).map(toLimit),
    credentials: (p.credentials ?? []).map((c) => ({
      name: c.name,
      secretRef: c.secretRef ?? '',
      headerName: c.headerName ?? '',
      allow: c.allow ?? [],
      limits: (c.limits ?? []).map(toLimit),
    })),
  })

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Panel
        title={`Integrations (${groups.length === all.length ? all.length : `${groups.length} of ${all.length}`})`}
        subtitle="An extension is the container: one integration, its providers, and the models it serves."
        actions={
          <>
            {/* No filter until there is something to filter: a search box over
                four rows is furniture. */}
            {all.length > 6 && (
              <FilterField value={q} onChange={setQ} placeholder="extension, provider, host" />
            )}
            <Button size="small" variant="outlined" onClick={() => setEditing(blankExtension())}>
              Add extension
            </Button>
          </>
        }
        flush
      >
        {all.length === 0 && (
          <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
            None yet. <strong>Add provider</strong> starts from a table of known OpenAI-compatible
            endpoints, or takes a custom one.
          </Typography>
        )}
        {all.length > 0 && groups.length === 0 && (
          <Typography variant="body2" sx={{ p: 2, color: C.textFaint }}>
            No integration or provider matches “{q}”.
          </Typography>
        )}
        {groups.map((g) => (
          <Box key={g.name}>
            {/* The extension itself. Clicking it opens the same YAML editor the
                flat Extensions panel used to own. */}
            <Row
              onClick={() => {
                if (g.ext) void openEntry('extension', g.name).then(setEditing)
              }}
            >
              <Stack
                direction="row"
                spacing={1.25}
                useFlexGap
                sx={{
                  alignItems: 'baseline',
                  flexWrap: 'wrap',
                }}
              >
                <Typography variant="subtitle2" sx={{ minWidth: 120 }}>
                  {g.name}
                </Typography>
                {g.pool && (
                  <Tooltip title="A virtual extension: it has no endpoint of its own and pools its members' catalogues. Membership is re-derived on every refresh, so a provider withdrawing a model does not take the pool down.">
                    <Chip size="small" color="info" variant="outlined" label="pool" />
                  </Tooltip>
                )}
                {g.ext?.cmd && (
                  <Tooltip title="One local process serving several models. They load, unload and are accounted for together, because they are the same bytes.">
                    <Chip size="small" variant="outlined" label="spawned" />
                  </Tooltip>
                )}
                {g.ext?.server && <Chip size="small" variant="outlined" label={g.ext.server} />}
                {g.provs.length > 0 && (
                  <Chip
                    size="small"
                    variant="outlined"
                    label={`${g.provs.length} provider${g.provs.length === 1 ? '' : 's'}`}
                  />
                )}
                {(g.ext?.provides ?? []).length > 0 && (
                  <Typography variant="caption" sx={{ color: C.textFaint }}>
                    serves {(g.ext?.provides ?? []).join(', ')}
                  </Typography>
                )}
                {/* This badge read `lane free` — the config's word for the
                    object, as the label, with the operator's name for it
                    trailing after (OB-3). The name is the readable half and
                    goes first; `lane` survives in the gloss, because the
                    person who opens the YAML needs the two words joined. */}
                {g.pool?.lanes?.map((l) => (
                  <Tooltip
                    key={l.lane}
                    title={`Every model in this pool joins the ${l.lane} fallback list at position ${l.order}, so asking for ${l.lane} reaches the pool. The config file calls this a lane.`}
                  >
                    <Chip size="small" variant="outlined" label={`${l.lane} · fallback list`} />
                  </Tooltip>
                ))}
                {/* Adding a provider FROM the group it joins. The dialog used to
                    ask which one in a dropdown labelled with the config's word
                    for it; asked here, the click is the answer. The global
                    button in the page header still exists for "I do not know
                    yet". */}
                <Button
                  size="small"
                  onClick={(e) => {
                    e.stopPropagation() // the row itself opens the extension editor
                    onAddTo(g.name)
                  }}
                >
                  + Provider
                </Button>
              </Stack>
              {/* The extension's own commentary out of the config. Attributed
                  rather than rewritten: the words are the operator's, and the
                  reader could not tell them from the product's. */}
              {g.ext?.notes && <OperatorNote text={g.ext.notes} />}
            </Row>

            {/* Its providers, indented to say so. */}
            <Box sx={{ pl: 3, borderLeft: `2px solid ${C.border}`, ml: 2 }}>
              {g.provs.map((p) => {
                // A provider with no explicit credentials still has one — the
                // implicit "default" carrying the provider's own headers — and
                // the catalogue is fetched per credential, so that is what
                // Browse uses.
                const creds = p.credentials ?? []
                const browseCred = creds[0]?.name ?? 'default'
                return (
                  <Row key={`${p.extension}/${p.name}`}>
                    <Stack
                      direction="row"
                      spacing={1.5}
                      useFlexGap
                      sx={{
                        alignItems: 'center',
                        flexWrap: 'wrap',
                      }}
                    >
                      <Box sx={{ minWidth: 200 }}>
                        {/* No extension label: the row above and the indent say it. */}
                        <Typography variant="subtitle2">{p.name}</Typography>
                        <Typography variant="caption" sx={{ color: C.textMuted }}>
                          {p.host}:{p.port}
                          {p.basePath}
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
                        {/* Where this provider's models come from. A directory
                            filter is NOT one of them — it only changes what
                            Browse opens pre-filtered to. */}
                        {Number(p.provides ?? 0) > 0 && (
                          <Tooltip title="Models written into the extension YAML by hand. Edit them there, not here.">
                            <Chip size="small" variant="outlined" label={`${p.provides} declared`} />
                          </Tooltip>
                        )}
                        {p.manual && (
                          <Tooltip title="Models are chosen one at a time off this provider's directory.">
                            <Chip size="small" variant="outlined" label="hand-picked" />
                          </Tooltip>
                        )}
                        {p.directory?.freeOnly && (
                          <Tooltip title="Browse opens pre-filtered to free models. A browsing default only — it imports nothing.">
                            <Chip size="small" variant="outlined" label="browse: free" />
                          </Tooltip>
                        )}
                        {(p.limits ?? []).map((l, i) => (
                          <Chip
                            key={i}
                            size="small"
                            variant="outlined"
                            label={`provider ${l.usd ? `$${l.usd}` : l.req}/${l.per}`}
                          />
                        ))}
                        {creds.length === 0 && (
                          <Tooltip title="No credentials block: the provider's own proxy headers are the one implicit account.">
                            <Chip size="small" variant="outlined" label="implicit account" />
                          </Tooltip>
                        )}
                        {creds.map((c) => (
                          <Chip
                            key={c.name}
                            size="small"
                            color={c.hasSecret || !c.secretRef ? 'success' : 'warning'}
                            variant="outlined"
                            label={
                              !c.secretRef
                                ? `${c.name} · no key`
                                : c.hasSecret
                                  ? `${c.name} · ${c.secretRef}`
                                  : `${c.name} · ${c.secretRef} not stored`
                            }
                          />
                        ))}
                      </Stack>

                      <Stack direction="row" spacing={1}>
                        <Button
                          size="small"
                          startIcon={<SearchIcon />}
                          onClick={() => setBrowse({ provider: p.name, credential: browseCred })}
                        >
                          Browse
                        </Button>
                        {/* Declaring a model by hand, for a provider whose
                            catalogue cannot be browsed — Groq and Cerebras
                            publish no pricing or context, so their directories
                            cannot be filtered — or for one model you want
                            pinned by name regardless of what the catalogue says
                            today. */}
                        <Tooltip title="Declare a model of this provider by hand, instead of choosing one off its directory.">
                          <Button
                            size="small"
                            startIcon={<AddIcon />}
                            onClick={() => setAddLocal({ provider: p.name, extension: p.extension })}
                          >
                            Model
                          </Button>
                        </Tooltip>
                        <Button size="small" onClick={() => setEdit(toInitial(p))}>
                          Edit
                        </Button>
                      </Stack>
                    </Stack>
                  </Row>
                )
              })}
            </Box>
          </Box>
        ))}
      </Panel>

      {/* The Pools and Extensions panels are gone: both described objects that
          are now rows in Integrations above. `free` used to appear three times
          — as an extension, as a pool, and implicitly as the parent of three
          unattached providers — which is three places to look for one thing. */}

      {/* The credential store sits under the providers that reference it rather
          than at the bottom of the page: a secretRef is only ever read in the
          sentence "which account does this provider use". */}
      <Panel title={`Credential store (${secrets.length})`}>
        <Typography variant="body2" sx={{ opacity: 0.75, mb: 1 }}>
          Names only, never values — referenced from a provider as <code>${'{NAME}'}</code>. No
          endpoint returns a value, which is what keeps the config document safe to read, back up
          and share.
        </Typography>
        <Stack
          direction="row"
          spacing={1}
          useFlexGap
          sx={{
            flexWrap: 'wrap',
          }}
        >
          {secrets.map((s) => (
            <Chip key={s} size="small" label={s} />
          ))}
          {secrets.length === 0 && (
            <Typography variant="caption" sx={{ opacity: 0.7 }}>
              empty
            </Typography>
          )}
        </Stack>
      </Panel>

      <ProviderDialog
        open={addTo !== null}
        defaultExtension={addTo || undefined}
        onClose={() => onAddTo(null)}
        secrets={secrets}
      />
      <ProviderDialog
        open={edit != null}
        onClose={() => setEdit(null)}
        initial={edit ?? undefined}
        secrets={secrets}
      />
      {addLocal && (
        <ProviderModelDialog
          open
          onClose={() => setAddLocal(null)}
          provider={addLocal.provider}
          extension={addLocal.extension}
        />
      )}
      {browse && (
        <CatalogDialog
          open
          onClose={() => setBrowse(null)}
          provider={browse.provider}
          credential={browse.credential}
        />
      )}
      <EntryEditor
        editing={editing}
        onChange={setEditing}
        onClose={() => setEditing(null)}
        invalidate={['providers', 'config']}
      />
    </Box>
  )
}
