import { test, expect } from '@playwright/test'
import { login } from './helpers'

// 界面断言（doc/48）：这几条**不需要模型**——都是「页面上有没有这个东西/这句话」。
// 跟 e2e/files.spec.ts 一样，断言的是「用户看得见的」，不是内部实现。

test('侧边栏有最近会话，能直接回到旧对话', async ({ page }) => {
  await login(page)
  const recent = page.getByRole('region', { name: '会话' })
  await expect(recent).toBeVisible()
  // 列表里至少有一场（开发库里有历史会话）；标题不能是空
  const first = recent.locator('.recent-item').first()
  await expect(first).toBeVisible({ timeout: 15_000 })
  await expect(first.locator('.recent-title')).not.toHaveText('')
})

test('默认项目有徽标，且不提供删除/重命名', async ({ page }) => {
  await login(page)
  await page.getByRole('button', { name: '项目', exact: true }).click()
  const card = page.locator('.project').filter({ has: page.locator('.badge.default') }).first()
  await expect(card).toBeVisible({ timeout: 15_000 })
  await expect(card.locator('.badge.default')).toHaveText('默认')
  // 默认项目不可删（服务端 409 114005）——按钮不该出现
  await expect(card.getByRole('button', { name: '删除', exact: true })).toHaveCount(0)
  await expect(card.getByRole('button', { name: '重命名', exact: true })).toHaveCount(0)
})

test('没有子代理时，对话页顶部不出现「子代理」条', async ({ page }) => {
  await login(page)
  // 新会话里没有子代理（子代理要模型真的委派才会出现）
  await page.getByRole('button', { name: '新对话', exact: true }).click()
  await expect(page.locator('.subagents')).toHaveCount(0)
})

test('密钥的值框留空：留空 = 不改动（不是删除）', async ({ page }) => {
  await login(page)
  await page.getByRole('button', { name: '团队', exact: true }).click()
  await page.getByRole('tab', { name: '设置', exact: true }).click()
  const valueBox = page.locator('input[aria-label="密钥值"]').first()
  await expect(valueBox).toBeVisible({ timeout: 15_000 })
  // 服务端回的是掩码 '••••'，它**不该**躺在输入框里（doc/48 C2）
  await expect(valueBox).toHaveValue('')
  await expect(valueBox).toHaveAttribute('placeholder', '已保存（留空则不改动）')
  // 删除走独立的按钮 + 确认，不再是「移除」那种无确认的一删了事
  await expect(page.getByRole('button', { name: '删除这把密钥' }).first()).toBeVisible()
})

test('保存一张卡片不会冲掉另一张卡片里未保存的编辑', async ({ page }) => {
  await login(page)
  await page.getByRole('button', { name: '团队', exact: true }).click()
  await page.getByRole('tab', { name: '设置', exact: true }).click()
  await page.waitForTimeout(800)

  // 在「模型」卡片填一个新值（**故意不保存**）
  const modelBox = page.locator('label:has-text("默认模型") input')
  const savedModel = await modelBox.inputValue()
  await modelBox.fill('e2e-probe-unsaved-model')

  // 去「密钥」卡片，**一个密钥都不改**，直接点它的保存
  // （密钥表原样提交：真密钥以 '••••' 带回 = 服务端沿用原值 → 不污染任何数据）
  await page.getByRole('button', { name: '保存密钥', exact: true }).click()
  await page.waitForTimeout(1200)

  // 模型卡片里**没保存**的编辑必须还在——不能被这次「保存密钥」冲掉
  expect(await modelBox.inputValue()).toBe('e2e-probe-unsaved-model')

  // 服务端的模型也不能被顺带改掉（保存密钥 ≠ 保存模型）
  const cfg = await page.evaluate(async () => {
    const t = sessionStorage.getItem('tako.configure.token')
    const r = await fetch('/api/tenants/' + sessionStorage.getItem('tako.configure.tenant') + '/runtime-config', {
      headers: { Authorization: 'Bearer ' + t },
    })
    return (await r.json()).data.config
  })
  expect(cfg.modelRef).toBe(savedModel)
})

test('个人工作区的成员页说人话，并给一条出路', async ({ page }) => {
  await login(page)
  await page.getByRole('button', { name: '团队', exact: true }).click()
  await page.getByRole('tab', { name: '成员', exact: true }).click()
  await expect(page.getByText('这是你一个人的工作区')).toBeVisible({ timeout: 15_000 })
  // 旧文案在 owner 视角下自相矛盾（你就是所有者）
  await expect(page.getByText('个人工作区没有成员')).toHaveCount(0)
  await expect(page.getByRole('button', { name: '+ 创建团队工作区' })).toBeVisible()
})

test('会话列表不再把内部 session id 当主信息', async ({ page }) => {
  await login(page)
  await page.getByRole('button', { name: '项目', exact: true }).click()
  const card = page.locator('.project').first()
  await card.getByRole('button', { name: '查看对话', exact: true }).click()
  const session = card.locator('.session').first()
  await expect(session).toBeVisible({ timeout: 15_000 })
  // id 只在 title 属性里（可查），不作为可见文字
  await expect(session.locator('.meta')).not.toContainText('s_01M5')
})
