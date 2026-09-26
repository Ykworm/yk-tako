// 本文件：taskboard 的 middleware——BeforeAgent 把四个记账工具注进 runCtx.Tools
// （eino plantask 同款机制，adk/middlewares/plantask/plantask.go 的 BeforeAgent），
// 并导出拼进系统提示词的记账约定（Instruction）。
package taskboard

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
)

// handler 是 taskboard 的 middleware 实现。store 是看板正本（跨 REPL 轮次存活），
// tools 是 New 时一次性生成的四个记账工具（操作同一份 store）。
//
// 实现接口：adk.ChatModelAgentMiddleware —— github.com/cloudwego/eino/adk 包
// handler.go:139 定义的 TypedChatModelAgentMiddleware[*schema.Message]（eino v0.9.21；
// 第 259 行起别名 ChatModelAgentMiddleware）。内嵌 adk.BaseChatModelAgentMiddleware
// （同文件 :277）提供其余钩子的空实现，本类型覆盖 BeforeAgent。
type handler struct {
	adk.BaseChatModelAgentMiddleware
	store *Store
	tools []tool.BaseTool
}

// 编译期校验 handler 实现了 adk.ChatModelAgentMiddleware。
var _ adk.ChatModelAgentMiddleware = (*handler)(nil)

// New 创建 taskboard middleware；store 由调用方注入，
// 测试与驱动层可持有同一份正本（查询看板、断言状态）。
func New(store *Store) (adk.ChatModelAgentMiddleware, error) {
	tools, err := makeTools(store)
	if err != nil {
		return nil, err
	}
	return &handler{store: store, tools: tools}, nil
}

// BeforeAgent 实现 adk.ChatModelAgentMiddleware 的 BeforeAgent 钩子
// （github.com/cloudwego/eino/adk/handler.go:142）：框架在每次 agent run 启动时调用，
// 给 middleware 一次改本轮指令和工具表的机会（钩子注释原文 "called before each agent
// run, allowing modification of the agent's instruction and tools configuration"）。
// 本实现借它把四个记账工具追加进本轮工具表——不追加，模型这一轮就看不见 TaskCreate
// 等工具；先拷贝 runCtx 再改，不动调用方原值（plantask 同款做法）。
func (h *handler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	if runCtx == nil {
		return ctx, runCtx, nil
	}
	nRunCtx := *runCtx
	nRunCtx.Tools = append(nRunCtx.Tools, h.tools...)
	return ctx, &nRunCtx, nil
}

// Instruction 是拼进 agent 系统提示词的记账约定（cmd/yktako 把它接在
// config.SystemPrompt() 后面）。约束的工具级细则在各工具的 description 里，
// 这里只讲全局边界：什么时候建表、三层分离。
const Instruction = `任务看板约定：
- 请求需要 ≥3 步、或用户一次列了多件事时，先用 TaskCreate 建看板再动手；一步能完成的请求不要建表。
- 看板行用 taskId 寻址，用 TaskUpdate 改状态；纪律见 TaskUpdate 的工具描述。
- Plan（方案）与看板分离：需要时先口述方案给用户，看板文案不必逐行等于方案。
- 看板是你的施工记录，不是动作日志：一条任务下可以有任意多次工具调用，不要求一一对应。`
