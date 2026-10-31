// skillslim_test.go 钉住 B7（doc/52 D33）：正文什么时候让位、让位之后三处消费点各自看到什么。
// 错了不会崩：判据错 → 正文一直重付（钱）；marker 错 → 模型重读时拿到半截；fold/trim 漏认 → 半截正文进摘要。
package runengine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// 认块与造 marker：成功体命中、失败信封/已换过/普通结果不命中；名字取第一个属性。
func TestSkillSlimMarker(t *testing.T) {
	body := `<skill name="demo" source="tenant" bytes="42">` + "\n正文 MARKER-BODY\n</skill>"
	marker, ok := skillSlimMarker(body)
	if !ok || marker != `<skill name="demo" folded="true" reload="read_skill" tasks="见看板"/>` {
		t.Fatalf("marker = %q ok=%v", marker, ok)
	}
	for _, text := range []string{
		`{"code":116001,"message":"找不到这个技能","data":{"name":"demo"}}`,
		marker, // 已换过：幂等，不再套一层
		"普通工具结果",
		`<skill name="demo" source="tenant">正文没闭合`,
		`<skill name="">空名字</skill>`,
	} {
		if _, ok := skillSlimMarker(text); ok {
			t.Errorf("不该命中：%q", text)
		}
	}
}

// 判据：正文之后（行序更晚）有 TaskCreate 才换；只读不办事、先建任务后读、失败信封都不换。
func TestSkillSlimPlan(t *testing.T) {
	skillCall := func(id string) []schema.ToolCall {
		return []schema.ToolCall{{ID: id, Function: schema.FunctionCall{Name: "read_skill"}}}
	}
	taskCall := func(id string) []schema.ToolCall {
		return []schema.ToolCall{{ID: id, Function: schema.FunctionCall{Name: "TaskCreate"}}}
	}
	body := `<skill name="demo" source="tenant" bytes="1">` + "\n正文\n</skill>"
	cases := []struct {
		name string
		msgs []*schema.Message
		want bool
	}{
		{
			name: "读了正文、之后建了任务 → 换",
			msgs: []*schema.Message{
				schema.SystemMessage("sys"),
				schema.UserMessage("办"),
				schema.AssistantMessage("", skillCall("c1")),
				schema.ToolMessage(body, "c1"),
				schema.AssistantMessage("", taskCall("c2")),
				schema.ToolMessage("看板：…", "c2"),
			},
			want: true,
		},
		{
			name: "只读不建任务 → 不换",
			msgs: []*schema.Message{
				schema.SystemMessage("sys"),
				schema.AssistantMessage("", skillCall("c1")),
				schema.ToolMessage(body, "c1"),
				schema.AssistantMessage("读完干别的", nil),
			},
			want: false,
		},
		{
			name: "先建任务后读正文 → 不换（判据是行序更晚的调用，不是「建过就行」）",
			msgs: []*schema.Message{
				schema.SystemMessage("sys"),
				schema.AssistantMessage("", taskCall("c2")),
				schema.ToolMessage("看板：…", "c2"),
				schema.AssistantMessage("", skillCall("c1")),
				schema.ToolMessage(body, "c1"),
			},
			want: false,
		},
		{
			name: "失败信封 + 任务 → 不换（没有正文可让位）",
			msgs: []*schema.Message{
				schema.SystemMessage("sys"),
				schema.AssistantMessage("", skillCall("c1")),
				schema.ToolMessage(`{"code":116107,"message":"已禁用","data":{"name":"demo"}}`, "c1"),
				schema.AssistantMessage("", taskCall("c2")),
			},
			want: false,
		},
		{
			name: "中间隔了别的工具也认（只看行序）",
			msgs: []*schema.Message{
				schema.SystemMessage("sys"),
				schema.AssistantMessage("", skillCall("c1")),
				schema.ToolMessage(body, "c1"),
				schema.AssistantMessage("", []schema.ToolCall{{ID: "c9", Function: schema.FunctionCall{Name: "bash"}}}),
				schema.ToolMessage("输出", "c9"),
				schema.AssistantMessage("", taskCall("c2")),
			},
			want: true,
		},
	}
	for _, c := range cases {
		got := skillSlimPlan(c.msgs)
		_, hit := got["c1"]
		if hit != c.want {
			t.Errorf("%s：命中=%v，want %v", c.name, hit, c.want)
		}
	}
}

