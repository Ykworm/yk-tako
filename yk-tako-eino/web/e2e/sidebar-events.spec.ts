import { expect, test, type Page } from '@playwright/test'
import { apiLogin, BASE, login } from './helpers'

// doc/50 A5：侧栏常驻项目名与项目行，改名/删项目后必须自己重拉一次。
// 事件只带 {type,id}（doc/46 §3.1），所以断言的是「不刷新页面就换成新真相」。
// 造的项目用时间戳命名、收尾删掉，不碰开发库里既有的项目。

async function api(
  token: string,
  method: string,
  path: string,
  body?: unknown,
): Promise<Record<string, unknown>> {
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  })
  const json = (await res.json()) as { code: number; message: string; data: Record<string, unknown> }
  if (json.code !== 0) throw new Error(`${method} ${path} 失败：code=${json.code} ${json.message}`)
  return json.data
}

function treeRow(page: Page, name: string) {
  return page.locator('.tree-group').filter({ has: page.locator('.tree-name', { hasText: name }) })
}

test('同一窗口在项目页改名，侧栏当场换名（不靠刷新）', async ({ page }) => {
  const { token } = await apiLogin()
  const stamp = Date.now()
  const name = `A5改名${stamp}`
  const renamed = `A5已改${stamp}`
  const created = await api(token, 'POST', '/api/workspaces', { name })
  const id = (created.workspace as { id: string }).id
  try {
    await login(page)
    await expect(treeRow(page, name)).toBeVisible()

    await page.getByRole('navigation', { name: '页面' }).getByRole('button', { name: '项目', exact: true }).click()
    const card = page.locator('.project').filter({ hasText: name }).first()
    await card.getByRole('button', { name: '重命名' }).click()
    // 点开改名后卡片标题换成表单，原来的名字不再是文本——改用「同时只有一个改名表单」定位。
    const form = page.locator('form.rename')
    await form.locator('input').fill(renamed)
    await form.getByRole('button', { name: '保存' }).click()
    await expect(
      page.locator('.project').filter({ hasText: renamed }).first().locator('.project-title strong'),
    ).toHaveText(renamed)

    // 当事 tab 的 BroadcastChannel 自回声被挡掉，所以这条只可能由服务端事件流兑现。
    await expect(treeRow(page, renamed)).toBeVisible()
    await expect(treeRow(page, name)).toHaveCount(0)
  } finally {
    await api(token, 'DELETE', `/api/workspaces/${id}`)
  }
})

test('另一个窗口改名，本窗口侧栏不刷新就换名', async ({ browser }) => {
  const { token } = await apiLogin()
  const stamp = Date.now()
  const name = `A5跨窗改名${stamp}`
  const renamed = `A5跨窗已改${stamp}`
  const created = await api(token, 'POST', '/api/workspaces', { name })
  const id = (created.workspace as { id: string }).id
  const ctx = await browser.newContext()
  try {
    const a = await ctx.newPage()
    const b = await ctx.newPage()
    await login(a)
    await login(b)
    await expect(treeRow(a, name)).toBeVisible()
    await expect(treeRow(b, name)).toBeVisible()

    await api(token, 'PATCH', `/api/workspaces/${id}`, { name: renamed })

    await expect(treeRow(b, renamed)).toBeVisible()
    await expect(treeRow(a, renamed)).toBeVisible()
    await expect(treeRow(b, name)).toHaveCount(0)
  } finally {
    await api(token, 'DELETE', `/api/workspaces/${id}`)
    await ctx.close()
  }
})

test('别处删项目，侧栏那一行自己消失', async ({ browser }) => {
  const { token } = await apiLogin()
  const name = `A5删除${Date.now()}`
  const created = await api(token, 'POST', '/api/workspaces', { name })
  const id = (created.workspace as { id: string }).id
  const ctx = await browser.newContext()
  try {
    const a = await ctx.newPage()
    const b = await ctx.newPage()
    await login(a)
    await login(b)
    await expect(treeRow(a, name)).toBeVisible()
    await expect(treeRow(b, name)).toBeVisible()

    await api(token, 'DELETE', `/api/workspaces/${id}`)

    await expect(treeRow(b, name)).toHaveCount(0)
    await expect(treeRow(a, name)).toHaveCount(0)
  } finally {
    await ctx.close()
  }
})