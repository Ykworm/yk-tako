// Markdown 渲染（doc/48 A1）。
//
// 为什么要有这个文件：`doc/19-v6/19v6-03:118` 明写「`text` 里的 `\n` 原样保留，
// **客户端按 markdown 渲染**，排版不受影响」——而页面此前是纯文本插值，模型输出的
// 标题、表格、代码块、列表全变成字面符号（`##`、`|`、```` ``` ````）。
//
// 选型：`marked` + `dompurify`。pi / opencode / WeKnora 三家参考实现都是这个组合
// （doc/48 §2 A1 有出处）；marked 的 `sanitize` 选项早已废弃，官方指引就是配 DOMPurify。
//
// **分层是为了能单测**：renderMarkdown 是纯函数、不碰 DOM → Node 内置 test runner 直接测；
// sanitizeHtml 要 DOM → 只能在浏览器里验（playwright），所以两半的断言分开放。

import { marked } from 'marked'
import DOMPurify from 'dompurify'

marked.setOptions({ gfm: true, breaks: true })

// —— 裸链接的边界（doc/49 A10）——
// marked 的自动链接不认全角标点：模型写「见 https://example.com/a，后面」时，逗号和后面的中文会被
// 一起编码进 href（`a%EF%BC%8C%E5%90%8E%E9%9D%A2`），点开是坏地址、显示也把句子吞进链接。
// 换成「遇到全角标点就停」的内联 url 规则：链接止于标点前，标点与后文留作普通文本。
// 只改**裸链接**；显式 [文字](地址) 是写的人明确指定的，原样尊重。
const CJK_STOP = /[，。、；：！？）》」』】]/

marked.use({
  extensions: [
    {
      name: 'url',
      level: 'inline',
      start(src: string) {
        const i = src.search(/https?:\/\/|www\./)
        return i < 0 ? undefined : i
      },
      tokenizer(src: string) {
        const m = /^(?:https?:\/\/|www\.)[^\s<]*/.exec(src)
        if (!m) return undefined
        let raw = m[0]
        const cut = raw.search(CJK_STOP)
        if (cut >= 0) raw = raw.slice(0, cut)
        // GFM 已处理大部分 ASCII 收尾；这里补上它放过的引号/右括号（「见 (https://x)。」）。
        raw = raw.replace(/[.,;:!?'")”’]+$/, '')
        // 截完只剩协议或只剩 www. 的，交给后面的文本规则，不造空链接。
        if (!/^(?:https?:\/\/|www\.)\S+$/.test(raw)) return undefined
        return {
          type: 'link',
          raw,
          href: raw.startsWith('www.') ? 'http://' + raw : raw,
          title: null,
          text: raw,
          tokens: [{ type: 'text', raw, text: raw }],
        }
      },
    },
  ],
})

/** markdown → HTML 字符串（**未消毒**）。纯函数，不依赖 DOM，可在 Node 里直接测。 */
export function renderMarkdown(text: string): string {
  return marked.parse(text, { async: false }) as string
}

/** HTML → 安全 HTML：剥掉 <script>、事件属性、javascript: URL。需要 DOM。 */
export function sanitizeHtml(html: string): string {
  return DOMPurify.sanitize(html)
}

/** 气泡正文用的完整管线：模型写进来的原始 HTML 被转义、危险属性被剥掉。 */
export function renderSafe(text: string): string {
  return sanitizeHtml(renderMarkdown(text))
}
