package runengine

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/components/tool"
)

// TestTaskToolSurfaces 钉住三类角色的工具面（施工包 §11.1 第 8 条）：
//   - Task 执行者拿得到 ask_user 与后台命令的读/停（bash_output／stop_bash），但不能改图、
//     不能再建编排、不能走来源会话的计划审批，也拿不到 wait_task（按任务号等待是来源会话的能力）；
//   - 计划复核 run 只留只读与两把决策工具（新工具默认不可用，失败方向与执行者相反）；
//   - 交付 run 是严格只读白名单（doc/73 §10）：bash／等人／再编排都不进池。
func TestTaskToolSurfaces(t *testing.T) {
	ctx := context.Background()
	all := []tool.BaseTool{}
	names := []string{
		"ask_user", "bash", "bash_output", "stop_bash", "wait_task",
		"read_text_file", "revise_orchestration", "continue_orchestration",
		"delegate_task", "create_orchestration", "enter_plan_mode", "submit_plan", "memory", "use_skill",
	}
	for _, n := range names {
		all = append(all, &fakeInvokableTool{info: &schema.ToolInfo{Name: n}})
	}
	gotNames := func(ts []tool.BaseTool) map[string]bool {
		out := make(map[string]bool, len(ts))
		for _, x := range ts {
			info, err := x.Info(ctx)
			if err != nil {
				t.Fatalf("Info: %v", err)
			}
			out[info.Name] = true
		}
		return out
	}

	s := newTestStore(t, testLease, testTTL)
	w := &Worker{store: s, tools: all}
	execTools := w.agentTaskExecutorBase(ctx, nil)
	if st := w.agentTaskSubmitTool(); st != nil {
		execTools = append(execTools, st)
	}
	exec := gotNames(execTools)
	for _, want := range []string{"ask_user", "bash", "bash_output", "stop_bash", "read_text_file", "submit_task_result"} {
		if !exec[want] {
			t.Fatalf("Task 执行者缺工具 %q：%v", want, exec)
		}
	}
	for _, denied := range []string{
		"revise_orchestration", "continue_orchestration", "delegate_task", "create_orchestration",
		"enter_plan_mode", "submit_plan", "memory", "use_skill",
		// wait_task 由 agentTaskNoTools 剔出（执行者没有本会话的其它 Task 可等，doc/82 §4-10）。
		"wait_task",
	} {
		if exec[denied] {
			t.Fatalf("Task 执行者拿到了 %q：%v", denied, exec)
		}
	}

	// 白名单档不裁交付口：coder 就是 tools: 白名单，submit_task_result 若留在基座里会被
	// resolveToolPool 裁掉，执行者永远交不出结果（result_not_submitted）。保留名跳过 + 装配后补。
	listed := []string{"read_text_file", taskSubmitToolName}
	pool, err := resolveToolPool(ctx, AgentSpec{Tools: &listed}, w.agentTaskExecutorBase(ctx, nil))
	if err != nil {
		t.Fatalf("白名单写了保留名 submit_task_result 不该报未知名: %v", err)
	}
	if len(pool) != 1 {
		t.Fatalf("白名单过滤后只该剩 read_text_file（submit 由装配追加），got %d", len(pool))
	}
	pool = append(pool, w.agentTaskSubmitTool())
	if !gotNames(pool)["submit_task_result"] {
		t.Fatal("白名单规格的最终池里没有 submit_task_result")
	}

	review := gotNames(keepPlanReviewTools(ctx, all))
	for _, want := range []string{"read_text_file", "revise_orchestration", "continue_orchestration"} {
		if !review[want] {
			t.Fatalf("计划复核 run 缺工具 %q：%v", want, review)
		}
	}
	for _, denied := range []string{"ask_user", "bash", "delegate_task", "memory"} {
		if review[denied] {
			t.Fatalf("计划复核 run 拿到了 %q：%v", denied, review)
		}
	}

	delivery := gotNames(keepDeliveryReadOnlyTools(ctx, all))
	if delivery["ask_user"] || delivery["revise_orchestration"] || delivery["delegate_task"] {
		t.Fatalf("交付 run 还留着等人／再编排工具：%v", delivery)
	}
	if delivery["bash"] || delivery["submit_task_result"] || delivery["memory"] || delivery["use_skill"] {
		t.Fatalf("交付 run 拿到了非只读工具：%v", delivery)
	}
	if !delivery["read_text_file"] {
		t.Fatalf("交付 run 被误删了只读工具：%v", delivery)
	}
}

// TestSubmitTaskResultErrorAsResult 钉住 submit_task_result 进执行者池时包了 wrapToolErrors：
// 提交被拒（验证未过／没有 run 身份等业务错误）必须回注「错误：…」结果让执行者返工，
// 而不是裸抛 error 被 ToolsNode 当 NodeRunError 打挂整场 run（doc/74 §8.2 验证返工的前提）。
// 工具本身由 agentTaskSubmitTool 单独造——它装配在规格过滤之后，不在执行者基座里。
func TestSubmitTaskResultErrorAsResult(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	w := &Worker{store: s}
	var submit tool.InvokableTool
	if st := w.agentTaskSubmitTool(); st != nil {
		info, err := st.Info(ctx)
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.Name == "submit_task_result" {
			submit, _ = st.(tool.InvokableTool)
		}
	}
	if submit == nil {
		t.Fatal("执行者池里没有 submit_task_result")
	}
	// 没有 run 身份 → 工具返回业务错误；包装后应是 nil error + 「错误：…」文本。
	out, err := submit.InvokableRun(ctx, `{"summary":"x","artifacts":[]}`)
	if err != nil {
		t.Fatalf("submit_task_result 拒绝裸抛了 error（漏包 wrapToolErrors，会打挂 run）：%v", err)
	}
	if !strings.HasPrefix(out, "错误：") || !strings.Contains(out, "没有 run 身份") {
		t.Fatalf("拒绝应回注错误文本供返工，got %q", out)
	}
}
