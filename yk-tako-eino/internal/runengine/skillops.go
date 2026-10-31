// skillops.go 是「折了也知道用过哪个技能」的机械留痕（doc/52 D31，B8）。
//
// 病灶：技能正文是一条 tool 行，折叠后它只剩摘要里的一句话——而摘要由模型写，技能名这种细节
// 它**可能不写**。于是压缩之后 manager 不知道自己这场用过什么技能：任务详情要重拾时想不起
// 该重读哪一份正文，更糟的是凭半截记忆编步骤。治法与 fileOps 同一套：全链路 0% LLM——
// 从被折行的 assistant tool_calls 里机械提取 read_skill / use_skill 与 name 参数（**不解析正文**），
// 拼成一块钉进摘要末尾，下一代按同一块继承（程序从调用记录重抄，不靠模型记性）。
//
// 块里**不写看板任务号**：任务号交给看板回答（run_tasks.skill_name + TaskList 带 skillName），
// compact 因此不碰 DB（D31 选项乙，§2.9）。
//
// 与 fileOps 块的关系：并排钉在摘要末尾（fileOps 在前、skillOps 在后），同一套继承与三档收口
// （全量钉 → 两表各砍 50 重渲染 → errStillOver）。块标记带版本位，没有标记的旧散文摘要无从继承。
package runengine

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// skillOpEntry 是一条技能记录：名字 + 来源工具 + 来源 run。
// 来源注记随条目走——块里每行要写「（use_skill，r_XXX）」，裸名字撑不住审计回指。
type skillOpEntry struct {
	name  string
	tool  string
	runID RunID
}

// skillOps 是一次压缩事件进行中的收集容器：用过（use_skill）与看过（read_skill）两张清单。
// 分开的原因与 fileOps 的 written/edited 同款：「交过底」与「只翻过」在续做时不是一回事。
type skillOps struct {
	used []skillOpEntry
	read []skillOpEntry
}

// add 往指定清单追一条：同一张清单内按名字去重（保留先进的一条 = 淘汰顺序里更老的那条）。
func (o *skillOps) add(bucket string, e skillOpEntry) {
	var list *[]skillOpEntry
	switch bucket {
	case "used":
		list = &o.used
	case "read":
		list = &o.read
	default:
		return
	}
	for _, x := range *list {
		if x.name == e.name {
			return
		}
	}
	*list = append(*list, e)
}

// extractSkillOps 从一批模型侧消息的 tool_calls 列提取技能记录（纯函数，fold 定案时喂被压段的行）。
// 只认 read_skill / use_skill：read_skill_file 是资源读取，正文与技能身份无关，不进块。
// 坏 arguments JSON 跳过 + log（与 fileops 同一条容错：不拖垮提取、也不猜）。
func extractSkillOps(msgs []ModelMessage) skillOps {
	var ops skillOps
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			var bucket string
			switch tc.Function.Name {
			case "use_skill":
				bucket = "used"
			case "read_skill":
				bucket = "read"
			default:
				continue
			}
			name := skillNameFromArgs(tc.Function.Arguments, tc.Function.Name)
			if name == "" {
				continue
			}
			ops.add(bucket, skillOpEntry{name: name, tool: tc.Function.Name, runID: m.RunID})
		}
	}
	return ops
}

// skillNameFromArgs 取调用参数里的 name（两个技能工具的唯一必填字段）。
func skillNameFromArgs(argsJSON, toolName string) string {
	if strings.TrimSpace(argsJSON) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		log.Printf("skillops: 工具 %s 的 arguments 非法 JSON，跳过: %v", toolName, err)
		return ""
	}
	name, _ := m["name"].(string)
	return strings.TrimSpace(name)
}

// resolvedSkillOps 是归并后的两张渲染表。
type resolvedSkillOps struct {
	used []skillOpEntry // 用过 = use_skill
	read []skillOpEntry // 看过 = read_skill − 用过（同一技能只写一次，交过底的更有信息量）
}

// resolveSkillOps 把两张清单归并：用过在前、看过剔掉用过的名字。
// 常态不封顶——封顶是减灾档（第 2 档），由调用方在含块收口不过时调 capSkillOps。
func resolveSkillOps(ops skillOps) resolvedSkillOps {
	taken := make(map[string]bool, len(ops.used))
	used := make([]skillOpEntry, 0, len(ops.used))
	for _, e := range ops.used {
		if taken[e.name] {
			continue
		}
		taken[e.name] = true
		used = append(used, e)
	}
	read := make([]skillOpEntry, 0, len(ops.read))
	for _, e := range ops.read {
		if taken[e.name] {
			continue
		}
		read = append(read, e)
	}
	return resolvedSkillOps{used: used, read: read}
}

