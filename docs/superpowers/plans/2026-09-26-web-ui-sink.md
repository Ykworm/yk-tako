# 最小 Web-UI T1：事件 sink 重构 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `adkrun` 的「产生事件」与「呈现事件」拆开——驱动层只产出结构化事件，REPL 退化成第一个 sink；重构前后终端输出逐字符一致（《10》§2.4 步 1）。

**Architecture:** 事件契约（`EventKind` / `Event` / `Sink` / 分类函数 `eventFrom`）留在 `internal/adkrun`——分类要 `IsBoardTool`，属于「产生」的一部分；REPL 的格式化搬到消费方 `cmd/yktako/sink.go`；`Run` / `consume` 穿透 `sink` 参数，`nil` = 不呈现。口径（doc/11 §3.2 T1.1）：T1 只落 4 类呈现事件 `message / tool_call / tool_result / board`；`approval` 不走 sink（将来由消费方 ask 产出，HTTP 在 T2.3 推 SSE）、`error` 由 `Run` 返回调用方。

**Tech Stack:** Go 1.27、eino v0.9.21（`adk` / `schema`）、标准库测试。上游依据：《10》§4 事件契约、doc/11 §3 侦察结论与 T1.1–T1.6 子步骤。

**执行说明（与默认流程的有意偏离，原因可检验）：**
1. 不新建 worktree/分支：改动面 = `yk-tako-eino/{internal/adkrun,cmd/yktako}` + README/doc，与仓库既有「直接提交 master」习惯一致（见 `git log`）。
2. 提交消息按仓库风格写中文短句，不加 `feat:` 前缀；每个 Task 末尾提交一次。
3. Task 0 先清账（P1 移动 + 台账 + 本计划本身），此后每笔提交只含本计划改动。
4. 本计划只覆盖 T1；T2 按《10》§7 等 sink 定型后再出计划。
5. 所有新增/修改代码按 AGENTS.md：中文注释、文件头与函数头齐全，测试与假数据同样适用。

---

## File Structure

| 文件 | 动作 | 职责 |
|---|---|---|
| `yk-tako-eino/internal/adkrun/event.go` | 新建 | 事件契约：`EventKind` 常量、`Event`、`Sink`、分类函数 `eventFrom`；承接包文档（字母序第一个文件） |
| `yk-tako-eino/internal/adkrun/event_test.go` | 新建 | 分类单测（Task 1）+ `consume` 事件序列测试（Task 3） |
| `yk-tako-eino/internal/adkrun/turn.go` | 修改 | `Run` / `consume` 加 `sink` 参数；删 `printMessage`；包头改「本文件：」 |
| `yk-tako-eino/internal/adkrun/turn_test.go` | 修改 | `Run` 调用补 `nil` sink |
| `yk-tako-eino/cmd/yktako/sink.go` | 新建 | REPL sink：事件 → 终端文本（原 `printMessage` 格式逐字符复刻） |
| `yk-tako-eino/cmd/yktako/sink_test.go` | 新建 | 格式锁定测试（字节级断言） |
| `yk-tako-eino/cmd/yktako/main.go` | 修改 | 构造 REPL sink 并注入 `Run` |

不改：`internal/repl`（保持「与 Eino 无关」的承诺）、`internal/taskboard`（集成测试直接用 `runner.Run`，不受 `adkrun.Run` 签名影响，doc/11 §3.1 第 1 条）。

---

## Task 0: 清账提交（P1 收尾 + 台账 + 本计划）

**Files:**
- 已暂存 rename：`doc/10-MAF审批回路与Eino对照.md` → `maf/doc/10-MAF审批回路与Eino对照.md`
- 已修改：`doc/01-任务看板与单入口.md`、`maf/README.md`、`maf/Program.cs`
- 未跟踪：`doc/11-最小Web-UI实施计划.md`、`docs/superpowers/plans/2026-09-26-web-ui-sink.md`

- [ ] **Step 1: 核对待提交清单**

Run: `git status --short`
Expected: 只有上面列出的 rename / 修改 / 两个新文件；`yk-tako-eino/` 下无改动。

- [ ] **Step 2: 提交**

```bash
git add -A doc maf/README.md maf/Program.cs docs/superpowers/plans/2026-09-26-web-ui-sink.md
git commit -m "doc 收尾：MAF 对照篇归入 maf/doc/；最小 Web-UI 台账与 T1 执行计划入档"
```

- [ ] **Step 3: 确认工作区干净**

Run: `git status --short`
Expected: 无输出。

---

## Task 1 (T1.1): 事件契约与分类函数

**Files:**
- Create: `yk-tako-eino/internal/adkrun/event.go`
- Create: `yk-tako-eino/internal/adkrun/event_test.go`
- Modify: `yk-tako-eino/internal/adkrun/turn.go:1-4`（包文档让位：`event.go` 字母序在前，AGENTS.md 注释规范）

