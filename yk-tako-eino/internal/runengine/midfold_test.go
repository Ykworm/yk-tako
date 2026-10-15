// midfold 的钩子层回归网（A4）：不触 PG/Redis，直测转换层坐标、容器读写、两个钩子出口与
// 「折后再锁 base 的同 run 二次折叠」。折叠算法本身的走位归 compact_test（A2）——这里只钉
// 接线的接缝：state ⇄ mh 形状、base 自更新、staged 追加、final 取货、断言违例出口。
package runengine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// assertRows 逐字段比 mh 行（ToolCalls 比 ID/名/参数三字段）。
func assertRows(t *testing.T, got, want []ModelMessage) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("行数 = %d, want %d\ngot:  %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.RunID != w.RunID || g.Seq != w.Seq || g.Role != w.Role || g.Text != w.Text || g.ToolCallID != w.ToolCallID {
			t.Fatalf("行 %d = %+v, want %+v", i, g, w)
		}
		if len(g.ToolCalls) != len(w.ToolCalls) {
			t.Fatalf("行 %d ToolCalls 数 = %d, want %d", i, len(g.ToolCalls), len(w.ToolCalls))
		}
		for j := range w.ToolCalls {
			gj, wj := g.ToolCalls[j], w.ToolCalls[j]
			if gj.ID != wj.ID || gj.Function.Name != wj.Function.Name || gj.Function.Arguments != wj.Function.Arguments {
				t.Fatalf("行 %d 调用 %d = %+v, want %+v", i, j, gj, wj)
			}
		}
	}
}

// assertContents 比消息序列的 Content（重写形状的整段快照）。
func assertContents(t *testing.T, msgs []*schema.Message, want ...string) {
	t.Helper()
	if len(msgs) != len(want) {
		got := make([]string, len(msgs))
		for i, m := range msgs {
			got[i] = m.Content
		}
		t.Fatalf("消息数 = %d, want %d\ngot:  %q\nwant: %q", len(msgs), len(want), got, want)
	}
	for i, w := range want {
		if msgs[i].Content != w {
			t.Fatalf("消息 %d = %q, want %q", i, msgs[i].Content, w)
		}
	}
}

// getTimeCall 造一条 get_time 调用（fixture 惯例：无 fileOps 判定命中，摘要不被钉块污染）。
func getTimeCall(id string) []schema.ToolCall {
	return []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "get_time", Arguments: `{}`}}}
}

// ---- convertState：索引锁定与坐标规则（表测）----

