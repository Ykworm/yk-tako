// 本文件：ADK Runner 的驱动循环——Run 跑一轮、consume 收事件并产出、interrupt 审批。
// 事件契约（Event / Sink / eventFrom 的分类）在 event.go。
package adkrun

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"yk-tako-eino/internal/guard"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/taskboard"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// Store 是内存版 checkpoint。进程退出就没了，够演 interrupt。
//
// 实现接口：compose.CheckPointStore —— github.com/cloudwego/eino/compose 包
// checkpoint.go:53 的同名别名（eino v0.9.21）；adk.RunnerConfig.CheckPointStore
// 用的 adk.CheckPointStore 是同一个类型（eino/adk/runner.go:64），覆盖 Get / Set。
type Store struct {
	mu sync.Mutex
	m  map[string][]byte
}

// 编译期校验 Store 实现了 adk.CheckPointStore（即 compose.CheckPointStore）。
var _ adk.CheckPointStore = (*Store)(nil)

// NewStore 建一个空的内存 checkpoint。
func NewStore() *Store {
	return &Store{m: map[string][]byte{}}
}

// Get 实现 compose.CheckPointStore 的 Get 方法：Runner 续跑时按 checkpointID 回调它
// 取回图状态（compose 的 checkPointer.get 反序列化用）。这里返回内部字节的拷贝，
// 调用方改不到 store；key 不存在返回 ok=false，Runner 视为从头跑。
func (s *Store) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.m[key]
	if !ok {
		return nil, false, nil
	}
	out := make([]byte, len(value))
	copy(out, value)
	return out, true, nil
}

// Set 实现 compose.CheckPointStore 的 Set 方法：图在暂停（如 interrupt）等存点时刻
// 由 compose 的 checkPointer.set 回调，按 checkpointID 写入序列化后的图状态。
// 这里同样存拷贝，防止调用方后续改字节穿透 store。
func (s *Store) Set(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, len(value))
	copy(out, value)
	s.m[key] = out
	return nil
}

// Ask 在图暂停时问人。返回 approve 或 reject。
type Ask func(info guard.ApprovalInfo) (string, error)

// Run 跑一轮，遇到 interrupt 就问人再 Resume，直到这轮结束。
// agent 是主 agent：过滤历史用的 root 名从它自己的 Name 取，不再让脚本另传一份。
// 以前 root 要脚本手写，和 ChatModelAgentConfig.Name 写重复——对不上时不报错也不 panic，
// 只是历史悄悄变空、子代理前缀不打了，属于最难查的那种错。
// 事件里的 AgentName 用的就是同一个 Name：adk 的 flowAgent 直接内嵌 Agent
// （adk/flow.go:42），Name 原样转发给里层的 ChatModelAgent。
// 只有 root 的消息回到历史，子代理的过程只打印。
//
// 参数类型用 adk.Agent（github.com/cloudwego/eino/adk 包 interface.go:467 的
// TypedAgent[*schema.Message] 别名，Name 在 :454 声明）：这里只要 Name，runner 自己拿着 agent。
func Run(ctx context.Context, runner *adk.Runner, agent adk.Agent, checkpointID string, input []*schema.Message, ask Ask) ([]*schema.Message, error) {
	root := agent.Name(ctx)
	var opts []adk.AgentRunOption
	if checkpointID != "" {
		opts = append(opts, adk.WithCheckPointID(checkpointID))
	}
	iter := runner.Run(ctx, input, opts...)
	var history []*schema.Message
	for {
		msgs, interrupts, err := consume(iter, root)
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

// consume 收完一轮事件：打印过程、攒主 agent 的消息、收 interrupt。
func consume(iter *adk.AsyncIterator[*adk.AgentEvent], root string) ([]*schema.Message, []*adk.InterruptCtx, error) {
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
			printMessage(event.AgentName, root, event.Output.MessageOutput, msg)
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

// printMessage 打印一条事件消息，子代理的消息带名字前缀。
func printMessage(agentName, root string, mv *adk.MessageVariant, msg *schema.Message) {
	if msg == nil {
		return
	}
	prefix := ""
	if agentName != "" && agentName != root {
		prefix = "[" + agentName + "] "
	}
	if len(msg.ToolCalls) > 0 {
		// len(msg.ToolCalls) > 0：模型返回的助手消息（Role=Assistant），带 tool_calls，表示「我要调工具」→ 打印 → 🔧 调用: xxx 后 return。

		names := make([]string, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			names = append(names, call.Function.Name)
		}
		fmt.Printf("  → 🔧 %s调用: %s\n", prefix, strings.Join(names, ", "))
		return
	}
	if mv.Role == schema.Tool || msg.Role == schema.Tool {
		// mv.Role == schema.Tool || msg.Role == schema.Tool：工具执行完返回的回执消息（Role=Tool），不带 ToolCalls，只有 Content → 打印 ← 🔧 结果: xxx。
		name := mv.ToolName
		if name == "" {
			name = msg.ToolName
		}
		if taskboard.IsBoardTool(name) {
			// 账本工具的回执自带看板与催更块（taskboard/tools.go 的 withBoard），
			// 原样投影，不走 Clip 截断——截断会把看板截烂。
			fmt.Printf("  📋 %s%s\n", prefix, msg.Content)
			return
		}
		fmt.Printf("  ← 🔧 %s%s: %s\n", prefix, name, repl.Clip(msg.Content, 120))
		return
	}
	if msg.Content != "" {
		fmt.Printf("\n🐙 %s%s\n\n", prefix, msg.Content)
	}
}

// approvalFrom 把 interrupt 的载荷转成审批信息，类型对不上就退化成一段文本。
func approvalFrom(info any) guard.ApprovalInfo {
	switch typed := info.(type) {
	case guard.ApprovalInfo:
		return typed
	case *guard.ApprovalInfo:
		if typed != nil {
			return *typed
		}
	}
	return guard.ApprovalInfo{Message: fmt.Sprint(info)}
}