- [ ] **Step 1: 写分类测试（先失败）**

`yk-tako-eino/internal/adkrun/event_test.go`：

```go
// 本文件：事件契约的单元测试——eventFrom 的分类口径（《10》§4.2 呈现四类）
// 与 consume 的事件产出顺序逐条钉死；改口径必须同步改这里。
package adkrun

import (
	"strings"
	"testing"

	"yk-tako-eino/internal/taskboard"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// TestEventFrom 覆盖六类输入：工具调用、账本回执（board）、普通工具回执、
// 工具名从 msg 兜底、正文、子代理事件的名字透传。
func TestEventFrom(t *testing.T) {
	callMsg := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read_file"}}})
	replyMsg := schema.ToolMessage("文件内容", "c1", schema.WithToolName("read_file"))
	boardMsg := schema.ToolMessage("📋 看板", "c2", schema.WithToolName(taskboard.ToolList))
	textMsg := schema.AssistantMessage("你好", nil)

	cases := []struct {
		name  string
		agent string
		mv    *adk.MessageVariant
		msg   *schema.Message
		want  Event
	}{
		{
			name: "工具调用", agent: "yk-tako",
			mv:  &adk.MessageVariant{Role: schema.Assistant, Message: callMsg},
			msg: callMsg,
			want: Event{Kind: EventToolCall, Agent: "yk-tako", Role: schema.Assistant, ToolNames: []string{"read_file"}},
		},
		{
			name: "账本回执走看板", agent: "yk-tako",
			mv:  &adk.MessageVariant{Role: schema.Tool, ToolName: taskboard.ToolList, Message: boardMsg},
			msg: boardMsg,
			want: Event{Kind: EventBoard, Agent: "yk-tako", Role: schema.Tool, ToolName: taskboard.ToolList, Text: "📋 看板"},
		},
		{
			name: "普通工具回执", agent: "yk-tako",
			mv:  &adk.MessageVariant{Role: schema.Tool, ToolName: "read_file", Message: replyMsg},
			msg: replyMsg,
			want: Event{Kind: EventToolResult, Agent: "yk-tako", Role: schema.Tool, ToolName: "read_file", Text: "文件内容"},
		},
		{
			name: "工具名从 msg 兜底", agent: "yk-tako",
			mv:  &adk.MessageVariant{Role: schema.Tool, Message: replyMsg},
			msg: replyMsg,
			want: Event{Kind: EventToolResult, Agent: "yk-tako", Role: schema.Tool, ToolName: "read_file", Text: "文件内容"},
		},
		{
			name: "正文", agent: "yk-tako",
			mv:  &adk.MessageVariant{Role: schema.Assistant, Message: textMsg},
			msg: textMsg,
			want: Event{Kind: EventMessage, Agent: "yk-tako", Role: schema.Assistant, Text: "你好"},
		},
		{
			name: "子代理名字透传", agent: "worker",
			mv:  &adk.MessageVariant{Role: schema.Assistant, Message: textMsg},
			msg: textMsg,
			want: Event{Kind: EventMessage, Agent: "worker", Role: schema.Assistant, Text: "你好"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := eventFrom(tc.agent, tc.mv, tc.msg)
			if !ok {
				t.Fatal("eventFrom ok = false，want true")
			}
			if got.Kind != tc.want.Kind || got.Agent != tc.want.Agent || got.Role != tc.want.Role ||
				got.Text != tc.want.Text || got.ToolName != tc.want.ToolName ||
				strings.Join(got.ToolNames, ",") != strings.Join(tc.want.ToolNames, ",") {
				t.Fatalf("eventFrom = %+v，want %+v", got, tc.want)
			}
		})
	}
}

// TestEventFromNoContent 验证没有可呈现内容时静默跳过（旧 printMessage 的空分支语义）：
// nil 消息与空内容都不产出事件。
func TestEventFromNoContent(t *testing.T) {
	if _, ok := eventFrom("yk-tako", &adk.MessageVariant{Role: schema.Assistant}, nil); ok {
		t.Fatal("nil 消息不应产出事件")
	}
	if _, ok := eventFrom("yk-tako", &adk.MessageVariant{Role: schema.Assistant}, &schema.Message{Role: schema.Assistant}); ok {
		t.Fatal("空内容不应产出事件")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/adkrun/ -run 'TestEventFrom'`
Expected: 编译失败，`undefined: eventFrom`、`undefined: EventKind`。

- [ ] **Step 3: 实现 `event.go`**

`yk-tako-eino/internal/adkrun/event.go`：

