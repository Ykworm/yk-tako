// 本文件是持久 Task 创建计划的纯函数测试（施工包 §12.1）：
// 不碰 PG / Redis，钉住防环与形状校验、plan_hash 的顺序不变性、单节点转换、下游首条输入。
package runengine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// goodCmd 是一张合法的两层图：A、B 并行，C 汇总（施工包 §12.1 的标准形状）。
func goodCmd() CreateOrchestrationCommand {
	return CreateOrchestrationCommand{
		IdempotencyKey:  "k1",
		WorkspaceID:     "ws_1",
		SourceSessionID: "s_1",
		Goal:            "调研 A、B 后汇总",
		Tasks: []planTask{
			{Key: "C", Content: "汇总成报告", Agent: "coder"},
			{Key: "A", Content: "调研 A", Agent: "researcher"},
			{Key: "B", Content: "调研 B", Agent: "researcher"},
		},
		Links: []planLink{
			{Parent: "A", Child: "C"},
			{Parent: "B", Child: "C"},
		},
	}
}

// TestPlanHashOrderInvariant 钉住幂等比对的根基（施工包 §12.1）：
// 输入顺序变化但逻辑计划相同 → 同哈希（重试不会被误判成另一份计划）；
// 任何内容字节变化 → 不同哈希（真换计划必须判 117102 冲突）。
func TestPlanHashOrderInvariant(t *testing.T) {
	p1, err := normalizeOrchestrationPlan(goodCmd())
	if err != nil {
		t.Fatalf("合法计划被拒：%v", err)
	}
	reversed := goodCmd()
	for i, j := 0, len(reversed.Tasks)-1; i < j; i, j = i+1, j-1 {
		reversed.Tasks[i], reversed.Tasks[j] = reversed.Tasks[j], reversed.Tasks[i]
	}
	for i, j := 0, len(reversed.Links)-1; i < j; i, j = i+1, j-1 {
		reversed.Links[i], reversed.Links[j] = reversed.Links[j], reversed.Links[i]
	}
	p2, err := normalizeOrchestrationPlan(reversed)
	if err != nil {
		t.Fatalf("乱序后的同一计划被拒：%v", err)
	}
	h1, h2 := mustHash(t, p1), mustHash(t, p2)
	if h1 != h2 {
		t.Fatalf("顺序变化哈希变了：%s != %s", h1, h2)
	}
	// 内容变化必变：改 goal、改 content、改边，三种各验一次。
	for _, mutate := range []func(*CreateOrchestrationCommand){
		func(c *CreateOrchestrationCommand) { c.Goal += "（改）" },
		func(c *CreateOrchestrationCommand) { c.Tasks[0].Content += "（改）" },
		func(c *CreateOrchestrationCommand) { c.Links = nil },
	} {
		c := goodCmd()
		mutate(&c)
		p3, err := normalizeOrchestrationPlan(c)
		if err != nil {
			continue // 删边后 B 也是最终节点，被「恰好一个最终」拒绝——同样应与原哈希不同，跳过哈希
		}
		if h3 := mustHash(t, p3); h3 == h1 {
			t.Fatalf("内容变了哈希没变（mutate 后 plan 仍与原计划同哈希）")
		}
	}
}

