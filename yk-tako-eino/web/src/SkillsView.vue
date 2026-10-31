<script setup lang="ts">
// 技能页（doc/52 C4/C5）：列表 / 详情 / 开关 / 预览后安装 / 删除。
//
// 这一页是管理面，不是对话面：它只调 §6.1 那组端点（列表、详情、PATCH 开关、
// 预览、安装、DELETE），不读 run、不碰正文工具。所有「变了」都靠服务端真源重拉——
// 开关成功先用 PATCH 回来的整行就地替换，再让 skills_changed 事件触发一次重拉（doc/46 §7.6），
// 两个副本/两个窗口不会各说一套。
//
// 安装分两段（D24）：先 POST /api/skills/preview（只解析、不写盘），卡片列候选全表，
// 人勾选后才 POST /api/skills/install。`--list` 命令因此与预览同义——粘进来就停在这张卡上。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  ApiError,
  checkSkill,
  deleteSkill,
  getSkill,
  installSkillPackage,
  installSkills,
  listSkills,
  patchSkill,
  previewSkillPackage,
  previewSkills,
  rescanSkills,
  type InvalidSkill,
  type Skill,
  type SkillDependency,
  type SkillDetail,
  type SkillInstallResponse,
  type SkillPreviewResponse,
  type SkillPreviewSource,
} from './api'
import { tenantState } from './auth'
import ConfirmDialog from './ConfirmDialog.vue'
import { onMutation } from './mutation'

const emit = defineEmits<{
  // 缺依赖时的补装入口（doc/52 F3）：交给 App 切到工具区并预填命令名。
  'upload-tool': [name: string]
}>()

const phase = ref<'loading' | 'ready' | 'error'>('loading')
const skills = ref<Skill[]>([])
const invalid = ref<InvalidSkill[]>([])
const banner = ref('')
const notice = ref('')
const busy = ref(false)
const query = ref('')

// 描述全文折叠：按名字记「已展开」的几行，默认只显示两行。
const expanded = ref<Record<string, boolean>>({})

// 详情抽屉
const detail = ref<SkillDetail | null>(null)
const detailLoading = ref(false)

// 依赖现场检查（doc/52 D1）：结果只在抽屉里展示，打开另一条技能就清掉——
// 那是「刚才那次检查」，不是这一行的固有属性（missing 每台主机、每一刻都可能变）。
const depBusy = ref(false)
const depCheck = ref<{ name: string; deps: SkillDependency[]; checkedOn: string } | null>(null)

// 删除确认
const pendingDelete = ref<Skill | null>(null)

// 安装：一个输入框 + 「预览」，三种输入都从这里进（来源串 / npx skills add 命令 / 本地包）。
// 预览只解析不写盘；选中集默认取服务端的 selected（没给选择且只有一个候选时就是它）。
const source = ref('')
const preview = ref<SkillPreviewResponse | null>(null)
// 预览时用的那份输入原样存下来：装的就是刚看过的那份，不在两拍之间重新解析用户在框里改过的字。
const previewReq = ref<{ source?: string; command?: string } | null>(null)
const picked = ref<Record<string, boolean>>({})
const previewBusy = ref(false)
const installBusy = ref(false)
const pkgEl = ref<HTMLInputElement | null>(null)
// 来源是本地包时记住它：安装要把同一份字节再上行一次（服务端不缓存预览）。
const pkgFile = ref<File | null>(null)

const pickedNames = computed(() =>
  preview.value ? preview.value.skills.filter((c) => picked.value[c.name]).map((c) => c.name) : [],
)

// 开关与安装是 owner/admin 的动作（路由上也挂 RequireTenantRole("admin")）；
// 普通成员与查看者看到的是只读列表——按钮不画，比画出来再 403 好。
const canAdmin = computed(() => {
  const role = tenantState.value.role
  return role === 'owner' || role === 'admin'
})

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return skills.value
  return skills.value.filter(
    (s) => s.name.toLowerCase().includes(q) || s.description.toLowerCase().includes(q),
  )
})

let reloadTimer: ReturnType<typeof setTimeout> | null = null

function scheduleReload() {
  if (reloadTimer) clearTimeout(reloadTimer)
  reloadTimer = setTimeout(() => void load(), 120)
}

