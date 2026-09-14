import graphql from '@graphql-eslint/eslint-plugin'
import tseslint from 'typescript-eslint'

// Validates the GraphQL operations embedded in our TS (the graphql(`…`) tags)
// against the committed SDL snapshot (gen/schema.graphql). tsc doesn't re-check
// query snippets, so this is the offline enforcement layer. Run via `pnpm lint`.

// A PANEL SUBTITLE IS A LABEL. Panel.tsx has the argument; this is the half of
// it a machine can check.
//
// The header bar is one line and clips with an ellipsis, which is right for a
// label and wrong for a sentence: the subject is at the front and what happens
// is at the back, so the cut takes the consequence and leaves something that
// still reads as finished. It cost a reader the clause that said whether
// pressing Change would cut off a request in flight, and he did not press it.
//
// A convention did not hold — 6906b46 moved one such sentence out and the class
// came back on two more screens — because nothing failed when it was broken.
// Now something does. Two sentences in a subtitle is the signal, because prose
// is what arrives here and prose has sentence breaks; a single-clause label
// does not. Whether a ONE-sentence subtitle names a consequence is a judgement
// no regex makes, and stays with the author and the reviewer.
const sentenceBreak = /[.!?]["'’”)\]]?\s+["'‘“(\[]?[\p{L}\p{N}]/u

// Every string an attribute value can be built out of, unwrapped: the ternaries
// and template literals a subtitle is usually assembled from. Template quasis
// are tested one at a time, so an interpolation never reads as a break.
function stringsIn(node, out) {
  if (!node) return out
  switch (node.type) {
    case 'Literal':
      if (typeof node.value === 'string') out.push(node.value)
      break
    case 'TemplateLiteral':
      for (const q of node.quasis) out.push(q.value.cooked ?? '')
      break
    case 'JSXExpressionContainer':
      stringsIn(node.expression, out)
      break
    case 'ConditionalExpression':
      stringsIn(node.consequent, out)
      stringsIn(node.alternate, out)
      break
    case 'LogicalExpression':
    case 'BinaryExpression':
      stringsIn(node.left, out)
      stringsIn(node.right, out)
      break
    case 'TSAsExpression':
      stringsIn(node.expression, out)
      break
  }
  return out
}

const corrallm = {
  rules: {
    'subtitle-is-a-label': {
      meta: {
        type: 'problem',
        docs: { description: 'Panel subtitles are labels; prose goes in note=' },
        schema: [],
        messages: {
          prose:
            'This subtitle is more than one sentence, and the header bar clips with an ellipsis — the tail will be lost at a width you cannot see. Keep one clause here and move the rest to note=. See Panel.tsx.',
        },
      },
      create(context) {
        return {
          JSXAttribute(node) {
            if (node.name?.type !== 'JSXIdentifier' || node.name.name !== 'subtitle') return
            if (!stringsIn(node.value, []).some((s) => sentenceBreak.test(s))) return
            context.report({ node, messageId: 'prose' })
          },
        }
      },
    },
  },
}

export default [
  // Generated/snapshot files aren't linted.
  { ignores: ['dist/**', 'src/gql/**', 'src/routeTree.gen.ts', 'gen/**'] },

  // 1. Extract embedded GraphQL from TS into virtual .graphql documents.
  {
    files: ['src/**/*.{ts,tsx}'],
    languageOptions: {
      parser: tseslint.parser,
      parserOptions: { ecmaFeatures: { jsx: true } },
    },
    processor: graphql.processor,
    plugins: { '@typescript-eslint': tseslint.plugin, corrallm },
    rules: {
      '@typescript-eslint/no-explicit-any': 'error',
      'corrallm/subtitle-is-a-label': 'error',
    },
  },

  // 2. Lint those documents against the schema snapshot.
  {
    files: ['**/*.graphql'],
    plugins: { '@graphql-eslint': graphql },
    languageOptions: {
      parser: graphql.parser,
      parserOptions: {
        graphQLConfig: {
          schema: 'gen/schema.graphql',
          documents: ['src/**/*.{ts,tsx}'],
        },
      },
    },
    rules: {
      '@graphql-eslint/fields-on-correct-type': 'error',
      '@graphql-eslint/known-argument-names': 'error',
      '@graphql-eslint/known-type-names': 'error',
      '@graphql-eslint/known-directives': 'error',
      '@graphql-eslint/known-fragment-names': 'error',
      '@graphql-eslint/fragments-on-composite-type': 'error',
      '@graphql-eslint/executable-definitions': 'error',
      '@graphql-eslint/no-unused-fragments': 'error',
      '@graphql-eslint/possible-fragment-spread': 'error',
      '@graphql-eslint/provided-required-arguments': 'error',
      '@graphql-eslint/scalar-leafs': 'error',
      '@graphql-eslint/unique-argument-names': 'error',
      '@graphql-eslint/value-literals-of-correct-type': 'error',
      '@graphql-eslint/variables-are-input-types': 'error',
      '@graphql-eslint/variables-in-allowed-position': 'error',
    },
  },
]