// 每枪重写：state 与 base 一起换；没建任务时两处都不动。
func TestSlimSkillBlocksStateAndBase(t *testing.T) {
	body := `<skill name="demo" source="tenant" bytes="1">` + "\n正文\n</skill>"
	marker := `<skill name="demo" folded="true" reload="read_skill" tasks="见看板"/>`
	rows := func(withTask bool) []ModelMessage {
		out := []ModelMessage{
			{RunID: "r1", Seq: 1, Role: "user", Text: "办"},
			{RunID: "r1", Seq: 2, Role: "assistant", ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read_skill"}}}},
			{RunID: "r1", Seq: 3, Role: "tool", Text: body, ToolCallID: "c1"},
		}
		if withTask {
			out = append(out,
				ModelMessage{RunID: "r1", Seq: 4, Role: "assistant", ToolCalls: []schema.ToolCall{{ID: "c2", Function: schema.FunctionCall{Name: "TaskCreate"}}}},
				ModelMessage{RunID: "r1", Seq: 5, Role: "tool", Text: "看板：…", ToolCallID: "c2"},
			)
		}
		return out
	}

	for _, withTask := range []bool{false, true} {
		box := newFoldBox("s1", "r1")
		box.setBase(rows(withTask))
		state := &adk.ChatModelAgentState{Messages: append([]*schema.Message{schema.SystemMessage("sys")}, toSchemaMessages(rows(withTask))...)}
		slimSkillBlocks(box, state)

		want := body
		if withTask {
			want = marker
		}
		if got := baseRowText(box.baseRows(), "c1"); got != want {
			t.Errorf("withTask=%v：base 正文 = %q，want %q", withTask, got, want)
		}
		gotState := ""
		for _, m := range state.Messages {
			if m.Role == schema.Tool && m.ToolCallID == "c1" {
				gotState = m.Content
			}
		}
		if gotState != want {
			t.Errorf("withTask=%v：state 正文 = %q，want %q", withTask, gotState, want)
		}
	}

	// box 为 nil 时只改 state（单测/异常路径），不 panic。
	state := &adk.ChatModelAgentState{Messages: append([]*schema.Message{schema.SystemMessage("sys")}, toSchemaMessages(rows(true))...)}
	slimSkillBlocks(nil, state)
	for _, m := range state.Messages {
		if m.Role == schema.Tool && m.ToolCallID == "c1" && m.Content != marker {
			t.Errorf("无盒时 state 未换 marker：%q", m.Content)
		}
	}
}

func baseRowText(rows []ModelMessage, id string) string {
	for _, r := range rows {
		if r.Role == "tool" && r.ToolCallID == id {
			return r.Text
		}
	}
	return ""
}

// fold 转录：技能正文整块写 marker，不进 2000 runes 截断，也不把半截正文送进摘要。
func TestRenderFoldRowSkillMarker(t *testing.T) {
	body := `<skill name="demo" source="tenant" bytes="9000">` + "\n" + strings.Repeat("正", 9000) + "\n</skill>"
	drop := []runRows{{id: "r1", rows: []ModelMessage{
		{RunID: "r1", Seq: 2, Role: "assistant", ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "use_skill"}}}},
		{RunID: "r1", Seq: 3, Role: "tool", Text: body, ToolCallID: "c1"},
	}}}
	got := renderFold(nil, drop)
	if !strings.Contains(got, "[Tool result use_skill]: "+`<skill name="demo" folded="true" reload="read_skill" tasks="见看板"/>`) {
		t.Fatalf("摘要里没有 marker：%q", got)
	}
	if strings.Contains(got, "正正正") || strings.Contains(got, "截断") {
		t.Fatalf("半截正文进了摘要：%q", got[:200])
	}
}

// trim：技能块不进预算、不被夹。只放技能块 = 不动手；混着普通结果时只裁普通那条。
func TestTrimCandidateSkipsSkillBody(t *testing.T) {
	counter := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
	skillBody := `<skill name="demo" source="tenant" bytes="50000">` + "\n" + strings.Repeat("正", 50000) + "\n</skill>"

	only := []ModelMessage{{Role: "tool", Text: skillBody, ToolCallID: "c1"}}
	out, _, _, fired := trimCandidate(counter, 40960, only) // 预算 8192
	if fired {
		t.Fatalf("技能块单独存在时不该动手（总量被排除）")
	}
	if out[0].Text != skillBody {
		t.Fatalf("技能块被改了")
	}

	big := strings.Repeat("a", 20000)
	mixed := []ModelMessage{
		{Role: "tool", Text: skillBody, ToolCallID: "c1"},
		{Role: "tool", Text: big, ToolCallID: "c2"},
	}
	out2, _, _, fired2 := trimCandidate(counter, 40960, mixed)
	if !fired2 {
		t.Fatalf("普通结果 20000 > 预算 8192，应当动手")
	}
	// B8（D30）：动手时技能块整块换 marker，不做头尾夹。
	if out2[0].Text != `<skill name="demo" folded="true" reload="read_skill" tasks="见看板"/>` {
		t.Fatalf("技能块没被整块替换：%q", out2[0].Text[:40])
	}
	if mixed[0].Text != skillBody {
		t.Fatalf("入参被改写（copy-on-write 违例）")
	}
	if out2[1].Text == big {
		t.Fatalf("普通结果没被裁")
	}
	if !strings.HasPrefix(out2[1].Text, fmt.Sprintf(trimMarkerFormat, len(big))) {
		t.Fatalf("普通结果的 marker 形态不对：%q", out2[1].Text[:40])
	}
}
