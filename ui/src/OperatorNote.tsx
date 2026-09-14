import { useState } from 'react'
import { Box, Button, Typography } from '@mui/material'
import { C } from '@/theme'

/**
 * SOMEBODY ELSE'S WORDS, MARKED AS SOMEBODY ELSE'S.
 *
 * `notes` is a real config field: free text the operator wrote about a provider
 * or an extension, in their own vocabulary, for themselves. It is the one kind
 * of text on these screens the product did not author, and OB-3 does not reach
 * it — rewriting a person's own note, or hiding it, would be the wrong fix and
 * is not this component's job.
 *
 * The defect is that a reader cannot TELL. A run read
 * "P16 free-tier aggregator (plan/p16-free-aggregator.md)…" and
 * "the difference is only that there is no cmd, so no residency and…" sitting
 * in the same muted caption the product's own explanations use, and concluded
 * the page was talking down to him: "That's someone's to-do list, not an answer
 * for me. There's a literal file path sitting on my screen." He was right about
 * the register and wrong about the author, and only the second half is fixable.
 *
 * So the note is ATTRIBUTED, not edited. Three signals, none of them a hover:
 *
 *   - a byline over it, naming the file it came out of. The label is what
 *     settles the question the reader is actually asking — "is this thing
 *     telling me what to do?" — and it costs one faint line per note;
 *   - a rule down the left, the mark a quotation has carried since long before
 *     dashboards, so the block reads as lifted rather than written;
 *   - italic, which is the same signal again for anyone who takes the shape of
 *     a paragraph before its label.
 *
 * WHAT IT DOES NOT DO is shorten it. The clamp is three rendered lines with a
 * visible control that opens the rest in place — NOT one line with the
 * remainder on a tooltip, which is what this replaced (8d7ddb1): hover reaches
 * nobody who does not hover (OB-10), and half a line summarises nothing. Not
 * unclamped either — the notes on this box run past ten lines, and a wall of
 * somebody else's commentary between two rows is how a list stops being a list.
 *
 * A collapsed note can still end mid-word, and this is the one place on these
 * screens where that is allowed: the way to finish the sentence is on screen,
 * and the sentence is not the product saying what a control will do to a
 * request somebody is waiting on.
 */
export function OperatorNote({ text }: { text: string }) {
  const [open, setOpen] = useState(false)
  // Rendered lines are not source lines, so this is not a count of what is
  // hidden — only whether anything can be. Short notes get no control at all.
  const canOverflow = text.includes('\n') || text.length > 160
  const clamped = canOverflow && !open
  return (
    <Box
      sx={{
        mt: 0.75,
        pl: 1.25,
        borderLeft: `2px solid ${C.border}`,
        // A note belongs to its own row, not to the row's chips: full width so
        // it starts a line of its own wherever it is dropped into a flex Stack.
        flexBasis: '100%',
      }}
    >
      <Typography
        variant="overline"
        component="div"
        sx={{ color: C.textFaint, fontSize: 9.5, lineHeight: 1.6, letterSpacing: 0.6 }}
      >
        Note from the config file
      </Typography>
      <Typography
        variant="caption"
        component="div"
        sx={{
          color: C.textMuted,
          fontStyle: 'italic',
          whiteSpace: 'pre-wrap',
          ...(clamped
            ? {
                display: '-webkit-box',
                WebkitLineClamp: 3,
                WebkitBoxOrient: 'vertical',
                overflow: 'hidden',
              }
            : {}),
        }}
      >
        {text}
      </Typography>
      {canOverflow && (
        <Button
          size="small"
          sx={{ px: 0, minWidth: 0 }}
          onClick={(e) => {
            e.stopPropagation() // the row itself opens an editor on some pages
            setOpen(!open)
          }}
        >
          {open ? 'Show less' : 'Show all notes'}
        </Button>
      )}
    </Box>
  )
}
