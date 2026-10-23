<script setup lang="ts">
// PlanDoc 把方案文本当文档读（对齐 kimi 的 plan box）：标题/列表/引用/代码块，
// 行内粗体与代码。纯前端轻量解析，不引入依赖；渲染只用插值（自动转义），不拼 HTML。
type DocBlock =
  | { kind: 'h'; level: number; text: string }
  | { kind: 'p'; text: string }
  | { kind: 'li'; text: string; no?: number }
  | { kind: 'quote'; text: string }
  | { kind: 'code'; text: string }
  | { kind: 'hr' }
  | { kind: 'table'; head: string[]; rows: string[][] }

type InlineToken = { text: string; strong?: boolean; code?: boolean }

defineProps<{ text: string }>()

function tableCells(line: string): string[] {
  const t = line.trim().replace(/^\|/, '').replace(/\|$/, '')
  return t.split('|').map((c) => c.trim())
}

function isTableRule(cells: string[]): boolean {
  return cells.length > 0 && cells.every((c) => /^:?-{2,}:?$/.test(c))
}

function docBlocks(src: string): DocBlock[] {
  const out: DocBlock[] = []
  const lines = src.split('\n')
  let fenced = false
  let code: string[] = []
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].replace(/\r$/, '')
    if (line.trim().startsWith('```')) {
      if (fenced) {
        out.push({ kind: 'code', text: code.join('\n') })
        code = []
        fenced = false
      } else {
        fenced = true
      }
      continue
    }
    if (fenced) {
      code.push(line)
      continue
    }
    const t = line.trim()
    if (!t) continue
    // 表格：连续 | … | 行，第二行是分隔行（|---|）才认，否则按普通段落。
    if (t.startsWith('|')) {
      const head = tableCells(t)
      const rule = i + 1 < lines.length ? tableCells(lines[i + 1]) : []
      if (rule.length === head.length && isTableRule(rule)) {
        const rows: string[][] = []
        let j = i + 2
        for (; j < lines.length; j++) {
          const row = lines[j].trim()
          if (!row.startsWith('|')) break
          const cells = tableCells(row)
          if (cells.some((c) => c !== '')) rows.push(cells)
        }
        out.push({ kind: 'table', head, rows })
        i = j - 1
        continue
      }
    }
    const h = /^(#{1,4})\s+(.*)$/.exec(t)
    if (h) {
      out.push({ kind: 'h', level: h[1].length, text: h[2] })
      continue
    }
    if (/^(-{3,}|\*{3,})$/.test(t)) {
      out.push({ kind: 'hr' })
      continue
    }
    const ul = /^[-*+]\s+(.*)$/.exec(t)
    if (ul) {
      out.push({ kind: 'li', text: ul[1] })
      continue
    }
    const ol = /^(\d+)[.)]\s+(.*)$/.exec(t)
    if (ol) {
      out.push({ kind: 'li', text: ol[2], no: Number(ol[1]) })
      continue
    }
    const quote = /^>\s?(.*)$/.exec(t)
    if (quote) {
      out.push({ kind: 'quote', text: quote[1] })
      continue
    }
    out.push({ kind: 'p', text: t })
  }
  if (fenced && code.length) out.push({ kind: 'code', text: code.join('\n') })
  return out
}

