// Small display helpers shared across observability views. gat emits Long
// (int64) as a string, so timestamps/dwell arrive as strings — parse defensively.

export function fmtTime(msStr: string | number): string {
  const ms = typeof msStr === 'string' ? Number(msStr) : msStr
  if (!Number.isFinite(ms) || ms <= 0) return '—'
  return new Date(ms).toLocaleString()
}

/**
 * "How long since", from an RFC3339 timestamp — the form a roster is read in.
 *
 * A wall-clock date answers "when" and leaves the reader doing arithmetic to
 * get "is this caller still around", which is the actual question a last-seen
 * column is asked. The absolute time stays available as a tooltip.
 */
export function fmtAgo(iso?: string | null): string {
  if (!iso) return 'never'
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return '—'
  const s = Math.max(0, (Date.now() - t) / 1000)
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export function fmtDuration(msStr: string | number): string {
  const ms = typeof msStr === 'string' ? Number(msStr) : msStr
  if (!Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${ms} ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)} s`
  return `${Math.floor(s / 60)}m ${Math.round(s % 60)}s`
}

export function fmtUSD(n: number): string {
  if (!Number.isFinite(n)) return '—'
  if (n === 0) return '$0'
  if (n < 0.01) return `$${n.toFixed(5)}`
  return `$${n.toFixed(4)}`
}

export function fmtBytes(nStr: string | number): string {
  const n = typeof nStr === 'string' ? Number(nStr) : nStr
  if (!Number.isFinite(n)) return '—'
  if (n === 0) return '0'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`
}

// fmtMiB formats a MiB-denominated metric (VRAM footprints from the tune
// cache / GPU probe) via fmtBytes. 0 (or absent) means "unmeasured", not
// "zero bytes" for these fields, so it renders as '—' rather than '0'.
export function fmtMiB(nStr: string | number): string {
  const n = typeof nStr === 'string' ? Number(nStr) : nStr
  if (!Number.isFinite(n) || n <= 0) return '—'
  return fmtBytes(n * 1024 * 1024)
}

export function fmtInt(nStr: string | number): string {
  const n = typeof nStr === 'string' ? Number(nStr) : nStr
  return Number.isFinite(n) ? n.toLocaleString() : '—'
}

// capLabel shortens a capability for a chip, keeping STT and TTS distinct.
export function capLabel(c?: string): string {
  switch (c) {
    case 'audio.stt':
      return 'stt'
    case 'audio.realtime':
      return 'realtime'
    case 'audio.tts':
      return 'tts'
    case 'embeddings':
      return 'embed'
    default:
      return c || 'chat'
  }
}

// extractMessage pulls the useful sentence out of a GraphQL client error.
//
// graphql-request throws an object carrying the whole response, so String(e) is
// a wall of serialized JSON with the one readable sentence buried at the front.
// The server's refusals are written to be read — "X is a member of lane(s) chat
// — remove it there first" — and that is what belongs on screen.
//
// Lives here because there were already two private copies, and a third was
// about to be written.
export function extractMessage(e: unknown): string {
  const any = e as { response?: { errors?: { message?: string }[] }; message?: string }
  const first = any?.response?.errors?.[0]?.message
  return first || any?.message || String(e)
}
