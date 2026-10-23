<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, type AgentRow, type McpItem, type McpServer, type RuntimeConfig, type ToolPolicy } from './configureApi'
import { authState, loginWithPassword, readTenant, readToken } from './auth'
import ConfirmDialog from './ConfirmDialog.vue'
import MembersView from './MembersView.vue'

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

// status 只决定「是不是失败」（code !== 0 已说明），不进文案：用户不需要 409/400 这些网关语义，
// 「这个名字已经有一份配置」本身就是完整的一句话。
function show(_status: number, message: string, code: number) {
  banner.value = code === 0 ? '' : message || '请求失败'
}

async function login() {
  banner.value = ''
  busy.value = true
  try {
    const res = await loginWithPassword(email.value, password.value)
    if (!res.ok) {
      banner.value = res.message
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
const teamSub = ref<'settings' | 'members'>(
  sessionStorage.getItem('tako.team.sub.' + readTenant().id) === 'members' ? 'members' : 'settings',
)
watch(teamSub, (value) => {
  const id = readTenant().id
  if (id) sessionStorage.setItem('tako.team.sub.' + id, value)
})
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

// 新建撞名要硬拦：服务端对「与内置同名」会建一份 override，并用请求里的空说明/空规格覆盖内置那份，
// 用户看到的是一具空壳；而「编辑内置」本来就走左侧选中它（PUT），不需要从新建进。
// 两侧都要查——root 与 subagent 是同一个名字空间的不同视图，撞另一侧同样会被服务端当同类处理。
async function nameTaken(name: string): Promise<boolean> {
  if (agents.value.some((a) => a.name === name)) return true
  const other = tab.value === 'root' ? 'subagent' : 'root'
  const res = await api<{ agents: AgentRow[] }>('GET', `/api/tenants/${tenant.value.id}/agents?kind=${other}`)
  return res.body.code === 0 && !!res.body.data?.agents.some((a) => a.name === name)
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
    if (creating.value) {
      const name = row.name.trim()
      if (!name) {
        show(0, '请填写助手名字', 1)
        return
      }
      if (await nameTaken(name)) {
        show(0, `「${name}」已经是内置或已有助手，要改它请在左侧选中后编辑`, 1)
        return
      }
    }
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
    // 导入的默认可见性与手工新建一致（仅自己）；不写出来，用户会以为已经共享给团队。
    flash('已导入（默认仅自己可见，要共享请打开后改成团队可见）')
  } finally {
    busy.value = false
  }
}

const modelRef = ref('')
const network = ref(false)

// 密钥行（doc/48 C2）：value 一律是**用户新填的值**，空 = 不改动。
//
// 服务端回的是掩码 '••••'，那不是真值。以前它直接躺在输入框的 value 里，
// 用户一改就成了 '•••sk-xxx' 这种带占位符前缀的垃圾串——保存进去密钥当场失效，
// 而界面照样弹「已保存」。现在：输入框里**只放用户自己填的东西**，
// 服务端有没有这把钥匙用 hasValue 记，「留空」从此只有一种含义。
type SecretRow = {
  name: string
  value: string
  hasValue: boolean // 服务端那边这把密钥存在（值是掩码还是真值都算）
  removed: boolean // 标记删除（保存后生效）
}
const secretRows = ref<SecretRow[]>([])
const showSecret = ref(false)
const removeSecretName = ref('')
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
  savedConfig = cfg
  modelRef.value = cfg.modelRef || ''
  network.value = !!cfg.sandbox?.network
  const secrets = cfg.secrets || {}
  originalSecrets.value = Object.keys(secrets)
  // 服务端只回掩码 '••••'——那不是真值，不该躺在输入框里（见 SecretRow 的注释）。
  secretRows.value = Object.keys(secrets).sort().map((name) => ({
    name,
    value: '',
    hasValue: true,
    removed: false,
  }))
  mcpRows.value = Object.keys(cfg.mcpServers || {}).sort().map((name) => {
    const sc: McpServer = cfg.mcpServers[name]
    return { name, command: sc.command || '', args: (sc.args || []).join(' '), envText: envText(sc.env) }
  })
  banner.value = ''
}

function addSecret() {
  secretRows.value.push({ name: '', value: '', hasValue: false, removed: false })
}

function addMcp() {
  mcpRows.value.push({ name: '', command: '', args: '', envText: '' })
}

// 删一把密钥：先标记、**保存后才生效**，并要一次确认。
// 「移除」这个词听起来只是「从列表里拿掉」，可它删的是真密钥——所以改名 + 确认。
function askRemoveSecret(row: SecretRow) {
  removeSecretName.value = row.name
}

function cancelRemoveSecret() {
  removeSecretName.value = ''
}

function confirmRemoveSecret() {
  const row = secretRows.value.find((r) => r.name === removeSecretName.value)
  if (row) {
    row.removed = true
    row.value = ''
  }
  removeSecretName.value = ''
}

// 最近一次从服务端读到的完整配置（**已保存的值**）。
//
// 保存某张卡片时，其它卡片用这份「已保存的值」而不是「当前编辑中的值」——
// 否则在「模型」卡片点保存，会把「密钥」卡片里还没保存的编辑一起提交，
// 「每张卡片各有各的保存」就名存实亡了（用户只想存模型，密钥却被改了）。
let savedConfig: RuntimeConfig | null = null

// secretsPayload 把这张表拼成请求体里的 secrets。三条规则（doc/48 C2）：
//   ① 标记删除的 → ''（服务端 = 删掉这把）
//   ② 填了新值的 → 新值
//   ③ 没动过的（留空、但服务端有）→ '••••'（服务端认这个占位符 = 保留原值）
// 「留空」从此**只有一种含义：不改动**。之前它还兼着「删除」，一个状态两种含义最容易出事。
function secretsPayload(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const row of secretRows.value) {
    const name = row.name.trim()
    if (!name) continue
    if (row.removed) {
      out[name] = ''
      continue
    }
    if (row.value) {
      out[name] = row.value
      continue
    }
    if (row.hasValue) out[name] = '••••'
  }
  // 表里没有的原有 key = 用户把那一行删掉或改名了 → 显式删掉（与服务端整体 PUT 的效果一致）。
  for (const name of originalSecrets.value) {
    if (!(name in out)) out[name] = ''
  }
  return out
}

