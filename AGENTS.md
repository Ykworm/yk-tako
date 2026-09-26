# AGENTS.md —— yk-tako 仓库协作规则

## 仓库结构

- `yk-tako-eino/`：主线实现（Go + Eino），唯一入口 `cmd/yktako`，活跃开发。
- `maf/`：MAF（.NET）对照实现，**冻结存档**，不再推进。
- `doc/`：定案与实现记录。2026-09-26 起新系列（旧 01–06 已随单入口收敛删除）。

## 注释规则（硬性要求）

**一律中文注释。** 标识符、包名、接口名、协议名等专有名词保留原文。

1. **每个文件**都在 `package` 声明正上方写文件头注释，说明本文件负责什么。每个包里
   字母序第一个文件用 `Package xxx ...` 开头作为包文档，其余文件用 `本文件：……` 开头。
2. **每个 type / struct / interface / func / method / 常量组 / 非显然字段**都要有中文注释。
3. **测试文件同样适用**：文件头、Test 函数、假模型（fake/mock）类型与方法一样要写。
4. 注释解释「为什么、使用场景、约束、坑点」，不要只复述代码。
5. 行为改了必须同步改注释，禁止留过时注释（例：标记从 session 挪到 run-local 后，
   相关注释一并更新）。

## 接口实现 / 继承必须标清楚（重点）

Eino 大量靠「接口 + 嵌入（embedding）」组织代码，只读方法签名看不出它是谁要求的钩子。
所以规定：

1. **类型注释**里必须写清楚：
   - 实现了哪个接口：`import 路径 + 所在文件 + 接口名`（行号、依赖版本可写可不写，写了更好）；
   - `embed` 进来的基类（如 `adk.BaseChatModelAgentMiddleware`）要说明它提供什么；
   - 覆盖了接口里的哪些方法/钩子。
2. **每个实现接口的方法注释**要讲清三层，缺一不可：
   - **接口归属**：实现的哪个接口、哪个方法/钩子（读者常常直接跳到方法定义）；
   - **钩子的作用**：框架为什么定义它、什么时候被调用、输入输出在生命周期里代表什么
     ——这层语义光看本仓库实现读不出来，不写就得去翻依赖源码；
   - **本实现的目的与效果**：借这个钩子达成什么、系统行为因此有什么不同、有什么约束。
   只写「实现 XXX 钩子」再复述一遍代码动作（如「把工具追加到工具表」）不算合格。
3. **编译期断言**：实现接口的类型，旁边加一行 `var _ pkg.Interface = (*Type)(nil)`，
   并用中文注释说明校验的是什么。接口属于 Eino、标准库还是本仓库，格式一致。
4. 同一条规则适用于所有语言/框架（TS 实现已删，但若之后再引入其他栈，照此执行）。

示例（本仓库真实写法，见 `yk-tako-eino/internal/guard/middleware.go`）：

```go
// toolGuardHandler 是命令守卫：扫规则命中，按模式拦、问或放行。
// ……
//
// 实现接口：adk.ChatModelAgentMiddleware —— github.com/cloudwego/eino/adk 包
// handler.go:139 定义的 TypedChatModelAgentMiddleware[*schema.Message]（eino v0.9.21；
// 第 259 行起别名 ChatModelAgentMiddleware）。内嵌 adk.BaseChatModelAgentMiddleware
// （同文件 :277）提供其余钩子的空实现，本类型覆盖 WrapInvokableToolCall。
type toolGuardHandler struct {
	adk.BaseChatModelAgentMiddleware
	engine *Engine
}

// 编译期校验 toolGuardHandler 实现了 adk.ChatModelAgentMiddleware。
var _ adk.ChatModelAgentMiddleware = (*toolGuardHandler)(nil)

// WrapInvokableToolCall 实现 adk.ChatModelAgentMiddleware 的 WrapInvokableToolCall
// 钩子（github.com/cloudwego/eino/adk/handler.go:193）：框架在每次工具即将执行时调用它，
// 传入真执行的 endpoint、接收包过一层的新 endpoint——middleware 就是靠这个挂进工具执行链。
// 本实现借这层拦截：先认自己那笔中断的续跑，再扫规则决定放行、打断还是拒绝。
func (h *toolGuardHandler) WrapInvokableToolCall(...) { ... }
```

标准库接口同样标注（例如某类型实现 `encoding/json` 的 `Marshaler` 时，在方法注释里写明）。
经 `utils.InferTool` 生成的工具，也要在函数注释里注明它最终实现
`tool.InvokableTool`（`components/tool/interface.go:42`），示例见 `internal/taskboard/tools.go`。

## 表达风格（对话与文档）

回答、设计讨论、`doc/` 文档一律按**推理链**展开，禁止跳跃式断言：

1. **先立前提再下结论**：每个结论前先给出它依赖的前提/事实；没有前置论证的断言不许出现。
2. **一步一推进**：按「前提 → 推导 → 结论 → 潜在反例/边界」展开，上一步没站稳不进下一步；
   每段只推进一步推理。
3. **说清「为什么」**：给出选项时必须附带取舍分析（为什么选 A 不选 B），不要只报结论。
4. **可检验**：能量化的给数字，能指认的给文件路径/行号；不确定的明确标注「不确定」及原因。
5. **反例优先**：主动找自己结论的反例与适用边界，找到了就写出来，而不是回避。

## 常用命令

```bash
cd yk-tako-eino
gofmt -l .        # 应输出空
go build ./...
go vet ./...
go test ./...     # 单元测试不打模型，不需要 API Key
```
