// 本文件：adkrun 的单元测试——Run 从 agent 自己取 root 名的那条路径（用假 agent，不打模型）。
// 账本工具回执的看板投影由 taskboard.IsBoardTool 判定，投影格式测试在 taskboard 包。
package adkrun

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// fakeAgent 是假的主 agent：不读输入，只按事先给的台词吐助手消息事件。
// 用来验证 root 名取自 agent 自己的 Name——这里不设 AgentName，
// 由 adk 的 flowAgent 拿 Name 盖上（adk/flow.go:517），正是线上那条路。
//
// 实现接口：adk.Agent —— github.com/cloudwego/eino/adk 包 interface.go:467 的
// TypedAgent[*schema.Message] 别名（Name / Description / Run 在 :453 起声明）。
// 真身是 ChatModelAgent；本测试只要求名字能被 adk 盖到事件上、Run 能吐事件。
type fakeAgent struct {
	name    string   // 冒充的名字，adk 会把它写进事件的 AgentName
	replies []string // 依次吐出的助手消息
}

// Name 实现 adk.Agent 的 Name：Runner 驱动 agent、给事件标 AgentName 都要它
// （adk 拿它盖事件的 AgentName，adkrun 拿它当 root）；本替身返回构造时的名字。
func (f *fakeAgent) Name(context.Context) string { return f.name }

// Description 实现 adk.Agent 的 Description：adk.Agent 接口要求的自我描述，
// 本测试用不到，返回固定串。
func (f *fakeAgent) Description(context.Context) string { return "假 agent" }

// Run 实现 adk.Agent 的 Run：Runner.Run 通过它启动一次执行并拿事件流；
// 本替身把台词变成助手消息事件发出去，发完关流。
func (f *fakeAgent) Run(_ context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer gen.Close()
		for _, reply := range f.replies {
			gen.Send(&adk.AgentEvent{
				Output: &adk.AgentOutput{
					MessageOutput: &adk.MessageVariant{
						Role:    schema.Assistant,
						Message: schema.AssistantMessage(reply, nil),
					},
				},
			})
		}
	}()
	return iter
}

// TestRunDerivesRootFromAgent 验证 root 名来自 agent 自己报的 Name。
// 旧写法要在调用处手写一遍名字，和 ChatModelAgentConfig.Name 写重复：
// 一旦两处走散，事件会被当成子代理的，消息静默进不了历史——这个测试就是盯这个坑。
func TestRunDerivesRootFromAgent(t *testing.T) {
	ctx := context.Background()
	agent := &fakeAgent{name: "假主脑", replies: []string{"第一轮答复", "第二轮答复"}}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent})

	history, err := Run(ctx, runner, agent, "", []*schema.Message{schema.UserMessage("在吗")}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(history) != len(agent.replies) {
		t.Fatalf("历史应有 %d 条助手消息，实际 %d 条: %+v", len(agent.replies), len(history), history)
	}
	for i, want := range agent.replies {
		if history[i] == nil || history[i].Content != want {
			t.Fatalf("第 %d 条历史应为 %q，实际 %+v", i, want, history[i])
		}
	}
}
