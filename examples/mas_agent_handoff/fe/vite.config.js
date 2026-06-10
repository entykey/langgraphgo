import { defineConfig } from 'vite'

export default defineConfig({
  server: {
    proxy: {
      '/chat':   'http://localhost:8080',
      '/config': 'http://localhost:8080',
    },
  },
  build: {
    outDir: 'dist',
  },
})
