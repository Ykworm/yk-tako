import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, type Page } from '@playwright/test'

// 登录助手（doc/48 A0）。
//
// 凭据从 yk-tako-eino/.env 读，**不写进仓库**、不打印到日志。
// .env 已被 .gitignore 忽略；这份文件只读它、不含任何密钥。
const here = dirname(fileURLToPath(import.meta.url))
const envPath = resolve(here, '../../.env')

/** vite 的地址（页面与 API 代理都走它）。playwright 的 baseURL 也用它。 */
export const BASE = process.env.TAKO_WEB_BASE ?? 'http://127.0.0.1:5173'

export function devAccount(): { email: string; password: string } {
  const text = readFileSync(envPath, 'utf8')
  const get = (key: string): string => {
    const m = text.match(new RegExp(`^${key}=(.*)$`, 'm'))
    return m ? m[1].trim().replace(/^["']|["']$/g, '') : ''
  }
  const email = get('TAKO_DEV_TEST_EMAIL')
  const password = get('TAKO_DEV_TEST_PASSWORD')
  if (!email || !password) {
    throw new Error(`读不到本地测试账号：${envPath}（缺 TAKO_DEV_TEST_EMAIL / TAKO_DEV_TEST_PASSWORD）`)
  }
  return { email, password }
}

/** 登录并等侧边栏出现（= 登录成功的可观察标志）。 */
export async function login(page: Page): Promise<void> {
  const { email, password } = devAccount()
  await page.goto('/')
  await page.locator('input[type=email]').fill(email)
  await page.locator('input[type=password]').fill(password)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByRole('button', { name: '新对话' })).toBeVisible({ timeout: 15_000 })
}

/**
 * 在 Node 侧直接登录拿 token——给那些「要在页面之外准备数据」的断言用
 * （造项目、造文件）。页面里的登录走 login()，两套并存不冲突。
 */
export async function apiLogin(): Promise<{ token: string; tenantId: string }> {
  const { email, password } = devAccount()
  const res = await fetch(`${BASE}/api/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  })
  const body = await res.json()
  if (body?.code !== 0) {
    throw new Error(`API 登录失败：code=${body?.code} message=${body?.message}`)
  }
  return { token: body.data.accessToken, tenantId: body.data.activeTenant.id }
}
