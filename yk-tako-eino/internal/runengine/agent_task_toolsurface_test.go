package runengine

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/components/tool"
)

// TestTaskToolSurfaces 钉住三类角色的工具面（施工包 §11.1 第 8 条）：
//   - Task 执行者拿得到 ask_user，但不能改图、不能再建编排、不能走来源会话的计划审批；
//   - 计划复核 run 只留只读与两把决策工具（新工具默认不可用，失败方向与执行者相反）；
//   - 交付 run 去掉一切等人与再编排，保证「协调 run complete」能收口。
func TestTaskToolSurfaces(t *testing.T) {
	ctx := context.Background()
	all := []tool.BaseTool{}
	names := []string{
		"ask_user", "bash", "read_text_file", "revise_orchestration", "continue_orchestration",
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
	exec := gotNames(w.agentTaskExecutorBase(ctx, nil))
	for _, want := range []string{"ask_user", "bash", "read_text_file", "submit_task_result"} {
		if !exec[want] {
			t.Fatalf("Task 执行者缺工具 %q：%v", want, exec)
		}
	}
	for _, denied := range []string{
		"revise_orchestration", "continue_orchestration", "delegate_task", "create_orchestration",
		"enter_plan_mode", "submit_plan", "memory", "use_skill",
	} {
		if exec[denied] {
			t.Fatalf("Task 执行者拿到了 %q：%v", denied, exec)
		}
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

	delivery := gotNames(dropDeliveryNoTools(ctx, all))
	if delivery["ask_user"] || delivery["revise_orchestration"] || delivery["delegate_task"] {
		t.Fatalf("交付 run 还留着等人／再编排工具：%v", delivery)
	}
	if !delivery["read_text_file"] || !delivery["bash"] {
		t.Fatalf("交付 run 被误删了只读／执行工具：%v", delivery)
	}
}
