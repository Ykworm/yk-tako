// 本文件：会话——每个会话一套专属 agent/runner（看板正本与 checkpoint 不跨会话串账）、
// 历史与单轮并发闸。
package webui

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/guard"
)

// approvalSeq 给审批请求发全局递增 id：前端回执只用它定位，不解析内容。
var approvalSeq atomic.Uint64

// session 是一个聊天会话的全部状态。除 agent/runner 不可变外，其余字段由 mu 保护：
// HTTP handler 与 run goroutine 会并发碰它们（-race 是合入门槛，《10》§6）。
type session struct {
	id     string
	agent  adk.Agent
	runner *adk.Runner

	mu      sync.Mutex
	history []*schema.Message // 本会话累积的历史，语义同 REPL 的 history
	turn    int               // 轮次计数：checkpointID 用它拼，且每会话独立
	active  bool              // 是否有 run 在跑（POST /api/chat 的 409 判定）

	pending map[string]chan string // 审批请求 id → 决策通道（ask 挂在这上面）
}

// newSession 建会话：agent 由 Server 的工厂造（每会话一套），runner 的
// checkpoint store 也是每会话一份——进程内存，重启即丢（《10》§4.4）。
func newSession(ctx context.Context, id string, agent adk.Agent) *session {
	return &session{
		id:      id,
		agent:   agent,
		runner:  adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, EnableStreaming: false, CheckPointStore: adkrun.NewStore()}),
		pending: map[string]chan string{},
	}
}

// beginTurn 开一轮：占并发闸、取消息快照。已有 run 在跑返回 ok=false（调用方回 409）。
func (s *session) beginTurn(message string) (turn int, messages []*schema.Message, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return 0, nil, false
	}
	s.active = true
	s.turn++
	turn = s.turn
	messages = append(append([]*schema.Message{}, s.history...), schema.UserMessage(message))
	return turn, messages, true
}

// finishTurn 收轮：成功才把用户消息与产出写进历史（与 REPL 的「err != nil 不落历史」一致）。
func (s *session) finishTurn(message string, produced []*schema.Message, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.history = append(s.history, schema.UserMessage(message))
		s.history = append(s.history, produced...)
	}
	s.active = false
}

// waitApproval 登记一个审批请求，返回等决策的通道。
func (s *session) waitApproval(id string) chan string {
	ch := make(chan string, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	return ch
}

// resolveApproval 把决策送进对应通道；id 不存在或已处理返回 false（HTTP 404）。
func (s *session) resolveApproval(id, decision string) bool {
	s.mu.Lock()
	ch, ok := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if !ok {
		return false
	}
	ch <- decision
	return true
}

// dropApproval 取消挂起的审批（run 的 ctx 取消时调用），避免 pending 泄漏。
func (s *session) dropApproval(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// runTurn 跑一轮：把 adkrun 事件经 sink 转成 SSE 载荷发进 events；遇 interrupt
// 时经 ask 推 approval 事件并挂起等 POST 回执。发送带 ctx 守卫：客户端断线时
// run 侧不卡死在发送上（handler 已停止读）。
func (s *session) runTurn(ctx context.Context, turn int, messages []*schema.Message, events chan<- sseEvent) ([]*schema.Message, error) {
	send := func(ev sseEvent) {
		select {
		case events <- ev:
		case <-ctx.Done():
		}
	}
	sink := func(ev adkrun.Event) { send(sseEventFromAdkrun(ev)) }
	// ask 是交给 adkrun.Run 的审批回调（《10》§4.3）：不阻塞任何输入，
	// ① 推一条 approval 事件给 SSE，② 挂起在这笔请求的通道上，等 POST /api/approval。
	// resume 值不变：仍返回 guard.ResumeApprove / guard.ResumeReject 字符串。
	ask := func(info guard.ApprovalInfo) (string, error) {
		id := fmt.Sprintf("%s-a%d", s.id, approvalSeq.Add(1))
		ch := s.waitApproval(id)
		send(sseEvent{Kind: "approval", Approval: &sseApproval{ID: id, Tool: info.Tool, Args: info.Args, Message: info.Message}})
		select {
		case decision := <-ch:
			return decision, nil
		case <-ctx.Done():
			s.dropApproval(id)
			return "", ctx.Err()
		}
	}
	return adkrun.Run(ctx, s.runner, s.agent, fmt.Sprintf("turn-%d", turn), messages, sink, ask)
}
