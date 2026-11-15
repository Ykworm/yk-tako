<script setup lang="ts">
// 对话页右侧工作区（doc/78）：固定三枚标签（项目／任务／Todo）+ 按打开顺序追加的文件标签。
// 收起后是一条窄轨，三个入口都在——收起的只是「现在看什么」，不是「能不能进」。
import { computed, ref, watch } from 'vue'
import type { AttentionItem, RunID } from './api'
import RunTodoPanel from './RunTodoPanel.vue'
import TasksView from './TasksView.vue'
import WorkspaceFileView from './WorkspaceFileView.vue'
import WorkspaceTree from './WorkspaceTree.vue'

type FixedTab = 'files' | 'tasks' | 'todo'

const props = defineProps<{
  workspaceId: string
  // doc/80：当前活动会话 id。转发给嵌入的任务页，并作为换会话重挂载的键、处理条限定词的依据。
  sessionId: string | null
  open: boolean
  attentionItems: AttentionItem[]
  reloadNonce: number
  dockFocus: {
    workspaceId: string
    taskId: string
    orchestrationId: string
    questionId: string
    notificationId: string
    nonce: number
  }
  // 父组件递增 nonce 请求打开某个文件（回复结尾的改动清单点进来）。
  fileRequest: { path: string; nonce: number }
  todoRunId: RunID | null
  boardText: string
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  'open-session': [sessionId: string]
  'show-tasks-page': []
  attention: [item: AttentionItem]
}>()

const TAB_KEY = 'tako.chatDockTab'
const FIXED: { key: FixedTab; label: string }[] = [
  { key: 'files', label: '项目' },
  { key: 'tasks', label: '任务' },
  { key: 'todo', label: 'Todo' },
]

// 只记固定标签：文件标签是临时态（刷新不重建，方案已拍），不能把「上次停在某个文件」当偏好。
function readFixedTab(): FixedTab {
  const v = localStorage.getItem(TAB_KEY)
  return v === 'tasks' || v === 'todo' ? v : 'files'
}

const fixedTab = ref<FixedTab>(readFixedTab())
const showFile = ref(false)
const fileTabs = ref<string[]>([])
const activeFile = ref('')

watch(fixedTab, (v) => localStorage.setItem(TAB_KEY, v))

function baseName(path: string): string {
  const i = path.lastIndexOf('/')
  return i < 0 ? path : path.slice(i + 1)
}

const activeTitle = computed(() => {
  if (showFile.value) return `文件 · ${baseName(activeFile.value)}`
  return FIXED.find((t) => t.key === fixedTab.value)?.label ?? '项目'
})

function switchFixed(key: FixedTab) {
  fixedTab.value = key
  showFile.value = false
  emit('update:open', true)
}

function openFile(path: string) {
  if (!path) return
  if (!fileTabs.value.includes(path)) fileTabs.value = [...fileTabs.value, path]
  activeFile.value = path
  showFile.value = true
  emit('update:open', true)
}

function closeFile(path: string) {
  const idx = fileTabs.value.indexOf(path)
  if (idx < 0) return
  const rest = fileTabs.value.filter((p) => p !== path)
  fileTabs.value = rest
  if (!showFile.value || activeFile.value !== path) return
  if (!rest.length) {
    showFile.value = false
    activeFile.value = ''
    return
  }
  // 关掉当前标签后落到右邻；关的是最后一个则落到新的最后一个。
  activeFile.value = rest[Math.min(idx, rest.length - 1)]
}

// 重复打开同一路径只切不新建（标签 key = path）。
watch(
  () => props.fileRequest.nonce,
  () => openFile(props.fileRequest.path),
)

// 处理项定位是「打开任务标签看这条」：nonce 变了就把标签切到任务。
watch(
  () => props.dockFocus.nonce,
  (nonce) => {
    if (nonce > 0) switchFixed('tasks')
  },
)

// 换项目时文件标签全部作废（路径只对原项目有意义）。
watch(
  () => props.workspaceId,
  () => {
    fileTabs.value = []
    activeFile.value = ''
    showFile.value = false
  },
)

// 处理条仍是项目级聚合（doc/80 §4.5）：列表落到「本会话」档时给它加一行限定词标题，
// 把「列表看本会话、处理条看本项目」写出来。嵌入的任务页通过 scopeChange 上报档位。
const tasksScope = ref<'session' | 'project'>(props.sessionId ? 'session' : 'project')
watch(
  () => props.sessionId,
  () => {
    tasksScope.value = props.sessionId ? 'session' : 'project'
  },
)

