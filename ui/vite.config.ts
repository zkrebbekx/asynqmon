import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'

export default defineConfig(({ command }) => ({
  // Production asset URLs in index.html go through the Go index template
  // (static.go renders "/[[" ... "]]" actions), so a library embedder's
  // RootPath (e.g. "/monitoring") prefixes every /assets request — the same
  // mechanism the favicon href already uses. With an empty root path the
  // action renders to "" and the URLs stay "/assets/...". Dev server keeps
  // the plain "/" base. Chunk-to-chunk imports are relative and unaffected.
  base: command === 'build' ? '/[[.RootPath]]/' : '/',
  experimental: {
    // URLs referenced from JS (the module-preload helper for lazy chunks and
    // their CSS) can't go through the Go template — only index.html is
    // rendered. Build them at runtime from window.ROOT_PATH, which the
    // index.html inline script sets before any module executes.
    renderBuiltUrl(filename, { hostType }) {
      if (hostType === 'js') {
        return { runtime: `(window.ROOT_PATH||"")+${JSON.stringify('/' + filename)}` };
      }
      return undefined; // html/css: fall back to `base`
    },
  },
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  build: {
    outDir: 'build',
    // recharts is intentionally isolated as its own cacheable vendor chunk.
    chunkSizeWarningLimit: 600,
    rollupOptions: {
      output: {
        // Chunk groups are the rolldown-native form of the old rollup
        // `manualChunks` callback. Vite 8 bundles with rolldown, which
        // IGNORES a `manualChunks` return for a module that another group
        // already claims — the callback could not move Redux Toolkit out of
        // the recharts chunk (issue #50). A group with a higher priority
        // wins, and one name must appear only once: two groups named
        // "vendor" made the second one lose, which dropped react-dom into
        // the recharts chunk and made the entry import all 500 kB of it.
        codeSplitting: {
          groups: [
            {
              // Everything the entry needs on every route. Redux Toolkit and
              // its deps sit here, NOT with recharts: recharts 3 depends on
              // them too, and store.ts makes them entry-reachable.
              name: 'vendor',
              test: /[\\/]node_modules[\\/](@reduxjs|immer|reselect|redux-thunk|use-sync-external-store|react|react-dom|react-router|react-router-dom|react-redux|redux|scheduler|clsx|tailwind-merge|class-variance-authority)[\\/]/,
              priority: 30,
            },
            {
              // recharts is loaded by the lazy Metrics view alone. Keep it in
              // its own cacheable chunk that no eager module imports.
              name: 'recharts',
              test: /[\\/]node_modules[\\/](recharts|d3-|victory)/,
              priority: 20,
            },
          ],
        },
      },
    },
  },
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
}))
