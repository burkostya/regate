import { defineConfig } from 'vite';
import { sveltekit } from '@sveltejs/kit/vite';

export default defineConfig({
  plugins: [sveltekit()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: process.env.REGATE_PROXY ?? 'http://127.0.0.1:8080', changeOrigin: false },
      '/mcp': { target: process.env.REGATE_PROXY ?? 'http://127.0.0.1:8080', changeOrigin: false }
    }
  }
});
