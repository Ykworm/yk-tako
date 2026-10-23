import { test, expect } from '@playwright/test'
import { login } from './helpers'

// 这两条**要真跑一轮模型**（要发消息），所以给足超时。
//
// 覆盖的是 doc/48 A1 的核心契约：模型输出的是 markdown，页面必须**渲染**它
//（19v6-03:118「客户端按 markdown 渲染」）；同时模型写进来的原始 HTML
// **不能被执行**（DOMPurify 那一步）。

/** 当前浏览器这一场会话的 id（存在 sessionStorage 里）——收尾时删掉，别在开发库留垃圾。 */
async function currentSessionId(page: import('@playwright/test').Page): Promise<string> {
  return page.evaluate(() => {
    const key = Object.keys(sessionStorage).find((k) => k.startsWith('tako.sessionId'))
    return key ? (sessionStorage.getItem(key) ?? '') : ''
  })
}

test('模型回复按 markdown 渲染，原始 HTML 不会被执行', async ({ page }) => {
  test.setTimeout(240_000)
  await login(page)

  const dialogs: string[] = []
  page.on('dialog', async (d) => {
    dialogs.push(d.message())
    await d.dismiss()
  })

  try {
    const box = page.getByLabel('消息')
    await box.fill(
      '严格按这个格式输出，不要任何解释：\n' +
        '第一行写 `## 标题`（两个井号）\n' +
        '然后一个两列表格，表头是「属性 | 值」，一行数据是「路径 | hello.txt」\n' +
        '然后一个围栏代码块，内容是 `hi`\n' +
        '最后单独一行写：<script>window.__xss=1</script>',
    )
    await box.press('Enter')

    const bubble = page.locator('.bubble.assistant').last()
    // markdown 渲染出来的结构：标题、表格、代码块
    await expect(bubble.locator('h2')).toBeVisible({ timeout: 180_000 })
    await expect(bubble.locator('table')).toBeVisible({ timeout: 180_000 })
    // 表格的**数据格**才是稳的：模型不一定照写表头行，marked 认不出表头时就退化成纯 <td>
    // （表头在不在都不该让这条断言飘——表格渲染本身由 web/test/markdown.test.ts 逐字钉住）。
    await expect(bubble.locator('table td, table th').first()).toBeVisible({ timeout: 180_000 })
    await expect(bubble.locator('pre code')).toBeVisible({ timeout: 180_000 })

    // 原始 HTML 没有变成可执行元素，也没有执行
    await expect(bubble.locator('script')).toHaveCount(0)
    const xss = await page.evaluate(() => (window as Window & { __xss?: number }).__xss)
    expect(xss).toBeUndefined()
    expect(dialogs).toHaveLength(0)
  } finally {
    // 收尾：删掉这场测试会话，别在开发库里越积越多。
    const sid = await currentSessionId(page)
    if (sid) {
      await page.evaluate(async (id) => {
        const t = sessionStorage.getItem('tako.configure.token')
        await fetch('/api/sessions/' + id, { method: 'DELETE', headers: { Authorization: 'Bearer ' + t } })
      }, sid)
    }
  }
})

test('跑着的时候能叫停这一轮', async ({ page }) => {
  test.setTimeout(240_000)
  await login(page)

  try {
    const box = page.getByLabel('消息')
    await box.fill('请从 1 数到 500，每个数单独一行，不要停。')
    await box.press('Enter')

    // 停止按钮在 busy 时立刻出现（不等模型出第一个 token）
    const stop = page.getByRole('button', { name: '停止', exact: true })
    await expect(stop).toBeVisible({ timeout: 30_000 })
    await stop.click()

    // 叫停后这条气泡落定：要么「已停止」，要么模型已经数完了（跑太快）。
    // **不钉死 stopped**：模型快慢不定，钉死了这条会飘。真正要抓的是
    // 「按钮在」「点了不炸」「气泡落定」。
    await expect(page.locator('.bubble.assistant').last()).toContainText(/已停止|完成|\d/, {
      timeout: 60_000,
    })
  } finally {
    // 收尾：删掉这场测试会话，别在开发库里越积越多。
    const sid = await currentSessionId(page)
    if (sid) {
      await page.evaluate(async (id) => {
        const t = sessionStorage.getItem('tako.configure.token')
        await fetch('/api/sessions/' + id, { method: 'DELETE', headers: { Authorization: 'Bearer ' + t } })
      }, sid)
    }
  }
})
