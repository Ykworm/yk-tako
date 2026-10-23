import { test, expect } from '@playwright/test'
import { mkdir, writeFile, readFile, rm } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { apiLogin, BASE, login } from './helpers'

const here = dirname(fileURLToPath(import.meta.url))

// 开发引擎的 workspace 根：项目目录在 <root>/tenants/<tid>/workspaces/<wid>/（scope.go）。
// 产出文件在**文件系统**上——要造出「目录里有文件」这个前提，只能直接写盘。
const WS_ROOT = process.env.TAKO_WS_ROOT ?? resolve(here, '../../../tmp/workspace')

// 项目文件区（doc/48 B1-2）：列目录 + 钻取 + 下载。
//
// 这一条钉的是「用户真的能把 agent 写出的文件拿走」：
//   列表里的文件名 = 磁盘上的文件名（页面上看得见）
//   下载下来的字节 = 磁盘上的字节（逐字相同，不是重新生成的）

test('项目页能列出项目目录里的文件，并能把它下载走', async ({ page }) => {
  const { token, tenantId } = await apiLogin()
  const name = `e2e-files-${Date.now()}`

  // 1) 建项目
  const created = await fetch(`${BASE}/api/workspaces`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
    body: JSON.stringify({ name }),
  }).then((r) => r.json())
  const wid = created.data.workspace.id

  // 2) 往项目目录里造文件（正文 + 一个子目录）
  const dir = resolve(WS_ROOT, 'tenants', tenantId, 'workspaces', wid)
  await mkdir(resolve(dir, 'sub'), { recursive: true })
  await writeFile(resolve(dir, 'hello.txt'), 'Hello, world!\n')
  await writeFile(resolve(dir, 'sub', 'nested.txt'), 'nested\n')

  try {
    // 3) 打开项目页 → 展开这个项目的「文件」
    await login(page)
    await page.getByRole('button', { name: '项目', exact: true }).click()
    const card = page.locator('.project').filter({ hasText: name })
    await expect(card).toBeVisible({ timeout: 15_000 })
    await card.getByRole('button', { name: '文件', exact: true }).click()

    // 4) 根目录里能看见那个文件；子目录里的文件**不在**根列表（这是「根目录」的语义）
    await expect(card.getByText('hello.txt')).toBeVisible({ timeout: 15_000 })
    await expect(card.getByText('nested.txt')).toHaveCount(0)

    // 5) 钻进子目录 → 看见子目录里的文件；面包屑能回到根
    await card.getByRole('button', { name: 'sub/' }).click()
    await expect(card.getByText('nested.txt')).toBeVisible({ timeout: 15_000 })
    await card.getByRole('button', { name: '项目根' }).click()
    await expect(card.getByText('hello.txt')).toBeVisible({ timeout: 15_000 })

    // 6) 文本文件能直接看出内容（不用下载再打开）
    await card.getByRole('button', { name: '预览', exact: true }).first().click()
    await expect(card.locator('.file-text')).toContainText('Hello, world!')

    // 7) 下载 → 拿到的字节与磁盘上逐字相同
    const [download] = await Promise.all([
      page.waitForEvent('download'),
      card.getByRole('button', { name: '下载', exact: true }).first().click(),
    ])
    expect(download.suggestedFilename()).toBe('hello.txt')
    const saved = await download.path()
    expect(await readFile(saved!, 'utf8')).toBe('Hello, world!\n')
  } finally {
    // 收尾：把测试项目删掉，别在开发库里留垃圾
    await fetch(`${BASE}/api/workspaces/${wid}`, {
      method: 'DELETE',
      headers: { Authorization: `Bearer ${token}` },
    }).catch(() => undefined)
    await rm(dir, { recursive: true, force: true }).catch(() => undefined)
  }
})
