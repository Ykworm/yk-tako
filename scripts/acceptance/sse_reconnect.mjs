// SSE 重连自愈的活体验证（配合 scripts/acceptance/sse_reconnect.sh 跑）。
//
// 场景：**页面静止**（没有任何 API 调用 → 没人去刷新那个 access token），
// 而 SSE 连接断了（引擎重启 / 网络抖动 / 代理超时）→ onerror → 重建时用的还是
// 那个**过期**的 token → 服务端一律 401 → 再 onerror → …… 无限重连刷爆引擎日志。
//
// 修好之后：onerror 先自己刷一次 token，再用新 token 重建 → 401 只出现一次。
//
// 引擎用 TAKO_ACCESS_TTL=20s 起（20 秒就过期），断线靠**重启引擎**制造。
import { readFileSync, openSync } from 'node:fs'
import { execSync, spawn } from 'node:child_process'
import { createRequire } from 'node:module'

const REPO0 = process.env.TAKO_REPO ?? '/Users/yuweikang/dev/ykcode/learn/yk-tako'
// playwright 装在 web/ 下（devDependency）。ESM 从本文件位置解析不到它，
// 所以用 createRequire 把解析根指到 web/。
const require = createRequire(`${REPO0}/yk-tako-eino/web/`)
const { chromium } = require('playwright')

const WEB = process.env.TAKO_WEB_BASE ?? 'http://127.0.0.1:5174'
const ENGINE = process.env.TAKO_ENGINE_BASE ?? 'http://127.0.0.1:8944'
const BIN = process.env.TAKO_ENGINE_BIN ?? '/tmp/yktako-acc'
const LOG = process.env.TAKO_SSE_LOG ?? '/tmp/yktako-acc-sse.log'
const REPO = process.env.TAKO_REPO ?? '/Users/yuweikang/dev/ykcode/learn/yk-tako'
const ENV_PATH = process.env.TAKO_ENV ?? `${REPO}/yk-tako-eino/.env`
const PORT = new URL(ENGINE).port

const env = readFileSync(ENV_PATH, 'utf8')
const g = (k) => (env.match(new RegExp(`^${k}=(.*)$`, 'm')) || [, ''])[1].trim().replace(/^["']|["']$/g, '')

const mark = readFileSync(LOG, 'utf8').split('\n').length

/** 重启引擎：断掉 SSE 连接。新进程的日志要落盘（stdio:'ignore' 会把日志丢掉）。 */
function restartEngine() {
  try { execSync(`pkill -f 'engine 127.0.0.1:${PORT}' || true`) } catch { /* 本来就没跑 */ }
  execSync('sleep 1')
  const fd = openSync(LOG, 'a')
  spawn(BIN, [
    '-engine', `127.0.0.1:${PORT}`,
    '-pg', 'host=127.0.0.1 port=5433 dbname=tako sslmode=disable',
    '-redis', 'redis://127.0.0.1:6390/9',
    '-workspace', `${REPO}/tmp/workspace-sse`,
  ], {
    env: {
      ...process.env,
      TAKO_JWT_SECRET: 'dev-only-jwt-secret-change-me-32bytes',
      TAKO_DATA_KEY: 'data-key-aaaaaaaaaaaaaaaaaaaaaaaa32',
      TAKO_ACCESS_TTL: '20s',
    },
    // 同样必须在 yk-tako-eino/ 下跑：引擎从 cwd 找 .env（模型密钥）
    cwd: `${REPO}/yk-tako-eino`,
    detached: true,
    stdio: ['ignore', fd, fd],
  }).unref()
}

const b = await chromium.launch()
const ctx = await b.newContext({ viewport: { width: 1200, height: 800 } })
const page = await ctx.newPage()
await page.goto(WEB)
await page.locator('input[type=email]').fill(g('TAKO_DEV_TEST_EMAIL'))
await page.locator('input[type=password]').fill(g('TAKO_DEV_TEST_PASSWORD'))
await page.getByRole('button', { name: '登录', exact: true }).click()
await page.getByRole('button', { name: '新对话' }).waitFor({ timeout: 20000 })

const tokenOf = () => page.evaluate(() => {
  const k = Object.keys(sessionStorage).find((x) => x.startsWith('tako.configure.token'))
  return k ? (sessionStorage.getItem(k) ?? '') : ''
})
const before = await tokenOf()

// 等 access token（20 秒 TTL）过期——**页面什么都不做**。
await page.waitForTimeout(25000)
// 断开 SSE 连接：重启引擎。
restartEngine()
console.log('已重启引擎（SSE 连接断开，access token 已过期）')
await page.waitForTimeout(22000)

const after = await tokenOf()
await b.close()
await new Promise((r) => setTimeout(r, 2500)) // gin 日志异步写，等它 flush

const lines = readFileSync(LOG, 'utf8').split('\n').slice(mark)
const ev = lines.filter((l) => l.includes('GET      "/api/events'))
const unauthorized = ev.filter((l) => l.includes('| 401 |')).length
const ok = ev.filter((l) => l.includes('| 200 |')).length
const refreshed = lines.filter((l) => l.includes('POST     "/api/auth/refresh')).length

console.log('token 换新了吗:', before !== after ? '是' : '否')
console.log('/api/events 请求数:', ev.length, '| 401:', unauthorized, '| 200:', ok)
console.log('/api/auth/refresh 次数:', refreshed)

if (unauthorized > 2) {
  console.log(`❌ 401 出现 ${unauthorized} 次（>2）→ 重连死循环没修好`)
  process.exit(1)
}
if (ok < 1) {
  console.log('❌ 断线后没有一次 200 → 事件流没能自愈')
  process.exit(1)
}
if (refreshed < 1) {
  console.log('❌ 没有主动刷过 token → onerror 里的刷新逻辑没跑')
  process.exit(1)
}
console.log('✅ 断线后自愈：401 只出现 ' + unauthorized + ' 次，随后重连成功')