```go
// Package adkrun 是 ADK Runner 的驱动层：消费 Runner 的事件流并分类产出（呈现归消费方
// sink）、攒历史、跑 interrupt 审批回路，外加一个内存版 checkpoint store。唯一入口
// cmd/yktako 使用；驱动循环在 turn.go，事件契约在本文件。
package adkrun

import (
	"yk-tako-eino/internal/taskboard"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// EventKind 是事件的种类标签，取值对应《10》§4.2 事件契约的呈现四类。
// 只有这四类经过 Sink：approval 由消费方的 ask 实现产出（HTTP 入口在 T2.3 推 SSE）、
// error 由 Run 返回调用方——两者都不属于「呈现」，T1 保持原有行为不变。
type EventKind string

// 呈现事件的四种 Kind（《10》§4.2 表的 message / tool_call / tool_result / board）。
const (
	// EventMessage 是代理正文（助手消息 Content）；子代理的由 sink 决定加前缀。
	EventMessage EventKind = "message"
	// EventToolCall 是模型请求调用工具，一条事件带本批全部工具名。
	EventToolCall EventKind = "tool_call"
	// EventToolResult 是工具回执（Role=Tool 的消息），Text 为原始内容（截断归 sink）。
	EventToolResult EventKind = "tool_result"
	// EventBoard 是账本工具回执的看板投影，Text 含看板与催更原文（sink 不得截断）。
	EventBoard EventKind = "board"
)

// Event 是 adkrun 产出的一条结构化呈现事件。字段只带「发生了什么」：
// 前缀、emoji、截断等格式全部归 sink 决定（REPL sink 复刻旧 printMessage 的终端格式）。
type Event struct {
	Kind      EventKind       // 事件种类，决定 Text / ToolName / ToolNames 哪个有意义
	Agent     string          // 事件所属 agent 名（adk 盖的 AgentName），子代理前缀由 sink 决定
	Role      schema.RoleType // 消息角色：正文/调用为 assistant，回执/看板为 tool
	Text      string          // 正文 / 工具回执原文 / 看板原文
	ToolName  string          // 工具名（tool_result / board 用）
	ToolNames []string        // 工具名列表（tool_call 用，一条助手消息可带多个）
}

// Sink 是事件消费函数：adkrun 只负责产出，打印、推流等呈现由消费方实现。
// 用函数类型而非 interface：单方法、无状态，与同包的 Ask（turn.go）同一风格。
// nil 合法：Run 收到 nil 表示不呈现（只攒历史、收 interrupt），测试与无头调用可用。
type Sink func(Event)

// eventFrom 把一条消息输出分类成呈现事件。ok=false 表示没有可呈现内容（空消息），
// 调用方直接忽略。分类口径与重构前 printMessage 的分支一一对应：
// ToolCalls→tool_call、Role==Tool→tool_result/board、Content→message。
// mv 与 msg 的关系：Role / ToolName 以 mv（框架元数据）优先，msg 兜底。
func eventFrom(agentName string, mv *adk.MessageVariant, msg *schema.Message) (Event, bool) {
	if msg == nil {
		return Event{}, false
	}
	if len(msg.ToolCalls) > 0 {
		names := make([]string, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			names = append(names, call.Function.Name)
		}
		return Event{Kind: EventToolCall, Agent: agentName, Role: msg.Role, ToolNames: names}, true
	}
	if mv.Role == schema.Tool || msg.Role == schema.Tool {
		name := mv.ToolName
		if name == "" {
			name = msg.ToolName
		}
		kind := EventToolResult
		if taskboard.IsBoardTool(name) {
			kind = EventBoard
		}
		return Event{Kind: kind, Agent: agentName, Role: msg.Role, Text: msg.Content, ToolName: name}, true
	}
	if msg.Content != "" {
		return Event{Kind: EventMessage, Agent: agentName, Role: msg.Role, Text: msg.Content}, true
	}
	return Event{}, false
}
```

- [ ] **Step 4: 改 `turn.go` 包头注释（包文档移入 event.go）**

原（`turn.go:1-4`）：

```go
// Package adkrun 是 ADK Runner 的驱动层：事件消费（打印过程、攒历史）与
// interrupt 审批回路，外加一个内存版 checkpoint store。唯一入口 cmd/yktako
// 使用，实现都在本文件。
package adkrun
```

改为：

```go
// 本文件：ADK Runner 的驱动循环——Run 跑一轮、consume 收事件并产出、interrupt 审批。
// 事件契约（Event / Sink / eventFrom 的分类）在 event.go。
package adkrun
```

