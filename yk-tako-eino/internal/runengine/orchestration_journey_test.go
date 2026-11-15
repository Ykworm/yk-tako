// 本文件是路径账本（journey）只读投影的单测（施工包 §12.1）：真 PG（5433 隔离临时库）+ 真 Redis。
//
// 覆盖面：初始版本由读侧从 created_revision=0 的 Task 合成（revision=0、reason=initial，修订表里没有这一版）；
// 顶层 planRevision 取存储值、summary.planRevisions = planRevision+1；被接受的修订还原 added／cancelled；
// 终态 Task（成功／失败／取消）都进时间线、pending 不进；失败节点带机器原因；结果复用与继任组来自
// 组容器 handoff.md／successor.json；relatedOrchestrationIds 指向前代与继任；steps 按 at 稳定排序；
// 两次读回字节一致；跨租户／未知 id 与不存在同形（errOrchNotFound）。
//
// 这些测试不装配模型、不启动 worker：建组走真实创建路径，修订走真实修订路径，其余靠原始 UPDATE 摆状态。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

// taskIDByKey 读组内某 key 的 Task id。
func taskIDByKey(t *testing.T, s *Store, orchID, key string) string {
	t.Helper()
	var tk AgentTask
	if err := s.db.Where("orchestration_id = ? AND task_key = ?", orchID, key).Take(&tk).Error; err != nil {
		t.Fatalf("读任务 %s/%s: %v", orchID, key, err)
	}
	return tk.ID
}

func decodeJourneySteps(t *testing.T, raws []json.RawMessage) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(raws))
	for _, raw := range raws {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("解步骤失败: %v\n%s", err, raw)
		}
		out = append(out, m)
	}
	return out
}

// stepBy 从时间线里取第一条满足谓词的步骤（找不到即失败）。
func stepBy(t *testing.T, steps []map[string]any, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	for _, st := range steps {
		if pred(st) {
			return st
		}
	}
	t.Fatalf("没有匹配的步骤：%v", steps)
	return nil
}

func asStrings(t *testing.T, v any) []string {
	t.Helper()
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("不是数组: %v", v)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("数组元素不是字符串: %v", e)
		}
		out = append(out, s)
	}
	return out
}

func asNum(t *testing.T, v any) int {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("不是数字: %v", v)
	}
	return int(f)
}

