// 本文件：ADK Runner 的驱动循环——Run 跑一轮、consume 收事件并产出、interrupt 审批。
// 事件契约（Event / Sink / eventFrom 的分类）在 event.go。
package adkrun

import (
	"context"
	"fmt"
	"io"
	"sync"

	"yk-tako-eino/internal/guard"

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
			mv := event.Output.MessageOutput
			var msg *schema.Message
			var err error
			if mv.IsStreaming && mv.MessageStream != nil && mv.Role != schema.Tool && mv.ToolName == "" {
				msg, err = streamAssistant(event.AgentName, mv, sink)
			} else {
				msg, err = mv.GetMessage()
				if err == nil && sink != nil {
					if ev, ok := eventFrom(event.AgentName, mv, msg); ok {
						sink(ev)
					}
				}
			}
			if err != nil {
				return msgs, interrupts, err
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

// streamAssistant 逐块读助手正文。每多一块字就交给 sink 一条 Partial 事件，
// 文本是到目前为止的全文。流正常结束后再交一条非 Partial 的最终事件。
// 最终若是工具调用，用 Discard 丢掉半截正文。流出失败用 Drop，半截不进入历史。
func streamAssistant(agent string, mv *adk.MessageVariant, sink Sink) (*schema.Message, error) {
	var chunks []*schema.Message
	var acc string
	for {
		chunk, err := mv.MessageStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if sink != nil {
				sink(Event{Kind: EventMessage, Drop: true, Agent: agent})
			}
			return nil, err
		}
		chunks = append(chunks, chunk)
		if chunk.Content == "" {
			continue
		}
		acc += chunk.Content
		if sink != nil {
			role := chunk.Role
			if role == "" {
				role = mv.Role
			}
			sink(Event{Kind: EventMessage, Partial: true, Agent: agent, Role: role, Text: acc})
		}
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	msg, err := schema.ConcatMessages(chunks)
	if err != nil {
		if sink != nil {
			sink(Event{Kind: EventMessage, Drop: true, Agent: agent})
		}
		return nil, err
	}
	if sink != nil {
		if ev, ok := eventFrom(agent, mv, msg); ok {
			if ev.Kind != EventMessage && acc != "" {
				sink(Event{Kind: EventMessage, Discard: true, Agent: agent})
			}
			sink(ev)
		} else if acc != "" {
			sink(Event{Kind: EventMessage, Discard: true, Agent: agent})
		}
	}
	return msg, nil
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