// capSkillOps 淘汰最老：超出上限从头部砍（进池先后 = 淘汰顺序）。
func capSkillOps(list []skillOpEntry) []skillOpEntry {
	if len(list) <= fileOpsTableCap {
		return list
	}
	return list[len(list)-fileOpsTableCap:]
}

// skillOpsBlockOpen / skillOpsBlockClose 是块的稳定标记（含版本位 v1）。分隔符沿用
// fileOpsBlockSep——两块并排钉在同一份摘要末尾。
const (
	skillOpsBlockOpen  = "<!-- tako:skill-ops v1 -->"
	skillOpsBlockClose = "<!-- /tako:skill-ops -->"
)

// renderSkillOpLine 渲染一条技能记录行：
//
//   - demo（use_skill，r_XXXX）
func renderSkillOpLine(e skillOpEntry) string {
	return fmt.Sprintf("- %s（%s，%s）", e.name, e.tool, e.runID)
}

// renderSkillOpsBlock 渲染块（用过组在前、看过组在后；空组整段省略）。两组都空 → 空串 = 无块。
func renderSkillOpsBlock(r resolvedSkillOps) string {
	if len(r.used) == 0 && len(r.read) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(skillOpsBlockOpen)
	b.WriteString("\n")
	if len(r.used) > 0 {
		b.WriteString("用过:\n")
		for _, e := range r.used {
			b.WriteString(renderSkillOpLine(e))
			b.WriteString("\n")
		}
	}
	if len(r.read) > 0 {
		b.WriteString("看过:\n")
		for _, e := range r.read {
			b.WriteString(renderSkillOpLine(e))
			b.WriteString("\n")
		}
	}
	b.WriteString(skillOpsBlockClose)
	return b.String()
}

// parseSkillOpsBlock 解析一段摘要文本里的块（继承半边；失败 = 按空处理 + log）。
// 取最后一个开标记：程序钉的块永远在末尾，模型 echo 进来的旧块靠后才是权威。
func parseSkillOpsBlock(text string) skillOps {
	i := strings.LastIndex(text, skillOpsBlockOpen)
	if i < 0 {
		return skillOps{}
	}
	rest := text[i+len(skillOpsBlockOpen):]
	j := strings.Index(rest, skillOpsBlockClose)
	if j < 0 {
		log.Printf("skillops: 摘要里的 skill-ops 块缺闭标记，按空处理")
		return skillOps{}
	}
	var ops skillOps
	bucket := ""
	for _, raw := range strings.Split(rest[:j], "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
		case line == "用过:":
			bucket = "used"
		case line == "看过:":
			bucket = "read"
		case bucket == "" || !strings.HasPrefix(line, "- "):
			log.Printf("skillops: skill-ops 块里跳过无法解析的行: %q", line)
		default:
			e, ok := parseSkillOpLine(strings.TrimPrefix(line, "- "))
			if !ok {
				log.Printf("skillops: skill-ops 块条目行形状不对，跳过: %q", line)
				continue
			}
			ops.add(bucket, e)
		}
	}
	return ops
}

// parseSkillOpLine 解析一条记录行（renderSkillOpLine 的逆）：name（tool，runID）。
// 技能名只有 [a-z0-9-]（skills_scan.go 的 skillNamePattern），不会出现「（」或「，」。
func parseSkillOpLine(s string) (skillOpEntry, bool) {
	open := strings.LastIndex(s, "（")
	if open <= 0 || !strings.HasSuffix(s, "）") {
		return skillOpEntry{}, false
	}
	parts := strings.Split(s[open+len("（"):len(s)-len("）")], "，")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return skillOpEntry{}, false
	}
	return skillOpEntry{name: s[:open], tool: parts[0], runID: RunID(parts[1])}, true
}

// inheritSkillOps 解析全部旧摘要文本里的块并合并（多块按文本序，早的先进池）。
func inheritSkillOps(oldTexts []string) skillOps {
	var ops skillOps
	for _, t := range oldTexts {
		ops = mergeSkillOps(ops, parseSkillOpsBlock(t))
	}
	return ops
}

// mergeSkillOps 两批并一批：旧条目在前、新提取排后——这个先后就是淘汰顺序。
func mergeSkillOps(old, next skillOps) skillOps {
	return skillOps{
		used: concatSkillOpEntries(old.used, next.used),
		read: concatSkillOpEntries(old.read, next.read),
	}
}

func concatSkillOpEntries(a, b []skillOpEntry) []skillOpEntry {
	out := make([]skillOpEntry, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}