const offMutation = onMutation((evt) => {
  // 事件只通知、不带状态：收到就重拉（doc/46 §3.1）。别的窗口装/删/禁用都会到这里。
  if (evt.type === 'skills_changed') scheduleReload()
})

async function load() {
  try {
    const res = await listSkills()
    skills.value = res.skills
    invalid.value = res.invalid
    phase.value = 'ready'
  } catch (err) {
    phase.value = 'error'
    banner.value = err instanceof ApiError ? err.message : '技能列表没拉到'
  }
}

function fail(err: unknown) {
  if (err instanceof ApiError) {
    banner.value = err.message
    return
  }
  banner.value = '请求失败，请再试一次'
}

async function openDetail(row: Skill) {
  banner.value = ''
  depCheck.value = null
  detailLoading.value = true
  detail.value = { skill: row, body: '' }
  try {
    detail.value = await getSkill(row.name)
  } catch (err) {
    detail.value = null
    fail(err)
  } finally {
    detailLoading.value = false
  }
}

function closeDetail() {
  detail.value = null
  depCheck.value = null
}

// 依赖是「这台机器此刻」的事实（D1）：只读、现查，不与列表里的 missing 混用同一个缓存。
async function checkDeps(row: Skill) {
  if (depBusy.value) return
  banner.value = ''
  depBusy.value = true
  try {
    const res = await checkSkill(row.name)
    depCheck.value = { name: row.name, deps: res.dependencies, checkedOn: res.checkedOn }
  } catch (err) {
    fail(err)
  } finally {
    depBusy.value = false
  }
}

async function toggle(row: Skill) {
  if (busy.value) return
  banner.value = ''
  busy.value = true
  try {
    const res = await patchSkill(row.name, !row.enabled)
    const i = skills.value.findIndex((s) => s.name === row.name)
    if (i >= 0) skills.value[i] = res.skill
    if (detail.value?.skill.name === row.name) detail.value.skill = res.skill
  } catch (err) {
    fail(err)
  } finally {
    busy.value = false
  }
}

async function removeConfirmed() {
  const row = pendingDelete.value
  if (!row) return
  banner.value = ''
  busy.value = true
  try {
    await deleteSkill(row.name)
    skills.value = skills.value.filter((s) => s.name !== row.name)
    if (detail.value?.skill.name === row.name) detail.value = null
    pendingDelete.value = null
    notice.value = `已删除技能：${row.name}`
  } catch (err) {
    fail(err)
  } finally {
    busy.value = false
  }
}

async function rescan() {
  if (busy.value) return
  banner.value = ''
  notice.value = ''
  busy.value = true
  try {
    const res = await rescanSkills()
    await load()
    notice.value =
      `重扫完成：新增 ${res.added.length}、更新 ${res.updated.length}、移除 ${res.removed.length}` +
      (res.roots.length ? `（扫了 ${res.roots.length} 个根）` : '')
  } catch (err) {
    fail(err)
  } finally {
    busy.value = false
  }
}

// npx skills add …（npx 可省）走 command；其余一律当来源串（owner/repo、URL、包链接）。
function looksCommand(text: string): boolean {
  return /^(npx\s+)?skills\s/i.test(text.trim())
}

function requestOf(text: string): { source?: string; command?: string } {
  return looksCommand(text) ? { command: text.trim() } : { source: text.trim() }
}

function applyPreview(res: SkillPreviewResponse, req: { source?: string; command?: string } | null, file: File | null) {
  preview.value = res
  previewReq.value = req
  pkgFile.value = file
  picked.value = {}
  for (const name of res.selected) picked.value[name] = true
}

function clearPreview() {
  preview.value = null
  previewReq.value = null
  pkgFile.value = null
  picked.value = {}
}

async function runPreview() {
  const text = source.value.trim()
  if (!text) {
    banner.value = '先粘一条来源或 npx skills add … 命令'
    return
  }
  banner.value = ''
  notice.value = ''
  previewBusy.value = true
  try {
    const req = requestOf(text)
    applyPreview(await previewSkills(req), req, null)
  } catch (err) {
    clearPreview()
    fail(err)
  } finally {
    previewBusy.value = false
  }
}