// 与左侧处理项同一套文案；这里只负责窄轨/标签页内的标签名。
const ATTENTION_LABEL: Record<string, string> = {
  question: '待回答',
  plan_review: '计划待决策',
  interrupted: '已中断，待恢复',
  delivery_failed: '交付失败',
  publication_blocked: '发布受阻',
}
function attentionLabel(item: AttentionItem): string {
  if (item.kind === 'question' && item.count > 1) return `待回答（${item.count}）`
  return ATTENTION_LABEL[item.kind] ?? item.kind
}

function itemKey(item: AttentionItem): string {
  return `${item.kind}:${item.taskId || item.orchestrationId}:${item.notificationId}`
}
</script>

<template>
  <aside
    v-if="workspaceId"
    class="chat-dock"
    :class="{ collapsed: !open }"
    aria-label="工作区"
    data-testid="chat-dock"
  >
    <template v-if="open">
      <header class="dock-head">
        <span class="dock-title" :title="activeTitle">{{ activeTitle }}</span>
        <button
          v-if="!showFile && fixedTab === 'tasks'"
          type="button"
          class="dock-link"
          @click="emit('show-tasks-page')"
        >任务页</button>
        <button
          type="button"
          class="dock-fold"
          aria-label="收起工作区"
          aria-expanded="true"
          @click="emit('update:open', false)"
        >›</button>
      </header>

      <!-- 标签条整行换行：文件标签多了也不出现横向滚动条。 -->
      <nav class="dock-tabs" aria-label="工作区标签">
        <button
          v-for="t in FIXED"
          :key="t.key"
          type="button"
          :class="['dock-tab', { on: !showFile && fixedTab === t.key }]"
          @click="switchFixed(t.key)"
        >{{ t.label }}</button>
        <span
          v-for="path in fileTabs"
          :key="path"
          :class="['dock-tab', 'file', { on: showFile && activeFile === path }]"
        >
          <button type="button" class="dock-tab-open" :title="path" @click="openFile(path)">{{ baseName(path) }}</button>
          <button type="button" class="dock-tab-close" :aria-label="`关闭 ${path}`" @click.stop="closeFile(path)">×</button>
        </span>
      </nav>

      <div class="dock-pane">
        <WorkspaceFileView
          v-if="showFile"
          :workspace-id="workspaceId"
          :path="activeFile"
        />
        <WorkspaceTree
          v-else-if="fixedTab === 'files'"
          :workspace-id="workspaceId"
          :active-path="activeFile"
          @open="openFile"
        />
        <template v-else-if="fixedTab === 'tasks'">
          <!-- 处理条仍是项目级（doc/80 §4.5）：列表在本会话档时写清处理条的范围。 -->
          <p v-if="attentionItems.length && tasksScope === 'session'" class="dock-attn-note">需要我处理 · 本项目</p>
          <ul v-if="attentionItems.length" class="dock-attn" data-testid="dock-attention">
            <li v-for="item in attentionItems" :key="itemKey(item)">
              <button type="button" class="dock-attn-item" @click="emit('attention', item)">
                <span class="dock-attn-title">{{ item.title }}</span>
                <span class="dock-attn-kind">{{ attentionLabel(item) }}</span>
              </button>
            </li>
          </ul>
          <TasksView
            embedded
            :key="`${workspaceId}:${sessionId ?? ''}`"
            :active-workspace-id="workspaceId"
            :active-session-id="sessionId"
            :reload-nonce="reloadNonce"
            :focus-workspace-id="dockFocus.workspaceId"
            :focus-task-id="dockFocus.taskId"
            :focus-orchestration-id="dockFocus.orchestrationId"
            :focus-question-id="dockFocus.questionId"
            :focus-notification-id="dockFocus.notificationId"
            :focus-nonce="dockFocus.nonce"
            @scope-change="tasksScope = $event"
            @open-session="(id) => emit('open-session', id)"
          />
        </template>
        <RunTodoPanel
          v-else
          :run-id="todoRunId"
          :board-text="boardText"
          :reload-nonce="reloadNonce"
        />
      </div>
    </template>

    <!-- 窄轨：三个入口都在，任务入口带处理项计数。 -->
    <div v-else class="dock-rail" role="group" aria-label="工作区入口">
      <button
        v-for="t in FIXED"
        :key="t.key"
        type="button"
        class="dock-rail-item"
        @click="switchFixed(t.key)"
      >
        <span class="dock-rail-title">{{ t.label }}</span>
        <span v-if="t.key === 'tasks' && attentionItems.length" class="dock-rail-count">{{ attentionItems.length }}</span>
      </button>
    </div>
  </aside>
