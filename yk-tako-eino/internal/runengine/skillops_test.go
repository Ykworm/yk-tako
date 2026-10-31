// skillops_test.go 钉住 B8（doc/52 D31）：技能记录块的提取、渲染、继承、与 D33 marker 共存。
// 错了不会崩：摘要把技能名丢了，压缩之后 manager 想不起重读哪份正文——只能凭半截记忆编。
package runengine

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func skillCallFor(id, name, args string) schema.ToolCall {
	return schema.ToolCall{ID: id, Function: schema.FunctionCall{Name: name, Arguments: args}}
}

// 提取：只认 read_skill/use_skill，名字来自 arguments 的 name；坏 JSON 跳过、不猜。
func TestExtractSkillOps(t *testing.T) {
	msgs := []ModelMessage{
		{RunID: "r1", Seq: 2, Role: "assistant", ToolCalls: []schema.ToolCall{
			skillCallFor("c1", "use_skill", `{"name":"demo"}`),
		}},
		{RunID: "r1", Seq: 3, Role: "tool", Text: "正文", ToolCallID: "c1"},
		{RunID: "r1", Seq: 4, Role: "assistant", ToolCalls: []schema.ToolCall{
			skillCallFor("c2", "use_skill", `{"name": " demo "}`), // 同名字去重
			skillCallFor("c3", "read_skill", `{"name":"other"}`),
			skillCallFor("c4", "read_skill_file", `{"name":"demo","path":"a.md"}`), // 资源不算
			skillCallFor("c5", "read_skill", `not-json`),                          // 坏 JSON 跳过
		}},
		{RunID: "r2", Seq: 1, Role: "user", Text: "问"},
	}
	ops := resolveSkillOps(extractSkillOps(msgs))
	if len(ops.used) != 1 || ops.used[0].name != "demo" || ops.used[0].runID != "r1" {
		t.Fatalf("used = %+v", ops.used)
	}
	if len(ops.read) != 1 || ops.read[0].name != "other" || ops.read[0].tool != "read_skill" {
		t.Fatalf("read = %+v", ops.read)
	}
}

// 同一技能先用后看只记一次（交过底的更有信息量）。
func TestResolveSkillOpsDedupAcrossBuckets(t *testing.T) {
	var ops skillOps
	ops.add("used", skillOpEntry{name: "demo", tool: "use_skill", runID: "r1"})
	ops.add("read", skillOpEntry{name: "demo", tool: "read_skill", runID: "r2"})
	ops.add("read", skillOpEntry{name: "other", tool: "read_skill", runID: "r2"})
	got := resolveSkillOps(ops)
	if len(got.used) != 1 || len(got.read) != 1 || got.read[0].name != "other" {
		t.Fatalf("resolve = %+v", got)
	}
}

// 块往返：渲染字节逐字、解析回来等值；空两组 = 空串。
func TestSkillOpsBlockRoundTrip(t *testing.T) {
	res := resolvedSkillOps{
		used: []skillOpEntry{{name: "demo", tool: "use_skill", runID: "r1"}},
		read: []skillOpEntry{{name: "other", tool: "read_skill", runID: "r2"}},
	}
	want := "<!-- tako:skill-ops v1 -->\n用过:\n- demo（use_skill，r1）\n看过:\n- other（read_skill，r2）\n<!-- /tako:skill-ops -->"
	if got := renderSkillOpsBlock(res); got != want {
		t.Fatalf("块字节：\n got %q\nwant %q", got, want)
	}
	back := resolveSkillOps(parseSkillOpsBlock("摘要正文\n\n" + want))
	if len(back.used) != 1 || back.used[0] != (skillOpEntry{name: "demo", tool: "use_skill", runID: "r1"}) {
		t.Fatalf("解析回来 used = %+v", back.used)
	}
	if len(back.read) != 1 || back.read[0].name != "other" {
		t.Fatalf("解析回来 read = %+v", back.read)
	}
	if renderSkillOpsBlock(resolvedSkillOps{}) != "" {
		t.Fatal("空两组应回空串")
	}
}

// 继承：旧摘要的块先入池、新提取排后；同名字去重保先进。
func TestSkillOpsInheritAndMerge(t *testing.T) {
	old := "摘要一\n\n<!-- tako:skill-ops v1 -->\n用过:\n- demo（use_skill，r0）\n<!-- /tako:skill-ops -->"
	var next skillOps
	next.add("used", skillOpEntry{name: "demo", tool: "use_skill", runID: "r1"})
	next.add("used", skillOpEntry{name: "another", tool: "use_skill", runID: "r1"})
	got := resolveSkillOps(mergeSkillOps(inheritSkillOps([]string{old}), next))
	if len(got.used) != 2 || got.used[0].runID != "r0" || got.used[1].name != "another" {
		t.Fatalf("继承合并 = %+v", got.used)
	}
}

// 与 D33 marker 共存：mh 里正文是 marker、摘要里是 skill-ops 块——两处都能看到技能名。
func TestSkillOpsCoexistsWithSlimMarker(t *testing.T) {
	body := `<skill name="demo" source="tenant" bytes="1">` + "\n正文\n</skill>"
	rows := []ModelMessage{
		{RunID: "r1", Seq: 2, Role: "assistant", ToolCalls: []schema.ToolCall{
			skillCallFor("c1", "use_skill", `{"name":"demo"}`),
		}},
		{RunID: "r1", Seq: 3, Role: "tool", Text: body, ToolCallID: "c1"},
	}
	// D33：同一批行里出现更晚的 TaskCreate → mh 那条 tool 行是 marker。
	drop := []runRows{{id: "r1", rows: append(rows,
		ModelMessage{RunID: "r1", Seq: 4, Role: "assistant", ToolCalls: []schema.ToolCall{
			skillCallFor("c2", "TaskCreate", `{"subject":"x"}`),
		}},
	)}}
	foldText := renderFold(nil, drop)
	if strings.Contains(foldText, "正文") {
		t.Fatalf("fold 输入里还有正文：%q", foldText)
	}
	if !strings.Contains(foldText, `folded="true"`) {
		t.Fatalf("fold 输入里没有 marker：%q", foldText)
	}
	block := renderSkillOpsBlock(resolveSkillOps(extractSkillOps(dropMessages(drop))))
	if !strings.Contains(block, "- demo（use_skill，r1）") {
		t.Fatalf("skill-ops 块里没有技能名：%q", block)
	}
}
