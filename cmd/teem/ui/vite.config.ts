import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api':     { target: 'http://localhost:7777' },
      '/control': { target: 'http://localhost:7777' },
      '/teams':   { target: 'http://localhost:7777' },
    },
  },
});
