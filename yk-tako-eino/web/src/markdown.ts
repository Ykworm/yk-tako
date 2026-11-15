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

import { marked, Renderer } from 'marked'
import DOMPurify from 'dompurify'

marked.setOptions({ gfm: true, breaks: true })

// 宽表在窄屏会顶出气泡：表格自己不能横向滚，右侧列就永远看不到。
// 给每张表套一层滚动壳，宽度仍由表格内容决定（滚动壳在 CSS 里限宽）。
// 注意不能自己 new Renderer 再调用——那份实例没有 parser，单元格会炸；
// 这里借用 marked 正在用的渲染器（this），它已经绑好 parser。
const baseTable = Renderer.prototype.table
marked.use({
  renderer: {
    table(this: Renderer, token) {
      return `<div class="table-scroll">${baseTable.call(this, token)}</div>`
    },
  },
})

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

/**
 * 渲染选项。
 *
 * breaks = 「单个换行要不要折行」：
 * - 聊天气泡用 true（全局默认）：模型常把一句话拆成多行，不折行会挤成一坨。
 * - **文件预览用 false**：文件里的换行是作者排版的结果，标准 markdown 语义下
 *   单个换行**不**等于换行（GitHub 的 README 就是这样渲染的）。
 */
export interface RenderOpts {
  breaks?: boolean
}

/** 共享排版样式的类名。样式在 `markdown.css`（全局），三个入口共用同一份。 */
export const MD_BODY_CLASS = 'md-body'

/**
 * markdown → HTML 字符串（**未消毒**）。纯函数，不依赖 DOM，可在 Node 里直接测。
 *
 * 不传 opts 时行为与改动前**逐字相同**（全局 setOptions 的 gfm:true / breaks:true 照旧生效）。
 * 传 { breaks: false } 只改**这一次**的解析：marked 的 parse(src, opts) 是
 * 「全局默认值 + 本次 opts」合并（`{...this.defaults, ...opts}`），不回写全局。
 * 所以 breaks 只在显式给出时才放进本次 opts——写成 `breaks: undefined` 也会覆盖掉全局值。
 */
export function renderMarkdown(text: string, opts?: RenderOpts): string {
  const perCall: RenderOpts = opts?.breaks === undefined ? {} : { breaks: opts.breaks }
  return marked.parse(text, { async: false, ...perCall }) as string
}

/** HTML → 安全 HTML：剥掉 <script>、事件属性、javascript: URL。需要 DOM。 */
export function sanitizeHtml(html: string): string {
  return DOMPurify.sanitize(html)
}

/**
 * 完整管线：原始 HTML 被转义、危险属性被剥掉。
 *
 * 聊天气泡与**项目里的 md 文件**走同一条管线 = 同一份安全保证：
 * md 文件也是外部内容（agent 写的、用户上传的），同样可能有 <script>、onerror。
 * v-html 只能喂这个函数的输出，不许直接喂 marked.parse 的裸结果。
 */
export function renderSafe(text: string, opts?: RenderOpts): string {
  return sanitizeHtml(renderMarkdown(text, opts))
}