async function pickPackage(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  banner.value = ''
  notice.value = ''
  previewBusy.value = true
  try {
    applyPreview(await previewSkillPackage(file), null, file)
    source.value = ''
  } catch (err) {
    clearPreview()
    fail(err)
  } finally {
    previewBusy.value = false
    // 清空选择：同一个包再选一次也要能触发 change。
    input.value = ''
  }
}

async function installPreviewed() {
  const p = preview.value
  if (!p || installBusy.value) return
  if (!pickedNames.value.length) {
    banner.value = '先勾选要装的技能'
    return
  }
  banner.value = ''
  notice.value = ''
  installBusy.value = true
  try {
    let res: SkillInstallResponse
    if (pkgFile.value) {
      res = await installSkillPackage(pkgFile.value, pickedNames.value)
    } else if (previewReq.value) {
      res = await installSkills({ ...previewReq.value, skills: pickedNames.value })
    } else {
      banner.value = '预览已经过期，请重新预览'
      return
    }
    const names = res.installed.map((e) => e.name).join('、')
    notice.value = `已安装：${names || '（没有新增）'}`
    if (res.warnings.length) notice.value += `；提示：${res.warnings.join('；')}`
    clearPreview()
    source.value = ''
    await load()
  } catch (err) {
    fail(err)
  } finally {
    installBusy.value = false
  }
}

// 预览卡上的来源一行：github/gitlab 是 owner/repo（或 namespace/repo），archive 是链接，
// upload 换成人话。与确认卡（SkillInstallCard）同一套口径。
function previewSourceLabel(src: SkillPreviewSource): string {
  if (src.kind === 'github') return src.owner && src.repo ? `${src.owner}/${src.repo}` : src.url
  if (src.kind === 'gitlab') return src.namespace && src.repo ? `${src.namespace}/${src.repo}` : src.url
  if (src.kind === 'upload') return '本地上传的包'
  return src.url
}

function describeMore(row: Skill): boolean {
  return row.description.length > 120
}

function fmtWhen(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

async function copyPath() {
  const p = detail.value?.skill.path
  if (!p) return
  try {
    await navigator.clipboard.writeText(p)
    notice.value = '路径已复制'
  } catch {
    notice.value = '复制失败，请手动选中路径'
  }
}

function onKeydown(ev: KeyboardEvent) {
  if (ev.key === 'Escape' && detail.value) closeDetail()
}

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  void load()
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
  offMutation()
  if (reloadTimer) clearTimeout(reloadTimer)
})
</script>