// TestNormalizePlanRejectsBadShapes 逐条钉住创建计划的拒绝面（施工包 §12.1）：
// 环、自依赖、未知端、重复 key／边、零／多个最终节点、key 词法、尺寸上限、必填缺失。
func TestNormalizePlanRejectsBadShapes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateOrchestrationCommand)
		want   string // 期望错误信息里的片段
	}{
		{"环 A→B→A", func(c *CreateOrchestrationCommand) {
			c.Tasks = []planTask{{Key: "A", Content: "x", Agent: "coder"}, {Key: "B", Content: "y", Agent: "coder"}}
			c.Links = []planLink{{Parent: "A", Child: "B"}, {Parent: "B", Child: "A"}}
		}, "成环"},
		{"自依赖", func(c *CreateOrchestrationCommand) {
			c.Tasks = []planTask{{Key: "A", Content: "x", Agent: "coder"}}
			c.Links = []planLink{{Parent: "A", Child: "A"}}
		}, "自依赖"},
		{"未知端", func(c *CreateOrchestrationCommand) {
			c.Links = append(c.Links, planLink{Parent: "C", Child: "ghost"})
		}, "不存在"},
		{"重复 key", func(c *CreateOrchestrationCommand) {
			c.Tasks = append(c.Tasks, planTask{Key: "A", Content: "另一件", Agent: "coder"})
		}, "重复"},
		{"重复边", func(c *CreateOrchestrationCommand) {
			c.Links = append(c.Links, planLink{Parent: "A", Child: "C"})
		}, "重复"},
		{"两个最终节点", func(c *CreateOrchestrationCommand) {
			c.Tasks = append(c.Tasks, planTask{Key: "D", Content: "另一份交付", Agent: "coder"})
		}, "最终"},
		{"key 非法字符", func(c *CreateOrchestrationCommand) {
			c.Tasks[0].Key = "a b"
		}, "key"},
		{"key 数字外开头", func(c *CreateOrchestrationCommand) {
			c.Tasks[0].Key = "-abc"
		}, "key"},
		{"goal 超限", func(c *CreateOrchestrationCommand) {
			c.Goal = strings.Repeat("标", taskGoalMaxRunes+1)
		}, "goal"},
		{"content 超限", func(c *CreateOrchestrationCommand) {
			c.Tasks[0].Content = strings.Repeat("标", taskContentMaxRunes+1)
		}, "内容"},
		{"幂等键超限", func(c *CreateOrchestrationCommand) {
			c.IdempotencyKey = strings.Repeat("k", idempotencyMaxBytes+1)
		}, "128"},
		{"幂等键空", func(c *CreateOrchestrationCommand) {
			c.IdempotencyKey = "  "
		}, "idempotencyKey"},
		{"执行者空", func(c *CreateOrchestrationCommand) {
			c.Tasks[0].Agent = " "
		}, "执行者"},
		{"空任务表", func(c *CreateOrchestrationCommand) {
			c.Tasks = nil
		}, "1–32"},
	}
	for _, tc := range cases {
		cmd := goodCmd()
		tc.mutate(&cmd)
		_, err := normalizeOrchestrationPlan(cmd)
		if err == nil {
			t.Errorf("%s：被放行了，必须拒", tc.name)
			continue
		}
		pe, ok := err.(*planError)
		if !ok {
			t.Errorf("%s：错误类型 = %T，要 *planError", tc.name, err)
			continue
		}
		if !strings.Contains(pe.reason, tc.want) {
			t.Errorf("%s：错误 = %q，要含 %q", tc.name, pe.reason, tc.want)
		}
	}
}

// TestNormalizePlanAccepts 边界内侧也要钉：33 件拒、32 件收；单节点无边收。
func TestNormalizePlanAccepts(t *testing.T) {
	ok := goodCmd()
	if _, err := normalizeOrchestrationPlan(ok); err != nil {
		t.Fatalf("标准两层图被拒：%v", err)
	}
	// 32 件星形：A 是唯一汇聚点。
	star := CreateOrchestrationCommand{IdempotencyKey: "k", WorkspaceID: "w", Goal: "g"}
	for i := 0; i < agentTaskMaxCount-1; i++ {
		star.Tasks = append(star.Tasks, planTask{Key: fmt.Sprintf("t%d", i), Content: "并行件", Agent: "coder"})
		star.Links = append(star.Links, planLink{Parent: fmt.Sprintf("t%d", i), Child: "final"})
	}
	star.Tasks = append(star.Tasks, planTask{Key: "final", Content: "汇总", Agent: "coder"})
	if _, err := normalizeOrchestrationPlan(star); err != nil {
		t.Fatalf("32 件星形被拒：%v", err)
	}
	star.Tasks = append(star.Tasks, planTask{Key: "extra", Content: "第 33 件", Agent: "coder"})
	if _, err := normalizeOrchestrationPlan(star); err == nil {
		t.Fatalf("33 件被放行")
	}
	// 单节点无边：delegate_task 转出来的形状。
	single := CreateOrchestrationCommand{
		IdempotencyKey: "k", WorkspaceID: "w", Goal: "g",
		Tasks: []planTask{{Key: "main", Content: "单件", Agent: "coder"}},
	}
	p, err := normalizeOrchestrationPlan(single)
	if err != nil {
		t.Fatalf("单节点被拒：%v", err)
	}
	if fk := p.finalKey(); fk != "main" {
		t.Fatalf("单节点最终 key = %q，要 main", fk)
	}
	// 两层图 A,B→C：最终节点是**汇** C（出度 0），不是根。finalKey 标记反写（把 child
	// 当成 has-child）时这里会返回 A——调度收组与交付都会盯着错误的 Task。
	twoLayer := CreateOrchestrationCommand{
		IdempotencyKey: "k", WorkspaceID: "w", Goal: "g",
		Tasks: []planTask{{Key: "a", Content: "A", Agent: "researcher"},
			{Key: "b", Content: "B", Agent: "researcher"},
			{Key: "c", Content: "C", Agent: "coder"}},
		Links: []planLink{{Parent: "a", Child: "c"}, {Parent: "b", Child: "c"}},
	}
	p2, err := normalizeOrchestrationPlan(twoLayer)
	if err != nil {
		t.Fatalf("两层图被拒：%v", err)
	}
	if fk := p2.finalKey(); fk != "c" {
		t.Fatalf("两层图最终 key = %q，要汇 c", fk)
	}
}

