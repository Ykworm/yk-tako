// skillslim.go 是「正文用完就让位」的唯一一处判定（doc/52 D33，B7）。
//
// 技能正文是一条 tool 行：read_skill / use_skill 的结果是一条 `<skill name=… bytes=…>正文</skill>`
// （skilltools.go 的 renderSkillBody）。它进了消息历史之后每一枪原样重发，直到折叠把它截成半截
// 塞进摘要——一份 16k token 的正文、一份满额资源，会一直重付到折叠为止。
//
// 让位的判据是**确定性证据**而不是「读过了」：同一个技能的正文之后（行序更晚的 assistant 行）
// 出现了 TaskCreate 调用 = 已经照着它建过看板任务。命中后那条 tool 行的正文整块换成 marker：
//
//	<skill name="demo" folded="true" reload="read_skill" tasks="见看板"/>
//
// 只读不办事的技能不换（marker 会逼模型重读一次，而那份正文还没用完）。判据只看 ToolCalls、
// 不查库、也不看任务的 skill_name（doc/52 D33 代价③）——少一次查询，也避开「先建任务后 use_skill」
// 的时序歧义。判据同样不区分「正文是被 read_skill 还是 use_skill 读进来的」：两个工具的结果同形，
// 凡建过任务就让位。
//
// 三个消费点共用 marker 的形状：
//   - 每枪重写（slimSkillBlocks，挂在 midfoldHandler.BeforeModelRewriteState 最上面）——
//     与压缩无关，-compact-window=0 也执行；state 与 base 一起改，数输入与落库都看到 marker；
//   - fold 转录（compact.go 的 renderFoldRow）——被折的正文整块写 marker 进摘要，不走 2000 runes 截断；
//   - trim（compact.go 的 trimCandidate）——技能块不进工具结果预算、不进还原队列，不被静默夹。
//
// mh 存的是重写后的形态（模型实际所见，doc/52 §2.5 存储层）；runlog 的 payload.output 仍是
// 发射时的全文——两处不是同一个东西（§10.1-14）。
package runengine

import (
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// skillSlimMarkerFormat 是正文让位后留下的那一行。属性次序固定（name 恒在第一个），
// 让 skillBodyName 用最笨的前缀解析也能读回来；reload 指回 read_skill 而不是 use_skill——
// 重读只要正文，不该顺手再声明一次执行。
const skillSlimMarkerFormat = `<skill name="%s" folded="true" reload="read_skill" tasks="见看板"/>`

// skillBodyName 认出一个技能正文块并取出 name 属性。
//
// 认块的条件收紧到「以 `<skill name="` 开头、以 `</skill>` 结尾」：正文里出现的 `<skill>`
// 示例已被转义（escapeSkillXML 只放行 `>`），所以真块只有 renderSkillBody 造的这一个；
// 失败信封是 JSON，不以 `<skill` 开头，天然不命中。
func skillBodyName(text string) (string, bool) {
	const prefix = `<skill name="`
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, "</skill>") {
		return "", false
	}
	rest := text[len(prefix):]
	i := strings.IndexByte(rest, '"')
	if i <= 0 {
		return "", false
	}
	return rest[:i], true
}

// skillSlimMarker 把正文块换成 marker；不是正文块（失败信封、已换过、普通工具结果）= false。
func skillSlimMarker(text string) (string, bool) {
	name, ok := skillBodyName(text)
	if !ok {
		return "", false
	}
	return fmt.Sprintf(skillSlimMarkerFormat, name), true
}

// isSkillBodyText 判断一条 tool 行是不是技能正文块（trim 的排除判据）。
func isSkillBodyText(text string) bool {
	_, ok := skillBodyName(text)
	return ok
}

// skillSlimPlan 从按行序排好的 state.Messages 里算出「哪些 toolCallId 的正文该换成 marker」。
//
// 从后往前扫，见到 assistant 行的 TaskCreate 就记下「更晚的地方建过任务」；之后遇到的
// read_skill / use_skill 结果只要还是正文块就进集合——「之后」由扫描方向直接保证。
// 工具名从全量 assistant 行的 ToolCalls 建表得到（tool 行只有 ToolCallID，adk 的 state 不存名字）。
func skillSlimPlan(msgs []*schema.Message) map[string]bool {
	names := make(map[string]string, len(msgs))
	for _, m := range msgs {
		if m.Role != schema.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			names[tc.ID] = tc.Function.Name
		}
	}
	out := make(map[string]bool)
	taskCreated := false
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		switch m.Role {
		case schema.Assistant:
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "TaskCreate" {
					taskCreated = true
				}
			}
		case schema.Tool:
			if !taskCreated {
				continue
			}
			switch names[m.ToolCallID] {
			case "read_skill", "use_skill":
				if _, ok := skillSlimMarker(m.Content); ok {
					out[m.ToolCallID] = true
				}
			}
		}
	}
	return out
}

// slimSkillBlocks 在每次模型调用前把「已建过任务」的技能正文换成 marker（D33 的每枪重写）。
//
// 改两处：
//   - state.Messages：模型这一枪实际收到的输入；
//   - box.base：mh 里的老行。convertState 的老行从 base 取、不从 state 取——只改 state 的话
//     数输入与 AfterAgent 落库都还是全文（数偏大只是保守，落库偏大就是「库 ≠ 模型所见」）。
//
// 判据每次从 state 现算（不缓存）：「建过任务」是行序事实，折叠会改变行序（折掉的那段只剩摘要），
// 缓一份集合就要跟着折叠换代，而重算是 O(行数)、每次模型调用一次，不值得为它引状态。
func slimSkillBlocks(box *foldBox, state *adk.ChatModelAgentState) {
	if state == nil || len(state.Messages) == 0 {
		return
	}
	ids := skillSlimPlan(state.Messages)
	if len(ids) == 0 {
		return
	}
	for _, m := range state.Messages {
		if m.Role != schema.Tool || !ids[m.ToolCallID] {
			continue
		}
		if marker, ok := skillSlimMarker(m.Content); ok {
			m.Content = marker
		}
	}
	if box == nil {
		return
	}
	box.slimBase(ids)
}

// slimBase 在容器锁内把 base 里命中的 tool 行换成 marker。
// 单独成方法而不是让调用方直接改字段：base 的读写口是 setBase/baseRows 这一对（锁在容器里），
// 绕过它们改字段迟早与 guard 侧的读撞上。
func (b *foldBox) slimBase(ids map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.base {
		row := &b.base[i]
		if row.Role != "tool" || !ids[row.ToolCallID] {
			continue
		}
		if marker, ok := skillSlimMarker(row.Text); ok {
			row.Text = marker
		}
	}
}
