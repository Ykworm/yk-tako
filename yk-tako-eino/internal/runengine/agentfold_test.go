package runengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// A1（17-子代理折叠）三钉：①超限折、只重写子代理自己的 state、root 的 foldBox 一根手指
// 不碰（禁令①）；②未超限不折、放行前滤思考（doc/24 §3 在子代理侧的补课）；③休眠门只滤。
// 形状对照 root 侧 TestMidfoldUsageBaselineDecidesAndStripsReasoning；计数沿用 bigCounter
// （BIG 前缀 200 tok，其余 0）。
func TestAgentFoldHandler(t *testing.T) {
	build := func(withReasoning bool) *adk.ChatModelAgentState {
		ans := schema.AssistantMessage("答", nil)
		if withReasoning {
			ans.ReasoningContent = strings.Repeat("思", 10)
		}
		return &adk.ChatModelAgentState{Messages: []*schema.Message{
			schema.SystemMessage("人设"),
			schema.UserMessage("任务"),
			ans,
			schema.ToolMessage("BIG结果", "c1"),
		}}
	}
	newHandler := func(limit int, sums *int) (*agentFoldHandler, *foldBox) {
		box := newFoldBox("s_root", "r_root")
		return newAgentFoldHandler("researcher", bigCounter(), 0, limit,
			func(context.Context, string) (string, error) {
				*sums++
				return "S", nil
			}, nil), box
	}

	t.Run("超限折叠：只折子代理 state，root 容器不写", func(t *testing.T) {
		var sums int
		handler, box := newHandler(150, &sums)
		state := build(true)
		ctx := withFoldBox(context.Background(), box)
		if _, _, err := handler.BeforeModelRewriteState(ctx, state, nil); err != nil {
			t.Fatalf("BeforeModelRewriteState: %v", err)
		}
		if sums == 0 {
			t.Fatal("BIG结果 200 tok > limit 150，应走折叠")
		}
		if len(state.Messages) < 2 || state.Messages[1].Role != schema.User ||
			!strings.Contains(state.Messages[1].Content, "S") {
			t.Fatalf("重写形状违例（队首 system + 摘要 user 行）：%+v", state.Messages)
		}
		for _, m := range state.Messages {
			if m.ReasoningContent != "" {
				t.Fatalf("折后仍带思考：%q", m.ReasoningContent)
			}
		}
		if n := len(box.baseRows()); n != 0 {
			t.Fatalf("root 容器 base 被写：%d 行", n)
		}
		if n := len(box.stagedList()); n != 0 {
			t.Fatalf("root 容器 staged 被写：%d 条", n)
		}
	})

	t.Run("未超限：不折、放行前滤思考", func(t *testing.T) {
		var sums int
		handler, box := newHandler(150, &sums)
		ans := schema.AssistantMessage("答", nil)
		ans.ReasoningContent = strings.Repeat("思", 10)
		state := &adk.ChatModelAgentState{Messages: []*schema.Message{
			schema.SystemMessage("人设"), schema.UserMessage("问"), ans,
		}}
		if _, _, err := handler.BeforeModelRewriteState(withFoldBox(context.Background(), box), state, nil); err != nil {
			t.Fatalf("BeforeModelRewriteState: %v", err)
		}
		if sums != 0 {
			t.Fatalf("小输入不该折：sums=%d", sums)
		}
		if state.Messages[2].Content != "答" || state.Messages[2].ReasoningContent != "" {
			t.Fatalf("应只滤思考：content=%q reasoning=%q",
				state.Messages[2].Content, state.Messages[2].ReasoningContent)
		}
	})

	t.Run("休眠门：只滤思考", func(t *testing.T) {
		var sums int
		handler, box := newHandler(0, &sums)
		state := build(true)
		if _, _, err := handler.BeforeModelRewriteState(withFoldBox(context.Background(), box), state, nil); err != nil {
			t.Fatalf("BeforeModelRewriteState: %v", err)
		}
		if sums != 0 {
			t.Fatalf("limit=0 不该折：sums=%d", sums)
		}
		if state.Messages[2].ReasoningContent != "" {
			t.Fatalf("休眠门下仍应滤思考：%q", state.Messages[2].ReasoningContent)
		}
	})
}

// ---- A2：委派失败路径——折叠链错误与厂商溢出转错误文本，父 run 保持 complete（planning D2）----

type stubInvokeTool struct {
	err error
}

func (s *stubInvokeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "stub"}, nil
}
func (s *stubInvokeTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	return "", s.err
}

func newStubDelegation(name string, err error) *delegationTool {
	return &delegationTool{inner: &stubInvokeTool{err: err}, sem: make(chan struct{}, 1),
		capBytes: 0, name: name}
}

func TestDelegationToolConvertsFoldFailures(t *testing.T) {
	t.Run("折叠链错误 → 错误文本，err 为 nil", func(t *testing.T) {
		d := newStubDelegation("researcher", fmt.Errorf("%s 摘要失败: boom", foldChainErrorMark))
		out, err := d.InvokableRun(context.Background(), "{}")
		if err != nil {
			t.Fatalf("不该上抛：%v", err)
		}
		if !strings.HasPrefix(out, "错误：子代理 researcher 上下文折叠失败：") || !strings.Contains(out, "boom") {
			t.Fatalf("文本形状违例：%q", out)
		}
	})
	t.Run("graph 层 [NodeRunError] 壳不影响判定", func(t *testing.T) {
		wrapped := fmt.Errorf("[NodeRunError] %s 摘要失败（验货拒收）: x", foldChainErrorMark)
		d := newStubDelegation("researcher", wrapped)
		out, err := d.InvokableRun(context.Background(), "{}")
		if err != nil || !strings.HasPrefix(out, "错误：子代理 researcher 上下文折叠失败：") {
			t.Fatalf("应转文本：out=%q err=%v", out, err)
		}
	})
	t.Run("厂商溢出拒收 → 窗口溢出文本", func(t *testing.T) {
		d := newStubDelegation("researcher", errors.New(
			"This model's maximum context length is 4096 tokens. However, you requested 9000 tokens"))
		out, err := d.InvokableRun(context.Background(), "{}")
		if err != nil || !strings.HasPrefix(out, "错误：子代理 researcher 上下文窗口溢出：") {
			t.Fatalf("应转溢出文本：out=%q err=%v", out, err)
		}
	})
	t.Run("其他错误维持裸上抛", func(t *testing.T) {
		d := newStubDelegation("researcher", errors.New("网络炸了"))
		if out, err := d.InvokableRun(context.Background(), "{}"); err == nil || out != "" {
			t.Fatalf("应裸上抛：out=%q err=%v", out, err)
		}
	})
}
