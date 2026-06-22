import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import react from '@vitejs/plugin-react-swc'

// UI (Vite) on 6503; Go API on 6502. Proxy every server surface (REST under
// /api/v1, GraphQL at /api/graphql, schema views under /api/schema).
export default defineConfig({
  // The TanStack Router plugin must run before react; it generates src/routeTree.gen.ts
  // from src/routes/ (file-based routing).
  plugins: [tanstackRouter({ target: 'react' }), react()],
  resolve: {
    // `@/…` → src/…
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: 6503,
    host: true,
    strictPort: true,
    proxy: {
      '/api': { target: 'http://localhost:6502', changeOrigin: true, ws: true },
    },
  },
})
