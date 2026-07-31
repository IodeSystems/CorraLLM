import { Box, Table, TableBody, TableCell, TableHead, TableRow, Typography } from '@mui/material'
import { C } from '@/theme'

/**
 * A deliberately small markdown renderer for probe descriptions.
 *
 * Probe prose is authored in this repo, not user-supplied, so the syntax in play
 * is a known subset: `##` headings, paragraphs, `-` bullets, pipe tables,
 * `**bold**` and `` `code` ``. Rendering that needs ~100 lines, where a markdown
 * library would be a runtime dependency carried for one panel.
 *
 * Anything it does not recognise falls through as text rather than disappearing,
 * so an unsupported construct reads as slightly plain — never as a blank space
 * where a paragraph should be.
 */

/** Inline: `code`, **bold**, *italic*. Split-based, so no regex backtracking. */
function inline(src: string, keyBase: string): React.ReactNode[] {
  const out: React.ReactNode[] = []
  // Code first: its content must not be re-scanned for emphasis, or a path like
  // `a*b*c` would render half-italic.
  src.split('`').forEach((chunk, i) => {
    if (i % 2 === 1) {
      out.push(
        <Box
          key={`${keyBase}-c${i}`}
          component="code"
          sx={{
            px: 0.5,
            py: 0.1,
            borderRadius: 0.5,
            bgcolor: C.raised,
            border: `1px solid ${C.border}`,
            fontSize: '0.86em',
          }}
        >
          {chunk}
        </Box>,
      )
      return
    }
    chunk.split(/(\*\*[^*]+\*\*)/g).forEach((part, j) => {
      if (part.startsWith('**') && part.endsWith('**') && part.length > 4) {
        out.push(<b key={`${keyBase}-b${i}-${j}`}>{part.slice(2, -2)}</b>)
      } else if (part) {
        out.push(<span key={`${keyBase}-t${i}-${j}`}>{part}</span>)
      }
    })
  })
  return out
}

const isTableRow = (l: string) => l.trim().startsWith('|')
/** A `|---|---|` separator carries no content and must not become a row. */
const isTableSep = (l: string) => /^\s*\|[\s|:-]+\|\s*$/.test(l)
const cells = (l: string) =>
  l
    .trim()
    .replace(/^\|/, '')
    .replace(/\|$/, '')
    .split('|')
    .map((c) => c.trim())

export function ProbeMarkdown({ text }: { text: string }) {
  const lines = text.replace(/\r\n/g, '\n').split('\n')
  const blocks: React.ReactNode[] = []
  let para: string[] = []
  let bullets: string[] = []

  const flushPara = () => {
    if (!para.length) return
    const key = `p${blocks.length}`
    blocks.push(
      <Typography key={key} variant="body2" sx={{ mb: 1.25, lineHeight: 1.65 }}>
        {inline(para.join(' '), key)}
      </Typography>,
    )
    para = []
  }
  const flushBullets = () => {
    if (!bullets.length) return
    const key = `u${blocks.length}`
    blocks.push(
      <Box key={key} component="ul" sx={{ pl: 3, m: 0, mb: 1.25 }}>
        {bullets.map((b, i) => (
          <Typography key={i} component="li" variant="body2" sx={{ lineHeight: 1.65, mb: 0.4 }}>
            {inline(b, `${key}-${i}`)}
          </Typography>
        ))}
      </Box>,
    )
    bullets = []
  }
  const flushAll = () => {
    flushPara()
    flushBullets()
  }

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]
    const t = line.trim()

    if (t === '') {
      flushAll()
      continue
    }
    if (t.startsWith('#')) {
      flushAll()
      const level = t.match(/^#+/)?.[0].length ?? 2
      const key = `h${blocks.length}`
      blocks.push(
        <Typography
          key={key}
          variant={level <= 2 ? 'subtitle2' : 'body2'}
          sx={{ mt: blocks.length ? 2 : 0, mb: 0.75, fontWeight: 700, color: C.text }}
        >
          {inline(t.replace(/^#+\s*/, ''), key)}
        </Typography>,
      )
      continue
    }
    if (t.startsWith('- ') || t.startsWith('* ')) {
      flushPara()
      bullets.push(t.slice(2))
      continue
    }
    if (isTableRow(t)) {
      flushAll()
      const rows: string[] = []
      while (i < lines.length && isTableRow(lines[i])) {
        if (!isTableSep(lines[i])) rows.push(lines[i])
        i++
      }
      i-- // the loop's i++ will step past the terminator
      if (rows.length) {
        const head = cells(rows[0])
        const key = `tb${blocks.length}`
        blocks.push(
          <Box key={key} sx={{ overflowX: 'auto', mb: 1.5 }}>
            <Table size="small">
              <TableHead>
                <TableRow>
                  {head.map((h, j) => (
                    <TableCell key={j} sx={{ fontWeight: 700 }}>
                      {inline(h, `${key}-h${j}`)}
                    </TableCell>
                  ))}
                </TableRow>
              </TableHead>
              <TableBody>
                {rows.slice(1).map((r, ri) => (
                  <TableRow key={ri}>
                    {cells(r).map((cv, ci) => (
                      <TableCell key={ci}>{inline(cv, `${key}-${ri}-${ci}`)}</TableCell>
                    ))}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Box>,
        )
      }
      continue
    }
    para.push(t)
  }
  flushAll()

  return <Box>{blocks}</Box>
}
