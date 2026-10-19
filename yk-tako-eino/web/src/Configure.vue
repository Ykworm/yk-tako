<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, type AgentRow, type McpItem, type McpServer, type RuntimeConfig, type ToolPolicy } from './configureApi'
import { authState, loginWithPassword, readTenant, readToken } from './auth'

const props = defineProps<{ page: 'agents' | 'team' }>()

const email = ref('')
const password = ref('')
const showPassword = ref(false)
const token = ref(readToken())
const tenant = ref(readTenant())
const banner = ref('')
const note = ref('')
const busy = ref(false)
const confirming = ref(false)
let noteTimer = 0

function flash(message: string) {
  note.value = message
  window.clearTimeout(noteTimer)
  noteTimer = window.setTimeout(() => {
    if (note.value === message) note.value = ''
  }, 2400)
}

// 登录态是共享的：对话页登录过，这里直接是已登录；这里退出，对话页也退回登录表单。
const loggedIn = computed(() => authState.value)

function show(status: number, message: string, code: number) {
  banner.value = code === 0 ? '' : status + '：' + (message || '请求失败')
}

async function login() {
  banner.value = ''
  busy.value = true
  try {
    const res = await loginWithPassword(email.value, password.value)
    if (!res.ok) {
      banner.value = res.status ? `${res.status}：${res.message}` : res.message
      return
    }
    token.value = readToken()
    tenant.value = readTenant()
    password.value = ''
    await reload()
  } finally {
    busy.value = false
  }
}

const tab = ref<'root' | 'subagent'>('root')
const agents = ref<AgentRow[]>([])
const selected = ref<AgentRow | null>(null)
const namesText = ref('')
const creating = ref(false)

function policyLine(p: ToolPolicy): string {
  const names = (p.names || []).join('、')
  if (p.mode === 'allow') return names ? `只用 ${names}` : '只用列出的工具'
  if (p.mode === 'exclude') return names ? `不用 ${names}` : '排除列出的工具'
  if (p.mode === 'none') return '不用工具'
  return '继承工具'
}

function sourceLabel(source: string): string {
  if (source === 'override') return '已覆盖'
  if (source === 'custom') return '自定义'
  return '默认'
}

async function loadAgents() {
  if (!loggedIn.value) return
  const res = await api<{ agents: AgentRow[] }>('GET', `/api/tenants/${tenant.value.id}/agents?kind=${tab.value}`)
  if (res.body.code !== 0 || !res.body.data) {
    show(res.status, res.body.message, res.body.code)
    return
  }
  banner.value = ''
  agents.value = res.body.data.agents
}

function selectAgent(row: AgentRow) {
  creating.value = false
  confirming.value = false
  selected.value = {
    ...row,
    toolPolicy: { mode: row.toolPolicy.mode || 'inherit', names: [...(row.toolPolicy.names || [])] },
  }
  namesText.value = (row.toolPolicy.names || []).join(', ')
}

function closeEditor() {
  selected.value = null
  creating.value = false
  confirming.value = false
}

function startCustom() {
  creating.value = true
  confirming.value = false
  selected.value = {
    id: '',
    defaultId: '',
    source: 'custom',
    overridden: false,
    name: '',
    description: '',
    spec: '',
    modelRef: '',
    toolPolicy: { mode: 'inherit', names: [] },
    visibility: 'private',
  }
  namesText.value = ''
}

function policyFromForm(): ToolPolicy {
  const mode = selected.value?.toolPolicy.mode || 'inherit'
  const names = namesText.value.split(',').map((s) => s.trim()).filter(Boolean)
  return { mode, names: mode === 'allow' || mode === 'exclude' ? names : [] }
}

async function inferKind(name: string): Promise<string> {
  const other = tab.value === 'root' ? 'subagent' : 'root'
  const res = await api<{ agents: AgentRow[] }>('GET', `/api/tenants/${tenant.value.id}/agents?kind=${other}`)
  if (res.body.code !== 0 || !res.body.data) return tab.value
  return res.body.data.agents.some((a) => a.name === name) ? 'both' : tab.value
}