// TestSingleTaskCommand 钉住单件 Task 的转换（施工包 §6.1）：
// key 固定 main、goal 从 content 纯截断派生、其余字段原样带过。
func TestSingleTaskCommand(t *testing.T) {
	long := strings.Repeat("长", taskGoalMaxRunes+5)
	cmd := singleTaskCommand(CreateTaskCommand{
		IdempotencyKey:  "idem",
		WorkspaceID:     "ws",
		SourceSessionID: "s1",
		SourceRunID:     "r1",
		Content:         long,
		Agent:           "coder",
		ModelRef:        "deepseek/deepseek-flash",
	})
	plan, err := normalizeOrchestrationPlan(cmd)
	if err != nil {
		t.Fatalf("单件转换后计划被拒：%v", err)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].Key != singleTaskKey {
		t.Fatalf("转换后节点 = %+v，要单节点 key=main", plan.Tasks)
	}
	if plan.Tasks[0].Content != long {
		t.Fatalf("content 被动了")
	}
	if want := truncatePlain(long, taskGoalMaxRunes); plan.Goal != want {
		t.Fatalf("goal = %d 字，要纯截断到 %d 字", utf8Len(plan.Goal), taskGoalMaxRunes)
	}
	if plan.SourceSessionID != "s1" || plan.WorkspaceID != "ws" {
		t.Fatalf("来源会话／项目没带过：%+v", plan)
	}
}

// TestBuildTaskFirstInput 钉住执行 run 的首条输入（施工包 §7.3）：
// 四段固定（goal → 本件 → 上游结果 → 执行约定）；上游只带 summary/artifacts/metadata 与
// taskId、resultRevision 引用，**不带父任务 content**——执行者看交接摘要续工，不看父的聊天。
func TestBuildTaskFirstInput(t *testing.T) {
	parentA := AgentTask{ID: "at_A", TaskKey: "A", Content: "A 的原始指令（不该出现）", ResultRevision: 2,
		Result: mustJSONStr(t, TaskResult{
			Summary: "A 的交接摘要",
			Artifacts: []TaskArtifact{{Path: "reports/a.md", Kind: "report"}},
			Metadata: map[string]any{"rows": float64(42)},
		})}
	parentB := AgentTask{ID: "at_B", TaskKey: "B", Content: "B 的原始指令（不该出现）", ResultRevision: 1,
		Result: mustJSONStr(t, TaskResult{Summary: "B 的交接摘要"})}
	child := AgentTask{ID: "at_C", TaskKey: "C", Content: "把两份调研合并"}

	got := buildTaskFirstInput("整组目标", child, []AgentTask{parentB, parentA})
	for _, want := range []string{
		"## 编排目标", "整组目标",
		"## 本件任务", "key: C", "把两份调研合并",
		"## 上游结果", "### A", "at_A", "resultRevision=2", "A 的交接摘要", "reports/a.md", "kind=report",
		"### B", "at_B", "resultRevision=1", "B 的交接摘要",
		"## 执行约定", "submit_task_result",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("首条输入缺 %q\n---\n%s", want, got)
		}
	}
	for _, banned := range []string{"原始指令"} {
		if strings.Contains(got, banned) {
			t.Fatalf("首条输入不该带父任务原文 %q\n---\n%s", banned, got)
		}
	}
	// 上游按 key 排序：A 出现在 B 之前。
	if strings.Index(got, "### A") > strings.Index(got, "### B") {
		t.Fatalf("上游结果没按 key 排序")
	}
	// 无 parent 的单节点：没有「上游结果」段。
	solo := buildTaskFirstInput("目标", AgentTask{ID: "at_m", TaskKey: "main", Content: "单件"}, nil)
	if strings.Contains(solo, "上游结果") {
		t.Fatalf("无上游还写了「上游结果」段")
	}
	if !strings.Contains(solo, "key: main") || !strings.Contains(solo, "执行约定") {
		t.Fatalf("单节点输入缺本件／约定段：%s", solo)
	}
}

func mustHash(t *testing.T, p *orchPlan) string {
	t.Helper()
	h, err := p.hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// mustJSONStr 把结果形状落成 result 列的同款字节（parseTaskResult 读它）。
func mustJSONStr(t *testing.T, r TaskResult) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
