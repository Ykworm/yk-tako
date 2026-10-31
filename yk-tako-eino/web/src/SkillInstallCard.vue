<script setup lang="ts">
// 安装确认卡（doc/52 D24 / C5）：只从 AskQuestion.install 渲染。
//
// 这一层是**授权面**，不是聊天内容：模型能写的只有一次 skill_install 工具调用，
// 卡片上的来源、候选全表、选中集、字节/文件数全部由程序解析后放进帧 args（和行 args），
// 组件只照字段画。模型正文里写「[安装]」按钮或链接进不来——它没有字段可落。
//
// 只画「安装」「取消」两个动作：人对一次点击负责，程序再按 owner/admin 核一遍权限
// （端点上的 403 才是闸，这里的隐藏只是不给点了必然失败的按钮）。
import { computed } from 'vue'
import type { AskQuestion, InstallProposal } from './chat'

const props = defineProps<{
  q: AskQuestion
  // 当前登录租户的 owner/admin 才有点击权（与端点同一把尺）。
  canApprove: boolean
  // 这个来源是否在本会话的用户消息里出现过；false 时卡片顶一行提醒。
  seen: boolean
  busy?: boolean
  // 还在等：历史卡片传 false——这一轮已收口，行即使 pending 也没有等待者了。
  live?: boolean
  // 这张卡是不是**底部的操作面**（ask dock）。历史卡片只是档案：即使还活着也不画按钮，
  // 否则同一个提议会同时出现两套「安装/取消」。
  interactive?: boolean
}>()

const emit = defineEmits<{ approve: []; cancel: [] }>()

const prop = computed<InstallProposal | undefined>(() => props.q.install)

const selected = computed(() => new Set(prop.value?.skills ?? []))

// 来源一行：github/gitlab 是 owner/repo（或 namespace/repo），archive 是链接本身，
// upload 没有可再解析的来源串。ref 单独一行标出来——「装哪个版本」是核对的一部分。
const sourceLabel = computed(() => {
  const s = prop.value?.source
  if (!s) return ''
  if (s.kind === 'github') return s.owner && s.repo ? `${s.owner}/${s.repo}` : s.url
  if (s.kind === 'gitlab') return s.namespace && s.repo ? `${s.namespace}/${s.repo}` : s.url
  if (s.kind === 'upload') return '本地上传的包'
  return s.url
})

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}
</script>

<template>
  <section class="install-card" data-testid="install-card">
    <template v-if="prop">
      <dl class="facts">
        <dt>来源</dt>
        <dd><code>{{ sourceLabel }}</code></dd>
        <template v-if="prop.source.ref">
          <dt>版本</dt>
          <dd><code>{{ prop.source.ref }}</code></dd>
        </template>
        <template v-if="prop.source.url && prop.source.url !== sourceLabel">
          <dt>下载地址</dt>
          <dd><code>{{ prop.source.url }}</code></dd>
        </template>
      </dl>

      <p v-if="!seen" class="unseen">此来源在本会话中未出现——请先确认这是你自己要装的东西。</p>

      <p v-if="prop.skills.length" class="chosen">将安装：{{ prop.skills.join('、') }}</p>
      <p v-else class="chosen none">没有选中任何技能——这张卡不会装任何东西。</p>

      <ul class="cands">
        <li
          v-for="c in prop.candidates"
          :key="c.name"
          :class="['cand', { on: selected.has(c.name) }]"
          :data-skill="c.name"
        >
          <span class="mark" aria-hidden="true">{{ selected.has(c.name) ? '✓' : '○' }}</span>
          <div class="cand-body">
            <div class="cand-head">
              <strong>{{ c.name }}</strong>
              <span class="meta">{{ fmtBytes(c.bytes) }} · {{ c.files }} 个文件</span>
              <span v-if="selected.has(c.name)" class="sel">已选</span>
            </div>
            <p class="desc">{{ c.description || '（没有描述）' }}</p>
            <p v-if="c.requires?.length" class="req">依赖：{{ c.requires.join('、') }}</p>
          </div>
        </li>
      </ul>

      <ul v-if="prop.warnings.length" class="warns">
        <li v-for="(w, i) in prop.warnings" :key="i">{{ w }}</li>
      </ul>

      <div v-if="q.status === 'pending' && interactive && canApprove" class="actions">
        <button type="button" class="btn ghost" :disabled="busy" @click="emit('cancel')">取消</button>
        <button type="button" class="btn primary" :disabled="busy" @click="emit('approve')">
          {{ busy ? '正在处理…' : '安装' }}
        </button>
      </div>
      <p v-else-if="q.status === 'pending' && interactive" class="note">已提交，等管理员确认。</p>
      <p v-else-if="q.status === 'pending' && live" class="note">等待确认中——操作在下方输入区。</p>
      <p v-else-if="q.status === 'pending'" class="note">
        这一轮已经收口，等待已结束。要装这个来源，请去技能页重新预览。
      </p>
      <p v-else-if="q.status === 'stale'" class="note warn">
        这条安装请求已失效（暂存目录已被清理），请重新发起。
      </p>
      <p v-else class="result" :class="{ canceled: q.decision === 'cancel' }">
        {{ q.submitted || (q.decision === 'cancel' ? '已取消安装。' : '已处理。') }}
      </p>
    </template>
    <template v-else>
      <p class="note warn">这条安装确认读不出来，未提供任何按钮。要装请在技能页重新预览。</p>
    </template>
  </section>