- [ ] **Step 5: 跑测试与静态检查**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./internal/adkrun/ -v`
Expected: `gofmt` 无输出；`TestEventFrom`、`TestEventFromNoContent`、既有 `TestRunDerivesRootFromAgent` 全过。

- [ ] **Step 6: 提交**

```bash
git add yk-tako-eino/internal/adkrun/event.go yk-tako-eino/internal/adkrun/event_test.go yk-tako-eino/internal/adkrun/turn.go
git commit -m "adkrun：事件契约（Event/Sink/分类）与单测；包文档移入 event.go"
```

---

## Task 2 (T1.3 前半，P2 关闭): REPL sink 与格式锁定

**Files:**
- Create: `yk-tako-eino/cmd/yktako/sink.go`
- Create: `yk-tako-eino/cmd/yktako/sink_test.go`

- [ ] **Step 1: 写格式锁定测试（先失败）**

`yk-tako-eino/cmd/yktako/sink_test.go`：

```go
// 本文件：REPL sink 的格式锁定测试——逐字符断言终端输出，钉住重构前的
// printMessage 格式（《10》§6 关口 2）；改输出格式必须同步改这里。
package main

import (
	"bytes"
	"strings"
	"testing"

	"yk-tako-eino/internal/adkrun"
)

// TestREPLSinkFormat 覆盖四种事件 + 子代理前缀 + 长回执截断六种输出。
func TestREPLSinkFormat(t *testing.T) {
	long := strings.Repeat("a", 130)
	cases := []struct {
		name string
		ev   adkrun.Event
		want string
	}{
		{
			name: "工具调用",
			ev:   adkrun.Event{Kind: adkrun.EventToolCall, Agent: "yk-tako", ToolNames: []string{"read_file", "calculate"}},
			want: "  → 🔧 调用: read_file, calculate\n",
		},
		{
			name: "工具回执",
			ev:   adkrun.Event{Kind: adkrun.EventToolResult, Agent: "yk-tako", ToolName: "read_file", Text: "第一行\n第二行"},
			want: "  ← 🔧 read_file: 第一行 第二行\n",
		},
		{
			name: "回执超长截断",
			ev:   adkrun.Event{Kind: adkrun.EventToolResult, Agent: "yk-tako", ToolName: "read_file", Text: long},
			want: "  ← 🔧 read_file: " + strings.Repeat("a", 120) + "…\n",
		},
		{
			// 看板原文自带「📋 任务看板」头（taskboard/tools.go 的 board），sink 又贴
			// 「  📋 」标记——重构前终端就是两个 📋，逐字符锁死，不许顺手去重。
			name: "看板回执不截断",
			ev:   adkrun.Event{Kind: adkrun.EventBoard, Agent: "yk-tako", ToolName: "TaskList", Text: "📋 看板\n第二行"},
			want: "  📋 📋 看板\n第二行\n",
		},
		{
			name: "正文",
			ev:   adkrun.Event{Kind: adkrun.EventMessage, Agent: "yk-tako", Text: "你好"},
			want: "\n🐙 你好\n\n",
		},
		{
			name: "子代理带前缀",
			ev:   adkrun.Event{Kind: adkrun.EventMessage, Agent: "worker", Text: "子代理说"},
			want: "\n🐙 [worker] 子代理说\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			newREPLSink(&buf, "yk-tako")(tc.ev)
			if got := buf.String(); got != tc.want {
				t.Fatalf("输出不符\nwant: %q\n got: %q", tc.want, got)
			}
		})
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/yktako/ -run 'TestREPLSinkFormat'`
Expected: 编译失败，`undefined: newREPLSink`。

- [ ] **Step 3: 实现 `sink.go`（P2 落点在此定死：`cmd/yktako`）**

`yk-tako-eino/cmd/yktako/sink.go`：

```go
// 本文件：REPL 的 sink——把 adkrun 产出的事件写成终端文本（呈现归消费方）。
package main

import (
	"fmt"
	"io"
	"strings"

	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/repl"
)

// newREPLSink 造一个写终端的事件消费函数，输出格式与重构前 adkrun 的 printMessage
// 逐字符一致（《10》§6 关口 2「REPL sink 打印格式不破」）。
//
// w 由调用方给：main 传 os.Stdout，测试传 bytes.Buffer——格式锁定测试靠它做字节级断言。
// root 是主 agent 名：非 root 的 agent 事件带「[名字] 」前缀（原逻辑搬来）。
// 落点选 cmd/yktako 的理由（P2 关闭）：格式是消费方自己的事；将来 HTTP sink 输出的是
// SSE JSON，与终端文本没有可共享的格式化，不必为此新开 internal/sink。
func newREPLSink(w io.Writer, root string) adkrun.Sink {
	return func(ev adkrun.Event) {
		prefix := ""
		if ev.Agent != "" && ev.Agent != root {
			prefix = "[" + ev.Agent + "] "
		}
		switch ev.Kind {
		case adkrun.EventToolCall:
			fmt.Fprintf(w, "  → 🔧 %s调用: %s\n", prefix, strings.Join(ev.ToolNames, ", "))
		case adkrun.EventBoard:
			// 账本工具的回执自带看板与催更块（taskboard/tools.go 的 withBoard），
			// 原样投影，不走 Clip 截断——截断会把看板截烂。看板原文自带
			// 「📋 任务看板」头，故终端显示「  📋 📋 …」两个标记——这是重构前的
			// 既有格式，本重构只搬不改，不许顺手去重。
			fmt.Fprintf(w, "  📋 %s%s\n", prefix, ev.Text)
		case adkrun.EventToolResult:
			fmt.Fprintf(w, "  ← 🔧 %s%s: %s\n", prefix, ev.ToolName, repl.Clip(ev.Text, 120))
		case adkrun.EventMessage:
			fmt.Fprintf(w, "\n🐙 %s%s\n\n", prefix, ev.Text)
		}
	}
}
```

- [ ] **Step 4: 跑测试通过**

Run: `gofmt -l . && go test ./cmd/yktako/ -v`
Expected: `gofmt` 无输出；`TestREPLSinkFormat` 六个子用例全过。

- [ ] **Step 5: 提交**

```bash
git add yk-tako-eino/cmd/yktako/sink.go yk-tako-eino/cmd/yktako/sink_test.go
git commit -m "yktako：REPL sink（事件→终端）与格式锁定测试；P2 落点定在 cmd/yktako"
```

---

## Task 3 (T1.2 + T1.3 后半 + T1.4): 驱动层接线

**Files:**
- Modify: `yk-tako-eino/internal/adkrun/event_test.go`（追加 consume 序列测试）
- Modify: `yk-tako-eino/internal/adkrun/turn.go`（`Run` / `consume` / 删 `printMessage` / imports）
- Modify: `yk-tako-eino/internal/adkrun/turn_test.go:61`
- Modify: `yk-tako-eino/cmd/yktako/main.go`（构造 sink + 注入）
- Modify: `yk-tako-eino/internal/taskboard/tools.go:29`（`IsBoardTool` 注释里引用的 `printMessage` 改指 `eventFrom`——删函数后该引用会悬空，AGENTS 禁过时注释）

- [ ] **Step 1: 先追加 consume 事件序列测试（失败）**

`event_test.go` 末尾追加：

```go
// TestConsumeEmitsEvents 用脚本事件流验证 consume 的产出顺序与载荷：
// 工具调用 → 看板回执 → 正文（T1.5 的事件序列断言）；同时确认 root 消息仍进历史。
func TestConsumeEmitsEvents(t *testing.T) {
	callMsg := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: taskboard.ToolList}}})
	boardMsg := schema.ToolMessage("📋 看板", "c1", schema.WithToolName(taskboard.ToolList))
	textMsg := schema.AssistantMessage("收尾", nil)

	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer gen.Close()
		for _, m := range []*schema.Message{callMsg, boardMsg, textMsg} {
			gen.Send(&adk.AgentEvent{
				AgentName: "yk-tako",
				Output: &adk.AgentOutput{MessageOutput: &adk.MessageVariant{
					Role:     m.Role,
					ToolName: m.ToolName,
					Message:  m,
				}},
			})
		}
	}()

	var got []Event
	msgs, interrupts, err := consume(iter, "yk-tako", func(ev Event) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(interrupts) != 0 {
		t.Fatalf("interrupts 应为空，实际 %v", interrupts)
	}
	if len(got) != 3 || got[0].Kind != EventToolCall || got[1].Kind != EventBoard || got[2].Kind != EventMessage {
		t.Fatalf("事件序列 = %+v，want [tool_call board message]", got)
	}
	if len(msgs) != 3 {
		t.Fatalf("history 应收到 3 条 root 消息（含工具回执），实际 %d", len(msgs))
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/adkrun/ -run 'TestConsumeEmitsEvents'`
Expected: 编译失败，`consume` 参数不匹配（当前只有 2 参）。

- [ ] **Step 3: 改 `turn.go`**

3a. imports 去掉 `strings`、`yk-tako-eino/internal/repl`、`yk-tako-eino/internal/taskboard`（前两者只被 `printMessage` 用，`taskboard` 的判定已移入 `event.go`），最终为：

```go
import (
	"context"
	"fmt"
	"sync"

	"yk-tako-eino/internal/guard"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)
```

3b. `Run` 加 `sink Sink` 参数（位置在 `input` 之后、`ask` 之前），注释同步补 nil 语义、把「子代理的过程只打印」改为「只产事件」：

```go
// Run 跑一轮，遇到 interrupt 就问人再 Resume，直到这轮结束。
// agent 是主 agent：过滤历史用的 root 名从它自己的 Name 取，不再让脚本另传一份。
// 以前 root 要脚本手写，和 ChatModelAgentConfig.Name 写重复——对不上时不报错也不 panic，
// 只是历史悄悄变空、子代理前缀不打了，属于最难查的那种错。
// 事件里的 AgentName 用的就是同一个 Name：adk 的 flowAgent 直接内嵌 Agent
// （adk/flow.go:42），Name 原样转发给里层的 ChatModelAgent。
// 只有 root 的消息回到历史，子代理的过程只产事件。
//
// sink 收每条呈现事件（《10》§4.2 的 message/tool_call/tool_result/board 四类）；
// 传 nil 合法，表示不呈现（只攒历史、收 interrupt），无头调用与测试可用。
//
// 参数类型用 adk.Agent（github.com/cloudwego/eino/adk 包 interface.go:467 的
// TypedAgent[*schema.Message] 别名，Name 在 :454 声明）：这里只要 Name，runner 自己拿着 agent。
func Run(ctx context.Context, runner *adk.Runner, agent adk.Agent, checkpointID string, input []*schema.Message, sink Sink, ask Ask) ([]*schema.Message, error) {
	root := agent.Name(ctx)
	var opts []adk.AgentRunOption
	if checkpointID != "" {
		opts = append(opts, adk.WithCheckPointID(checkpointID))
	}
	iter := runner.Run(ctx, input, opts...)
	var history []*schema.Message
	for {
		msgs, interrupts, err := consume(iter, root, sink)
		history = append(history, msgs...)
		if err != nil {
			return history, err
		}
		if len(interrupts) == 0 {
			return history, nil
		}
		if ask == nil {
			return history, fmt.Errorf("agent 暂停等待审批，但这个脚本没有接审批")
		}
		targets := map[string]any{}
		for _, point := range interrupts {
			info := approvalFrom(point.Info)
			if info.Tool == "" {
				info.Message = fmt.Sprint(point.Info)
			}
			decision, err := ask(info)
			if err != nil {
				return history, err
			}
			targets[point.ID] = decision
		}
		iter, err = runner.ResumeWithParams(ctx, checkpointID, &adk.ResumeParams{Targets: targets})
		if err != nil {
			return history, err
		}
	}
}
```

3c. `consume` 加 `sink Sink` 参数，调用点改为「分类构事件 → sink」；history 攒取与 interrupt 收集一行不动：

```go
// consume 收完一轮事件：产出呈现事件、攒主 agent 的消息、收 interrupt。
func consume(iter *adk.AsyncIterator[*adk.AgentEvent], root string, sink Sink) ([]*schema.Message, []*adk.InterruptCtx, error) {
	var msgs []*schema.Message
	var interrupts []*adk.InterruptCtx
	if iter == nil {
		return nil, nil, fmt.Errorf("agent 没有返回事件")
	}
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			return msgs, interrupts, event.Err
		}
		if event.Output != nil && event.Output.MessageOutput != nil {
			msg, err := event.Output.MessageOutput.GetMessage()
			if err != nil {
				return msgs, interrupts, err
			}
			if sink != nil {
				// 分类（含 IsBoardTool）在驱动层完成：HTTP sink 将来只做呈现，
				// 不能再把 taskboard 的判定重做一遍。
				if ev, ok := eventFrom(event.AgentName, event.Output.MessageOutput, msg); ok {
					sink(ev)
				}
			}
			if msg != nil && event.AgentName == root {
				msgs = append(msgs, msg)
			}
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			for _, point := range event.Action.Interrupted.InterruptContexts {
				if point != nil && point.IsRootCause {
					interrupts = append(interrupts, point)
				}
			}
			if len(interrupts) == 0 {
				interrupts = append(interrupts, event.Action.Interrupted.InterruptContexts...)
			}
		}
	}
	return msgs, interrupts, nil
}
```

3d. 删除整个 `printMessage` 函数（原 `turn.go:156-193`，含其注释）。`approvalFrom` 及其后的 `Store` 相关代码不动。

3e. 同步 `internal/taskboard/tools.go:29` 的注释（质量审查发现的原计划遗漏）：

原：`// IsBoardTool 判断工具名是不是四个记账工具之一。adkrun 的 printMessage 用它做看板投影。`
改：`// IsBoardTool 判断工具名是不是四个记账工具之一。adkrun 的 eventFrom 用它把回执分到看板事件。`

