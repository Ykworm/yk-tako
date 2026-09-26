// 本文件：会话——每个会话一套专属 agent/runner（看板正本与 checkpoint 不跨会话串账）、
// 历史与单轮并发闸。
package webui

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"yk-tako-eino/internal/adkrun"
)

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
}

// newSession 建会话：agent 由 Server 的工厂造（每会话一套），runner 的
// checkpoint store 也是每会话一份——进程内存，重启即丢（《10》§4.4）。
func newSession(ctx context.Context, id string, agent adk.Agent) *session {
	return &session{
		id:     id,
		agent:  agent,
		runner: adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, EnableStreaming: false, CheckPointStore: adkrun.NewStore()}),
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

// runTurn 跑一轮：把 adkrun 事件经 sink 转成 SSE 载荷发进 events。
// 发送带 ctx 守卫：客户端断线时 run 侧不卡死在发送上（handler 已停止读）。
func (s *session) runTurn(ctx context.Context, turn int, messages []*schema.Message, events chan<- sseEvent) ([]*schema.Message, error) {
	send := func(ev sseEvent) {
		select {
		case events <- ev:
		case <-ctx.Done():
		}
	}
	sink := func(ev adkrun.Event) { send(sseEventFromAdkrun(ev)) }
	// 审批回调先传 nil（T2.3 换成挂起等 POST 的实现）；nil 是 adkrun 定义的
	// 「不接审批」语义，本步测试的剧本不触发 interrupt。
	return adkrun.Run(ctx, s.runner, s.agent, fmt.Sprintf("turn-%d", turn), messages, sink, nil)
}
