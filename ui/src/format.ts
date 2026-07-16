// Small display helpers shared across observability views. gat emits Long
// (int64) as a string, so timestamps/dwell arrive as strings — parse defensively.

export function fmtTime(msStr: string | number): string {
  const ms = typeof msStr === 'string' ? Number(msStr) : msStr
  if (!Number.isFinite(ms) || ms <= 0) return '—'
  return new Date(ms).toLocaleString()
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