- [ ] **Step 4: 接线 `turn_test.go` 与 `main.go`**

`turn_test.go:61`：

```go
	history, err := Run(ctx, runner, agent, "", []*schema.Message{schema.UserMessage("在吗")}, nil, nil)
```

`main.go`（`runner` 创建之后、REPL 循环之前加一行；`Run` 调用补 `sink`）：

```go
	// 呈现归消费方：REPL sink 写 stdout；root 从 agent 自己取，与 adkrun.Run 同源。
	sink := newREPLSink(os.Stdout, agent.Name(ctx))
```

```go
		produced, err := adkrun.Run(ctx, runner, agent, fmt.Sprintf("turn-%d", turn), messages, sink, func(info guard.ApprovalInfo) (string, error) {
```

- [ ] **Step 5: 全量检查**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./... -race`
Expected: `gofmt` 无输出；全部包测试通过（含 taskboard 集成测试——它们走 `runner.Run`，不受影响）。

- [ ] **Step 6: 提交**

```bash
git add yk-tako-eino/internal/adkrun/event_test.go yk-tako-eino/internal/adkrun/turn.go yk-tako-eino/internal/adkrun/turn_test.go yk-tako-eino/cmd/yktako/main.go yk-tako-eino/internal/taskboard/tools.go
git commit -m "adkrun：事件产出接线，打印从驱动层搬走；Run/consume 加 sink（nil=不呈现）"
```

---

## Task 4 (T1.5): 验收关口

**Files:** 无（发现问题回到对应 Task 修）

- [ ] **Step 1: 机器验收（《10》§6 关口 2）**

```bash
cd yk-tako-eino
gofmt -l .
go build ./...
go vet ./...
go test ./... -race
grep -rn 'fmt.Printf' internal/adkrun/
```

Expected: `gofmt` 无输出；build/vet 通过；测试全绿；`grep` 无输出（呈现彻底出包）。

- [ ] **Step 2: 人工判定（需要 `.env` 配好模型 key；没有可跳过并记录）**

```bash
cd yk-tako-eino && go run ./cmd/yktako
```

输入一句多步任务（例：`帮我看看：先查现在时间，再算 1234*5678，最后写个总结`），对照四条输出格式：

- 工具调用：`  → 🔧 调用: <工具名>`（子代理带 `[名字] ` 前缀）
- 工具回执：`  ← 🔧 <工具名>: <单行内容，超 120 字带 …>`
- 看板：`  📋 <看板原文，多行不截断>`
- 正文：空行 + `🐙 <内容>` + 空行

内容因模型而异，**只看格式**是否与重构前一致。

- [ ] **Step 3: 记录验收结论**

把结果（含无法人工验收的原因，若跳过）写进 Task 5 的台账日志。

---

## Task 5 (T1.6): 文档同步与台账

**Files:**
- Modify: `yk-tako-eino/README.md:73`
- Modify: `doc/01-任务看板与单入口.md:85`
- Modify: `doc/07-任务规划与看板.md:160`
- Modify: `doc/10-最小Web-UI.md`（§4 锚点 + `approval` 行口径）
- Modify: `doc/09-设计了解.md`（§5/§11 锚点刷新）
- Modify: `doc/11-最小Web-UI实施计划.md`（状态、P2、日志）
- 复查: 根 `README.md`

- [ ] **Step 1: `yk-tako-eino/README.md:73`**

原：`internal/adkrun    事件打印（含看板投影）、interrupt 审批回路、内存 checkpoint`
改：`internal/adkrun    事件产出（Event/Sink，呈现归消费方）、interrupt 审批回路、内存 checkpoint`

- [ ] **Step 2: `doc/01-任务看板与单入口.md:85`**

原：`- **流式投影**：`adkrun` 的 `printMessage` 认四个账本工具名，回执格式化打印 📋 看板`
改：`- **流式投影**：`adkrun` 按账本工具名把回执分类成 board 事件，📋 看板投影由消费方 sink 打印（REPL sink 在 `cmd/yktako/sink.go`）`

- [ ] **Step 3: `doc/07-任务规划与看板.md:160`**

原：`| 流式投影（`adkrun` 认看板工具，原样打印 📋 看板） | `internal/adkrun/turn.go` | ✅ Task 4 |`
改：`| 流式投影（分类归 `adkrun`（`IsBoardTool`），打印归 REPL sink） | `internal/adkrun/event.go` + `cmd/yktako/sink.go` | ✅ Task 4 |`

- [ ] **Step 4: `doc/10-最小Web-UI.md` 锚点刷新**

先跑 `grep -n 'printMessage\|turn\.go' doc/10-最小Web-UI.md`，逐处替换（行号以执行时为准）：

- `:28-29`、`:120`：`printMessage` / `turn.go:157` 的指向改为「分类在 `internal/adkrun/event.go`（`eventFrom`），格式化在 `cmd/yktako/sink.go`」；
- `:123`：「`printMessage` 整体搬过去」改为「分类留 `adkrun.eventFrom`，格式化搬 `cmd/yktako/sink.go`」；
- `:132`、`:137-140`、`:144`：`consume` / 分类行号 → 新 `turn.go` 的 `consume` 行号与 `event.go` 的 `eventFrom` 行号；
- `:140`：`approval` 行末尾补「（T1 口径：由消费方 ask 产出，不经 sink）」；
- `:154`、`:159`：`ask` / `ResumeWithParams` 行号按新 `turn.go` 刷新；
- `:169`：`Store`（`turn.go:25`）确认是否漂移，未动则不改。

- [ ] **Step 5: `doc/09-设计了解.md` 锚点刷新**

先跑 `grep -n 'turn\.go:' doc/09-设计了解.md`（约 15 处），逐处按新代码刷新；重点：

- `:157-160`（子代理前缀）：不再在 `adkrun`，改指 `cmd/yktako/sink.go` 的 `newREPLSink`；
- `:134`（筛历史）：`consume` 新行号；
- `Run` 入口 / `root` / `checkpointID`（原 `:74/:75/:83/:77-79/:106`）：按新 `turn.go` 刷新；
- `:142-150`（interrupt 过滤）、`:98-110`（写入端）：按新 `turn.go` 刷新；
- `:64-70`（Run 注释里 root 那笔账）：按新注释行号刷新；
- `:20/:30-31`（Store 与编译期断言）：未动则不改。

- [ ] **Step 6: 根 `README.md` 复查**

Run: `grep -n '打印\|事件\|adkrun' README.md`
Expected: 无涉及 `adkrun` 打印职责的表述；有则同步改，没有则跳过。

- [ ] **Step 7: 更新 `doc/11-最小Web-UI实施计划.md`**

- §1 状态表：T1 行改 ✅（带 commits 与日期）；
- §3.2：T1.1–T1.6 全部勾 ✅；
- §3.1 第 2 条：「待 T1.3 定夺并记录理由」改为「已定：`cmd/yktako`（P2 关闭，理由记在 `sink.go` 注释）」；
- §5：P2 行改 ✅（落点 `cmd/yktako`，理由一行）；
- §3.4：清单逐项勾 ✅（`doc/02` 与 `maf/doc/10-MAF…` 两项「不改」同样勾）；
- §6：倒序追加 T1 完成日志（日期 | T1 | 各 commit | 摘要，含 Task 4 验收结论）。

- [ ] **Step 8: 提交**

```bash
git add yk-tako-eino/README.md doc/01-任务看板与单入口.md doc/07-任务规划与看板.md doc/09-设计了解.md doc/10-最小Web-UI.md doc/11-最小Web-UI实施计划.md
git commit -m "doc：T1 事件 sink 完成——锚点刷新、README 与台账同步"
```

---

## 不在本计划范围（防范围膨胀，依据 doc/11 §5 与《10》§5）

- `approval` / `error` 的事件化：T2.3 再定（approval 目前由消费方 ask 产出）；
- token 级 delta、HTTP + SSE、会话存储、embed 单页：T2，等 T1 定型后另出计划；
- `internal/repl` 的任何改动（保持「与 Eino 无关」）。

---

## 执行偏差记录（执行后补记，2026-09-26）

计划在实施中被两轮审查修正，代码以提交为准，偏差逐条记录：

| 位置 | 计划原文 | 实际执行 | 依据 |
|---|---|---|---|
| Task 1 测试 | `TestEventFrom` 6 用例 | 8 用例（补「调用与正文并存时弃正文」「回执角色只取 msg」） | 质量审查：钉分类优先级与 mv.Role 路径 → `eaa8e41` |
| Task 1 注释 | 「Role / ToolName 以 mv 优先，msg 兜底」 | 改为「tool 判定取 OR；ToolName mv 优先、msg 兜底；Event.Role 一律取 msg」 | 同上（注释虚述） |
| Task 2 看板用例 | `want: "  📋 看板\n第二行\n"`（计划笔误） | `"  📋 📋 看板\n第二行\n"`——旧 printMessage 同样双 marker，逐字符一致优先 | 实现者发现、双审确认 |
| Task 2 测试加固 | 6 用例 | 11 用例（前缀矩阵、空 Agent、未知 Kind）+ `default` 注释 + `taskboard.ToolList` | 质量审查 → `6950f85` |
| Task 3 范围 | 未列 `taskboard/tools.go` | 增补 3e：`IsBoardTool` 注释改指 `eventFrom` | 质量审查发现悬空引用 |
| Task 3 测试加固 | 序列测试只断言 Kind | 断言载荷 + 子代理透传；新增 interrupt 同帧测试（后改名并钉根因过滤） | 质量审查 → `d609d3d`、`6ecd330` |
| Task 5 收尾 | Step 8 的 git add 清单未含计划文件本身；doc/10 §1 的 main.go 锚点首轮漏刷 | 计划文件随本次 doc 提交入库；main.go 锚点于终审修正 | 质量审查 → 本修正提交 |
| 终审 | `Run`→`sink` 转发无回归、`Event.Role` 注释不精确 | Run 级事件断言 + 字段注释补正 | 终审审查 → 本提交 |

T1 提交对应：Task 0 `e1aafd9`；Task 1 `31e88a6`+`eaa8e41`；Task 2 `12e08c0`+`6950f85`；Task 3 `c0e4c7b`+`d609d3d`+`6ecd330`；Task 4 验收（无代码提交，证据见 doc/11 §6）；Task 5 本文件所在提交。
