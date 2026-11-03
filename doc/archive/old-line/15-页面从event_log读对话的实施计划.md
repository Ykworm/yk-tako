# 15 · 页面从 event_log 读对话的实施计划

> 性质：实施计划。按《14》的读法改代码。MQ 同步写入落地后，窗口帧丢失问题消失。
> 本计划替代 doc/14 的交接说明。实施完成后删除 doc/14。

## 0. 目标

刷新或重新打开页面时，已结束的对话从 `event_log` 按会话懒加载画出来。Chrome 不再保存助手正文。

## 1. 新增 API：按会话读历史

### 1.1 路由

```
GET /api/session/history?session=<id>&before=<cursor>&limit=<n>
```

- `session`：必填，会话 id
- `before`：分页游标，格式 `<created_at>|<run_id>`，为空表示取最新一页
- `limit`：页大小，缺省 20，上限 50

### 1.2 响应

```json
{
  "runs": [
    {
      "runId": "r_xxx",
      "createdAt": "2026-09-30T10:00:00Z",
      "status": "completed",
      "events": [ /* sseEvent 数组，按 seq 升序 */ ]
    }
  ],
  "nextCursor": "2026-09-30T09:00:00Z|r_yyy"
}
```

- `runs` 按 `created_at` 降序（最新的在前）
- `nextCursor` 为空表示没有更早的 run

### 1.3 后端实现

在 `logdb.go` 新增：

```go
// HistoryRun 是一条已结束 run 的档案和事件。
type HistoryRun struct {
    RunID     string     `json:"runId"`
    CreatedAt string     `json:"createdAt"`
    Status    string     `json:"status"`
    Events    []sseEvent `json:"events"`
}

// HistoryPage 是一页历史 run。
type HistoryPage struct {
    Runs       []HistoryRun `json:"runs"`
    NextCursor string       `json:"nextCursor"`
}

// HistoryRuns 返回会话里已结束的 run，按 created_at 降序，最多 limit 条。
// before 非空时，只返回 created_at < before.time 或 (created_at = before.time AND run_id < before.runId) 的行。
// 不返回 status='running' 的行——正在跑的 run 由 /api/session 报告，页面用 from=0 接上。
func (d *LogDB) HistoryRuns(ctx context.Context, sessionID string, beforeTime, beforeRunID string, limit int) ([]HistoryRun, error)

// HistoryPage 组装一页历史，包含 run 档案和事件。
func (d *LogDB) HistoryPage(ctx context.Context, sessionID string, cursor string, limit int) (HistoryPage, error)
```

在 `server.go` 新增：

```go
mux.HandleFunc("GET /api/session/history", s.handleHistory)

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request)
```

### 1.4 查询逻辑

```sql
-- 第一页（cursor 为空）
SELECT run_id, created_at, status
  FROM run_archive
 WHERE session_id = ? AND status != 'running'
 ORDER BY created_at DESC, run_id DESC
 LIMIT ?

-- 后续页（cursor = "2026-09-30T10:00:00Z|r_xxx"）
SELECT run_id, created_at, status
  FROM run_archive
 WHERE session_id = ? AND status != 'running'
   AND (created_at < ? OR (created_at = ? AND run_id < ?))
 ORDER BY created_at DESC, run_id DESC
 LIMIT ?
```

对每个 run，用现有 `readAll` 读事件。

## 2. 前端改动

### 2.1 删除的代码

- `backupKey()` 函数
- `loadBackups()` 函数
- `rememberBackup()` 函数
- `restoreBackups()` 函数
- `renderMessage` 里的 `rememberBackup(ev)` 调用
- 页面打开时的 `restoreBackups(false)` 调用
- 页面打开时遍历 `loadBackups()` 画气泡的代码

### 2.2 新增的代码

**状态**：

```javascript
const history = {
  loading: false,   // 正在请求历史
  done: false,      // 没有更早的 run 了
  cursor: '',       // 下一页游标
  runs: new Set(),  // 已经画过的 runId，避免重复
};
```

**滚到顶触发**：

```javascript
log.addEventListener('scroll', () => {
  if (log.scrollTop < 50 && !history.loading && !history.done) {
    loadHistory();
  }
});
```

**加载历史**：

```javascript
async function loadHistory() {
  if (history.loading || history.done) return;
  history.loading = true;
  try {
    const params = new URLSearchParams({ session: sessionId });
    if (history.cursor) params.set('before', history.cursor);
    const resp = await fetch('/api/session/history?' + params);
    if (!resp.ok) throw new Error(resp.status);
    const page = await resp.json();
    renderHistoryPage(page.runs);
    history.cursor = page.nextCursor || '';
    history.done = !page.nextCursor;
  } catch (e) {
    console.error('加载历史失败', e);
  } finally {
    history.loading = false;
  }
}
```

**渲染历史**：