</template>

<style scoped>
.chat-dock {
  flex: none;
  width: clamp(22rem, 30vw, 36rem);
  min-height: 0;
  display: flex;
  flex-direction: column;
  border-left: 1px solid var(--line);
  background: var(--bg);
}
.chat-dock.collapsed { width: 2.4rem; }

.dock-head {
  flex: none;
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: 0.85rem 0.75rem 0.4rem;
}
.dock-title {
  margin-right: auto;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: var(--text-sm);
  font-weight: 600;
}
.dock-link,
.dock-fold {
  flex: none;
  border: 0;
  background: none;
  color: var(--muted);
  font-size: var(--text-xs);
  cursor: pointer;
  padding: 0.15rem 0.25rem;
}
.dock-link:hover,
.dock-fold:hover { color: var(--ink); }

/* 换行不横向滚动：标签多了往下一行排。 */
.dock-tabs {
  flex: none;
  display: flex;
  flex-wrap: wrap;
  gap: 0.25rem;
  padding: 0 0.6rem 0.5rem;
}
.dock-tab {
  border: 1px solid var(--line);
  border-radius: 999px;
  background: var(--bg-raised);
  color: var(--muted);
  font: inherit;
  font-size: var(--text-xs);
  padding: 0.2rem 0.6rem;
  cursor: pointer;
}
.dock-tab:hover { color: var(--ink); }
.dock-tab.on {
  color: var(--accent-ink);
  border-color: var(--accent);
  background: var(--accent-soft);
}
.dock-tab.file {
  display: inline-flex;
  align-items: center;
  gap: 0.1rem;
  padding: 0 0.2rem 0 0.55rem;
  max-width: 100%;
}
.dock-tab-open {
  border: 0;
  background: none;
  color: inherit;
  font: inherit;
  padding: 0.2rem 0.05rem;
  cursor: pointer;
  max-width: 12rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.dock-tab-close {
  border: 0;
  background: none;
  color: var(--muted);
  font-size: 0.85rem;
  line-height: 1;
  padding: 0.15rem 0.25rem;
  cursor: pointer;
}
.dock-tab-close:hover { color: var(--danger); }

.dock-pane {
  flex: 1;
  min-height: 0;
  overflow: auto;
  display: flex;
  flex-direction: column;
}

.dock-attn {
  flex: none;
  list-style: none;
  margin: 0;
  padding: 0 0.6rem 0.5rem;
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  max-height: 9rem;
  overflow: auto;
}
/* 处理条是项目级（doc/80 §4.5）：列表在本会话档时给处理条加一行限定词标题。 */
.dock-attn-note {
  flex: none;
  margin: 0;
  padding: 0 0.6rem 0.25rem;
  font-size: 0.68rem;
  color: var(--muted);
}
.dock-attn-item {
  display: flex;
  flex-direction: column;
  gap: 0.15rem;
  width: 100%;
  text-align: left;
  border: 1px solid var(--line);
  border-radius: 8px;
  background: var(--bg-raised);
  padding: 0.4rem 0.55rem;
  cursor: pointer;
  font: inherit;
  color: inherit;
}
.dock-attn-title { font-size: var(--text-xs); }
.dock-attn-kind { font-size: 0.68rem; color: var(--warn); }

.dock-rail {
  height: 100%;
  width: 100%;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 0.4rem;
  padding: 0.85rem 0;
}
.dock-rail-item {
  border: 0;
  background: none;
  color: var(--muted);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 0.25rem;
  padding: 0.3rem 0;
  cursor: pointer;
  writing-mode: vertical-rl;
}
.dock-rail-item:hover {
  color: var(--ink);
  background: var(--hover);
}
.dock-rail-title { font-size: var(--text-xs); }
.dock-rail-count {
  font-size: 0.7rem;
  color: var(--warn);
  font-weight: 600;
}

/* ≤860px 保持今天的形态：工作区整体不显示，移动端仍走左侧「任务」入口。 */
@media (max-width: 860px) {
  .chat-dock { display: none; }
}
</style>
