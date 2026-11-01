import { test, expect, type Page } from '@playwright/test'
import { login } from './helpers'

// 多会话并发的失败基线（台账 tmp/60-多会话并发 A1）。
//
// 这两条**现在就应该红**：前端还是全局单实例 useChat()，切走会话会清空草稿与排队消息。
// 它们是 A3（多实例管理器）的验收锚——修完后转绿，红改绿不是改断言。
//
// 不依赖模型：会话列表/详情/log/帧全部用 page.route 造假。
// 认证仍走真引擎（登录与事件流不拦），只有对话面四条读接口被替身接管。

const A = 's_e2e_a'
const B = 's_e2e_b'
const W = 'w_e2e'
const W2 = 'w_e2e_target'

type MockOptions = { liveRunId?: string; twoWorkspaces?: boolean }

async function mockChatApi(page: Page, opts: MockOptions = {}): Promise<void> {
  const live = opts.liveRunId ?? ''
  const json = (data: unknown) =>
    JSON.stringify({ code: 0, message: '', data })
  const workspaces = [
    { id: W, name: 'E2E 项目', createdAt: '2026-11-01T00:00:00Z', visibility: 'private', owner: true, isDefault: true },
    ...(opts.twoWorkspaces
      ? [{ id: W2, name: 'E2E 目标项目', createdAt: '2026-11-01T00:00:00Z', visibility: 'private', owner: true, isDefault: false }]
      : []),
  ]

  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    const session = (id: string, liveRunId: string) => ({
      liveRunId,
      runlogOffset: 0,
      lastRunStatus: '',
      lastRunId: '',
      workspaceId: W,
    })

    if (path === '/api/sessions') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: json({
          sessions: [
            { id: A, title: 'E2E 会话 A', visibility: 'private', owner: true, createdAt: '2026-11-01T10:00:00Z', workspaceId: W },
            { id: B, title: 'E2E 会话 B', visibility: 'private', owner: true, createdAt: '2026-11-01T09:00:00Z', workspaceId: W },
          ],
        }),
      })
    }
    if (path === '/api/workspaces') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: json({ workspaces }),
      })
    }
    if (path === '/api/runs' && route.request().method() === 'POST') {
      // 不让它打到真引擎：这些用例只关心请求体里的落点。
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: json({ runId: 'r_e2e_new', sessionId: 's_e2e_new' }),
      })
    }
    if (path === `/api/sessions/${A}`) {
      return route.fulfill({ status: 200, contentType: 'application/json', body: json(session(A, live)) })
    }
    if (path === `/api/sessions/${B}`) {
      return route.fulfill({ status: 200, contentType: 'application/json', body: json(session(B, '')) })
    }
    if (path.endsWith('/log')) {
      return route.fulfill({ status: 200, contentType: 'application/json', body: json({ rows: [], nextOffset: 0 }) })
    }
    if (path.endsWith('/subagents')) {
      return route.fulfill({ status: 200, contentType: 'application/json', body: json({ subagents: [] }) })
    }
    if (path.endsWith('/questions')) {
      return route.fulfill({ status: 200, contentType: 'application/json', body: json([]) })
    }
    if (path.endsWith('/frames')) {
      // 一直「生成中」：让轮询循环活着，但不落终态（测试只关心前端状态机）。
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: json({ frames: [], nextOffset: 0, status: 'generating' }),
      })
    }
    // 其余（登录/刷新/租户/事件流）照常打到真引擎。
    return route.continue()
  })
}

function sidebarSession(page: Page, title: string) {
  return page.locator('.recent-item').filter({ hasText: title })
}

test('草稿随会话切换保留', async ({ page }) => {
  await mockChatApi(page)
  await login(page)
  await expect(sidebarSession(page, 'E2E 会话 A')).toBeVisible()

  await sidebarSession(page, 'E2E 会话 A').click()
  const composer = page.getByLabel('消息')
  await expect(composer).toBeVisible()
  const draft = `草稿-${Date.now()}`
  await composer.fill(draft)

  await sidebarSession(page, 'E2E 会话 B').click()
  await sidebarSession(page, 'E2E 会话 A').click()

  // 现状（单实例 openSession 清 draft）在这里红：输入框是空的。
  await expect(composer).toHaveValue(draft)
})