async function saveAgent() {
  const row = selected.value
  if (!row) return
  busy.value = true
  banner.value = ''
  try {
    const toolPolicy = policyFromForm()
    const fields = {
      name: row.name,
      description: row.description,
      spec: row.spec,
      modelRef: row.modelRef,
      toolPolicy,
      visibility: row.visibility || 'private',
    }
    if (row.source === 'default' || creating.value || !row.id) {
      const kind = row.source === 'default' ? await inferKind(row.name) : tab.value
      const res = await api<{ agent: AgentRow }>('POST', `/api/tenants/${tenant.value.id}/agents`, { ...fields, kind })
      if (res.body.code !== 0) {
        show(res.status, res.body.message, res.body.code)
        return
      }
    } else {
      const res = await api<{ agent: AgentRow }>('PUT', `/api/tenants/${tenant.value.id}/agents/${row.id}`, fields)
      if (res.body.code !== 0) {
        show(res.status, res.body.message, res.body.code)
        return
      }
    }
    creating.value = false
    await loadAgents()
    const fresh = agents.value.find((a) => a.name === row.name)
    if (fresh) selectAgent(fresh)
    flash('已保存')
  } finally {
    busy.value = false
  }
}

async function removeAgent() {
  const row = selected.value
  if (!row?.id || row.source === 'default') return
  busy.value = true
  try {
    const res = await api<null>('DELETE', `/api/tenants/${tenant.value.id}/agents/${row.id}`)
    if (res.body.code !== 0) {
      show(res.status, res.body.message, res.body.code)
      return
    }
    selected.value = null
    confirming.value = false
    await loadAgents()
    flash(row.source === 'override' ? '已重置为默认' : '已删除')
  } finally {
    busy.value = false
  }
}

async function onImport(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  const content = await file.text()
  busy.value = true
  try {
    const res = await api<unknown>('POST', `/api/tenants/${tenant.value.id}/agents/import`, {
      files: [{ name: file.name, content }],
    })
    if (res.body.code !== 0) {
      show(res.status, res.body.message, res.body.code)
      return
    }
    banner.value = ''
    await loadAgents()
    flash('已导入')
  } finally {
    busy.value = false
  }
}

const modelRef = ref('')
const network = ref(false)
const secretRows = ref<{ name: string; value: string; masked: boolean }[]>([])
const originalSecrets = ref<string[]>([])
const mcpRows = ref<{ name: string; command: string; args: string; envText: string }[]>([])
const systemMcp = ref<McpItem[]>([])
const customMcp = ref<McpItem[]>([])

function envText(env: Record<string, string> | undefined): string {
  return Object.entries(env || {}).map(([k, v]) => k + '=' + v).join('\n')
}

function parseEnv(text: string): Record<string, string> | null {
  const out: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed) continue
    const i = trimmed.indexOf('=')
    if (i <= 0) return null
    out[trimmed.slice(0, i)] = trimmed.slice(i + 1)
  }
  return out
}

async function loadConfig() {
  if (!loggedIn.value) return
  const res = await api<{ config: RuntimeConfig }>('GET', `/api/tenants/${tenant.value.id}/runtime-config`)
  if (res.body.code !== 0 || !res.body.data) {
    show(res.status, res.body.message, res.body.code)
    return
  }
  const cfg = res.body.data.config
  modelRef.value = cfg.modelRef || ''
  network.value = !!cfg.sandbox?.network
  const secrets = cfg.secrets || {}
  originalSecrets.value = Object.keys(secrets)
  secretRows.value = Object.keys(secrets).sort().map((name) => ({ name, value: secrets[name] || '', masked: secrets[name] === '••••' }))
  mcpRows.value = Object.keys(cfg.mcpServers || {}).sort().map((name) => {
    const sc: McpServer = cfg.mcpServers[name]
    return { name, command: sc.command || '', args: (sc.args || []).join(' '), envText: envText(sc.env) }
  })
  banner.value = ''
}

function addSecret() {
  secretRows.value.push({ name: '', value: '', masked: false })
}

function addMcp() {
  mcpRows.value.push({ name: '', command: '', args: '', envText: '' })
}

