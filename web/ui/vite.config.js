import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { resolve } from 'node:path'

const backendTarget = process.env.CA2A_BACKEND || 'http://127.0.0.1:7866'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': resolve(__dirname, 'src')
    }
  },
  server: {
    proxy: {
      '/admin/api': {
        target: backendTarget,
        changeOrigin: true
      }
    }
  },
  build: {
    emptyOutDir: true,
    target: 'es2020',
    outDir: resolve(__dirname, '../../internal/server/webui/dist')
  }
})
