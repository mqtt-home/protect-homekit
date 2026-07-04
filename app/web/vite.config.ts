import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  // Relative asset paths so the build works behind any ingress path.
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    // Local dev: proxy the API to a locally running bridge.
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
})