<template>
  <div class="skills">
    <header class="page-head">
      <div>
        <p class="kicker">技能</p>
        <h2>这个团队会用到的做法</h2>
        <p class="hint">
          技能是本地 Markdown 说明书。装好之后只把「有一项这个能力」放进助手的目录，真要用时才读正文。
        </p>
      </div>
      <div class="head-actions">
        <button v-if="canAdmin" type="button" :disabled="busy" @click="rescan">重扫磁盘</button>
      </div>
    </header>

    <p v-if="banner" class="banner error" role="alert">{{ banner }}</p>
    <p v-if="notice" class="banner ok" role="status">{{ notice }}</p>

    <!-- 安装入口（doc/52 C5 / D24）：一个输入框 + 预览，三种输入都从这里进。
         预览只解析不写盘；候选全表列出来，人勾选后才装——「看」和「装」是两次动作。 -->
    <section v-if="canAdmin" class="card install">
      <h3>安装技能</h3>
      <p class="muted">
        粘来源（owner/repo、GitHub/GitLab 仓库链接、.zip/.tar.gz 直链）、整条
        <code>npx skills add …</code> 命令，或选一个本地包。先预览：看清包里有什么，不会写盘。
      </p>
      <div class="install-row">
        <input
          v-model="source"
          type="text"
          placeholder="owner/repo、仓库链接、包链接，或 npx skills add …"
          aria-label="技能来源"
          @keydown.enter.prevent="runPreview"
        />
        <button type="button" :disabled="previewBusy || installBusy" @click="runPreview">
          {{ previewBusy ? '正在解析…' : '预览' }}
        </button>
        <button type="button" :disabled="previewBusy || installBusy" @click="pkgEl?.click()">选本地包</button>
        <input ref="pkgEl" type="file" accept=".zip,.tar.gz,.tgz" hidden aria-label="技能包" @change="pickPackage" />
      </div>

      <div v-if="preview" class="preview" data-testid="skills-preview">
        <header class="preview-head">
          <div class="preview-where">
            <code>{{ previewSourceLabel(preview.source) }}</code>
            <span v-if="preview.source.ref" class="ref">@ {{ preview.source.ref }}</span>
            <p
              v-if="preview.source.url && preview.source.url !== previewSourceLabel(preview.source)"
              class="preview-url"
            >{{ preview.source.url }}</p>
          </div>
          <button type="button" :disabled="installBusy" @click="clearPreview">取消</button>
        </header>

        <p v-if="preview.ambiguous" class="hint warn">
          来源里有 {{ preview.skills.length }} 个技能：勾选要装的（未勾的也列在下面，方便核对），再点「安装」。
        </p>

        <ul class="cands">
          <li v-for="c in preview.skills" :key="c.name" :class="['cand', { on: picked[c.name] }]" :data-skill="c.name">
            <label>
              <input v-model="picked[c.name]" type="checkbox" :disabled="installBusy" />
              <span class="cand-body">
                <span class="cand-head">
                  <strong>{{ c.name }}</strong>
                  <span class="cand-meta">{{ fmtBytes(c.bytes) }} · {{ c.files }} 个文件</span>
                </span>
                <span class="cand-desc">{{ c.description || '（没有描述）' }}</span>
                <span v-if="c.requires.length" class="cand-req">依赖：{{ c.requires.join('、') }}</span>
              </span>
            </label>
          </li>
        </ul>

        <ul v-if="preview.warnings.length" class="warns">
          <li v-for="(w, i) in preview.warnings" :key="i">{{ w }}</li>
        </ul>

        <p class="hint">
          预览不写盘；命令里的 <code>--list</code> 与预览同义，<code>-g</code>/<code>-a</code> 会被忽略
          （tako 只有一个落点：租户技能根）。
        </p>
        <footer class="preview-foot">
          <span class="muted">将安装 {{ pickedNames.length }} 个</span>
          <button
            type="button"
            class="primary"
            :disabled="installBusy || !pickedNames.length"
            @click="installPreviewed"
          >{{ installBusy ? '正在安装…' : '安装' }}</button>
        </footer>
      </div>
    </section>

    <section class="card">
      <div class="list-head">
        <h3>已安装 <span class="count">{{ skills.length }}</span></h3>
        <input v-model="query" type="search" placeholder="按名字或描述筛选" aria-label="筛选技能" />
      </div>

      <p v-if="phase === 'loading'" class="muted">正在加载…</p>
      <p v-else-if="phase === 'error'" class="muted">列表没拉到，稍后重试。</p>
      <p v-else-if="!filtered.length" class="muted">
        {{ skills.length ? '没有匹配的技能。' : '还没有技能。用上面的来源安装，或让运维同步系统技能。' }}
      </p>

      <ul v-else class="skill-list">
        <li v-for="row in filtered" :key="row.name" class="skill" :data-skill="row.name">
          <div class="skill-main">
            <div class="skill-title">
              <button type="button" class="name" @click="openDetail(row)">{{ row.name }}</button>
              <span class="tag" :class="row.source">{{ row.source === 'system' ? '系统' : '团队' }}</span>
              <span v-if="!row.enabled" class="tag off">已禁用</span>
              <span v-if="row.missing.length" class="tag warn" :title="'缺依赖：' + row.missing.join('、')">
                缺 {{ row.missing.length }} 个依赖
              </span>
            </div>
            <p class="desc" :class="{ clamp: describeMore(row) && !expanded[row.name] }">
              {{ row.description || '（没有描述）' }}
            </p>
            <button
              v-if="describeMore(row)"
              type="button"
              class="more"
              @click="expanded[row.name] = !expanded[row.name]"
            >{{ expanded[row.name] ? '收起' : '展开全文' }}</button>
            <p class="meta">
              <span>{{ fmtBytes(row.bytes) }}</span>
              <span>更新于 {{ fmtWhen(row.updatedAt) }}</span>
              <span v-if="row.requires.length">依赖：{{ row.requires.join('、') }}</span>
            </p>
          </div>
          <div class="skill-actions">
            <button v-if="canAdmin" type="button" :disabled="busy" @click="toggle(row)">
              {{ row.enabled ? '禁用' : '启用' }}
            </button>
            <button v-else type="button" disabled>{{ row.enabled ? '已启用' : '已禁用' }}</button>
            <button v-if="canAdmin" type="button" class="danger" @click="pendingDelete = row">删除</button>
          </div>
        </li>
      </ul>
    </section>

    <!-- 坏行：只给路径与原因，不给修复按钮——修复是改盘上文件再重扫，不是页面上点一下。 -->
    <section v-if="invalid.length" class="card">
      <h3>没通过校验</h3>
      <p class="muted">盘上有这些文件，但它们不会进助手目录。改好之后点「重扫磁盘」。</p>
      <ul class="bad-list">
        <li v-for="bad in invalid" :key="bad.path">
          <code>{{ bad.path }}</code>
          <span class="bad-reason">{{ bad.reason }}</span>
        </li>
      </ul>
    </section>

    <!-- 详情抽屉：只读正文 + 路径复制。正文是盘上原文（服务端逐字返回）。 -->
    <Teleport to="body">
      <div v-if="detail" class="sheet-overlay" @click.self="closeDetail">
        <aside class="sheet" role="dialog" aria-modal="true" :aria-label="detail.skill.name">
          <header class="sheet-head">
            <div>
              <h3>{{ detail.skill.name }}</h3>
              <p class="muted">{{ detail.skill.source === 'system' ? '系统技能' : '团队技能' }}</p>
            </div>
            <button type="button" @click="closeDetail">关闭</button>
          </header>
          <dl class="facts">
            <dt>状态</dt>
            <dd>{{ detail.skill.enabled ? '启用' : '已禁用' }}</dd>
            <dt>大小</dt>
            <dd>{{ fmtBytes(detail.skill.bytes) }}</dd>
            <dt>更新</dt>
            <dd>{{ fmtWhen(detail.skill.updatedAt) }}</dd>
            <dt>依赖</dt>
            <dd class="dep-line">
              <span>{{ detail.skill.requires.length ? detail.skill.requires.join('、') : '无' }}</span>
              <!-- 列表已带回 missing（本副本此刻）：缺的每个都能直接去工具区补 -->
              <button
                v-for="m in detail.skill.missing"
                :key="m"
                type="button"
                class="dep-upload"
                :data-dep="m"
                @click="emit('upload-tool', m)"
              >上传 {{ m }}</button>
              <button type="button" :disabled="depBusy" @click="checkDeps(detail.skill)">
                {{ depBusy ? '检查中…' : '检查依赖' }}
              </button>
            </dd>
            <dt>路径</dt>
            <dd class="path">
              <code>{{ detail.skill.path }}</code>
              <button type="button" @click="copyPath">复制</button>
            </dd>
          </dl>
          <!-- 依赖现场检查：只在本次抽屉里有效；path 是这台机器解析到的真实路径。 -->
          <div v-if="depCheck && depCheck.name === detail.skill.name" class="dep-check" data-testid="dep-check">
            <p class="muted">
              当前副本{{ depCheck.checkedOn === 'host' ? '（宿主 PATH）' : `（${depCheck.checkedOn}）` }}的检查结果：
            </p>
            <p v-if="!depCheck.deps.length" class="muted">这个技能没有声明依赖。</p>
            <ul v-else>
              <li v-for="d in depCheck.deps" :key="d.name" :class="d.found ? 'dep-ok' : 'dep-miss'">
                <span class="dep-mark">{{ d.found ? '✓' : '缺' }}</span>
                <code>{{ d.name }}</code>
                <span v-if="d.path" class="dep-path">{{ d.path }}</span>
                <button
                  v-else
                  type="button"
                  class="dep-upload"
                  :data-dep="d.name"
                  @click="emit('upload-tool', d.name)"
                >上传工具</button>
              </li>
            </ul>
          </div>
          <p v-if="detailLoading" class="muted">正在读正文…</p>
          <pre v-else class="body">{{ detail.body }}</pre>
        </aside>
      </div>
    </Teleport>

    <ConfirmDialog
      v-if="pendingDelete"
      title="删除这个技能？"
      :body="`「${pendingDelete.name}」会从团队技能里移除；正在跑的对话不受影响，下一场对话的目录里不再有它。盘上的目录会移进回收目录。`"
      confirm-text="删除"
      :busy="busy"
      :error="banner"
      @confirm="removeConfirmed"
      @cancel="pendingDelete = null"
    />
  </div>
