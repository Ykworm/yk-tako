import { test, expect } from '@playwright/test'
import { mkdir, writeFile, rm } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { apiLogin, BASE, login } from './helpers'

// md 文件预览（doc/79）：项目里的 .md 在两个入口都按 markdown 渲染显示，默认预览态，可切原文。
//
// 这一组断言**不需要真模型**：造文件用写盘，不发消息——快且不烧 token。
//
// **造样本的顺序有讲究**（施工包 §3.7，curl 实跑确认）：
//   1. POST /api/workspaces 只写库，**不建磁盘目录**；
//   2. 容器目录是第一次被**读**时才建的（ensureProjectLayout）：layout.json 不存在且容器为空
//      → 就地转正；layout.json 不存在但容器已有条目 → 409 117116「这个项目还需要迁移目录后才能使用」；
//   3. 所以必须**先打一次 GET /files**（让服务端把容器转正），**再写盘**。
//      反过来（先写盘再列目录）会拿到 409，页面显示「这个项目还需要迁移目录后才能使用」——
//      症状看着像「文件看不见」，根因却是顺序错。
//
// **写盘路径必须带 content/**（doc/76）：服务端只扫 content/，写容器根的文件它看不见。

const here = dirname(fileURLToPath(import.meta.url))
// 开发引擎的 workspace 根：项目目录在 <root>/tenants/<tid>/workspaces/<wid>/（scope.go）。
// 与 e2e/files.spec.ts:11 同一个值（同一个引擎进程）。
const WS_ROOT = process.env.TAKO_WS_ROOT ?? resolve(here, '../../../tmp/workspace')
// 截图落在施工材料目录（不入库）。
const SHOTS = resolve(here, '../../../tmp/79-md文件预览')

const MD_SAMPLE = [
  '# 设计说明',
  '',
  '## 背景',
  '',
  '本系统需要解决三个问题：',
  '',
  '- 拉取：轮询 → 增量',
  '- 渲染：原文符号 → 排好版的文档',
  '',
  '| 环节 | 现状 | 目标 |',
  '| --- | --- | --- |',
  '| 拉取 | 轮询 | 增量 |',
  '| 渲染 | 符号 | 文档 |',
  '',
  '```text',
  'const answer = 42',
  '```',
  '',
  '行内代码 `code`。',
  '',
  '> 引用一句话。',
  '',
  // 恶意 HTML：md 文件也是外部内容（agent 写的、用户上传的），
  // 与聊天气泡走同一条管线（marked → DOMPurify），这里盯它没被执行。
  '<script>window.__xss=1</script>',
  '<img src=x onerror="window.__xss=1">',
].join('\n')

type Project = { token: string; tenantId: string; wid: string; dir: string }

/** 建项目 → 让容器转正 → 往 content/ 写文件。 */
async function createProjectWithFiles(files: Record<string, string>): Promise<Project> {
  const { token, tenantId } = await apiLogin()
  const name = `e2e-md-${Date.now()}`
  const created = await fetch(`${BASE}/api/workspaces`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
    body: JSON.stringify({ name }),
  }).then((r) => r.json())
  if (created?.code !== 0) throw new Error(`建项目失败：code=${created?.code} ${created?.message}`)
  const wid = created.data.workspace.id
  const dir = resolve(WS_ROOT, 'tenants', tenantId, 'workspaces', wid)

  // ① 先列一次目录：服务端此刻才把容器转正（写 layout.json、建 content/ 与 runtime/）。
  const listed = await fetch(`${BASE}/api/workspaces/${wid}/files?path=.`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!listed.ok) throw new Error(`项目容器转正失败：HTTP ${listed.status}`)

  // ② 再写盘：落在 content/ 下（服务端只认这一棵树）。
  for (const [rel, body] of Object.entries(files)) {
    const full = resolve(dir, 'content', rel)
    await mkdir(dirname(full), { recursive: true })
    await writeFile(full, body)
  }
  return { token, tenantId, wid, dir }
}

/** 收尾：删掉测试项目，别在开发库里留垃圾。 */
async function destroyProject(p: Project): Promise<void> {
  await fetch(`${BASE}/api/workspaces/${p.wid}`, {
    method: 'DELETE',
    headers: { Authorization: `Bearer ${p.token}` },
  }).catch(() => undefined)
  await rm(p.dir, { recursive: true, force: true }).catch(() => undefined)
}

async function shot(page: import('@playwright/test').Page, name: string): Promise<void> {
  await mkdir(SHOTS, { recursive: true })
  await page.screenshot({ path: resolve(SHOTS, name), fullPage: false })
}