function inlineTokens(text: string): InlineToken[] {
  const out: InlineToken[] = []
  const re = /(\*\*[^*]+\*\*|`[^`]+`)/g
  let last = 0
  for (const m of text.matchAll(re)) {
    const at = m.index ?? 0
    if (at > last) out.push({ text: text.slice(last, at) })
    const token = m[0]
    if (token.startsWith('**')) out.push({ text: token.slice(2, -2), strong: true })
    else out.push({ text: token.slice(1, -1), code: true })
    last = at + token.length
  }
  if (last < text.length) out.push({ text: text.slice(last) })
  return out
}
</script>

<template>
  <div class="plan-doc">
    <template v-for="(blk, i) in docBlocks(text)" :key="i">
      <p v-if="blk.kind === 'h'" :class="['doc-h', `h${blk.level}`]">
        <template v-for="(tok, ti) in inlineTokens(blk.text)" :key="ti">
          <strong v-if="tok.strong">{{ tok.text }}</strong>
          <code v-else-if="tok.code">{{ tok.text }}</code>
          <template v-else>{{ tok.text }}</template>
        </template>
      </p>
      <p v-else-if="blk.kind === 'p'" class="doc-p">
        <template v-for="(tok, ti) in inlineTokens(blk.text)" :key="ti">
          <strong v-if="tok.strong">{{ tok.text }}</strong>
          <code v-else-if="tok.code">{{ tok.text }}</code>
          <template v-else>{{ tok.text }}</template>
        </template>
      </p>
      <p v-else-if="blk.kind === 'li'" class="doc-li">
        <span class="doc-mark">{{ blk.no != null ? `${blk.no}.` : '•' }}</span>
        <span>
          <template v-for="(tok, ti) in inlineTokens(blk.text)" :key="ti">
            <strong v-if="tok.strong">{{ tok.text }}</strong>
            <code v-else-if="tok.code">{{ tok.text }}</code>
            <template v-else>{{ tok.text }}</template>
          </template>
        </span>
      </p>
      <blockquote v-else-if="blk.kind === 'quote'" class="doc-quote">
        <template v-for="(tok, ti) in inlineTokens(blk.text)" :key="ti">
          <strong v-if="tok.strong">{{ tok.text }}</strong>
          <code v-else-if="tok.code">{{ tok.text }}</code>
          <template v-else>{{ tok.text }}</template>
        </template>
      </blockquote>
      <table v-else-if="blk.kind === 'table'" class="doc-table">
        <thead>
          <tr>
            <th v-for="(cell, ci) in blk.head" :key="ci">
              <template v-for="(tok, ti) in inlineTokens(cell)" :key="ti">
                <strong v-if="tok.strong">{{ tok.text }}</strong>
                <code v-else-if="tok.code">{{ tok.text }}</code>
                <template v-else>{{ tok.text }}</template>
              </template>
            </th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, ri) in blk.rows" :key="ri">
            <td v-for="(cell, ci) in row" :key="ci">
              <template v-for="(tok, ti) in inlineTokens(cell)" :key="ti">
                <strong v-if="tok.strong">{{ tok.text }}</strong>
                <code v-else-if="tok.code">{{ tok.text }}</code>
                <template v-else>{{ tok.text }}</template>
              </template>
            </td>
          </tr>
        </tbody>
      </table>
      <pre v-else-if="blk.kind === 'code'" class="doc-code">{{ blk.text }}</pre>
      <hr v-else-if="blk.kind === 'hr'" class="doc-hr" />
    </template>
  </div>
</template>

<style scoped>
.plan-doc {
  display: flex;
  flex-direction: column;
  gap: 0.32rem;
  max-height: 24rem;
  overflow-y: auto;
  padding: var(--space-1) var(--space-2) var(--space-1) 0;
  font-size: var(--text-base);
  line-height: 1.62;
  scrollbar-width: thin;
}
.plan-doc p,
.plan-doc blockquote,
.plan-doc pre {
  margin: 0;
}
.doc-h {
  margin-top: var(--space-2);
  font-weight: 600;
  line-height: 1.4;
}
.doc-h.h1 { font-size: var(--text-lg); }
.doc-h.h2 { font-size: var(--text-base); }
.doc-h.h3,
.doc-h.h4 { font-size: var(--text-sm); }
.doc-p { white-space: pre-wrap; }
.doc-li {
  display: flex;
  gap: var(--space-2);
  align-items: baseline;
}
.doc-mark {
  flex: none;
  min-width: 1rem;
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.doc-quote {
  padding: var(--space-1) 0 var(--space-1) var(--space-3);
  border-left: 2px solid var(--line-strong);
  color: var(--ink-soft);
}
.doc-code {
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--line);
  border-radius: var(--radius-control);
  background: var(--bg);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--text-xs);
  line-height: 1.55;
  white-space: pre-wrap;
  overflow-x: auto;
}
.doc-hr {
  border: 0;
  border-top: 1px solid var(--line);
}
.doc-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-xs);
  line-height: 1.5;
}
.doc-table th,
.doc-table td {
  border: 1px solid var(--line);
  padding: 6px var(--space-2);
  text-align: left;
  vertical-align: top;
}
.doc-table th {
  background: var(--bg);
  font-weight: 600;
}
.plan-doc code {
  padding: 0.05rem 0.25rem;
  border-radius: 4px;
  background: var(--hover);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 0.92em;
}
</style>
