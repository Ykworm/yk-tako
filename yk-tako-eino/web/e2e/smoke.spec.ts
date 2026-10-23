import { test, expect } from '@playwright/test'
import { login } from './helpers'

// 冒烟断言（doc/48 A0）：机器断言这条路通不通。
//
// 这一拍只验两件事：① 未登录时登录表单出得来；② 用本地测试账号能登录进去。
// 后面的界面断言（A2/A3/A5/C1…）都建在它上面——登录助手写不对会拦路，所以先钉死。

test('未登录时显示登录表单', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: '登录' })).toBeVisible()
  await expect(page.locator('input[type=email]')).toBeVisible()
  await expect(page.locator('input[type=password]')).toBeVisible()
})

test('用本地测试账号能登录进去', async ({ page }) => {
  await login(page)
  await expect(page.getByRole('button', { name: '新对话' })).toBeVisible()
  // 侧边栏四个页面入口都在
  for (const name of ['对话', '项目', '助手', '团队']) {
    await expect(page.getByRole('button', { name, exact: true })).toBeVisible()
  }
})
