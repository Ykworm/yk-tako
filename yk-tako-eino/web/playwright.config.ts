import { defineConfig, devices } from '@playwright/test'

// 界面断言的配置（doc/48 A0）。
//
// baseURL 指向 vite dev server（默认 5173）。引擎在 8901，由 vite 代理过去；
// 本套断言只驱动浏览器，不直接发 HTTP——要断 HTTP 字节的用例走 scripts/acceptance/。
//
// webServer：**默认不起**（复用已经在跑的 ./scripts/dev.sh）。CI 里没有 vite 时，
// 设 TAKO_E2E_START_VITE=1 让 playwright 自己拉一个。
const startVite = process.env.TAKO_E2E_START_VITE === '1'

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  expect: { timeout: 5_000 },
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL: process.env.TAKO_WEB_BASE ?? 'http://127.0.0.1:5173',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  ...(startVite
    ? {
        webServer: {
          command: 'npm run dev',
          url: process.env.TAKO_WEB_BASE ?? 'http://127.0.0.1:5173',
          reuseExistingServer: true,
          timeout: 60_000,
        },
      }
    : {}),
})