test('排队消息切走再切回仍在', async ({ page }) => {
  // A 假装有活 run：gateIdle 返回 false，投递留在本地队列。
  await mockChatApi(page, { liveRunId: 'r_e2e' })
  await login(page)
  await expect(sidebarSession(page, 'E2E 会话 A')).toBeVisible()

  await sidebarSession(page, 'E2E 会话 A').click()
  const composer = page.getByLabel('消息')
  await expect(composer).toBeVisible()
  await composer.fill('这条要排队')
  await page.getByLabel('发送').click()

  const queue = page.locator('.hint.queue')
  await expect(queue).toHaveText(/排队 1/)

  await sidebarSession(page, 'E2E 会话 B').click()
  await sidebarSession(page, 'E2E 会话 A').click()

  // 现状（openSession 清 pending）在这里红：排队提示没了。
  await expect(queue).toHaveText(/排队 1/)
})

// —— A3b：空对话的项目落点 ——

async function sentWorkspace(page: Page, text: string): Promise<string | undefined> {
  const runs = page.waitForRequest((r) => r.url().includes('/api/runs') && r.method() === 'POST')
  await page.getByLabel('消息').fill(text)
  await page.getByLabel('发送').click()
  const req = await runs
  return (JSON.parse(req.postData() ?? '{}') as { workspaceId?: string }).workspaceId
}

test('空对话能改项目，发送落在所选项目', async ({ page }) => {
  await mockChatApi(page, { twoWorkspaces: true })
  await login(page)

  const picker = page.getByLabel('新对话落在哪个项目')
  await expect(picker).toBeVisible()
  await picker.selectOption(W2)

  expect(await sentWorkspace(page, '落在目标项目')).toBe(W2)
  // 发过消息后会话已绑定项目，选择器收起。
  await expect(picker).toHaveCount(0)
})

test('侧栏项目行的＋直接在该项目开新对话', async ({ page }) => {
  await mockChatApi(page, { twoWorkspaces: true })
  await login(page)
  await expect(sidebarSession(page, 'E2E 会话 A')).toBeVisible()

  const group = page.locator('.tree-group').filter({ hasText: 'E2E 目标项目' })
  await group.getByRole('button', { name: '在 E2E 目标项目 新建对话' }).click()
  expect(await sentWorkspace(page, '从侧栏开的目标项目')).toBe(W2)
})

test('打开已有会话时没有落点选择器', async ({ page }) => {
  await mockChatApi(page)
  await login(page)
  await sidebarSession(page, 'E2E 会话 A').click()
  await expect(page.getByLabel('消息')).toBeVisible()
  await expect(page.getByLabel('新对话落在哪个项目')).toHaveCount(0)
})

// —— A5：侧栏运行中（本地 busy） ——

test('切走后侧栏仍标运行中，收口后消失', async ({ page }) => {
  await mockChatApi(page, { liveRunId: 'r_e2e' })
  await login(page)
  await sidebarSession(page, 'E2E 会话 A').click()
  await expect(sidebarSession(page, 'E2E 会话 A').locator('.recent-run')).toHaveText('运行中')

  await sidebarSession(page, 'E2E 会话 B').click()
  await expect(sidebarSession(page, 'E2E 会话 A').locator('.recent-run')).toBeVisible()

  // 让 A 的轮询收到终态：后台实例自己收口，标记消失（不需要重拉列表）。
  await page.route('**/api/runs/r_e2e/frames**', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        code: 0,
        message: '',
        data: { frames: [{ kind: 'terminal', status: 'complete' }], nextOffset: 1, status: 'done' },
      }),
    }),
  )
  await expect(sidebarSession(page, 'E2E 会话 A').locator('.recent-run')).toHaveCount(0, { timeout: 10_000 })
})

// —— A4：后台实例随删除事件收口 ——

test('后台会话被删后从侧栏消失', async ({ page }) => {
  await mockChatApi(page)
  await login(page)
  await sidebarSession(page, 'E2E 会话 A').click()
  await sidebarSession(page, 'E2E 会话 B').click()

  // 模拟另一个 tab 的广播（同一个 BroadcastChannel 名与事件形状）。
  const tenantId = await page.evaluate(() => sessionStorage.getItem('tako.configure.tenant'))
  await page.evaluate((tid) => {
    const ch = new BroadcastChannel('tako.mutations')
    ch.postMessage({ sourceId: 'e2e-other-tab', tenantId: tid, evt: { type: 'session_deleted', id: 's_e2e_a' } })
    ch.close()
  }, tenantId)

  await expect(sidebarSession(page, 'E2E 会话 A')).toHaveCount(0)
})
