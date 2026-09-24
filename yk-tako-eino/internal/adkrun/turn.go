package adkrun

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/guard"
	"yk-tako-eino/internal/planning"
	"yk-tako-eino/internal/repl"
)

// Store 是内存版 checkpoint。进程退出就没了，够演 interrupt。
type Store struct {
	mu sync.Mutex
	m  map[string][]byte
}

func NewStore() *Store {
	return &Store{m: map[string][]byte{}}
}

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
// root 是主 agent 的名字。只有它的消息会回到历史里，子代理的过程只打印。
func Run(ctx context.Context, runner *adk.Runner, checkpointID, root string, input []*schema.Message, ask Ask) ([]*schema.Message, error) {
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
				// 模型事件在 AfterModel 改写之前就发出了。半途而废提醒补上的
				// planning_nudge 不在那条助手消息里，工具回执却在。下一轮原样送回会变成
				// 没有对应 tool call 的 ToolMessage，接口直接 400。这里按 id 补上。
				ensureNudgeCall(msgs, msg)
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

func printMessage(agentName, root string, mv *adk.MessageVariant, msg *schema.Message) {
	if msg == nil {
		return
	}
	prefix := ""
	if agentName != "" && agentName != root {
		prefix = "[" + agentName + "] "
	}
	if len(msg.ToolCalls) > 0 {
		names := make([]string, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			names = append(names, call.Function.Name)
		}
		fmt.Printf("  → 🔧 %s调用: %s\n", prefix, strings.Join(names, ", "))
		return
	}
	if mv.Role == schema.Tool || msg.Role == schema.Tool {
		name := mv.ToolName
		if name == "" {
			name = msg.ToolName
		}
		if name == planning.NudgeTool || strings.HasPrefix(msg.Content, planning.NudgePrefix) {
			line := msg.Content
			if i := strings.IndexByte(line, '\n'); i >= 0 {
				line = line[:i]
			}
			fmt.Printf("  ↩️  %s%s（已跳回模型继续）\n", prefix, line)
			return
		}
		if name == planning.WriteTodosTool {
			if list, ok := planning.IconList(msg.Content); ok {
				fmt.Printf("  📋 %s任务清单:\n%s\n", prefix, repl.Indent(list))
			} else if list, ok := planning.StatusList(msg.Content); ok {
				fmt.Printf("  📋 %s任务清单: %s\n", prefix, list)
			}
		}
		fmt.Printf("  ← 🔧 %s%s: %s\n", prefix, name, repl.Clip(msg.Content, 120))
		return
	}
	if msg.Content != "" {
		fmt.Printf("\n🐙 %s%s\n\n", prefix, msg.Content)
	}
}

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

func ensureNudgeCall(history []*schema.Message, msg *schema.Message) {
	if msg == nil || msg.Role != schema.Tool || !planning.IsPlanningNudge(msg) || len(history) == 0 {
		return
	}
	prev := history[len(history)-1]
	if prev == nil || prev.Role != schema.Assistant {
		return
	}
	for _, call := range prev.ToolCalls {
		if msg.ToolCallID != "" && call.ID == msg.ToolCallID {
			return
		}
	}
	index := len(prev.ToolCalls)
	prev.ToolCalls = append(prev.ToolCalls, schema.ToolCall{
		Index: &index,
		ID:    msg.ToolCallID,
		Type:  "function",
		Function: schema.FunctionCall{
			Name:      planning.NudgeTool,
			Arguments: "{}",
		},
	})
}
