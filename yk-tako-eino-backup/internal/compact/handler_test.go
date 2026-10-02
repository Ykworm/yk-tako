// 本文件：Handler 把 Plan 的结果交回框架。原消息的 Content 保持不动，压缩成功才 Stage。
package compact

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fakeGen 只实现 Generate。摘要测试不需要 Stream。
//
// 实现接口：compact.Generator。方法集对应 model.ToolCallingChatModel 的 Generate
// （github.com/cloudwego/eino/components/model 包 interface.go，eino v0.9.21）。
type fakeGen struct {
	reply string
	calls int
	last  []*schema.Message
}

// 编译期校验 fakeGen 实现了 Generator。
var _ Generator = (*fakeGen)(nil)

// Generate 实现 Generator 的 Generate：记下输入，返回固定正文。
func (f *fakeGen) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	f.calls++
	f.last = in
	return schema.AssistantMessage(f.reply, nil), nil
}

// fakeMem 记下 Stage 和通知。检索按构造时给的字符串返回。
//
// 实现接口：compact.Memory。
type fakeMem struct {
	user    string
	ids     []string
	commits []Commit
	notes   []string
	writes  int
	hit     string
}

// 编译期校验 fakeMem 实现了 Memory。
var _ Memory = (*fakeMem)(nil)

func (m *fakeMem) SessionID() string    { return "s" }
func (m *fakeMem) OriginalUser() string { return m.user }
func (m *fakeMem) CurrentRunID() string { return "cur" }
func (m *fakeMem) CompletedRunIDs() []string {
	out := append([]string(nil), m.ids...)
	return out
}
func (m *fakeMem) Stage(c Commit) error {
	m.commits = append(m.commits, c)
	return nil
}
func (m *fakeMem) Notify(text string) { m.notes = append(m.notes, text) }
func (m *fakeMem) WriteRAG(string, string) error {
	m.writes++
	return nil
}
func (m *fakeMem) SearchRAG(string) (string, error) { return m.hit, nil }

// TestHandlerCompactsIntoNewMessages 钉住：超过阈值时 Stage 一次，返回的消息不是原来的对象。
func TestHandlerCompactsIntoNewMessages(t *testing.T) {
	old := schema.UserMessage(strings.Repeat("旧", 40))
	now := schema.UserMessage(strings.Repeat("今", 10))
	mem := &fakeMem{user: now.Content, ids: []string{"r1"}}
	gen := &fakeGen{reply: "事实：用户在问压缩。"}
	h, err := NewHandler(HandlerConfig{
		Model:         gen,
		FixedWindow:   63,
		CountPrompt:   testTokens,
		CountMessages: testTokens,
	}, mem)
	if err != nil {
		t.Fatal(err)
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{old, now}}
	_, got, err := h.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(mem.commits) != 1 {
		t.Fatalf("Stage %d 次", len(mem.commits))
	}
	if len(mem.commits[0].KeptRunIDs) != 0 || mem.commits[0].DroppedRunIDs[0] != "r1" {
		t.Fatalf("commit = %+v", mem.commits[0])
	}
	if !strings.Contains(mem.commits[0].CombinedUser, "事实") || !strings.HasSuffix(mem.commits[0].CombinedUser, now.Content) {
		t.Fatalf("拼接 = %q", mem.commits[0].CombinedUser)
	}
	if got.Messages[0] == old || got.Messages[0] == now {
		t.Fatal("返回的消息不应是原来的对象")
	}
	if old.Content != strings.Repeat("旧", 40) {
		t.Fatalf("旧消息被写成 %q", old.Content)
	}
	if len(mem.notes) != 1 || mem.notes[0] != "正在压缩" {
		t.Fatalf("通知 = %+v", mem.notes)
	}
	if gen.calls != 1 || len(gen.last) != 2 || gen.last[0].Role != schema.System {
		t.Fatalf("摘要调用 = %d，消息 = %+v", gen.calls, gen.last)
	}
}

// TestHandlerUnderLimitSkipsStage 钉住：没超过阈值时不 Stage，副本的 Content 改了也不影响原消息。
func TestHandlerUnderLimitSkipsStage(t *testing.T) {
	original := schema.UserMessage("短")
	mem := &fakeMem{user: "短"}
	h, err := NewHandler(HandlerConfig{
		FixedWindow:   100,
		CountPrompt:   func([]*schema.Message) int { return 1 },
		CountMessages: sumContent,
	}, mem)
	if err != nil {
		t.Fatal(err)
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{original}}
	_, got, err := h.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(mem.commits) != 0 || len(mem.notes) != 0 {
		t.Fatalf("不应压缩：commits=%d notes=%d", len(mem.commits), len(mem.notes))
	}
	got.Messages[0].Content = "打码后"
	if original.Content != "短" {
		t.Fatalf("原消息被写成 %q", original.Content)
	}
}
