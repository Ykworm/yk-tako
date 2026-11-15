<script setup lang="ts">
// 项目目录树（doc/78 §A4）：懒加载，目录首次展开才请求子目录；结果按路径缓存在组件内。
// 路径只来自服务端返回的条目名——这里不接收手输路径。
import { onMounted, reactive, ref, watch } from 'vue'
import { ApiError, listWorkspaceFiles, type WorkspaceFileEntry } from './api'

const props = defineProps<{ workspaceId: string; activePath: string }>()
const emit = defineEmits<{ open: [path: string] }>()

type Row = {
  key: string
  path: string
  name: string
  kind: 'file' | 'dir' | 'note'
  depth: number
  note?: string
  retry?: string
}

const cache = reactive(new Map<string, WorkspaceFileEntry[]>())
const expanded = reactive(new Set<string>())
const loading = reactive(new Set<string>())
const errors = reactive(new Map<string, string>())

const rows = ref<Row[]>([])
const rootLoaded = ref(false)
const rootError = ref('')
const refreshing = ref(false)

function errorText(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 401) return '请先登录'
    if (e.code === 114001) return '找不到这个项目'
    if (e.code === 117116) return '这个项目还需要迁移目录后才能使用'
    return e.message || '目录没拉到'
  }
  return '目录没拉到'
}

function build() {
  const out: Row[] = []
  const walk = (path: string, depth: number) => {
    const entries = cache.get(path)
    if (!entries) return
    for (const entry of entries) {
      const child = path === '.' ? entry.name : `${path}/${entry.name}`
      out.push({ key: child, path: child, name: entry.name, kind: entry.kind, depth })
      if (entry.kind !== 'dir' || !expanded.has(child)) continue
      if (loading.has(child)) {
        out.push({ key: `${child}:note`, path: child, name: '', kind: 'note', depth: depth + 1, note: '正在加载…' })
      } else if (errors.has(child)) {
        out.push({
          key: `${child}:note`,
          path: child,
          name: '',
          kind: 'note',
          depth: depth + 1,
          note: errors.get(child),
          retry: child,
        })
      } else {
        walk(child, depth + 1)
      }
    }
  }
  walk('.', 0)
  rows.value = out
}

async function loadDir(path: string, force = false) {
  if (!force && cache.has(path)) return
  if (loading.has(path)) return
  loading.add(path)
  errors.delete(path)
  build()
  try {
    const res = await listWorkspaceFiles(props.workspaceId, path)
    cache.set(path, res.entries)
    if (path === '.') {
      rootError.value = ''
      rootLoaded.value = true
    }
  } catch (e) {
    const text = errorText(e)
    if (path === '.') rootError.value = text
    else errors.set(path, text)
  } finally {
    loading.delete(path)
    build()
  }
}

function toggle(row: Row) {
  if (row.kind === 'file') {
    emit('open', row.path)
    return
  }
  if (row.kind !== 'dir') return
  if (expanded.has(row.path)) expanded.delete(row.path)
  else {
    expanded.add(row.path)
    void loadDir(row.path)
  }
  build()
}

function isOpen(path: string): boolean {
  return expanded.has(path)
}

async function refresh() {
  refreshing.value = true
  cache.clear()
  expanded.clear()
  loading.clear()
  errors.clear()
  rootError.value = ''
  rootLoaded.value = false
  build()
  await loadDir('.', true)
  refreshing.value = false
}

onMounted(() => {
  void loadDir('.')
})

watch(
  () => props.workspaceId,
  () => {
    cache.clear()
    expanded.clear()
    loading.clear()
    errors.clear()
    rootError.value = ''
    rootLoaded.value = false
    build()
    void loadDir('.')
  },
)

// 从回复结尾的改动清单点进来时，把文件的祖先目录逐层展开，否则选中项藏在一个折叠目录里。
watch(
  () => props.activePath,
  async (path) => {
    if (!path) return
    const parts = path.split('/')
    parts.pop()
    let cur = '.'
    for (const seg of parts) {
      cur = cur === '.' ? seg : `${cur}/${seg}`
      if (!expanded.has(cur)) {
        expanded.add(cur)
        await loadDir(cur)
      }
    }
    build()
  },
)
</script>

