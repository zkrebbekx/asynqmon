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
        manualChunks(id: string) {
          if (id.includes('node_modules')) {
            if (id.includes('recharts') || id.includes('d3-') || id.includes('victory')) return 'recharts';
            if (/[\\/](react|react-dom|react-router|react-router-dom|react-redux|redux)[\\/]/.test(id)) return 'vendor';
          }
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