async function saveConfig() {
  busy.value = true
  banner.value = ''
  try {
    const secrets: Record<string, string> = {}
    const seen = new Set<string>()
    for (const row of secretRows.value) {
      const name = row.name.trim()
      if (!name) continue
      seen.add(name)
      secrets[name] = row.masked ? '••••' : row.value
    }
    for (const name of originalSecrets.value) {
      if (!seen.has(name)) secrets[name] = ''
    }
    const mcpServers: Record<string, McpServer> = {}
    for (const row of mcpRows.value) {
      const name = row.name.trim()
      if (!name) continue
      const env = parseEnv(row.envText)
      if (!env) {
        banner.value = '400：MCP 环境要写成 KEY=VALUE，一行一个'
        return
      }
      mcpServers[name] = {
        command: row.command.trim(),
        args: row.args.split(' ').map((s) => s.trim()).filter(Boolean),
        env,
      }
    }
    const res = await api<{ config: RuntimeConfig }>('PUT', `/api/tenants/${tenant.value.id}/runtime-config`, {
      modelRef: modelRef.value.trim(),
      mcpServers,
      secrets,
      sandbox: { network: network.value },
    })
    if (res.body.code !== 0) {
      show(res.status, res.body.message, res.body.code)
      return
    }
    await loadConfig()
    // 状态是探针结果，不是配置本身：保存后旧结果已失效，当场重测（否则要再点一次「测试连接」）。
    await testMcp()
    if (!banner.value) flash('已保存，并重新测过连接')
  } finally {
    busy.value = false
  }
}

async function testMcp() {
  busy.value = true
  try {
    const res = await api<{ system: McpItem[]; custom: McpItem[] }>('GET', `/api/tenants/${tenant.value.id}/mcp-status`)
    if (res.body.code !== 0 || !res.body.data) {
      show(res.status, res.body.message, res.body.code)
      systemMcp.value = []
      customMcp.value = []
      return
    }
    banner.value = ''
    systemMcp.value = res.body.data.system || []
    customMcp.value = res.body.data.custom || []
  } finally {
    busy.value = false
  }
}

function stateClass(state: string, custom: boolean): string {
  if (state === 'degraded') return 'st-warn'
  if (state === 'failed') return 'st-bad'
  if (state === 'connected') return 'st-ok'
  if (custom && state !== 'connected') return 'st-bad'
  return 'st-muted'
}

async function reload() {
  if (props.page === 'agents') await loadAgents()
  else {
    await loadConfig()
    await testMcp()
  }
}

watch(tab, () => {
  selected.value = null
  creating.value = false
  confirming.value = false
  void loadAgents()
})

watch(() => props.page, () => {
  banner.value = ''
  if (loggedIn.value) void reload()
})

onMounted(() => {
  if (loggedIn.value) void reload()
})
</script>