<template>
  <div class="tree">
    <div class="tree-head">
      <span class="tree-root">项目目录</span>
      <button type="button" class="tree-refresh" :disabled="refreshing" @click="refresh">
        {{ refreshing ? '刷新中…' : '刷新' }}
      </button>
    </div>
    <p v-if="rootError" class="tree-error" role="alert">
      {{ rootError }}
      <button type="button" @click="loadDir('.', true)">重试</button>
    </p>
    <p v-else-if="rootLoaded && !rows.length" class="tree-note">这个项目还没有文件。</p>
    <template v-for="row in rows" :key="row.key">
      <div
        v-if="row.kind === 'note'"
        class="tree-note-row"
        :style="{ paddingLeft: `calc(0.55rem + ${row.depth} * 0.9rem)` }"
      >
        <span>{{ row.note }}</span>
        <button v-if="row.retry" type="button" @click="loadDir(row.retry, true)">重试</button>
      </div>
      <button
        v-else
        type="button"
        :class="['tree-row', row.kind, { on: row.kind === 'file' && row.path === activePath }]"
        :style="{ paddingLeft: `calc(0.55rem + ${row.depth} * 0.9rem)` }"
        :title="row.path"
        @click="toggle(row)"
      >
        <svg v-if="row.kind === 'dir'" class="tree-icon" viewBox="0 0 20 20" aria-hidden="true">
          <template v-if="isOpen(row.path)">
            <path d="M3 15.2V5.6h4.3l1.7 1.8H17v1.4H6.4z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" />
            <path d="M6.4 9.3H18l-1.9 6.4H4.8z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" />
          </template>
          <path v-else d="M3 15.2V5.6h4.3l1.7 1.8H17v7.8z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" />
        </svg>
        <svg v-else class="tree-icon" viewBox="0 0 20 20" aria-hidden="true">
          <path d="M5.5 3.5h5.6L15 7.4v9.1H5.5z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" />
          <path d="M11 3.6v3.9h4" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" />
        </svg>
        <span class="tree-name">{{ row.name }}</span>
        <span v-if="row.kind === 'dir'" class="tree-chev">{{ isOpen(row.path) ? '▾' : '▸' }}</span>
      </button>
    </template>
  </div>
</template>

<style scoped>
.tree {
  display: flex;
  flex-direction: column;
  padding: 0 0.35rem 0.6rem;
}
.tree-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: 0 0.25rem 0.35rem;
}
.tree-root {
  margin-right: auto;
  font-size: var(--text-xs);
  color: var(--muted);
  letter-spacing: 0.04em;
}
.tree-refresh {
  border: 0;
  background: none;
  color: var(--accent-ink);
  font-size: var(--text-xs);
  cursor: pointer;
  padding: 0.15rem 0.25rem;
}
.tree-refresh:disabled { color: var(--muted); cursor: default; }

.tree-row {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  width: 100%;
  border: 0;
  border-radius: 8px;
  background: none;
  color: var(--ink-soft);
  font: inherit;
  font-size: var(--text-xs);
  text-align: left;
  padding: 0.3rem 0.55rem;
  cursor: pointer;
}
.tree-row:hover { background: var(--hover); }
.tree-row.on {
  background: var(--accent-soft);
  color: var(--accent-ink);
}
.tree-icon {
  flex: none;
  width: 1rem;
  height: 1rem;
  color: var(--muted);
}
.tree-row.dir .tree-icon { color: var(--accent-ink); }
.tree-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tree-chev {
  flex: none;
  font-size: 0.65rem;
  color: var(--muted);
}

.tree-note,
.tree-error {
  margin: 0.3rem 0.55rem;
  font-size: var(--text-xs);
  color: var(--muted);
  line-height: 1.5;
}
.tree-error { color: var(--danger); }
.tree-error button,
.tree-note-row button {
  border: 0;
  background: none;
  color: var(--accent-ink);
  font-size: var(--text-xs);
  cursor: pointer;
  padding: 0 0.25rem;
}
.tree-note-row {
  display: flex;
  align-items: center;
  gap: 0.35rem;
  padding-top: 0.2rem;
  padding-bottom: 0.2rem;
  font-size: var(--text-xs);
  color: var(--muted);
}
</style>
