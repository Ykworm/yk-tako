// /api/auth/refresh 三态判定的机器回归网（doc/81）。
//
// 跑法（Node 22 内置 test runner，零新依赖）：
//
//   cd yk-tako-eino/web && npm test
//
// 病灶回顾：dev 环境重启引擎时，Vite 代理对打不出去的 /api/auth/refresh 写死回 500
//（body 是 text/plain）。旧代码把「拿到任何非 2xx」都当登录失效 → 开着的标签页被无端登出。
// 正解只有「HTTP 401 且响应是引擎信封」才算失效；其余（5xx、代理 500、非信封 401……）都是
// 暂时性故障，保留登录态等下一拍。
//
// auth.ts 模块初始化会读 sessionStorage（`authState = ref(hasSession())`）。Node 下没有
// sessionStorage，所以**先给 globalThis 打桩、再动态 import**，否则模块初始化就 ReferenceError。
// `node --test` 每个测试文件独立进程，桩不会外泄到别的用例。
import { test } from 'node:test'
import assert from 'node:assert/strict'

;(globalThis as { sessionStorage?: unknown }).sessionStorage = {
  getItem: () => null,
  setItem() {},
  removeItem() {},
}
const { readRefreshOutcome } = await import('../src/auth.ts')

test('200 引擎信封 code=0 带 accessToken 判为 ok', () => {
  assert.equal(
    readRefreshOutcome(200, { code: 0, message: '', data: { accessToken: 'a', refreshToken: 'b' } }),
    'ok',
  )
})

test('401 引擎信封 code=111105 判为 expired（真失效，唯一会登出的一态）', () => {
  assert.equal(
    readRefreshOutcome(401, { code: 111105, message: '登录已失效，请重新登录', data: null }),
    'expired',
  )
})

test('500 且 body 解析失败（Vite 代理重启窗口的 text/plain）判为 transient', () => {
  assert.equal(readRefreshOutcome(500, null), 'transient')
})

test('500 引擎内部错误信封判为 transient', () => {
  assert.equal(
    readRefreshOutcome(500, { code: 111408, message: '服务暂时不可用，请再试一次', data: null }),
    'transient',
  )
})

test('400 请求格式不对信封判为 transient（客户端 bug 不该踢人）', () => {
  assert.equal(
    readRefreshOutcome(400, { code: 111101, message: '请求格式不对', data: null }),
    'transient',
  )
})

test('401 但 body 非信封（网关 HTML）判为 transient', () => {
  assert.equal(readRefreshOutcome(401, null), 'transient')
})

test('401 但自称 code=0 的矛盾体保守判为 transient', () => {
  assert.equal(readRefreshOutcome(401, { code: 0, message: '', data: null }), 'transient')
})

test('200 但 body 非信封（被代理到别的服务的 HTML）判为 transient', () => {
  assert.equal(readRefreshOutcome(200, '<!doctype html>'), 'transient')
})
