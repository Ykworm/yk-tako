// Vue 3 响应式陷阱的回归网（doc/48 复审发现的 bug）。
//
// 坑长这样：`obj.x ?? (obj.x = {...})` —— **赋值表达式的值是原始对象（raw），不是 reactive proxy**。
// 后面 `row.list.push(x)` / `row.entries = data` 改的是 raw：数据确实变了（读得到），
// 但**没走 proxy 的 set 拦截器 → 依赖没被标脏 → 组件不重新渲染**。
//
// **精确的触发条件（有 await 隔开才会现形）**：
//   1. `obj.x = {...}`      → 标脏，排一次渲染
//   2. `await …`            → **让出执行权，Vue 在这里渲染了一次**（读到的是当时的值）
//   3. `row.list = 新值`    → raw 赋值 → 不标脏 → **不再有第二次渲染** → 界面停在第 2 步的样子
//
// 本项目踩过一次：`ProjectsView.vue` 的 `loadFiles`（文件列表永远空白——请求 200、条目齐全，
// 页面不动）。
//
// 本测试用 `watchEffect` 当「渲染函数」的替身：它每次被标脏就重跑一遍、**读到当时的值**，
// 于是「渲染发生在第几步」这件事变得可观测。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { reactive, watchEffect, nextTick } from 'vue'

type Row = { entries: number[] }

/** 修好的写法：先判空再赋值、再从对象里读出来用（拿到的是 proxy）。 */
async function loadFixed(obj: { files?: Row }, data: number[]) {
  if (!obj.files) {
    obj.files = { entries: [] }
  }
  const row = obj.files // ← 读出来 = proxy ✓
  row.entries = data
  await nextTick()
}

/** 踩坑的写法：`??` 的右操作数是赋值表达式，值是 raw。 */
async function loadBroken(obj: { files?: Row }, data: number[]) {
  const row = obj.files ?? (obj.files = { entries: [] }) // ← 值是 raw ✗
  await nextTick() // ← 渲染在这里发生（读到空）
  row.entries = data // ← raw 赋值 → 不标脏 → 不再渲染
  await nextTick()
}

/** chat.ts 里 `upsertQuestion` 的形态：赋值之后**紧接着** push，中间没有 await。 */
async function upsertBroken(obj: { questions?: { id: string }[] }, q: { id: string }) {
  const list = obj.questions ?? (obj.questions = [])
  list.push(q)
  await nextTick()
}

test('先判空再赋值、再读出来用：await 之后的数据也会触发重新渲染', async () => {
  const obj = reactive<{ files?: Row }>({})
  let rendered = ''
  watchEffect(() => { rendered = JSON.stringify(obj.files?.entries ?? null) })

  await loadFixed(obj, [1, 2])
  assert.equal(rendered, '[1,2]', '界面应看到 await 之后取到的数据')
})

test('对照：`??` 赋值写法在 await 之后就卡住了（这就是那个 bug）', async () => {
  const obj = reactive<{ files?: Row }>({})
  let rendered = ''
  watchEffect(() => { rendered = JSON.stringify(obj.files?.entries ?? null) })

  await loadBroken(obj, [1, 2])
  // 数据确实进去了（读得到），但**没有第二次渲染** → 界面停在 await 那会儿的空列表。
  assert.equal(JSON.stringify(obj.files?.entries), '[1,2]', '数据进得去——所以症状是「界面不动」而不是「数据丢了」')
  assert.equal(rendered, '[]', '这正是 bug：渲染停在 await 之前，界面空白')
})

test('赋值后紧接着 push（中间没有 await）不受影响——chat.ts 的形态本来就能显示', async () => {
  // 记这一条是为了别夸大：`upsertQuestion` 那处**不是** bug（赋值本身标脏，渲染在 push 之后）。
  // 它改成「先判空再赋值、再读出来用」是为了防御将来有人在中间插 await。
  const obj = reactive<{ questions?: { id: string }[] }>({})
  let rendered = ''
  watchEffect(() => { rendered = JSON.stringify(obj.questions?.map((q) => q.id) ?? null) })

  await upsertBroken(obj, { id: 'q1' })
  assert.equal(rendered, '["q1"]', 'push 在渲染之前发生，界面能显示')
})

test('同 id 再来一帧是合并，不是多一条', async () => {
  const obj = reactive<{ questions?: { id: string; status: string }[] }>({})
  const list = obj.questions ?? (obj.questions = [])
  const q = { id: 'q1', status: 'pending' }
  const i = list.findIndex((x) => x.id === q.id)
  if (i < 0) list.push(q)
  else list[i] = { ...list[i], ...q }
  const i2 = list.findIndex((x) => x.id === q.id)
  if (i2 < 0) list.push({ ...q, status: 'answered' })
  else list[i2] = { ...list[i2], status: 'answered' }
  assert.equal(list.length, 1)
  assert.equal(list[0]?.status, 'answered')
})
