import type { CodegenConfig } from '@graphql-codegen/cli'

// Reads the committed SDL snapshot so codegen + lint validate offline — no running
// server. Output lands in src/gql/ (gitignored).
const config: CodegenConfig = {
  overwrite: true,
  schema: 'gen/schema.graphql',
  documents: ['src/**/*.{ts,tsx}'],
  generates: {
    './src/gql/': {
      preset: 'client',
      config: {
        useTypeImports: true,
        // gat emits Long (int64 ids) as a JSON string — uniform always-string id contract.
        scalars: { Long: { input: 'string', output: 'string' } },
        // Emit GraphQL enums as string-literal unions so plain strings are assignable.
        enumsAsTypes: true,
      },
    },
  },
  ignoreNoDocuments: true,
}

export default config