func TestConvertState(t *testing.T) {
	tests := []struct {
		name    string
		base    []ModelMessage
		msgs    []*schema.Message
		wantSys string
		want    []ModelMessage
		wantErr bool
	}{
		{
			name: "基本切分与打戳：base 一行、新轮次从 2 起",
			base: []ModelMessage{{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"}},
			msgs: []*schema.Message{
				schema.SystemMessage("人设"),
				schema.UserMessage("问"),
				schema.AssistantMessage("", getTimeCall("c1")),
				schema.ToolMessage("12:00", "c1"),
			},
			wantSys: "人设",
			want: []ModelMessage{
				{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
				{RunID: "r_cur", Seq: 2, Role: "assistant", ToolCalls: getTimeCall("c1")},
				{RunID: "r_cur", Seq: 3, Role: "tool", Text: "12:00", ToolCallID: "c1"},
			},
		},
		{
			name: "接续 base 最大 seq：空洞不填（0/1/5 之后从 6 起）",
			base: []ModelMessage{
				{RunID: "r_cur", Seq: 0, Role: "user", Text: "S1"},
				{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
				{RunID: "r_cur", Seq: 5, Role: "assistant", Text: "a5"},
			},
			msgs: []*schema.Message{
				schema.SystemMessage("人设"),
				schema.UserMessage("S1"),
				schema.UserMessage("问"),
				schema.AssistantMessage("a5", nil),
				schema.AssistantMessage("答", nil),
			},
			wantSys: "人设",
			want: []ModelMessage{
				{RunID: "r_cur", Seq: 0, Role: "user", Text: "S1"},
				{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
				{RunID: "r_cur", Seq: 5, Role: "assistant", Text: "a5"},
				{RunID: "r_cur", Seq: 6, Role: "assistant", Text: "答"},
			},
		},
		{
			name:    "system 非首 → 错误",
			msgs:    []*schema.Message{schema.UserMessage("无 system")},
			wantErr: true,
		},
		{
			name: "state 行数少于基点 → 错误（重写链违例）",
			base: []ModelMessage{
				{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
				{RunID: "r_cur", Seq: 2, Role: "assistant", Text: "答"},
			},
			msgs:    []*schema.Message{schema.SystemMessage("人设")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys, history, err := convertState(tt.base, "r_cur", tt.msgs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望错误，得 sys=%q history=%+v", sys, history)
				}
				return
			}
			if err != nil {
				t.Fatalf("convertState: %v", err)
			}
			if sys != tt.wantSys {
				t.Fatalf("system = %q, want %q", sys, tt.wantSys)
			}
			assertRows(t, history, tt.want)
		})
	}
}

// ---- foldBox：并发冒烟（RWMutex 兜底不丢写）----

func TestFoldBoxConcurrentSmoke(t *testing.T) {
	box := newFoldBox("s_1", "r_1")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			box.setBase([]ModelMessage{{RunID: "r_1", Seq: n}})
			box.setFinal([]ModelMessage{{RunID: "r_1", Seq: n}})
			box.appendStaged(StagedCompaction{Summary: fmt.Sprintf("S%d", n)})
			_ = box.baseRows()
			_ = box.finalRows()
			_ = box.stagedList()
		}(i)
	}
	wg.Wait()
	if got := box.stagedList(); len(got) != 4 {
		t.Fatalf("staged 数 = %d, want 4（append 无丢失）", len(got))
	}
	if box.baseRows() == nil || box.finalRows() == nil {
		t.Fatalf("base/final 不应为 nil")
	}
}

// ---- 钩子出口：断言违例（裁定 7，不 panic、不落 final）----

func TestMidfoldHandlerAssertionErrors(t *testing.T) {
	handler := newMidfoldHandler(fakeCounter{f: func(string) int { return 0 }}, 100,
		func(context.Context, string) (string, error) { return "", nil }, nil)
	ctx := withFoldBox(context.Background(), newFoldBox("s_1", "r_1"))

	bad := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.UserMessage("无 system")}}
	if _, _, err := handler.BeforeModelRewriteState(ctx, bad, nil); err == nil {
		t.Fatalf("system 非首应报错")
	}
	if _, err := handler.AfterAgent(ctx, bad); err == nil {
		t.Fatalf("AfterAgent 同族断言应报错")
	}
	if box := foldBoxFrom(ctx); box.finalRows() != nil {
		t.Fatalf("断言违例不应落 final：%+v", box.finalRows())
	}
}

// ---- AfterAgent 取货口 + mhRowsFromFold 重编 ----

func TestAfterAgentFinalAndMhRowsFromFold(t *testing.T) {
	handler := newMidfoldHandler(fakeCounter{f: func(string) int { return 0 }}, 100, nil, nil)
	box := newFoldBox("s_1", "r_cur")
	box.setBase([]ModelMessage{{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"}})
	ctx := withFoldBox(context.Background(), box)

	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.SystemMessage("人设"),
		schema.UserMessage("问"),
		schema.AssistantMessage("答", nil),
	}}
	if _, err := handler.AfterAgent(ctx, state); err != nil {
		t.Fatalf("AfterAgent: %v", err)
	}
	assertRows(t, box.finalRows(), []ModelMessage{
		{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
		{RunID: "r_cur", Seq: 2, Role: "assistant", Text: "答"},
	})

	// 折后 final 的实形：旧 run 保留行夹在中间、本 run 行带 seq 空洞——重编从 2 连续。
	mixed := []ModelMessage{
		{RunID: "r_cur", Seq: 0, Role: "user", Text: "S1"},
		{RunID: "r_old", Seq: 2, Role: "assistant", Text: "老 run 保留行"},
		{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
		{RunID: "r_cur", Seq: 6, Role: "assistant", Text: "a6"},
		{RunID: "r_cur", Seq: 7, Role: "tool", Text: "果", ToolCallID: "c1"},
		{RunID: "r_cur", Seq: 8, Role: "assistant", Text: "终稿"},
	}
	assertRows(t, mhRowsFromFold("r_cur", mixed), []ModelMessage{
		{RunID: "r_cur", Seq: 2, Role: "assistant", Text: "a6"},
		{RunID: "r_cur", Seq: 3, Role: "tool", Text: "果", ToolCallID: "c1"},
		{RunID: "r_cur", Seq: 4, Role: "assistant", Text: "终稿"},
	})
}

// ---- 折后再锁 base 的同 run 二次折叠 ----

// 第一次 mid-run 折法把 base 换代为 [摘要, user, 剩余单位]；同 run 再超线时——旧摘要被并入
// 新摘要、交换单位从最老续折（每档重渲染重摘要）、staged 逐次追加、AttachRun 恒本 run
// （同槽换代的落点）；终态 AfterAgent 取货后 mhRows 连续。
func TestMidfoldHandlerSecondFoldSameRun(t *testing.T) {
	counter := fakeCounter{f: func(s string) int {
		if strings.HasPrefix(s, "BIG") {
			return 200 // 脚本化：只有 BIG 前缀的行有体积——触发/收敛独立于 tool_calls JSON 的字节数
		}
		return 0
	}}
	var sumInputs []string
	summarize := func(_ context.Context, in string) (string, error) {
		sumInputs = append(sumInputs, in)
		return fmt.Sprintf("S%d", len(sumInputs)), nil
	}
	handler := newMidfoldHandler(counter, 150, summarize, nil)
	box := newFoldBox("s_1", "r_cur")
	box.setBase([]ModelMessage{
		{RunID: "r_cur", Seq: 0, Role: "user", Text: "S0"},
		{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
	})
	ctx := withFoldBox(context.Background(), box)

	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.SystemMessage("人设"),
		schema.UserMessage("S0"),
		schema.UserMessage("问"),
		schema.AssistantMessage("", getTimeCall("cA")),
		schema.ToolMessage("BIG_tool3", "cA"),
		schema.AssistantMessage("", getTimeCall("cB")),
		schema.ToolMessage("小果", "cB"),
	}}

	// 折 #1：units = [a2+t3, a4+t5]，200 > 150 → 吐最老单位（a2+t3），组装 [S1, 问, a4, t5] 收敛。
	if _, _, err := handler.BeforeModelRewriteState(ctx, state, nil); err != nil {
		t.Fatalf("BeforeModelRewriteState #1: %v", err)
	}
	assertContents(t, state.Messages, "人设", "S1", "问", "", "小果")
	assertRows(t, box.baseRows(), []ModelMessage{
		{RunID: "r_cur", Seq: 0, Role: "user", Text: "S1"},
		{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
		{RunID: "r_cur", Seq: 4, Role: "assistant", ToolCalls: getTimeCall("cB")},
		{RunID: "r_cur", Seq: 5, Role: "tool", Text: "小果", ToolCallID: "cB"},
	})
	if got := box.stagedList(); len(got) != 1 || got[0].AttachRun != "r_cur" || got[0].Summary != "S1" {
		t.Fatalf("staged #1 = %+v, want 1 条挂 r_cur、摘要 S1", got)
	}
	if len(sumInputs) != 1 || !strings.Contains(sumInputs[0], "S0") {
		t.Fatalf("摘要输入 #1 = %q, want 含一代前摘要 S0", sumInputs)
	}

	// ADK 追加下一轮（asst cC + BIG 工具结果）→ 折 #2：旧摘要 S1 并入；最老单位 a4+t5 折后仍超
	// （t7 是新的 BIG）→ 再吐 a6+t7、组装 [S2, 问] 收敛（每档一次 summarize，共 2 次尝试）。
	state.Messages = append(state.Messages,
		schema.AssistantMessage("", getTimeCall("cC")),
		schema.ToolMessage("BIG_tool7", "cC"),
	)
	if _, _, err := handler.BeforeModelRewriteState(ctx, state, nil); err != nil {
		t.Fatalf("BeforeModelRewriteState #2: %v", err)
	}
	assertContents(t, state.Messages, "人设", "S3", "问")
	assertRows(t, box.baseRows(), []ModelMessage{
		{RunID: "r_cur", Seq: 0, Role: "user", Text: "S3"},
		{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
	})
	if got := box.stagedList(); len(got) != 2 || got[1].AttachRun != "r_cur" || got[1].Summary != "S3" {
		t.Fatalf("staged #1+#2 = %+v, want 2 条、第二条挂 r_cur、摘要 S3（第二档尝试的产出）", got)
	}
	if len(sumInputs) != 3 || !strings.Contains(sumInputs[0], "S0") || !strings.Contains(sumInputs[1], "S1") {
		t.Fatalf("摘要输入 = %q, want 第一条含 S0、第二条（重折第一档）含 S1", sumInputs)
	}

	// 终态：ADK 再加终稿 → AfterAgent 取货 → mhRowsFromFold 连续重编（折走的轮次不留行）。
	state.Messages = append(state.Messages, schema.AssistantMessage("终稿", nil))
	if _, err := handler.AfterAgent(ctx, state); err != nil {
		t.Fatalf("AfterAgent: %v", err)
	}
	assertRows(t, mhRowsFromFold("r_cur", box.finalRows()), []ModelMessage{
		{RunID: "r_cur", Seq: 2, Role: "assistant", Text: "终稿"},
	})
}

// ---- doc/24 ①：账单基线 + delta 的公式（纯函数表测）----

// runest 造一条带回执的思考回复：正文 content、思考 reasoning（长度 = 10 个「思」时估算给 10）、
// 账单三个数（p/c/r = PromptTokens / CompletionTokens / ReasoningTokens，r<0 表示厂商不给明细）。
func runest(content, reasoning string, p, c, r int) *schema.Message {
	m := schema.AssistantMessage(content, nil)
	m.ReasoningContent = reasoning
	u := &schema.TokenUsage{PromptTokens: p, CompletionTokens: c}
	if r >= 0 {
		u.CompletionTokensDetails.ReasoningTokens = r
	}
	m.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop", Usage: u}
	return m
}

// 公式四条：厂商明细优先于自测（1000+（50−30）+1）、无明细时滤前估思考、可见输出减成负数置 0、
// 锚点取队尾方向第一条有账单的 assistant（更前有账单的旧行不进 delta、锚点自身正文只由账单代表一次）。
func TestUsageBasedInputFormula(t *testing.T) {
	runeCount := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
	thinking := strings.Repeat("思", 10) // 自测口径下 10 tok
	newRows := func(anchor *schema.Message) []*schema.Message {
		return []*schema.Message{schema.SystemMessage("人设"), schema.UserMessage("问"), anchor,
			schema.ToolMessage("果", "c1")}
	}

	t.Run("厂商 ReasoningTokens 优先", func(t *testing.T) {
		est, ok := usageBasedInput(runeCount, "r_cur", newRows(runest("答复", thinking, 1000, 50, 30)))
		if !ok {
			t.Fatalf("有账单应成立")
		}
		// 锚点正文「答复」(2) 与思考 (10) 都不进 delta：锚点自身由 visible=20 代表一次。
		if est != (usageEstimate{baseline: 1000, visible: 20, delta: 1, total: 1021}) {
			t.Fatalf("est = %+v, want {1000 20 1 1021}", est)
		}
		// 变体一（思考算进 delta＝不减 ReasoningTokens）与变体二（锚点正文再数一遍）都必须更大。
		if dbl := est.baseline + 50 + est.delta; est.total >= dbl {
			t.Fatalf("思考减项未生效：est.total %d, 变体 %d", est.total, dbl)
		}
		if dbl := est.total + runeCount.Count("答复"); est.total >= dbl {
			t.Fatalf("锚点正文被算两遍：est.total %d, 变体 %d", est.total, dbl)
		}
	})

	t.Run("无明细时滤前自测思考", func(t *testing.T) {
		est, ok := usageBasedInput(runeCount, "r_cur", newRows(runest("答复", thinking, 1000, 50, -1)))
		if !ok || est.visible != 40 || est.total != 1041 {
			t.Fatalf("est = %+v ok=%v, want visible 40、total 1041（思考按 10 tok 减）", est, ok)
		}
	})

	t.Run("思考大于输出时置零", func(t *testing.T) {
		est, ok := usageBasedInput(runeCount, "r_cur", newRows(runest("答复", thinking, 1000, 5, 30)))
		if !ok || est.visible != 0 || est.total != 1001 {
			t.Fatalf("est = %+v ok=%v, want visible 0、total 1001", est, ok)
		}
	})

	t.Run("锚点取最近一条有账单的 assistant", func(t *testing.T) {
		msgs := []*schema.Message{schema.SystemMessage("人设"), schema.UserMessage("问"),
			runest("旧答", "", 9000, 9000, -1), schema.ToolMessage("旧果", "c1"),
			runest("新答", "", 100, 0, -1), schema.ToolMessage("果", "c2")}
		est, ok := usageBasedInput(runeCount, "r_cur", msgs)
		if !ok || est != (usageEstimate{baseline: 100, visible: 0, delta: 1, total: 101}) {
			t.Fatalf("est = %+v ok=%v, want 以最后一条有账单的锚点起算 {100 0 1 101}", est, ok)
		}
	})

	t.Run("无账单不进公式", func(t *testing.T) {
		msgs := []*schema.Message{schema.SystemMessage("人设"), schema.UserMessage("问"),
			schema.AssistantMessage("答", nil), schema.ToolMessage("果", "c1")}
		if est, ok := usageBasedInput(runeCount, "r_cur", msgs); ok {
			t.Fatalf("无账单应回退全量自测，got %+v", est)
		}
		// 全零账单 = 厂商给了个空壳，同义于没有账单。
		if _, ok := usageBasedInput(runeCount, "r_cur", newRows(runest("答", "", 0, 0, -1))); ok {
			t.Fatalf("三个整数全 0 的账单不应当锚点")
		}
	})

	// 折完那一次：toSchemaMessages 不拷贝 ResponseMeta → 锚点自然消失、退回全量自测（doc/24 §6）。
	t.Run("重写后账单消失", func(t *testing.T) {
		rewritten := toSchemaMessages([]ModelMessage{
			{RunID: "r_cur", Seq: 0, Role: "user", Text: "摘要"},
			{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
			{RunID: "r_cur", Seq: 2, Role: "assistant", Text: "答复"},
		})
		if est, ok := usageBasedInput(runeCount, "r_cur", append([]*schema.Message{schema.SystemMessage("人设")}, rewritten...)); ok {
			t.Fatalf("折后不应再找到锚点，got %+v", est)
		}
	})
}

// ---- doc/24 ②：钩子接线——有账单时以公式为准（同料全量口径要折、公式不折），放行前滤思考 ----

// bigCounter 脚本化：BIG 前缀 200 tok，其余 0（沿用本文件既有惯例）。
func bigCounter() fakeCounter {
	return fakeCounter{f: func(s string) int {
		if strings.HasPrefix(s, "BIG") {
			return 200
		}
		return 0
	}}
}

// 同一份 state：基点里一条 BIG 老行让全量口径（200）越线，而锚点账单说这一份只有 10 tok。
// 有账单 → 放行、一次都不摘要、思考被滤空；把账单抹掉 → 按全量口径进折叠链（对照）。
func TestMidfoldUsageBaselineDecidesAndStripsReasoning(t *testing.T) {
	build := func(withUsage bool) *adk.ChatModelAgentState {
		anchor := schema.AssistantMessage("答", nil)
		anchor.ReasoningContent = strings.Repeat("思", 10)
		if withUsage {
			anchor.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10}}
		}
		return &adk.ChatModelAgentState{Messages: []*schema.Message{
			schema.SystemMessage("人设"),
			schema.UserMessage("BIG_老摘要"),
			schema.UserMessage("问"),
			anchor,
			schema.ToolMessage("果", "c1"),
		}}
	}
	newHandler := func(summarize func(context.Context, string) (string, error)) (*midfoldHandler, *foldBox) {
		box := newFoldBox("s_1", "r_cur")
		box.setBase([]ModelMessage{
			{RunID: "r_cur", Seq: 0, Role: "user", Text: "BIG_老摘要"},
			{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
		})
		return newMidfoldHandler(bigCounter(), 150, summarize, nil), box
	}

	t.Run("有账单：公式放行，全量口径本会折", func(t *testing.T) {
		var sums int
		handler, box := newHandler(func(context.Context, string) (string, error) {
			sums++
			return "S", nil
		})
		state := build(true)
		ctx := withFoldBox(context.Background(), box)
		if _, _, err := handler.BeforeModelRewriteState(ctx, state, nil); err != nil {
			t.Fatalf("BeforeModelRewriteState: %v", err)
		}
		if sums != 0 || len(box.stagedList()) != 0 {
			t.Fatalf("账单口径 10 tok ≤ limit 150 不该折：sums=%d staged=%d", sums, len(box.stagedList()))
		}
		if got := box.baseRows(); len(got) != 2 {
			t.Fatalf("基点不该换代：%+v", got)
		}
		if rc := state.Messages[3].ReasoningContent; rc != "" {
			t.Fatalf("放行前未滤思考：%q", rc)
		}
	})

	t.Run("无账单：同一份料按全量口径折", func(t *testing.T) {
		var sums int
		handler, box := newHandler(func(context.Context, string) (string, error) {
			sums++
			return "S", nil
		})
		state := build(false)
		ctx := withFoldBox(context.Background(), box)
		if _, _, err := handler.BeforeModelRewriteState(ctx, state, nil); err != nil {
			t.Fatalf("BeforeModelRewriteState: %v", err)
		}
		if sums == 0 {
			t.Fatalf("全量口径 200 tok > limit 150 该折，一次都没摘要")
		}
		for _, m := range state.Messages {
			if m.ReasoningContent != "" {
				t.Fatalf("折后仍带思考：%q", m.ReasoningContent)
			}
		}
	})

	t.Run("折叠休眠也滤思考", func(t *testing.T) {
		handler, box := newHandler(nil)
		handler.limit = 0 // window=0：不折叠、不自测，但上一枪思考同样不再重发
		state := build(true)
		ctx := withFoldBox(context.Background(), box)
		if _, _, err := handler.BeforeModelRewriteState(ctx, state, nil); err != nil {
			t.Fatalf("BeforeModelRewriteState: %v", err)
		}
		if state.Messages[3].Content != "答" || state.Messages[3].ReasoningContent != "" {
			t.Fatalf("休眠门下应只滤思考：content=%q reasoning=%q",
				state.Messages[3].Content, state.Messages[3].ReasoningContent)
		}
	})
}