// 名字空着、却填了值的行会被 secretsPayload 静默丢掉：用户点保存、横幅说「密钥已保存」，
// 刷新后这行没了。保存前先拦，让它当场说清楚。
function secretNameMissing(): boolean {
  return secretRows.value.some((row) => !row.name.trim() && row.value.trim() !== '')
}

// mcpPayload 拼 MCP 配置；环境写错（不是 KEY=VALUE）时返回 null，由调用方报错。
function mcpPayload(): Record<string, McpServer> | null {
  const out: Record<string, McpServer> = {}
  for (const row of mcpRows.value) {
    const name = row.name.trim()
    if (!name) continue
    const env = parseEnv(row.envText)
    if (!env) return null
    out[name] = {
      command: row.command.trim(),
      args: row.args.split(' ').map((s) => s.trim()).filter(Boolean),
      env,
    }
  }
  return out
}

// 三张卡片各有自己的保存（doc/48 C3）。原来整页只有一个「保存」，藏在最后一张卡片（MCP）里——
// 改个默认模型要点到页面最底才找得到；而「测试连接」紧挨着表单内容，用户理所当然以为它测的是
// **正在编辑的内容**，它其实测的是服务端已存的配置。
//
// 服务端是**整体 PUT**：只传自己那块会把别的块清空 → 每次保存都带上完整配置。
async function saveConfig(what: 'model' | 'secrets' | 'mcp') {
  if (busy.value) return
  busy.value = true
  banner.value = ''
  try {
    if (what === 'secrets' && secretNameMissing()) {
      banner.value = '密钥名不能为空'
      return
    }
    const mcpServers = what === 'mcp' ? mcpPayload() : (savedConfig?.mcpServers ?? {})
    if (what === 'mcp' && !mcpServers) {
      banner.value = 'MCP 环境要写成 KEY=VALUE，一行一个'
      return
    }
    // 只提交**这张卡片**的编辑结果；其它块用服务端已保存的值原样带回
    // （secrets 里是掩码 '••••'，服务端认它 = 保留原值）。
    const body = {
      modelRef: what === 'model' ? modelRef.value.trim() : (savedConfig?.modelRef ?? ''),
      mcpServers: mcpServers ?? {},
      secrets: what === 'secrets' ? secretsPayload() : (savedConfig?.secrets ?? {}),
      sandbox: what === 'model' ? { network: network.value } : (savedConfig?.sandbox ?? { network: false }),
    }
    const res = await api<{ config: RuntimeConfig }>('PUT', `/api/tenants/${tenant.value.id}/runtime-config`, body)
    if (res.body.code !== 0) {
      show(res.status, res.body.message, res.body.code)
      return
    }
    // **不要整份 loadConfig()**：那会把用户在**其它卡片**里还没保存的编辑一起冲掉
    // （在「密钥」卡片点保存 → 「模型」卡片里刚填的模型被覆盖回旧值 = 用户白填了）。
    // 这里只更新「已保存值」里刚提交的那一块，编辑框保持原样。
    savedConfig = {
      ...((savedConfig ?? {}) as RuntimeConfig),
      ...(what === 'model' ? { modelRef: body.modelRef, sandbox: body.sandbox } : {}),
      ...(what === 'secrets' ? { secrets: body.secrets } : {}),
      ...(what === 'mcp' ? { mcpServers: body.mcpServers } : {}),
    }
    // 已标记删除的行：删除已经生效，把它从表里去掉（「撤销删除」在保存后已无意义——
    // 服务端那边已经是空值了，再撤销也回不来）。
    if (what === 'secrets') {
      secretRows.value = secretRows.value.filter((r) => !r.removed)
    }
    // **不再自动重测连接**：保存这个动作不该顺带拉起 MCP 子进程、也不该变慢。
    // 连接状态是探针结果，用户想看就点「测试已保存的配置」。
    flash(what === 'model' ? '模型已保存' : what === 'secrets' ? '密钥已保存' : 'MCP 已保存')
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
  else if (teamSub.value === 'settings') {
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

watch(teamSub, () => {
  banner.value = ''
  if (props.page === 'team' && loggedIn.value && teamSub.value === 'settings') void reload()
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
          <h2>{{ page === 'agents' ? '这个工作区能调用的助手' : (teamSub === 'members' ? '成员和邀请' : '模型、密钥和 MCP') }}</h2>
        </div>
        <p v-if="note" class="toast" role="status">{{ note }}</p>
      </header>
      <div v-if="page === 'team'" class="tabs subnav" role="tablist">
        <button type="button" role="tab" :class="{ on: teamSub === 'settings' }" :aria-selected="teamSub === 'settings'" @click="teamSub = 'settings'">设置</button>
        <button type="button" role="tab" :class="{ on: teamSub === 'members' }" :aria-selected="teamSub === 'members'" @click="teamSub = 'members'">成员</button>
      </div>
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
            <label>名字<input v-model="selected.name" maxlength="64" :disabled="selected.source === 'default' || selected.source === 'override'" required /></label>
            <label>说明<textarea v-model="selected.description" rows="3"></textarea></label>
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
            <label v-if="selected.toolPolicy.mode === 'allow' || selected.toolPolicy.mode === 'exclude'">工具名<textarea v-model="namesText" rows="2" placeholder="用逗号分开"></textarea></label>
          </div>
          <label>规格<textarea v-model="selected.spec" rows="16" class="spec-area"></textarea></label>
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

      <MembersView v-else-if="teamSub === 'members'" />

      <template v-else>
        <!-- 三张卡片各有自己的保存（doc/48 C3）。原来整页只有一个「保存」按钮，
             藏在最后一张卡片（MCP）底部——改个默认模型要点到页面最底才找得到。 -->
        <form class="stack" @submit.prevent="saveConfig('model')">
          <section class="card">
            <h3>模型</h3>
            <label>默认模型<input v-model="modelRef" placeholder="deepseek/deepseek-v4-flash" /></label>
            <label class="check"><input v-model="network" type="checkbox" />沙箱允许出网</label>
            <div class="row">
              <button type="submit" class="primary" :disabled="busy">{{ busy ? '正在保存…' : '保存模型设置' }}</button>
            </div>
          </section>
        </form>

        <form class="stack" @submit.prevent="saveConfig('secrets')">
          <section class="card">
            <h3>密钥</h3>
            <p class="muted">
              留空表示不改这把钥匙。要换值就填新值；要删就点「删除这把密钥」——标记后保存才生效。
            </p>
            <div v-for="(row, i) in secretRows" :key="'s' + i" class="pair" :class="{ removed: row.removed }">
              <input v-model="row.name" aria-label="密钥名" placeholder="DEEPSEEK_API_KEY" :disabled="row.removed" />
              <span class="secret">
                <input
                  v-model="row.value"
                  :type="showSecret ? 'text' : 'password'"
                  aria-label="密钥值"
                  :placeholder="row.hasValue ? '已保存（留空则不改动）' : '填入密钥值'"
                  :disabled="row.removed"
                  autocomplete="off"
                />
                <button type="button" class="text" @click="showSecret = !showSecret">{{ showSecret ? '隐藏' : '显示' }}</button>
              </span>
              <button v-if="!row.removed" type="button" @click="askRemoveSecret(row)">删除这把密钥</button>
              <button v-else type="button" @click="row.removed = false">撤销删除</button>
            </div>
            <div class="row">
              <button type="button" @click="addSecret">加一把密钥</button>
              <button type="submit" class="primary" :disabled="busy">{{ busy ? '正在保存…' : '保存密钥' }}</button>
            </div>
          </section>
        </form>

        <form class="stack" @submit.prevent="saveConfig('mcp')">
          <section class="card">
            <div class="row spread">
              <h3>自定义 MCP</h3>
              <!-- 「测试连接」测的是**服务端已存的配置**，不是正在编辑的内容——
                   名字里写清楚，免得用户以为测的是自己刚改的东西。 -->
              <button type="button" :disabled="busy" @click="testMcp">测试已保存的配置</button>
            </div>
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
              <button type="submit" class="primary" :disabled="busy">{{ busy ? '正在保存…' : '保存 MCP' }}</button>
            </div>
          </section>
        </form>

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

        <ConfirmDialog
          v-if="removeSecretName"
          title="删除这把密钥？"
          :body="`删掉 ${removeSecretName}？保存后它不再可用，模型调用会失败。`"
          :busy="busy"
          @confirm="confirmRemoveSecret"
          @cancel="cancelRemoveSecret"
        />
      </template>
    </template>
  </section>
</template>

<style scoped>
.cfg {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding: 28px 32px 40px;
  /* 宽屏限宽（doc/48 D1）：输入框拉满 1400px 时，一行字占半米宽。
     **width + max-width 都要**：父级是 column 方向的 flex（.stage），
     margin:auto 会吃掉交叉轴剩余空间、让本项按内容宽度收缩——
     只写 max-width 时，内容少的页面（如成员页）会自动缩成半宽。 */
  width: 100%;
  max-width: 53.75rem;
  margin: 0 auto;
}
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 1rem;
  margin-bottom: 1.2rem;
}
.kicker {
  margin: 0;
  color: var(--muted);
  font-size: var(--text-xs);
  font-weight: 600;
  letter-spacing: 0.1em;
  text-transform: uppercase;
}
h2 {
  margin: 0.15rem 0 0;
  font-size: var(--text-xl);
  font-weight: 600;
  letter-spacing: -0.02em;
}
h3, h4 { margin: 0; }
h3 { font-size: var(--text-lg); }
h4 { margin-top: 0.4rem; font-size: var(--text-sm); color: var(--ink-soft); font-weight: 600; }
.card, .pane, .login {
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  padding: 1.25rem;
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
/* 「显示」按钮浮在输入框右侧 → 值框要给它留出位置，别把 placeholder 挡住。 */
.secret input { padding-right: 2.9rem; }
/* 被标记删除的密钥行：划掉 + 变淡，别让它看起来还能用。 */
.pair.removed { opacity: 0.55; }
.pair.removed input { text-decoration: line-through; }
/* 规格（子代理系统提示词）：给足高度，别让最后一行被裁成半行（doc/48 D2）。 */
.spec-area { min-height: 18rem; line-height: 1.6; }
input, select, textarea {
  width: 100%;
  height: 36px;
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-control);
  padding: 0 0.7rem;
  background: var(--bg-raised);
  font-size: var(--text-sm);
}
textarea { height: auto; padding: 0.5rem 0.7rem; resize: vertical; line-height: 1.55; }
input:focus, select:focus, textarea:focus {
  outline: none;
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}
button, .file {
  height: 32px;
  border: 1px solid var(--line-strong);
  background: var(--bg-raised);
  border-radius: var(--radius-control);
  padding: 0 0.8rem;
  font-size: var(--text-sm);
  font-weight: 500;
}
.card > button { align-self: flex-start; }
.file { display: inline-flex; align-items: center; cursor: pointer; }
button:hover:not(:disabled), .file:hover { background: var(--hover); }
.primary {
  border: 0;
  background: var(--accent);
  color: #fff;
}
.primary:hover:not(:disabled) { background: var(--accent-ink); }
.primary:disabled { opacity: 0.55; }
.danger {
  border-color: var(--danger);
  background: var(--bg-raised);
  color: var(--danger);
}
.danger:hover:not(:disabled) { background: var(--danger-soft); }
.text {
  border: 0;
  background: transparent;
  color: var(--muted);
  padding-inline: 0.4rem;
}
.tabs {
  padding: 0.18rem;
  background: var(--bg);
  border-radius: var(--radius-control);
  border: 1px solid var(--line);
}
.subnav { margin: 0 0 1.2rem; align-self: flex-start; }
.tabs button {
  border: 0;
  background: transparent;
  color: var(--ink-soft);
  border-radius: calc(var(--radius-control) - 2px);
}
.tabs button.on {
  background: var(--bg-raised);
  color: var(--accent-ink);
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
  height: auto;
  text-align: left;
  border-radius: var(--radius-control);
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 0.2rem;
  padding: 0.7rem 0.75rem;
}
.item.on {
  border-color: var(--accent);
  background: var(--accent-soft);
}
.desc {
  color: var(--ink-soft);
  font-size: var(--text-sm);
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
.muted { color: var(--muted); font-size: var(--text-sm); margin: 0; }
.banner, .toast {
  margin: 0 0 0.8rem;
  border-radius: var(--radius-control);
  padding: 0.5rem 0.7rem;
  font-size: var(--text-sm);
}
.banner { color: var(--danger); background: var(--danger-soft); }
.toast { margin: 0; color: var(--accent-ink); background: var(--accent-soft); }
.pair input { width: auto; flex: 1; min-width: 8rem; }
.mcp { border-top: 1px solid var(--line); padding-top: 0.8rem; }
.mcp:first-of-type { border-top: 0; padding-top: 0; }
.check { flex-direction: row; align-items: center; font-size: var(--text-sm); }
.check input { width: auto; height: auto; }
.confirm { color: var(--ink-soft); font-size: var(--text-sm); }
.st-ok { color: var(--accent-ink); background: var(--accent-soft); }
.st-warn { color: var(--warn); background: var(--warn-soft); }
.st-bad { color: var(--danger); background: var(--danger-soft); }
.st-muted { color: var(--muted); background: var(--bg); }
.secret .text {
  position: absolute;
  right: 0.45rem;
  top: 50%;
  height: 24px;
  transform: translateY(-50%);
  background: var(--bg);
  border-radius: var(--radius-control);
  padding: 0 0.45rem;
}
@media (max-width: 980px) {
  .split, .fields { grid-template-columns: 1fr; }
  .pair { flex-direction: column; align-items: stretch; }
  .pair input { width: 100%; }
  .pair button { align-self: flex-start; }
  .cfg { padding: 1rem 0.8rem 2rem; }
}
</style>
