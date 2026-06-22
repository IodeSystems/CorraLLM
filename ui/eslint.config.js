import graphql from '@graphql-eslint/eslint-plugin'
import tseslint from 'typescript-eslint'

// Validates the GraphQL operations embedded in our TS (the graphql(`…`) tags)
// against the committed SDL snapshot (gen/schema.graphql). tsc doesn't re-check
// query snippets, so this is the offline enforcement layer. Run via `pnpm lint`.
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
    plugins: { '@typescript-eslint': tseslint.plugin },
    rules: {
      '@typescript-eslint/no-explicit-any': 'error',
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
