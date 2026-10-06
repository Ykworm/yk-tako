import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// dev 期 /api 穿透到引擎。目标可用 TAKO_ENGINE 覆盖（U5 要 -batch-window 3 的引擎才验得出截断）。
const engine = process.env.TAKO_ENGINE ?? 'http://127.0.0.1:8901'

export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      '/api': engine,
    },
  },
})