// TestOrchestrationJourneyAssembly 钉住 journey 的组装（施工包 §12.1）。
func TestOrchestrationJourneyAssembly(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "journey"), planEnv{})
	if err != nil {
		t.Fatalf("建组: %v", err)
	}
	orchID := resp.OrchestrationID

	var o AgentOrchestration
	if err := s.db.Where("id = ?", orchID).Take(&o).Error; err != nil {
		t.Fatal(err)
	}

	// 两次被接受的修订：rev1 取消 B；rev2 新增 D 挂到 C 之前（D→C，不换最终）。
	if _, err := s.CreatePlanRevision(ctx, testOwner, orchID, RevisionCommand{
		IdempotencyKey: "j-rev1", Reason: "不需要 B", CancelTasks: []string{"B"},
	}, planEnv{}); err != nil {
		t.Fatalf("rev1: %v", err)
	}
	if _, err := s.CreatePlanRevision(ctx, testOwner, orchID, RevisionCommand{
		IdempotencyKey: "j-rev2", BaseRevision: 1, Reason: "补充 D",
		AddTasks: []planTask{{Key: "D", Content: "整理数据", Agent: "researcher"}},
		AddLinks: []planLink{{Parent: "D", Child: "C"}},
	}, planEnv{}); err != nil {
		t.Fatalf("rev2: %v", err)
	}

	// 终态：A 成功、C 失败（带机器原因）；B 已被 rev1 取消（status=cancelled）；D 仍 pending（不进时间线）。
	aID := taskIDByKey(t, s, orchID, "A")
	cID := taskIDByKey(t, s, orchID, "C")
	mustExec(t, s, `UPDATE agent_tasks SET status = ?, finished_at = now(), updated_at = now() WHERE id = ?`,
		agentTaskStatusSucceeded, aID)
	mustExec(t, s, `UPDATE agent_tasks SET status = ?, error_code = ?, finished_at = now(), updated_at = now() WHERE id = ?`,
		agentTaskStatusFailed, taskErrRunError, cID)

	// 组容器：handoff（前代 or_prev_1 + 复用 A）+ successor（继任组 or_succ_1）。
	container, err := s.groupContainerDir(o)
	if err != nil {
		t.Fatal(err)
	}
	body, err := renderHandoff(handoffDoc{
		Kind: handoffKind, OrchestrationID: orchID, ReplanOf: "or_prev_1", ReplanDepth: 1,
		Reuse: []handoffReuseEntry{{TaskID: aID, ResultRevision: 1}},
	}, "上一组做法不适配", "换一组继续", "本地数据足够", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeHandoff(container, body); err != nil {
		t.Fatal(err)
	}
	if err := writeSuccessorMark(container, successorMark{
		OrchestrationID: orchID, SuccessorOrchestrationID: "or_succ_1",
		CreationKey: "j-succ", CreatedAt: o.CreatedAt.Add(5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	view, err := s.GetOrchestrationJourney(ctx, testOwner, orchID)
	if err != nil {
		t.Fatalf("GetOrchestrationJourney: %v", err)
	}

	if view.OrchestrationID != orchID {
		t.Fatalf("orchestrationId = %q, want %q", view.OrchestrationID, orchID)
	}
	if view.PlanRevision != 2 {
		t.Fatalf("planRevision = %d, want 2", view.PlanRevision)
	}
	wantSum := journeySummary{
		PlanRevisions: 3, TasksCreated: 4, Succeeded: 1, Failed: 1, Cancelled: 1,
		ReusedResults: 1, SuccessorGroups: 1,
	}
	if view.Summary != wantSum {
		t.Fatalf("summary = %+v\nwant %+v", view.Summary, wantSum)
	}
	if !reflect.DeepEqual(view.RelatedOrchestrationIDs, []string{"or_prev_1", "or_succ_1"}) {
		t.Fatalf("relatedOrchestrationIds = %v", view.RelatedOrchestrationIDs)
	}

	steps := decodeJourneySteps(t, view.Steps)
	// 1 初始 + 2 修订 + 3 终态 Task（A/B/C）+ 1 复用 + 1 继任 = 8。
	if len(steps) != 8 {
		t.Fatalf("steps 条数 = %d, want 8：%v", len(steps), steps)
	}

	// 首行是合成的初始版本：修订表里没有这一版，由读侧从 created_revision=0 的 Task 还原。
	init := steps[0]
	if init["type"] != "plan_revision" || asNum(t, init["revision"]) != 0 || init["reason"] != "initial" {
		t.Fatalf("首行不是初始版本：%v", init)
	}
	added := asStrings(t, init["added"])
	sort.Strings(added)
	if !reflect.DeepEqual(added, []string{"A", "B", "C"}) {
		t.Fatalf("初始版本 added = %v, want [A B C]", added)
	}

	// 被接受的两次修订：reason／added／cancelled 都从规范命令还原。
	rev1 := stepBy(t, steps, func(m map[string]any) bool {
		return m["type"] == "plan_revision" && asNum(t, m["revision"]) == 1
	})
	if rev1["reason"] != "不需要 B" {
		t.Fatalf("rev1 reason = %v", rev1["reason"])
	}
	if len(asStrings(t, rev1["added"])) != 0 {
		t.Fatalf("rev1 added = %v, want 空", rev1["added"])
	}
	if !reflect.DeepEqual(asStrings(t, rev1["cancelled"]), []string{"B"}) {
		t.Fatalf("rev1 cancelled = %v, want [B]", rev1["cancelled"])
	}
	rev2 := stepBy(t, steps, func(m map[string]any) bool {
		return m["type"] == "plan_revision" && asNum(t, m["revision"]) == 2
	})
	if rev2["reason"] != "补充 D" || !reflect.DeepEqual(asStrings(t, rev2["added"]), []string{"D"}) {
		t.Fatalf("rev2 = %v", rev2)
	}
	if _, ok := rev2["cancelled"]; ok {
		t.Fatalf("rev2 不应带 cancelled：%v", rev2)
	}

	// 终态 Task：成功、失败、取消都保留；pending 的 D 不进时间线。
	taskStep := func(key string) map[string]any {
		return stepBy(t, steps, func(m map[string]any) bool {
			return m["type"] == "task" && m["taskKey"] == key
		})
	}
	if got := taskStep("A"); got["status"] != agentTaskStatusSucceeded {
		t.Fatalf("A 步骤 = %v", got)
	}
	if got := taskStep("B"); got["status"] != agentTaskStatusCancelled {
		t.Fatalf("B 步骤 = %v（取消节点必须保留）", got)
	}
	if got := taskStep("C"); got["status"] != agentTaskStatusFailed || got["reason"] != taskErrRunError {
		t.Fatalf("C 步骤 = %v（失败节点须带机器原因）", got)
	}
	for _, st := range steps {
		if st["type"] == "task" && st["taskKey"] == "D" {
			t.Fatalf("pending 任务 D 不应进时间线：%v", st)
		}
	}

	// 复用与继任来自组容器文件。
	reuse := stepBy(t, steps, func(m map[string]any) bool { return m["type"] == "reuse" })
	if reuse["taskId"] != aID || asNum(t, reuse["resultRevision"]) != 1 {
		t.Fatalf("reuse 步骤 = %v", reuse)
	}
	succ := stepBy(t, steps, func(m map[string]any) bool { return m["type"] == "successor" })
	if succ["orchestrationId"] != "or_succ_1" {
		t.Fatalf("successor 步骤 = %v", succ)
	}

	// 带 at 的步骤按时间非降序排列。
	var last time.Time
	for _, st := range steps {
		v, ok := st["at"]
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, v.(string))
		if err != nil {
			t.Fatalf("at 不是时间: %v", v)
		}
		if at.Before(last) {
			t.Fatalf("步骤 at 逆序：%v < %v", at, last)
		}
		last = at
	}

	// 刷新／换副本读到的账本必须相同：只依赖 PG 行与组容器，不含进程内状态。
	again, err := s.GetOrchestrationJourney(ctx, testOwner, orchID)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(view)
	second, _ := json.Marshal(again)
	if string(first) != string(second) {
		t.Fatalf("两次读账本不一致：\n%s\n%s", first, second)
	}
}

// TestOrchestrationJourneyVisibility 钉住可见性：跨租户／非本人与不存在同形（errOrchNotFound）。
func TestOrchestrationJourneyVisibility(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	wsID, sessID := seedTaskEnv(t, s, "researcher")
	ctx := context.Background()
	resp, err := s.CreateAgentOrchestration(ctx, testOwner, mkTwoLayerCmd(wsID, sessID, "journey-vis"), planEnv{})
	if err != nil {
		t.Fatal(err)
	}
	stranger := principal{UserID: "u_other", TenantID: testOwner.TenantID, Role: "member"}
	if _, err := s.GetOrchestrationJourney(ctx, stranger, resp.OrchestrationID); !errors.Is(err, errOrchNotFound) {
		t.Fatalf("外人读组 err = %v, want errOrchNotFound", err)
	}
	if _, err := s.GetOrchestrationJourney(ctx, testOwner, "or_ghost"); !errors.Is(err, errOrchNotFound) {
		t.Fatalf("未知组 err = %v, want errOrchNotFound", err)
	}
}