test('对话页文件标签：md 按 markdown 渲染，能切回原文，切文件复位', async ({ page }) => {
  const p = await createProjectWithFiles({
    'doc.md': MD_SAMPLE,
    'second.md': '# 第二个文件\n\n这是另一份文档。\n',
    'plain.txt': '普通文本，不是 markdown。\n',
  })
  try {
    await login(page)

    // 这条空对话的落点切到测试项目——右侧工作区跟的是当前项目。
    const landing = page.getByLabel('新对话落在哪个项目')
    await expect(landing).toBeVisible({ timeout: 15_000 })
    await landing.selectOption(p.wid)

    const dock = page.getByTestId('chat-dock')
    await expect(dock).toBeVisible({ timeout: 15_000 })
    await dock.getByRole('button', { name: '项目', exact: true }).click()

    // 目录树里点开 md → 开文件标签
    await dock.getByRole('button', { name: 'doc.md', exact: true }).click()
    const view = dock.locator('.file-view')
    await expect(view).toBeVisible({ timeout: 15_000 })

    // 断言 1：渲染出的是结构（h2/table/pre code），不是 <pre> 里的字面符号
    await expect(view.locator('.file-md h2')).toBeVisible({ timeout: 15_000 })
    await expect(view.locator('.file-md table')).toBeVisible()
    await expect(view.locator('.file-md pre code')).toBeVisible()
    await expect(view.locator('.file-text')).toHaveCount(0)
    await shot(page, 'chat-md-preview.png')

    // 断言 2：md 里的恶意 HTML 没有执行（DOMPurify 那一步）
    expect(await page.evaluate(() => (window as Window & { __xss?: number }).__xss)).toBeUndefined()
    await expect(view.locator('script')).toHaveCount(0)

    // 断言 3：切「原文」看到原始字节，切「预览」回到渲染态
    await view.getByRole('button', { name: '原文', exact: true }).click()
    await expect(view.locator('.file-text')).toContainText('# 设计说明')
    await expect(view.locator('.file-md')).toHaveCount(0)
    await shot(page, 'chat-md-raw.png')
    await view.getByRole('button', { name: '预览', exact: true }).click()
    await expect(view.locator('.file-md h2')).toBeVisible()
    await expect(view.locator('.file-text')).toHaveCount(0)

    // 断言 4：切到另一个文件 → 回到预览态（原文态不复用）
    await dock.getByRole('button', { name: '项目', exact: true }).click()
    await dock.getByRole('button', { name: 'second.md', exact: true }).click()
    await expect(view.locator('.file-md')).toBeVisible({ timeout: 15_000 })
    await expect(view.locator('.file-text')).toHaveCount(0)
  } finally {
    await destroyProject(p)
  }
})

test('项目页文件列表：md 按 markdown 渲染，能切原文；非 md 不给切换控件', async ({ page }) => {
  // long.md 超过项目页 2000 字的预览上限：截断在**渲染之前**做，
  // 所以预览里能看到截断提示（渲染后截 HTML 会拦腰砍断表格与代码块）。
  const longMd = ['# 长文档', '', '| 列 | 值 |', '| --- | --- |']
    .concat(Array.from({ length: 400 }, (_, i) => `| 行 ${i} | ${'x'.repeat(20)} |`))
    .join('\n')
  const p = await createProjectWithFiles({
    'doc.md': MD_SAMPLE,
    'plain.txt': '普通文本，不是 markdown。\n',
    'long.md': longMd,
  })
  try {
    await login(page)
    // 点**左侧主导航**的「项目」进项目页。页面上有两个「项目」按钮（左侧导航 + 右侧工作区的
    // 「项目」标签，doc/78 的 ChatDock），所以这里把范围收窄到导航里——否则 strict mode 歧义。
    await page.getByRole('navigation', { name: '页面' }).getByRole('button', { name: '项目', exact: true }).click()
    const card = page.locator('.project').filter({ hasText: 'e2e-md-' })
    await expect(card).toBeVisible({ timeout: 15_000 })
    await card.getByRole('button', { name: '文件', exact: true }).click()

    const mdRow = card.locator('.file').filter({ hasText: 'doc.md' })
    const txtRow = card.locator('.file').filter({ hasText: 'plain.txt' })
    await expect(mdRow).toBeVisible({ timeout: 15_000 })

    // 断言 6：非 md 文件**不出现**切换控件（只有一种看法，md 才需要选）
    await expect(txtRow.getByRole('button', { name: '原文', exact: true })).toHaveCount(0)

    // 断言 5：md 默认预览态，渲染出结构
    await mdRow.getByRole('button', { name: '预览', exact: true }).click()
    await expect(mdRow.locator('.file-md h2')).toBeVisible({ timeout: 15_000 })
    await expect(mdRow.locator('.file-md table')).toBeVisible()
    await expect(mdRow.locator('.file-text')).toHaveCount(0)
    await shot(page, 'projects-md-preview.png')

    // 断言 6（同一条的另一面）：切换有效——切原文看到原始字节
    await mdRow.getByRole('button', { name: '原文', exact: true }).click()
    await expect(mdRow.locator('.file-text')).toContainText('# 设计说明')
    await expect(mdRow.locator('.file-md')).toHaveCount(0)
    await shot(page, 'projects-md-raw.png')
    await mdRow.getByRole('button', { name: '预览', exact: true }).click()
    await expect(mdRow.locator('.file-md h2')).toBeVisible()

    // 截断在渲染之前：预览里带着截断提示，且是渲染后的样子（不是半张表）
    const longRow = card.locator('.file').filter({ hasText: 'long.md' })
    await longRow.getByRole('button', { name: '预览', exact: true }).click()
    await expect(longRow.locator('.file-md')).toContainText('用「下载」拿全文', { timeout: 15_000 })
    await expect(longRow.locator('.file-md table')).toBeVisible()
  } finally {
    await destroyProject(p)
  }
})