<template>
  <section class="cfg">
    <form v-if="!loggedIn" class="card login" @submit.prevent="login">
      <h2>登录后配置这个工作区</h2>
      <label>邮箱<input v-model="email" type="email" autocomplete="username" required /></label>
      <label>密码
        <span class="secret">
          <input v-model="password" :type="showPassword ? 'text' : 'password'" autocomplete="current-password" required />
          <button type="button" class="text" @click="showPassword = !showPassword">{{ showPassword ? '隐藏' : '显示' }}</button>
        </span>
      </label>
      <button type="submit" class="primary" :disabled="busy">{{ busy ? '正在登录…' : '登录' }}</button>
      <p v-if="banner" class="banner">{{ banner }}</p>
    </form>

    <template v-else>
      <header class="page-head">
        <div>
          <p class="kicker">{{ page === 'agents' ? '助手' : '团队' }}</p>
          <h2>{{ page === 'agents' ? '这个工作区能调用的助手' : '模型、密钥和 MCP' }}</h2>
        </div>
        <p v-if="note" class="toast" role="status">{{ note }}</p>
      </header>
      <p v-if="banner" class="banner" role="alert">{{ banner }}</p>

      <div v-if="page === 'agents'" class="split">
        <div class="pane">
          <div class="row">
            <div class="tabs" role="tablist">
              <button type="button" role="tab" :class="{ on: tab === 'root' }" :aria-selected="tab === 'root'" @click="tab = 'root'">对话助手</button>
              <button type="button" role="tab" :class="{ on: tab === 'subagent' }" :aria-selected="tab === 'subagent'" @click="tab = 'subagent'">子代理</button>
            </div>
            <button type="button" @click="startCustom">新建</button>
            <label class="file">导入 Markdown<input type="file" accept=".md,text/markdown" hidden @change="onImport" /></label>
          </div>
          <p v-if="!agents.length" class="muted">这一栏还没有助手。</p>
          <ul class="list">
            <li v-for="a in agents" :key="a.source + ':' + a.name + ':' + a.id">
              <button
                type="button"
                class="item"
                :class="{ on: !creating && selected?.name === a.name && selected?.source === a.source }"
                @click="selectAgent(a)"
              >
                <span class="item-title">
                  <strong>{{ a.name }}</strong>
                  <span class="badge">{{ sourceLabel(a.source) }}</span>
                </span>
                <span v-if="a.description" class="desc">{{ a.description }}</span>
                <span class="muted">{{ a.modelRef || '继承模型' }} · {{ policyLine(a.toolPolicy) }}</span>
              </button>
            </li>
          </ul>
        </div>

        <form v-if="selected" class="pane editor" @submit.prevent="saveAgent">
          <div class="row spread">
            <h3>{{ creating ? '新建自定义助手' : selected.name }}</h3>
            <button type="button" class="text" @click="closeEditor">收起</button>
          </div>
          <div class="fields">
            <label>名字<input v-model="selected.name" :disabled="selected.source === 'default' || selected.source === 'override'" required /></label>
            <label>说明<input v-model="selected.description" /></label>
            <label>模型<input v-model="selected.modelRef" placeholder="留空则继承工作区模型" /></label>
            <label>可见性
              <select v-model="selected.visibility">
                <option value="private">仅自己</option>
                <option value="tenant">团队可见</option>
              </select>
            </label>
            <label>工具
              <select v-model="selected.toolPolicy.mode">
                <option value="inherit">继承</option>
                <option value="allow">只允许列出的</option>
                <option value="exclude">排除列出的</option>
                <option value="none">不用工具</option>
              </select>
            </label>
            <label v-if="selected.toolPolicy.mode === 'allow' || selected.toolPolicy.mode === 'exclude'">工具名<input v-model="namesText" placeholder="用逗号分开" /></label>
          </div>
          <label>规格<textarea v-model="selected.spec" rows="12"></textarea></label>
          <div class="row">
            <button type="submit" class="primary" :disabled="busy">{{ selected.source === 'default' ? '保存为覆盖' : (busy ? '正在保存…' : '保存') }}</button>
            <template v-if="selected.source === 'override' || (selected.source === 'custom' && selected.id)">
              <button v-if="!confirming" type="button" :disabled="busy" @click="confirming = true">{{ selected.source === 'override' ? '重置' : '删除' }}</button>
              <span v-else class="confirm">
                {{ selected.source === 'override' ? '重置会丢掉这份覆盖。' : '删除后这份自定义助手就没了。' }}
                <button type="button" class="danger" :disabled="busy" @click="removeAgent">确认</button>
                <button type="button" :disabled="busy" @click="confirming = false">取消</button>
              </span>
            </template>
          </div>
          <p v-if="selected.source === 'default'" class="muted">默认助手不能直接改。保存会复制成一份覆盖。</p>
        </form>
        <div v-else class="pane placeholder">
          <p>从左边选一个助手，或新建一个自定义助手。</p>
        </div>
      </div>

      <form v-else class="stack" @submit.prevent="saveConfig">
        <section class="card">
          <h3>模型</h3>
          <label>默认模型<input v-model="modelRef" placeholder="deepseek/deepseek-v4-flash" /></label>
          <label class="check"><input v-model="network" type="checkbox" />沙箱允许出网</label>
        </section>

        <section class="card">
          <h3>密钥</h3>
          <p class="muted">格子里仍是 •••• 时，保存不会改这把密钥。把格子清空再保存，会删掉它。</p>
          <div v-for="(row, i) in secretRows" :key="'s' + i" class="pair">
            <input v-model="row.name" aria-label="密钥名" placeholder="DEEPSEEK_API_KEY" />
            <input v-model="row.value" aria-label="密钥值" @input="row.masked = false" />
            <button type="button" @click="secretRows.splice(i, 1)">移除</button>
          </div>
          <button type="button" @click="addSecret">加一把密钥</button>
        </section>

        <section class="card">
          <h3>自定义 MCP</h3>
          <p v-if="!mcpRows.length" class="muted">还没有自定义 MCP。</p>
          <div v-for="(row, i) in mcpRows" :key="'m' + i" class="mcp">
            <div class="row spread">
              <strong>{{ row.name || '未命名' }}</strong>
              <button type="button" @click="mcpRows.splice(i, 1)">移除</button>
            </div>
            <div class="fields">
              <label>短名<input v-model="row.name" placeholder="短名" /></label>
              <label>命令<input v-model="row.command" placeholder="command" /></label>
            </div>
            <label>参数<input v-model="row.args" placeholder="args，空格分开" /></label>
            <label>环境<textarea v-model="row.envText" rows="3" placeholder="KEY=secret://DEEPSEEK_API_KEY"></textarea></label>
          </div>
          <div class="row">
            <button type="button" @click="addMcp">加一台 MCP</button>
            <button type="button" :disabled="busy" @click="testMcp">测试连接</button>
            <button type="submit" class="primary" :disabled="busy">{{ busy ? '正在保存…' : '保存' }}</button>
          </div>
        </section>

        <section class="card">
          <h3>连接状态</h3>
          <h4>系统 MCP</h4>
          <p v-if="!systemMcp.length" class="muted">没有系统 MCP</p>
          <ul class="status">
            <li v-for="it in systemMcp" :key="'sys' + it.name">
              <span :class="stateClass(it.state, false)">{{ it.state }}</span>
              <strong>{{ it.name }}</strong>
              <span v-if="it.reason" class="muted">{{ it.reason }}</span>
            </li>
          </ul>
          <h4>自定义 MCP</h4>
          <p v-if="!customMcp.length" class="muted">没有自定义 MCP</p>
          <ul class="status">
            <li v-for="it in customMcp" :key="'cus' + it.name">
              <span :class="stateClass(it.state, true)">{{ it.state }}</span>
              <strong>{{ it.name }}</strong>
              <span v-if="it.tools.length" class="muted">工具 {{ it.tools.join('、') }}</span>
              <span v-if="it.reason" class="muted">{{ it.reason }}</span>
            </li>
          </ul>
        </section>
      </form>
    </template>
  </section>
