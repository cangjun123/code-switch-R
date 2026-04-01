import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@wailsio/runtime': fileURLToPath(new URL('./src/wails-runtime.ts', import.meta.url)),
    },
  },
})
