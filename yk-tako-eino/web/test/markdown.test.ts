// markdown 渲染的机器断言（doc/48 A1）。
//
// 跑法（Node 22 内置 test runner，零新依赖）：
//
//   cd yk-tako-eino/web && node --experimental-strip-types --test test/markdown.test.ts
//
// **只测 renderMarkdown（纯函数、不碰 DOM）**。消毒那一步（DOMPurify）要 DOM，
// 它的断言在 playwright 里（web/e2e/markdown.spec.ts）——浏览器才是它的真实运行环境。
// 所以这里**不**断言 <script> 被转义：marked 本身不转义原始 HTML（那正是必须配
// DOMPurify 的原因），在这一层断言它反而会写出假的期望。
//
// 同理，renderSafe 在 Node 里调不动（DOMPurify 没有 DOM 就不是函数），
// 文件预览那条管线（renderSafe + breaks: false）的断言在 web/e2e/md-preview.spec.ts。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { renderMarkdown } from '../src/markdown.ts'

test('标题渲染成对应层级的 h', () => {
  assert.match(renderMarkdown('## 标题'), /<h2>标题<\/h2>/)
  assert.match(renderMarkdown('# 一级'), /<h1>一级<\/h1>/)
})

test('无序列表渲染成 ul/li', () => {
  const html = renderMarkdown('- a\n- b')
  assert.match(html, /<ul>/)
  assert.match(html, /<li>a<\/li>/)
  assert.match(html, /<li>b<\/li>/)
})

test('GFM 表格渲染成 table/th/td', () => {
  const html = renderMarkdown('| a | b |\n| --- | --- |\n| 1 | 2 |')
  assert.match(html, /<table>/)
  assert.match(html, /<th>a<\/th>/)
  assert.match(html, /<td>1<\/td>/)
})

// 窄屏宽表要能横向滚：表格外面必须有滚动壳，不能靠气泡自己撑宽。
test('表格套滚动壳', () => {
  const html = renderMarkdown('| a | b |\n| --- | --- |\n| 1 | 2 |')
  assert.match(html, /<div class="table-scroll"><table>/)
  assert.match(html, /<\/table>\s*<\/div>/)
})

test('围栏代码块带 language 类', () => {
  const html = renderMarkdown('```text\nx\n```')
  assert.match(html, /<pre><code class="language-text">/)
  assert.match(html, /x/)
})

test('行内代码与粗体', () => {
  assert.match(renderMarkdown('**粗**'), /<strong>粗<\/strong>/)
  assert.match(renderMarkdown('`code`'), /<code>code<\/code>/)
})

test('有序列表渲染成 ol', () => {
  const html = renderMarkdown('1. 第一\n2. 第二')
  assert.match(html, /<ol>/)
  assert.match(html, /<li>第一<\/li>/)
})

test('blockquote 与水平线', () => {
  assert.match(renderMarkdown('> 引文'), /<blockquote>/)
  assert.match(renderMarkdown('---'), /<hr/)
})

// doc/49 A10：裸链接止于全角标点，标点与后文留在链接外（不然 href 里会被编码成 %EF%BC%8C…）。
test('裸链接遇到中文标点就收尾', () => {
  const html = renderMarkdown('看这个 https://example.com/a，后面呢')
  assert.match(html, /<a href="https:\/\/example\.com\/a">/)
  assert.ok(!html.includes('href="https://example.com/a%EF%BC%8C'), '逗号不该被编码进 href')
  assert.match(html, /<\/a>，后面呢/)
})

test('裸链接不吞后文的全角句号和右括号', () => {
  const html = renderMarkdown('见 (https://example.com/x)。')
  assert.match(html, /<a href="https:\/\/example\.com\/x">/)
  assert.match(html, /<\/a>\)。/)
})

test('显式链接的地址原样尊重', () => {
  const html = renderMarkdown('[文字](https://example.com/a)')
  assert.match(html, /<a href="https:\/\/example\.com\/a">文字<\/a>/)
})

// —— doc/79：文件预览要按 markdown 渲染（md 文件默认走 breaks: false）——
//
// 下面的期望字节是**改动前实跑抄下来的**（不是手写的散文）：不传 opts 时输出必须逐字不变，
// 这是聊天气泡的回归网——气泡的换行规则（breaks: true）不能被文件预览改掉。
test('回归网：不传 opts 时输出与改动前逐字相同（标题）', () => {
  assert.equal(renderMarkdown('## 标题'), '<h2>标题</h2>\n')
})

test('回归网：不传 opts 时输出与改动前逐字相同（表格）', () => {
  assert.equal(
    renderMarkdown('| a | b |\n| --- | --- |\n| 1 | 2 |'),
    '<div class="table-scroll"><table>\n<thead>\n<tr>\n<th>a</th>\n<th>b</th>\n</tr>\n</thead>\n<tbody><tr>\n<td>1</td>\n<td>2</td>\n</tr>\n</tbody></table>\n</div>',
  )
})

test('回归网：不传 opts 时输出与改动前逐字相同（代码块）', () => {
  assert.equal(renderMarkdown('```text\nx\n```'), '<pre><code class="language-text">x\n</code></pre>\n')
})

// 换行规则分家：聊天气泡 breaks: true（模型常把一句拆多行），文件预览 breaks: false
//（文件里的换行是作者排版的结果，标准 markdown 语义下单换行不等于换行）。
test('breaks: false 时单个换行不产生 <br>', () => {
  const html = renderMarkdown('第一行\n第二行', { breaks: false })
  assert.ok(!html.includes('<br'), '不该出现 <br>：' + html)
  assert.match(html, /第一行\n第二行/)
})

test('默认（breaks: true）时单个换行产生 <br>', () => {
  const html = renderMarkdown('第一行\n第二行')
  assert.match(html, /<br>/)
})

// per-call 覆盖不许污染全局：传过 { breaks: false } 之后，默认渲染仍要是 breaks: true。
// 污染了的话，聊天气泡的换行规则会被文件预览悄悄改掉。
test('per-call 的 breaks: false 不污染全局', () => {
  const fileHtml = renderMarkdown('第一行\n第二行', { breaks: false })
  assert.ok(!fileHtml.includes('<br'), '文件预览不该有 <br>')
  const bubbleHtml = renderMarkdown('第一行\n第二行')
  assert.match(bubbleHtml, /<br>/, '全局的 breaks: true 必须还在')
})

// 表格滚动壳是全局注册的渲染器产物（markdown.ts 里 marked.use 注册的），
// per-call 覆盖不该把它顶掉——窄屏宽表要能横向滚。
test('表格滚动壳仍在（breaks: false 也一样）', () => {
  const html = renderMarkdown('| a | b |\n| --- | --- |\n| 1 | 2 |', { breaks: false })
  assert.match(html, /<div class="table-scroll"><table>/)
  assert.match(html, /<\/table>\s*<\/div>/)
})