</template>

<style scoped>
.cfg {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding: 1.4rem 1.4rem 2.5rem;
}
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 1rem;
  margin-bottom: 0.9rem;
}
.kicker {
  margin: 0;
  color: var(--accent-ink);
  font-size: 0.75rem;
  letter-spacing: 0.14em;
}
h2 {
  margin: 0.15rem 0 0;
  font-size: 1.55rem;
  font-weight: 620;
  letter-spacing: -0.03em;
}
h3, h4 { margin: 0; }
h3 { font-size: 1rem; }
h4 { margin-top: 0.4rem; font-size: 0.82rem; color: var(--ink-soft); font-weight: 600; }
.card, .pane, .login {
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: 18px;
  padding: 1rem;
}
.login, .card, .editor, .stack, .mcp {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}
.stack { gap: 0.85rem; }
.split {
  display: grid;
  grid-template-columns: minmax(16rem, 22rem) minmax(0, 1fr);
  gap: 0.85rem;
  align-items: start;
}
.pane { min-width: 0; }
.placeholder {
  min-height: 12rem;
  display: grid;
  place-items: center;
  color: var(--muted);
  text-align: center;
}
.row, .pair, .tabs, .confirm, .item-title, .status li {
  display: flex;
  gap: 0.45rem;
  align-items: center;
  flex-wrap: wrap;
}
.row.spread { justify-content: space-between; }
.fields {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 0.7rem;
}
label {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  font-size: 0.78rem;
  color: var(--ink-soft);
}
.secret { position: relative; display: block; }
input, select, textarea {
  width: 100%;
  border: 1px solid var(--line-strong);
  border-radius: 12px;
  padding: 0.55rem 0.7rem;
  background: #fff;
  font-size: 0.92rem;
}
textarea { resize: vertical; line-height: 1.55; }
input:focus, select:focus, textarea:focus {
  outline: none;
  border-color: #8fbfb0;
  box-shadow: 0 0 0 3px rgba(15, 110, 86, 0.12);
}
button, .file {
  border: 1px solid var(--line-strong);
  background: #fff;
  border-radius: 999px;
  padding: 0.38rem 0.8rem;
  font-size: 0.8rem;
}
.card > button { align-self: flex-start; }
.file { display: inline-flex; cursor: pointer; }
button:hover:not(:disabled), .file:hover { border-color: #cfc6b6; }
.primary {
  border: 0;
  background: var(--ink);
  color: #f6f1e7;
}
.primary:hover:not(:disabled) { background: #292524; }
.primary:disabled { opacity: 0.55; }
.danger {
  border-color: #fecdd3;
  background: var(--danger-soft);
  color: var(--danger);
}
.text {
  border: 0;
  background: transparent;
  color: var(--muted);
  padding-inline: 0.4rem;
}
.tabs {
  padding: 0.18rem;
  background: var(--bg);
  border-radius: 999px;
  border: 1px solid var(--line);
}
.tabs button {
  border: 0;
  background: transparent;
  color: var(--ink-soft);
}
.tabs button.on {
  background: var(--bg-raised);
  color: var(--ink);
  box-shadow: 0 1px 2px rgba(68, 48, 24, 0.06);
}
.list, .status {
  list-style: none;
  margin: 0.7rem 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.4rem;
}
.item {
  width: 100%;
  text-align: left;
  border-radius: 14px;
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 0.2rem;
  padding: 0.7rem 0.75rem;
}
.item.on {
  border-color: #8fbfb0;
  background: var(--accent-soft);
}
.desc {
  color: var(--ink-soft);
  font-size: 0.8rem;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.badge, .st-ok, .st-warn, .st-bad, .st-muted {
  font-size: 0.72rem;
  border-radius: 999px;
  padding: 0.05rem 0.45rem;
}
.badge { background: var(--accent-soft); color: var(--accent-ink); }
.muted { color: var(--muted); font-size: 0.78rem; margin: 0; }
.banner, .toast {
  margin: 0 0 0.8rem;
  border-radius: 12px;
  padding: 0.5rem 0.7rem;
  font-size: 0.82rem;
}
.banner { color: var(--danger); background: var(--danger-soft); }
.toast { margin: 0; color: var(--accent-ink); background: var(--accent-soft); }
.pair input { width: auto; flex: 1; min-width: 8rem; }
.mcp { border-top: 1px solid var(--line); padding-top: 0.8rem; }
.mcp:first-of-type { border-top: 0; padding-top: 0; }
.check { flex-direction: row; align-items: center; font-size: 0.88rem; }
.check input { width: auto; }
.confirm { color: var(--ink-soft); font-size: 0.8rem; }
.st-ok { color: #047857; background: #ecfdf5; }
.st-warn { color: #a16207; background: #fef9c3; }
.st-bad { color: #be123c; background: #ffe4e6; }
.st-muted { color: var(--muted); background: var(--bg); }
.secret .text {
  position: absolute;
  right: 0.45rem;
  top: 50%;
  transform: translateY(-50%);
  background: var(--bg);
  border-radius: 999px;
  padding: 0.15rem 0.45rem;
}
@media (max-width: 980px) {
  .split, .fields { grid-template-columns: 1fr; }
  .pair { flex-direction: column; align-items: stretch; }
  .pair input { width: 100%; }
  .pair button { align-self: flex-start; }
  .cfg { padding: 1rem 0.8rem 2rem; }
}
</style>