</template>

<style scoped>
.install-card {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  min-width: 0;
}
.facts {
  display: grid;
  grid-template-columns: 4.2rem minmax(0, 1fr);
  gap: 2px var(--space-2);
  margin: 0;
  font-size: var(--text-sm);
}
.facts dt { color: var(--muted); }
.facts dd { margin: 0; min-width: 0; }
.facts code {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--text-xs);
  word-break: break-all;
}
.unseen {
  margin: 0;
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-control);
  background: var(--warn-soft);
  color: var(--warn);
  font-size: var(--text-sm);
}
.chosen {
  margin: 0;
  color: var(--ink-soft);
  font-size: var(--text-sm);
}
.chosen.none { color: var(--warn); }
.cands {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  border: 1px solid var(--line);
  border-radius: var(--radius-control);
  overflow: hidden;
}
.cand {
  display: flex;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-3);
  border-top: 1px solid var(--line);
  min-width: 0;
}
.cand:first-child { border-top: 0; }
.cand.on { background: var(--accent-soft); }
.mark {
  flex-shrink: 0;
  width: 1rem;
  color: var(--muted);
  font-size: var(--text-sm);
  line-height: 1.6;
}
.cand.on .mark { color: var(--accent-ink); font-weight: 600; }
.cand-body { min-width: 0; flex: 1; }
.cand-head {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.cand-head strong { font-size: var(--text-sm); font-weight: 600; }
.meta { color: var(--muted); font-size: var(--text-xs); }
.sel {
  border-radius: 999px;
  padding: 0 6px;
  background: var(--accent);
  color: #fff;
  font-size: var(--text-xs);
}
.desc {
  margin: 2px 0 0;
  color: var(--ink-soft);
  font-size: var(--text-sm);
  line-height: 1.55;
  white-space: pre-wrap;
}
.req { margin: 2px 0 0; color: var(--muted); font-size: var(--text-xs); }
.warns {
  margin: 0;
  padding-left: 1.1rem;
  color: var(--warn);
  font-size: var(--text-xs);
  line-height: 1.6;
}
.actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-2);
  margin-top: var(--space-1);
}
.btn {
  height: 32px;
  border: 1px solid var(--line-strong);
  background: var(--bg-raised);
  border-radius: var(--radius-control);
  padding: 0 var(--space-3);
  font-size: var(--text-sm);
  font-weight: 500;
}
.btn:hover:not(:disabled) { background: var(--hover); }
.btn:disabled { opacity: 0.55; }
.btn.ghost { color: var(--ink-soft); }
.btn.primary { border-color: var(--accent); background: var(--accent); color: #fff; }
.btn.primary:hover:not(:disabled) { border-color: var(--accent-ink); background: var(--accent-ink); }
.note { margin: 0; color: var(--muted); font-size: var(--text-sm); }
.note.warn { color: var(--warn); }
.result {
  margin: 0;
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-control);
  background: var(--accent-soft);
  color: var(--accent-ink);
  font-size: var(--text-sm);
}
.result.canceled { background: var(--hover); color: var(--muted); }
</style>
