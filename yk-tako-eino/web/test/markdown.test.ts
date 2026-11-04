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
