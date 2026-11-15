// 本文件是路径账本（journey）的只读投影（方案真源 = doc/82 §10.3；施工包 §9）。
//
// 病灶：只给最终结果会把重规划与失败重试变成黑箱——用户看到一份报告，却不知道原计划被改过几次、
// 哪些节点失败、哪些岔路被放弃、哪些结果被复用、是否换过继任组。失败重试被页面平滑成「成功」后，
// 用户也无法判断结果是否可信、代价是否值得。
//
// journey 从持久记录派生，不从模型回答或前端内存重建：
//   - 计划修订 → agent_plan_revisions（只能修订路径写入，不含初始那一版）；
//   - 初始版本 → 由读侧从 created_revision=0 的 Task 与初始边合成（revision 取 0、reason 固定
//     initial）——不为了对齐页面在创建事务补写一条修订行，那会把只读投影变成新的写入真源；
//   - Task 终态 → agent_tasks 的持久 status（不用读侧派生的 interrupted）；
//   - 结果复用与继任 → 组容器里的 handoff.md／successor.json（handoffReplanOf 同源）；
//   - 交付尝试 → agent_notification_attempts（按编排组取本组的尝试行）。
//
// 计数只算持久行，不把同一节点因刷新重复计算。跨租户或不可见统一 404（errOrchNotFound）；
// 刷新与换副本读到的账本必须相同——它只依赖 PG 行与组容器文件，不含进程内状态。
package runengine

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// orchestrationJourneyView 是 GET /api/orchestrations/:id/journey 的 data（doc/82 §10.3 字段序）：
// orchestrationId → planRevision → summary → steps → relatedOrchestrationIds。
// planRevision 是存储里的当前修订号；summary.planRevisions 是含初始版本的版本总数（= planRevision+1）。
type orchestrationJourneyView struct {
	OrchestrationID string            `json:"orchestrationId"`
	PlanRevision    int               `json:"planRevision"`
	Summary         journeySummary    `json:"summary"`
	Steps           []json.RawMessage `json:"steps"`
	// RelatedOrchestrationIDs 是跨组追踪的邻接：指向本组的前代（replanOf）与继任组，供页面互相打开。
	RelatedOrchestrationIDs []string `json:"relatedOrchestrationIds"`
}

// journeySummary 是概览计数（doc/82 §10.3 字段序固定）。
type journeySummary struct {
	PlanRevisions    int `json:"planRevisions"`
	TasksCreated     int `json:"tasksCreated"`
	Succeeded        int `json:"succeeded"`
	Failed           int `json:"failed"`
	Cancelled        int `json:"cancelled"`
	Blocked          int `json:"blocked"`
	ReusedResults    int `json:"reusedResults"`
	SuccessorGroups  int `json:"successorGroups"`
	DeliveryAttempts int `json:"deliveryAttempts"`
}

// journeyRevisionStep 是 steps 里 type=plan_revision 的一行：初始合成行与每次被接受的修订共用。
// added 恒出现（初始无新增时是空数组）；cancelled 只在本次取消过节点时出现。
type journeyRevisionStep struct {
	Type      string    `json:"type"`
	Revision  int       `json:"revision"`
	At        time.Time `json:"at"`
	Reason    string    `json:"reason"`
	Added     []string  `json:"added"`
	Cancelled []string  `json:"cancelled,omitempty"`
}