```javascript
function renderHistoryPage(runs) {
  // runs 按 created_at 降序，画的时候要反过来（最早的在上面）
  for (let i = runs.length - 1; i >= 0; i--) {
    const run = runs[i];
    if (history.runs.has(run.runId)) continue;
    history.runs.add(run.runId);
    // 记住当前滚动位置，插入后恢复
    const scrollBottom = log.scrollHeight - log.scrollTop;
    for (const ev of run.events) {
      renderHistoryEvent(ev, run.runId);
    }
    log.scrollTop = log.scrollHeight - scrollBottom;
  }
}

function renderHistoryEvent(ev, runId) {
  // 历史回放：不弹审批卡，不改状态
  switch (ev.kind) {
    case 'start':
      // localEcho=false，画成用户气泡
      if (ev.text) appendUser(ev.text);
      break;
    case 'message':
      // 直接画，不用 openBubbles（不是流式）
      appendAssistant(ev.text, ev.agent);
      break;
    case 'tool_call':
      appendToolCall(ev.toolCalls, ev.agent);
      break;
    case 'tool_result':
      appendToolResult(ev.toolName, ev.text, ev.agent);
      break;
    case 'board':
      renderBoard(ev.text);
      break;
    case 'compact':
      appendNote(ev.text || '正在压缩');
      break;
    case 'error':
      // 画出来但不改状态
      appendError(ev.text);
      break;
    case 'stop':
      // 画出来但不改状态
      appendNote('已停止');
      break;
    case 'approval':
    case 'approval_result':
    case 'complete':
      // 历史里不弹审批卡，不处理结果
      break;
  }
}
```

### 2.3 页面打开顺序

```javascript
(async () => {
  // 1. 检查有没有正在跑的 run
  await attachLive();
  // 2. 如果有 liveRun，它会被 attachLive 用 from=0 接上，不画进历史
  // 3. 加载第一页历史
  await loadHistory();
  // 4. 滚到底部
  log.scrollTop = log.scrollHeight;
})();
```

`attachLive` 里拿到 `liveRun` 后，把它加进 `history.runs`，避免历史里重复画。

### 2.4 空态文案更新

```javascript
function showEmpty() {
  // ...
  el('p', '', '对话记录保存在服务端。刷新页面后会从服务端加载。'),
  // ...
}
```

## 3. 后端改动

### 3.1 logdb.go

新增 `HistoryRun`、`HistoryPage` 类型和 `HistoryPage` 方法。

### 3.2 server.go

新增 `handleHistory` 路由和处理函数。

### 3.3 保留的部分

- `RestoreMessage` 和 `handleRestore` 暂时保留，等 MQ 落地后再删
- `CompletedTurns` 不改，它只读 `produced_json IS NOT NULL` 的行，出错/停止的 run 不在里面
- `BeginRun` 签名不改
- `AppendSealed` 时序不改

## 4. 测试

### 4.1 logdb_test.go

新增：
- `TestHistoryPage_FirstPage`：无 cursor，返回最新 N 条
- `TestHistoryPage_NextPage`：有 cursor，返回更早的
- `TestHistoryPage_ExcludesRunning`：status='running' 的 run 不在结果里
- `TestHistoryPage_SameCreatedAt`：同一秒多条 run，用 run_id 区分

### 4.2 server_test.go

新增：
- `TestHandleHistory_OK`：正常请求
- `TestHandleHistory_MissingSession`：400
- `TestHandleHistory_InvalidCursor`：400

## 5. 文档更新

实施完成后：
1. 删除 `doc/14-对话不保存在浏览器.md`
2. 更新 `doc/10-最小Web-UI.md` §4.4：删掉"浏览器把已写完的正文记在本地"的描述
3. 更新 `docs/superpowers/plans/2026-09-29-webui-memory-read.md` 任务 8：在末尾注明本计划接替了这一条
4. 更新 `docs/superpowers/specs/2026-09-28-webui-stream-hub-design.md` D1：改为"历史从 event_log 按会话懒加载"

## 6. 不做的事

- 不改 `BeginRun` 签名
- 不重做上下文压缩
- 不把助手正文改成"整轮结束才写入 event_log"
- 不把 `seq` 或 `from` 写进 Chrome
- 不改 `model-windows.json`
- 不删 `RestoreMessage`（等 MQ 落地后再删）

## 7. 任务清单

- [x] 1. logdb.go：新增 HistoryRun、HistoryPage 类型
- [x] 2. logdb.go：实现 HistoryPage 方法
- [x] 3. logdb_test.go：新增历史分页测试
- [x] 4. server.go：新增 handleHistory 路由
- [x] 5. server_test.go：新增历史接口测试
- [x] 6. index.html：删除备份相关代码
- [x] 7. index.html：新增历史懒加载代码
- [x] 8. index.html：更新空态文案
- [x] 9. 更新 doc/10 §4.4
- [x] 10. 更新 superpowers plan 任务 8
- [x] 11. 更新 superpowers spec D1
- [x] 12. 删除 doc/14