</template>

<style scoped>
.skills {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding: 28px 32px 40px;
  width: 100%;
  max-width: 53.75rem;
  margin: 0 auto;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 1rem;
  margin-bottom: 0.4rem;
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
  margin: 0.15rem 0 0.2rem;
  font-size: var(--text-xl);
  font-weight: 600;
  letter-spacing: -0.02em;
}
h3 { margin: 0; font-size: var(--text-lg); font-weight: 600; }
.hint, .muted {
  margin: 0;
  color: var(--muted);
  font-size: var(--text-sm);
}
.card {
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: var(--radius-card);
  padding: 1.25rem;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.banner {
  margin: 0;
  border-radius: var(--radius-control);
  padding: var(--space-2) var(--space-3);
  font-size: var(--text-sm);
}
.banner.error { color: var(--danger); background: var(--danger-soft); }
.banner.ok { color: var(--accent-ink); background: var(--accent-soft); }
.install-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.install-row input[type='text'] {
  flex: 1;
  min-width: 14rem;
}
.preview {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  border: 1px solid var(--line);
  border-radius: var(--radius-control);
  padding: var(--space-3);
  background: var(--bg);
}
.preview-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
}
.preview-where { min-width: 0; }
.preview-where code {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--text-sm);
  word-break: break-all;
}
.preview-where .ref {
  margin-left: 0.35rem;
  color: var(--muted);
  font-size: var(--text-xs);
}
.preview-url {
  margin: 2px 0 0;
  color: var(--muted);
  font-size: var(--text-xs);
  word-break: break-all;
}
.hint.warn { color: var(--warn); }
.cands {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  border: 1px solid var(--line);
  border-radius: var(--radius-control);
  background: var(--bg-raised);
  overflow: hidden;
}
.cand { border-top: 1px solid var(--line); }
.cand:first-child { border-top: 0; }
.cand.on { background: var(--accent-soft); }
.cand label {
  display: flex;
  align-items: flex-start;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-3);
  cursor: pointer;
}
.cand input[type='checkbox'] { margin-top: 0.3rem; flex: none; }
.cand-body {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.cand-head { display: flex; align-items: baseline; gap: var(--space-2); flex-wrap: wrap; }
.cand-head strong { font-size: var(--text-sm); font-weight: 600; }
.cand-meta { color: var(--muted); font-size: var(--text-xs); }
.cand-desc {
  color: var(--ink-soft);
  font-size: var(--text-sm);
  line-height: 1.55;
  white-space: pre-wrap;
}
.cand-req { color: var(--muted); font-size: var(--text-xs); }
.warns {
  margin: 0;
  padding-left: 1.1rem;
  color: var(--warn);
  font-size: var(--text-xs);
  line-height: 1.6;
}
.preview-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}
input[type='text'], input[type='search'] {
  height: 34px;
  border: 1px solid var(--line-strong);
  border-radius: var(--radius-control);
  padding: 0 var(--space-3);
  background: var(--bg-raised);
  font-size: var(--text-sm);
}
input:focus {
  outline: none;
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}
button {
  height: 32px;
  border: 1px solid var(--line-strong);
  background: var(--bg-raised);
  border-radius: var(--radius-control);
  padding: 0 var(--space-3);
  font-size: var(--text-sm);
  font-weight: 500;
}
button:hover:not(:disabled) { background: var(--hover); }
button:disabled { opacity: 0.55; }
.primary { border-color: var(--accent); background: var(--accent); color: #fff; }
.primary:hover:not(:disabled) { border-color: var(--accent-ink); background: var(--accent-ink); }
.danger { border-color: var(--danger); background: var(--bg-raised); color: var(--danger); }
.danger:hover:not(:disabled) { background: var(--danger-soft); }
.head-actions { display: flex; gap: var(--space-2); }
.list-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  flex-wrap: wrap;
}
.list-head input { width: min(100%, 16rem); }
.count { color: var(--muted); font-size: var(--text-sm); font-weight: 500; }
.skill-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}
.skill {
  display: flex;
  gap: var(--space-4);
  align-items: flex-start;
  justify-content: space-between;
  padding: var(--space-3) 0;
  border-top: 1px solid var(--line);
}
.skill:first-child { border-top: 0; }
.skill-main { min-width: 0; }
.skill-title {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.name {
  border: 0;
  background: none;
  padding: 0;
  height: auto;
  font-weight: 600;
  font-size: var(--text-base);
  color: var(--ink);
}
.name:hover { color: var(--accent-ink); text-decoration: underline; }
.tag {
  border-radius: 999px;
  padding: 1px 7px;
  font-size: var(--text-xs);
  font-weight: 500;
  color: var(--muted);
  background: var(--hover);
}
.tag.system { color: var(--ink-soft); background: var(--line); }
.tag.off { color: var(--danger); background: var(--danger-soft); }
.tag.warn { color: var(--warn); background: var(--warn-soft); }
.desc {
  margin: var(--space-1) 0 0;
  color: var(--ink-soft);
  font-size: var(--text-sm);
  line-height: 1.6;
  white-space: pre-wrap;
}
.desc.clamp {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.more {
  border: 0;
  background: none;
  padding: 0;
  height: auto;
  margin-top: 2px;
  color: var(--accent-ink);
  font-size: var(--text-xs);
}
.meta {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  margin: var(--space-1) 0 0;
  color: var(--muted);
  font-size: var(--text-xs);
}
.skill-actions { display: flex; gap: var(--space-2); flex-shrink: 0; }
.bad-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.bad-list li { display: flex; flex-direction: column; gap: 2px; }
.bad-list code { font-size: var(--text-xs); word-break: break-all; }
.bad-reason { color: var(--warn); font-size: var(--text-sm); }
.sheet-overlay {
  position: fixed;
  inset: 0;
  z-index: 60;
  background: rgba(16, 17, 20, 0.4);
  display: flex;
  justify-content: flex-end;
}
.sheet {
  width: min(100%, 34rem);
  height: 100%;
  overflow: auto;
  background: var(--bg-raised);
  border-left: 1px solid var(--line);
  padding: var(--space-5);
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.sheet-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
}
.sheet-head h3 { font-size: var(--text-lg); }
.facts {
  display: grid;
  grid-template-columns: 4rem minmax(0, 1fr);
  gap: var(--space-1) var(--space-3);
  margin: 0;
  font-size: var(--text-sm);
}
.facts dt { color: var(--muted); }
.facts dd { margin: 0; color: var(--ink-soft); }
.facts .path {
  display: flex;
  align-items: flex-start;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.facts code { font-size: var(--text-xs); word-break: break-all; }
.facts .dep-line {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.dep-check {
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--line);
  border-radius: var(--radius-control);
  font-size: var(--text-sm);
}
.dep-check p { margin: 0 0 var(--space-1); }
.dep-check ul {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 0.2rem;
}
.dep-check li {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.dep-mark { font-weight: 600; }
.dep-ok .dep-mark { color: var(--ok, #2f7d4f); }
.dep-miss .dep-mark { color: var(--warn, #a3610d); }
.dep-path { color: var(--muted); font-size: var(--text-xs); word-break: break-all; }
.dep-upload { height: 26px; padding: 0 var(--space-2); font-size: var(--text-xs); }
.body {
  margin: 0;
  padding: var(--space-3);
  background: var(--bg);
  border: 1px solid var(--line);
  border-radius: var(--radius-control);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--text-xs);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}
@media (max-width: 720px) {
  .skills { padding: 20px 16px 32px; }
  .page-head { flex-direction: column; align-items: flex-start; }
  .skill { flex-direction: column; }
}
</style>