// journeyTaskStep 是 steps 里 type=task 的一行：一个已到终态的节点。failed 带机器原因（ErrorCode）。
type journeyTaskStep struct {
	Type    string `json:"type"`
	TaskID  string `json:"taskId"`
	TaskKey string `json:"taskKey"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
}

// journeyReuseStep 是 steps 里 type=reuse 的一行：一个被继任组复用的结果。
type journeyReuseStep struct {
	Type           string    `json:"type"`
	TaskID         string    `json:"taskId"`
	ResultRevision int       `json:"resultRevision"`
	At             time.Time `json:"at"`
}

// journeySuccessorStep 是 steps 里 type=successor 的一行：本组被换成了哪个继任组。
type journeySuccessorStep struct {
	Type            string    `json:"type"`
	OrchestrationID string    `json:"orchestrationId"`
	At              time.Time `json:"at"`
}

// journeyDeliveryStep 是 steps 里 type=delivery_attempt 的一行：一次交付尝试（哪个 run 处理了通知）。
type journeyDeliveryStep struct {
	Type    string    `json:"type"`
	Attempt int       `json:"attempt"`
	RunID   string    `json:"runId"`
	At      time.Time `json:"at"`
}

// journeyStepEntry 是排序用的中间项：at 决定时间线顺序，raw 是最终字节。
type journeyStepEntry struct {
	at  time.Time
	raw json.RawMessage
}

// mustStepJSON 把一步编成字节；这些结构体不会编失败，出错只记日志并跳过。
func (s *Store) stepJSON(v any) (json.RawMessage, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("orchestration journey: 步骤编组失败: %v", err)
		return nil, false
	}
	return json.RawMessage(b), true
}

// taskTerminalStatus 判断一个 Task 是否已到终态（时间线只画终态节点）。
func taskTerminalStatus(status string) bool {
	switch status {
	case agentTaskStatusSucceeded, agentTaskStatusFailed, agentTaskStatusCancelled, agentTaskStatusBlocked:
		return true
	default:
		return false
	}
}

// GetOrchestrationJourney 组装一个编排组的路径账本（施工包 §9）。可见性走既有属主链
// （orchestration → source_session → 本人），非本人／跨租户与不存在同形（errOrchNotFound）。
func (s *Store) GetOrchestrationJourney(ctx context.Context, p principal, id string) (orchestrationJourneyView, error) {
	var orows []AgentOrchestration
	if err := s.db.WithContext(ctx).Raw(`
SELECT o.* FROM agent_orchestrations o
  JOIN sessions s ON s.id = o.source_session_id
 WHERE o.id = ? AND o.tenant_id = ? AND s.user_id = ?`, id, p.TenantID, p.UserID).
		Scan(&orows).Error; err != nil {
		return orchestrationJourneyView{}, err
	}
	if len(orows) == 0 {
		return orchestrationJourneyView{}, errOrchNotFound
	}
	o := orows[0]

	var tasks []AgentTask
	if err := s.db.WithContext(ctx).Where("orchestration_id = ?", id).
		Order("created_at, task_key").Find(&tasks).Error; err != nil {
		return orchestrationJourneyView{}, err
	}
	var revs []AgentPlanRevision
	if err := s.db.WithContext(ctx).Where("orchestration_id = ?", id).
		Order("revision ASC").Find(&revs).Error; err != nil {
		return orchestrationJourneyView{}, err
	}
	var attempts []AgentNotificationAttempt
	if err := s.db.WithContext(ctx).Raw(`
SELECT a.* FROM agent_notification_attempts a
  JOIN agent_notifications n ON n.id = a.notification_id
 WHERE n.orchestration_id = ? AND a.tenant_id = ?
 ORDER BY a.created_at, a.id`, id, p.TenantID).Scan(&attempts).Error; err != nil {
		return orchestrationJourneyView{}, err
	}

	// 组容器里的继任与复用（文件不是真源，读失败只记日志，账本照常给出 PG 部分）。
	var (
		doc  *handoffDoc
		mark *successorMark
	)
	if container, err := s.groupContainerDir(o); err != nil {
		log.Printf("orchestration journey %s: 定位组容器失败: %v", o.ID, err)
	} else {
		if d, _, err := readHandoff(container, o.ID); err != nil {
			log.Printf("orchestration journey %s: 读 handoff 失败: %v", o.ID, err)
		} else {
			doc = d
		}
		if m, err := readSuccessorMark(container); err != nil {
			log.Printf("orchestration journey %s: 读 successor 失败: %v", o.ID, err)
		} else {
			mark = m
		}
	}

	// —— steps：按时间线拼装，最后按 at 稳定排序 ——
	entries := make([]journeyStepEntry, 0, len(tasks)+len(revs)+len(attempts)+2)

	// 初始版本：读侧从 created_revision=0 的 Task 合成（修订表里没有这一版）。
	initialAdded := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.CreatedRevision == 0 {
			initialAdded = append(initialAdded, t.TaskKey)
		}
	}
	if raw, ok := s.stepJSON(journeyRevisionStep{
		Type: "plan_revision", Revision: 0, At: o.CreatedAt, Reason: "initial", Added: initialAdded,
	}); ok {
		entries = append(entries, journeyStepEntry{at: o.CreatedAt, raw: raw})
	}

	// 每次被接受的修订：新增节点、取消节点与原因都从规范命令还原。
	for _, r := range revs {
		cmd := revisionFromRow(r)
		added := make([]string, 0, len(cmd.AddTasks))
		for _, t := range cmd.AddTasks {
			added = append(added, t.Key)
		}
		if raw, ok := s.stepJSON(journeyRevisionStep{
			Type: "plan_revision", Revision: r.Revision, At: r.CreatedAt, Reason: r.Reason,
			Added: added, Cancelled: cmd.CancelTasks,
		}); ok {
			entries = append(entries, journeyStepEntry{at: r.CreatedAt, raw: raw})
		}
	}

	// 每个终态 Task：失败节点与取消节点都保留（模型回答里没写的也必须出现）。
	for _, t := range tasks {
		if !taskTerminalStatus(t.Status) {
			continue
		}
		at := t.UpdatedAt
		if t.FinishedAt != nil {
			at = *t.FinishedAt
		}
		if raw, ok := s.stepJSON(journeyTaskStep{
			Type: "task", TaskID: t.ID, TaskKey: t.TaskKey, Status: t.Status, Reason: t.ErrorCode,
		}); ok {
			entries = append(entries, journeyStepEntry{at: at, raw: raw})
		}
	}

	// 结果复用：继任组 handoff 里的 reuse 清单（复用发生在换组那一刻）。
	var reuse []handoffReuseEntry
	if doc != nil {
		reuse = doc.Reuse
		for _, u := range reuse {
			if raw, ok := s.stepJSON(journeyReuseStep{
				Type: "reuse", TaskID: u.TaskID, ResultRevision: u.ResultRevision, At: o.CreatedAt,
			}); ok {
				entries = append(entries, journeyStepEntry{at: o.CreatedAt, raw: raw})
			}
		}
	}

	// 继任组 handoff：本组被换成了哪个组。
	if mark != nil && mark.SuccessorOrchestrationID != "" {
		if raw, ok := s.stepJSON(journeySuccessorStep{
			Type: "successor", OrchestrationID: mark.SuccessorOrchestrationID, At: mark.CreatedAt,
		}); ok {
			entries = append(entries, journeyStepEntry{at: mark.CreatedAt, raw: raw})
		}
	}

	// 完成通知与交付尝试：本组每一次被处理的尝试。
	for _, a := range attempts {
		if raw, ok := s.stepJSON(journeyDeliveryStep{
			Type: "delivery_attempt", Attempt: a.Attempt, RunID: a.RunID, At: a.CreatedAt,
		}); ok {
			entries = append(entries, journeyStepEntry{at: a.CreatedAt, raw: raw})
		}
	}

	// 时间线顺序来自持久记录：按 at 稳定排序（插入顺序是同一时刻的确定性 tie-break）。
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	steps := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		steps = append(steps, e.raw)
	}

	// —— summary 计数：只算持久行 ——
	sum := journeySummary{
		PlanRevisions:    o.PlanRevision + 1, // 含读侧合成的初始版本
		TasksCreated:     len(tasks),
		ReusedResults:    len(reuse),
		DeliveryAttempts: len(attempts),
	}
	for _, t := range tasks {
		switch t.Status {
		case agentTaskStatusSucceeded:
			sum.Succeeded++
		case agentTaskStatusFailed:
			sum.Failed++
		case agentTaskStatusCancelled:
			sum.Cancelled++
		case agentTaskStatusBlocked:
			sum.Blocked++
		}
	}
	if mark != nil && mark.SuccessorOrchestrationID != "" {
		sum.SuccessorGroups = 1
	}

	// —— 跨组追踪：前代（replanOf）与继任组 ——
	related := make([]string, 0, 2)
	if doc != nil && doc.ReplanOf != "" && doc.ReplanOf != o.ID {
		related = append(related, doc.ReplanOf)
	}
	if mark != nil && mark.SuccessorOrchestrationID != "" && mark.SuccessorOrchestrationID != o.ID {
		related = append(related, mark.SuccessorOrchestrationID)
	}

	return orchestrationJourneyView{
		OrchestrationID:         o.ID,
		PlanRevision:            o.PlanRevision,
		Summary:                 sum,
		Steps:                   steps,
		RelatedOrchestrationIDs: related,
	}, nil
}

// getOrchestrationJourney 读一个编排组的路径账本（施工包 §9）。读面只挂 authn，可见性在 store 层 JOIN。
func (s *Server) getOrchestrationJourney(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	view, err := s.store.GetOrchestrationJourney(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", view)
}
