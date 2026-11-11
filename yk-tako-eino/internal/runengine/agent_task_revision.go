// 本文件是计划修订的统一服务：规范化命令、预算与整图校验、接受修订、继续与历史读取
// （方案真源 = doc/72 §5/§6；施工包 §4.2–§4.4、§6）。HTTP 端点与模型工具都走这里，
// 不按入口各写一套校验。
//
// 确定性边界：修订事务只读写 PG，不在锁内调用模型；图版本用 base_revision CAS 推进；
// 幂等键落在 agent_plan_revisions 的唯一索引上（同键同命令返回原修订，不同命令 117108）。
package runengine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	errRevisionStale   = errors.New("plan revision stale")
	errOrchNotRevisable = errors.New("orchestration not revisable")
	errRevisionConflict = errors.New("revision idempotency key reused for another command")
)

const (
	revisionReasonMaxRunes = 8000
	// revisionLinksMax 是一个 32 节点 DAG 的最大边数（n*(n-1)/2），继续加边只会成环。
	revisionLinksMax = agentTaskMaxCount * (agentTaskMaxCount - 1) / 2
)

// RevisionCommand 是接受一次计划修订的入参（HTTP 与模型工具共用）。
type RevisionCommand struct {
	IdempotencyKey string
	BaseRevision   int
	Reason         string
	SourceRunID    string
	AddTasks       []planTask
	AddLinks       []planLink
	CancelTasks    []string
	FinalTaskKey   string
}

// revisionCommand 是规范化后的命令：数组按固定键排序，输入顺序不影响 command_hash。
type revisionCommand struct {
	Reason       string     `json:"reason"`
	AddTasks     []planTask `json:"addTasks"`
	AddLinks     []planLink `json:"addLinks"`
	CancelTasks  []string   `json:"cancelTasks"`
	FinalTaskKey string     `json:"finalTaskKey"`
}

// revisionTaskView 是修订历史里的节点视图：字段齐全（planCheckpoint 恒出现），与哈希输入分开。
type revisionTaskView struct {
	Key            string `json:"key"`
	Content        string `json:"content"`
	Agent          string `json:"agent"`
	ModelRef       string `json:"modelRef"`
	PlanCheckpoint bool   `json:"planCheckpoint"`
}

// revisionView 是 GET /api/orchestrations/:id/revisions 的一行（doc/72 §4.4 字段序）。
type revisionView struct {
	RevisionID   string             `json:"revisionId"`
	Revision     int                `json:"revision"`
	BaseRevision int                `json:"baseRevision"`
	Reason       string             `json:"reason"`
	AddTasks     []revisionTaskView `json:"addTasks"`
	AddLinks     []planLink         `json:"addLinks"`
	CancelTasks  []string           `json:"cancelTasks"`
	FinalTaskKey string             `json:"finalTaskKey"`
	SourceRunID  string             `json:"sourceRunId"`
	CreatedAt    time.Time          `json:"createdAt"`
}

// revisionResp 是 POST /api/orchestrations/:id/revisions 的 data（doc/72 §4.2 字段序）。
type revisionResp struct {
	RevisionID      string   `json:"revisionId"`
	OrchestrationID string   `json:"orchestrationId"`
	Revision        int      `json:"revision"`
	FinalTaskID     string   `json:"finalTaskId"`
	PlanHold        bool     `json:"planHold"`
	AddedTaskIDs    []string `json:"addedTaskIds"`
}

// continueResp 是 POST /api/orchestrations/:id/continue 的 data（doc/72 §4.3 字段序）。
type continueResp struct {
	OrchestrationID string `json:"orchestrationId"`
	Revision        int    `json:"revision"`
	PlanHold        bool   `json:"planHold"`
}

// normalizeRevisionCommand 做不碰数据库的纯校验并产出规范命令。任何一条不过都回 *planError
//（API 层 400/117101，原因进 data.reason）。
func normalizeRevisionCommand(cmd RevisionCommand) (*revisionCommand, error) {
	if strings.TrimSpace(cmd.IdempotencyKey) == "" {
		return nil, &planError{"idempotencyKey 必填"}
	}
	if len(cmd.IdempotencyKey) > idempotencyMaxBytes {
		return nil, &planError{"idempotencyKey 超过 128 字节"}
	}
	if cmd.BaseRevision < 0 || cmd.BaseRevision > agentTaskMaxCount {
		return nil, &planError{"baseRevision 不合法"}
	}
	if strings.TrimSpace(cmd.Reason) == "" {
		return nil, &planError{"修订原因必填"}
	}
	if utf8Len(cmd.Reason) > revisionReasonMaxRunes {
		return nil, &planError{fmt.Sprintf("修订原因超过 %d 字", revisionReasonMaxRunes)}
	}
	if len(cmd.AddTasks) > agentTaskMaxCount {
		return nil, &planError{"一次修订最多新增 32 件 Task"}
	}
	added := make(map[string]bool, len(cmd.AddTasks))
	for _, t := range cmd.AddTasks {
		if !taskKeyRe.MatchString(t.Key) {
			return nil, &planError{"Task key 只允许字母数字开头的 1–64 位字母数字、_ 或 -：" + t.Key}
		}
		if added[t.Key] {
			return nil, &planError{"新增 Task key 重复：" + t.Key}
		}
		added[t.Key] = true
		if strings.TrimSpace(t.Content) == "" {
			return nil, &planError{"Task 内容必填：" + t.Key}
		}
		if utf8Len(t.Content) > taskContentMaxRunes {
			return nil, &planError{fmt.Sprintf("Task 内容超过 %d 字：%s", taskContentMaxRunes, t.Key)}
		}
		if strings.TrimSpace(t.Agent) == "" {
			return nil, &planError{"Task 执行者必填：" + t.Key}
		}
	}
	if len(cmd.AddLinks) > revisionLinksMax {
		return nil, &planError{fmt.Sprintf("一次修订最多新增 %d 条依赖边", revisionLinksMax)}
	}
	linkSeen := make(map[planLink]bool, len(cmd.AddLinks))
	for _, l := range cmd.AddLinks {
		if strings.TrimSpace(l.Parent) == "" || strings.TrimSpace(l.Child) == "" {
			return nil, &planError{"依赖边必须同时给 parent 与 child"}
		}
		if l.Parent == l.Child {
			return nil, &planError{"依赖边不允许自依赖：" + l.Parent}
		}
		if linkSeen[l] {
			return nil, &planError{"依赖边重复：" + l.Parent + " -> " + l.Child}
		}
		linkSeen[l] = true
	}
	if len(cmd.CancelTasks) > agentTaskMaxCount {
		return nil, &planError{"一次修订最多取消 32 件 Task"}
	}
	cancelled := make(map[string]bool, len(cmd.CancelTasks))
	cleanCancel := make([]string, 0, len(cmd.CancelTasks))
	for _, k := range cmd.CancelTasks {
		if !taskKeyRe.MatchString(k) {
			return nil, &planError{"待取消的 Task key 不合法：" + k}
		}
		if cancelled[k] {
			return nil, &planError{"待取消的 Task key 重复：" + k}
		}
		cancelled[k] = true
		if added[k] {
			return nil, &planError{"同一修订里不能既新增又取消：" + k}
		}
		cleanCancel = append(cleanCancel, k)
	}
	if strings.TrimSpace(cmd.FinalTaskKey) != "" && !taskKeyRe.MatchString(cmd.FinalTaskKey) {
		return nil, &planError{"最终 Task key 不合法：" + cmd.FinalTaskKey}
	}
	if len(cmd.AddTasks) == 0 && len(cmd.AddLinks) == 0 && len(cleanCancel) == 0 &&
		strings.TrimSpace(cmd.FinalTaskKey) == "" {
		return nil, &planError{"修订命令为空"}
	}
	out := &revisionCommand{
		Reason:       cmd.Reason,
		AddTasks:     append([]planTask(nil), cmd.AddTasks...),
		AddLinks:     append([]planLink(nil), cmd.AddLinks...),
		CancelTasks:  cleanCancel,
		FinalTaskKey: strings.TrimSpace(cmd.FinalTaskKey),
	}
	sort.Slice(out.AddTasks, func(i, j int) bool { return out.AddTasks[i].Key < out.AddTasks[j].Key })
	sort.Slice(out.AddLinks, func(i, j int) bool {
		if out.AddLinks[i].Parent != out.AddLinks[j].Parent {
			return out.AddLinks[i].Parent < out.AddLinks[j].Parent
		}
		return out.AddLinks[i].Child < out.AddLinks[j].Child
	})
	sort.Strings(out.CancelTasks)
	if out.AddTasks == nil {
		out.AddTasks = []planTask{}
	}
	if out.AddLinks == nil {
		out.AddLinks = []planLink{}
	}
	return out, nil
}

func (c *revisionCommand) hash() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum), nil
}

// liveNodeStatuses 是参与「可达最终节点」检查的节点：修订后仍可能工作的节点。
// succeeded 只作路径中转，本身不要求到达最终（它已经交过结果）。
var liveNodeStatuses = map[string]bool{
	agentTaskStatusPending:   true,
	agentTaskStatusReady:     true,
	agentTaskStatusRunning:   true,
	agentTaskStatusSucceeded: true,
}

// activeWorkStatuses 是必须能沿边到达最终 Task 的状态（施工包 §6.2 第 11 条）。
var activeWorkStatuses = map[string]bool{
	agentTaskStatusPending: true,
	agentTaskStatusReady:   true,
	agentTaskStatusRunning: true,
}

// validateNewRevisionAgents 校验新增节点的执行者与显式模型 ref（与创建同一条规则）。
func (s *Store) validateNewRevisionAgents(ctx context.Context, p principal, tasks []planTask, env planEnv) error {
	if len(tasks) == 0 {
		return nil
	}
	if err := s.validatePlanAgents(ctx, p, &orchPlan{Tasks: tasks}); err != nil {
		return err
	}
	for _, t := range tasks {
		ref := strings.TrimSpace(t.ModelRef)
		if ref == "" || isInheritRef(ref) || env.resolveModel == nil {
			continue
		}
		if err := env.resolveModel(ctx, ref); err != nil {
			return fmt.Errorf("%w: %s: %v", errCreateModel, ref, err)
		}
	}
	return nil
}

// CreatePlanRevision 接受一次计划修订（施工包 §6.2 的 14 步压缩成：纯校验 → 事务）。
func (s *Store) CreatePlanRevision(ctx context.Context, p principal, orchID string, cmd RevisionCommand, env planEnv) (revisionResp, error) {
	norm, err := normalizeRevisionCommand(cmd)
	if err != nil {
		return revisionResp{}, err
	}
	hash, err := norm.hash()
	if err != nil {
		return revisionResp{}, err
	}
	commandJSON, err := json.Marshal(norm)
	if err != nil {
		return revisionResp{}, err
	}
	if err := s.validateNewRevisionAgents(ctx, p, norm.AddTasks, env); err != nil {
		return revisionResp{}, err
	}

	var resp revisionResp
	var changedTaskIDs []string
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o AgentOrchestration
		res := tx.Raw(`
SELECT o.* FROM agent_orchestrations o
  JOIN sessions s ON s.id = o.source_session_id
 WHERE o.id = ? AND o.tenant_id = ? AND s.user_id = ?
 FOR UPDATE OF o`, orchID, p.TenantID, p.UserID).Scan(&o)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errOrchNotFound
		}
		// 幂等先于状态检查：成功请求的幂等重打即使在组终态后也应返回原修订（施工包 §6.2 第 3 条）。
		var prior AgentPlanRevision
		qerr := tx.Where("tenant_id = ? AND orchestration_id = ? AND idempotency_key = ?",
			p.TenantID, orchID, cmd.IdempotencyKey).Take(&prior).Error
		if qerr == nil {
			if prior.CommandHash != hash {
				return errRevisionConflict
			}
			added, err := revisionAddedTaskIDs(tx, orchID, prior.Revision, prior.CommandJSON)
			if err != nil {
				return err
			}
			resp = revisionResp{
				RevisionID: prior.ID, OrchestrationID: orchID, Revision: prior.Revision,
				FinalTaskID: o.FinalTaskID, PlanHold: o.PlanHold, AddedTaskIDs: added,
			}
			return nil
		} else if !errors.Is(qerr, gorm.ErrRecordNotFound) {
			return qerr
		}
		if o.PlanRevision != cmd.BaseRevision {
			return errRevisionStale
		}
		if o.Status != orchStatusActive {
			return errOrchNotRevisable
		}
		var finalRow AgentTask
		if err := tx.Where("id = ?", o.FinalTaskID).Take(&finalRow).Error; err != nil {
			return err
		}
		if finalRow.StartedAt != nil {
			return errOrchNotRevisable
		}
		// 累计预算：历史节点（含已取消）计入，取消不释放；版本上限同为 32。
		var total int64
		if err := tx.Model(&AgentTask{}).Where("orchestration_id = ?", orchID).Count(&total).Error; err != nil {
			return err
		}
		if total+int64(len(norm.AddTasks)) > agentTaskMaxCount {
			return &planError{fmt.Sprintf("编排组最多 %d 件 Task（含历史），修订后将达到 %d 件",
				agentTaskMaxCount, total+int64(len(norm.AddTasks)))}
		}
		if o.PlanRevision >= agentTaskMaxCount {
			return &planError{fmt.Sprintf("编排组最多接受 %d 次修订", agentTaskMaxCount)}
		}
		// 锁住的顺序与调度侧一致：先组、再全部 Task 行。状态判断在锁内做，取消不会与领取并发。
		var tasks []AgentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("orchestration_id = ?", orchID).Find(&tasks).Error; err != nil {
			return err
		}
		var links []AgentTaskLink
		if err := tx.Where("orchestration_id = ?", orchID).Find(&links).Error; err != nil {
			return err
		}
		byKey := make(map[string]AgentTask, len(tasks))
		byID := make(map[string]AgentTask, len(tasks))
		for _, t := range tasks {
			byKey[t.TaskKey] = t
			byID[t.ID] = t
		}
		for _, t := range norm.AddTasks {
			if _, ok := byKey[t.Key]; ok {
				return &planError{"Task key 与组内已有节点重复：" + t.Key}
			}
		}
		cancelled := make(map[string]bool, len(norm.CancelTasks))
		for _, k := range norm.CancelTasks {
			t, ok := byKey[k]
			if !ok {
				return &planError{"待取消的 Task 不在本组：" + k}
			}
			switch t.Status {
			case agentTaskStatusPending, agentTaskStatusReady, agentTaskStatusBlocked:
			default:
				return &planError{"只能取消 pending／ready／blocked 的 Task：" + k + "（当前 " + t.Status + "）"}
			}
			cancelled[k] = true
		}
		if cancelled[finalRow.TaskKey] && norm.FinalTaskKey == "" {
			return &planError{"取消当前最终 Task 时必须指定新的最终节点"}
		}
		// 边：端点必须存在（现有或新增），目标只能是 pending/ready（新增节点按新建状态），
		// parent 不能是 failed/blocked/cancelled 历史。
		newLinkKeys := make(map[planLink]bool, len(norm.AddLinks))
		existingLinks := make(map[planLink]bool, len(links))
		for _, l := range links {
			p := byID[l.ParentTaskID]
			c := byID[l.ChildTaskID]
			existingLinks[planLink{Parent: p.TaskKey, Child: c.TaskKey}] = true
		}
		added := make(map[string]bool, len(norm.AddTasks))
		for _, t := range norm.AddTasks {
			added[t.Key] = true
		}
		parentOf := make(map[string][]string)
		for _, l := range norm.AddLinks {
			_, pNew := added[l.Parent]
			pTask, pOK := byKey[l.Parent]
			if !pNew && !pOK {
				return &planError{"依赖边 parent 不在本组：" + l.Parent}
			}
			_, cNew := added[l.Child]
			cTask, cOK := byKey[l.Child]
			if !cNew && !cOK {
				return &planError{"依赖边 child 不在本组：" + l.Child}
			}
			if existingLinks[l] || newLinkKeys[l] {
				return &planError{"依赖边已存在：" + l.Parent + " -> " + l.Child}
			}
			newLinkKeys[l] = true
			if !pNew {
				switch pTask.Status {
				case agentTaskStatusFailed, agentTaskStatusBlocked, agentTaskStatusCancelled:
					return &planError{"依赖边 parent 是终态历史：" + l.Parent + "（" + pTask.Status + "）"}
				}
			}
			if !cNew {
				switch cTask.Status {
				case agentTaskStatusPending, agentTaskStatusReady:
				default:
					return &planError{"依赖边 child 只能是 pending／ready：" + l.Child + "（当前 " + cTask.Status + "）"}
				}
			}
			parentOf[l.Child] = append(parentOf[l.Child], l.Parent)
		}
		// 新最终节点：必须存在、未被取消、当前 pending/ready（新增节点天然满足）。
		newFinalKey := finalRow.TaskKey
		if norm.FinalTaskKey != "" {
			newFinalKey = norm.FinalTaskKey
			if cancelled[newFinalKey] {
				return &planError{"新最终节点不能同时被取消：" + newFinalKey}
			}
			if t, ok := byKey[newFinalKey]; ok {
				switch t.Status {
				case agentTaskStatusPending, agentTaskStatusReady:
				default:
					return &planError{"新最终节点只能是 pending／ready：" + newFinalKey + "（当前 " + t.Status + "）"}
				}
			} else if !added[newFinalKey] {
				return &planError{"新最终节点不在本组：" + newFinalKey}
			}
		}
		// 组装修订后的活跃图：排除 failed/blocked/cancelled 与本批取消；succeeded 保留作路径中转。
		type node struct {
			key    string
			status string
			id     string
		}
		nodes := make(map[string]node, len(tasks)+len(norm.AddTasks))
		for _, t := range tasks {
			if cancelled[t.TaskKey] || !liveNodeStatuses[t.Status] {
				continue
			}
			nodes[t.TaskKey] = node{key: t.TaskKey, status: t.Status, id: t.ID}
		}
		for _, t := range norm.AddTasks {
			st := agentTaskStatusReady
			if len(parentOf[t.Key]) > 0 {
				st = agentTaskStatusPending
			}
			nodes[t.Key] = node{key: t.Key, status: st, id: ""}
		}
		if _, ok := nodes[newFinalKey]; !ok {
			return &planError{"最终节点不是可用的活跃节点：" + newFinalKey}
		}
		adj := make(map[string][]string, len(nodes))
		outdeg := make(map[string]int, len(nodes))
		for l := range existingLinks {
			if _, ok := nodes[l.Parent]; !ok {
				continue
			}
			if _, ok := nodes[l.Child]; !ok {
				continue
			}
			adj[l.Parent] = append(adj[l.Parent], l.Child)
			outdeg[l.Parent]++
		}
		for l := range newLinkKeys {
			adj[l.Parent] = append(adj[l.Parent], l.Child)
			outdeg[l.Parent]++
		}
		keys := make([]string, 0, len(nodes))
		linkList := make([]planLink, 0, len(adj))
		for k, children := range adj {
			for _, c := range children {
				linkList = append(linkList, planLink{Parent: k, Child: c})
			}
		}
		for k := range nodes {
			keys = append(keys, k)
		}
		if err := detectCycle(keys, linkList); err != nil {
			return err
		}
		if outdeg[newFinalKey] != 0 {
			return &planError{"最终节点不能有下游：" + newFinalKey}
		}
		for k, n := range nodes {
			if !activeWorkStatuses[n.status] {
				continue
			}
			if !reaches(k, newFinalKey, adj) {
				return &planError{"活跃节点无法沿依赖边到达最终节点：" + k}
			}
		}
		// 建行：新增 Task（created_revision=新版本）、新增边、取消、组版本与最终节点。
		revision := o.PlanRevision + 1
		newIDs := make(map[string]string, len(norm.AddTasks))
		rows := make([]AgentTask, 0, len(norm.AddTasks))
		for _, t := range norm.AddTasks {
			id, err := newRowID("at_")
			if err != nil {
				return err
			}
			newIDs[t.Key] = id
			rows = append(rows, AgentTask{
				ID: id, OrchestrationID: orchID, TaskKey: t.Key,
				TenantID: p.TenantID, WorkspaceID: o.WorkspaceID,
				Content: t.Content, AgentName: t.Agent, ModelRef: t.ModelRef,
				Status: nodes[t.Key].status, PlanCheckpoint: t.PlanCheckpoint,
				CreatedRevision: revision,
			})
		}
		if len(rows) > 0 {
			if err := tx.Create(&rows).Error; err != nil {
				if isUniqueViolation(err) {
					return &planError{"Task key 与组内已有节点重复"}
				}
				return err
			}
		}
		if len(newLinkKeys) > 0 {
			linkRows := make([]AgentTaskLink, 0, len(newLinkKeys))
			for l := range newLinkKeys {
				parentID := newIDs[l.Parent]
				if parentID == "" {
					parentID = byKey[l.Parent].ID
				}
				childID := newIDs[l.Child]
				if childID == "" {
					childID = byKey[l.Child].ID
				}
				linkRows = append(linkRows, AgentTaskLink{
					ParentTaskID: parentID, ChildTaskID: childID,
					TenantID: p.TenantID, OrchestrationID: orchID,
				})
			}
			if err := tx.Create(&linkRows).Error; err != nil {
				if isUniqueViolation(err) {
					return &planError{"依赖边已存在"}
				}
				return err
			}
			// ready 目标带上新 parent 后退回 pending，等父节点成功后再由调度提升。
			for l := range newLinkKeys {
				if added[l.Child] {
					continue
				}
				if err := tx.Model(&AgentTask{}).
					Where("id = ? AND status = ?", byKey[l.Child].ID, agentTaskStatusReady).
					Update("status", agentTaskStatusPending).Error; err != nil {
					return err
				}
				changedTaskIDs = append(changedTaskIDs, byKey[l.Child].ID)
			}
		}
		for k := range cancelled {
			if err := tx.Model(&AgentTask{}).
				Where("id = ? AND status IN ?", byKey[k].ID,
					[]string{agentTaskStatusPending, agentTaskStatusReady, agentTaskStatusBlocked}).
				Updates(map[string]any{"status": agentTaskStatusCancelled, "finished_at": nowUTC()}).Error; err != nil {
				return err
			}
			changedTaskIDs = append(changedTaskIDs, byKey[k].ID)
		}
		finalID := newIDs[newFinalKey]
		if finalID == "" {
			finalID = byKey[newFinalKey].ID
		}
		upd := tx.Exec(`UPDATE agent_orchestrations
 SET plan_revision = ?, final_task_id = ?, plan_hold = false, updated_at = now()
 WHERE id = ? AND status = ? AND plan_revision = ?`,
			revision, finalID, orchID, orchStatusActive, o.PlanRevision)
		if upd.Error != nil {
			return upd.Error
		}
		if upd.RowsAffected == 0 {
			return errRevisionStale
		}
		revID, err := newRowID("pr_")
		if err != nil {
			return err
		}
		rec := AgentPlanRevision{
			ID: revID, TenantID: p.TenantID, OrchestrationID: orchID,
			Revision: revision, BaseRevision: o.PlanRevision, SourceRunID: cmd.SourceRunID,
			IdempotencyKey: cmd.IdempotencyKey, CommandHash: hash,
			CommandJSON: string(commandJSON), Reason: norm.Reason, CreatedAt: nowUTC(),
		}
		if err := tx.Create(&rec).Error; err != nil {
			if isUniqueViolation(err) {
				return errRevisionStale
			}
			return err
		}
		for _, t := range norm.AddTasks {
			changedTaskIDs = append(changedTaskIDs, newIDs[t.Key])
		}
		resp = revisionResp{
			RevisionID: revID, OrchestrationID: orchID, Revision: revision,
			FinalTaskID: finalID, PlanHold: false,
			AddedTaskIDs: addedIDsFor(norm.AddTasks, newIDs),
		}
		return nil
	})
	if err != nil {
		return revisionResp{}, err
	}
	s.PublishEvent(ctx, p.TenantID, eventAgentOrchestrationsChanged, orchID)
	for _, id := range changedTaskIDs {
		s.PublishEvent(ctx, p.TenantID, eventAgentTasksChanged, id)
	}
	s.PublishTaskWake(ctx, p.TenantID, "revision", orchID)
	return resp, nil
}

// revisionAddedTaskIDs 是幂等重打时回读「该版新增了哪些 Task」：按 created_revision + 命令里的 key。
func revisionAddedTaskIDs(tx *gorm.DB, orchID string, revision int, commandJSON string) ([]string, error) {
	var cmd revisionCommand
	if err := json.Unmarshal([]byte(commandJSON), &cmd); err != nil {
		return []string{}, nil
	}
	if len(cmd.AddTasks) == 0 {
		return []string{}, nil
	}
	keys := make([]string, 0, len(cmd.AddTasks))
	for _, t := range cmd.AddTasks {
		keys = append(keys, t.Key)
	}
	var rows []AgentTask
	if err := tx.Where("orchestration_id = ? AND created_revision = ? AND task_key IN ?",
		orchID, revision, keys).Order("task_key").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out, nil
}

func addedIDsFor(tasks []planTask, ids map[string]string) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, ids[t.Key])
	}
	return out
}

// reaches 判断 from 能否沿边到达 to（BFS；32 个节点内足够）。
func reaches(from, to string, adj map[string][]string) bool {
	if from == to {
		return true
	}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nxt := range adj[cur] {
			if nxt == to {
				return true
			}
			if !seen[nxt] {
				seen[nxt] = true
				queue = append(queue, nxt)
			}
		}
	}
	return false
}

// ContinueOrchestration 沿当前图继续（doc/72 §4.3）：自然幂等，不另要 idempotencyKey。
// 只有实际把 plan_hold 从 true 置 false 才发事件与内部唤醒。
func (s *Store) ContinueOrchestration(ctx context.Context, p principal, orchID string, baseRevision int) (continueResp, error) {
	if baseRevision < 0 || baseRevision > agentTaskMaxCount {
		return continueResp{}, &planError{"baseRevision 不合法"}
	}
	changed := false
	var revision int
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o AgentOrchestration
		res := tx.Raw(`
SELECT o.* FROM agent_orchestrations o
  JOIN sessions s ON s.id = o.source_session_id
 WHERE o.id = ? AND o.tenant_id = ? AND s.user_id = ?
 FOR UPDATE OF o`, orchID, p.TenantID, p.UserID).Scan(&o)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errOrchNotFound
		}
		if o.PlanRevision != baseRevision {
			return errRevisionStale
		}
		if o.Status != orchStatusActive {
			return errOrchNotRevisable
		}
		revision = o.PlanRevision
		if !o.PlanHold {
			return nil
		}
		// 继续只看版本与暂停位：检查点可能就是把最终 Task 本身跑完的那一件，
		// 要求「最终 Task 未开始」会让这种组永远无法解除暂停（修订才有该约束）。
		upd := tx.Exec(`UPDATE agent_orchestrations SET plan_hold = false, updated_at = now()
 WHERE id = ? AND plan_hold = true AND plan_revision = ?`, orchID, o.PlanRevision)
		if upd.Error != nil {
			return upd.Error
		}
		changed = upd.RowsAffected > 0
		return nil
	})
	if err != nil {
		return continueResp{}, err
	}
	if changed {
		s.PublishEvent(ctx, p.TenantID, eventAgentOrchestrationsChanged, orchID)
		s.PublishTaskWake(ctx, p.TenantID, "continue", orchID)
	}
	return continueResp{OrchestrationID: orchID, Revision: revision, PlanHold: false}, nil
}

// ListPlanRevisions 读修订历史（施工包 §4.4：revision 升序）。
func (s *Store) ListPlanRevisions(ctx context.Context, p principal, orchID string) ([]revisionView, error) {
	var one int
	if err := s.db.WithContext(ctx).Raw(`
SELECT 1 FROM agent_orchestrations o
  JOIN sessions s ON s.id = o.source_session_id
 WHERE o.id = ? AND o.tenant_id = ? AND s.user_id = ?`, orchID, p.TenantID, p.UserID).
		Scan(&one).Error; err != nil {
		return nil, err
	}
	if one == 0 {
		return nil, errOrchNotFound
	}
	var rows []AgentPlanRevision
	if err := s.db.WithContext(ctx).Where("orchestration_id = ?", orchID).
		Order("revision ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]revisionView, 0, len(rows))
	for _, r := range rows {
		var cmd revisionCommand
		_ = json.Unmarshal([]byte(r.CommandJSON), &cmd)
		tasks := make([]revisionTaskView, 0, len(cmd.AddTasks))
		for _, t := range cmd.AddTasks {
			tasks = append(tasks, revisionTaskView{
				Key: t.Key, Content: t.Content, Agent: t.Agent,
				ModelRef: t.ModelRef, PlanCheckpoint: t.PlanCheckpoint,
			})
		}
		links := cmd.AddLinks
		if links == nil {
			links = []planLink{}
		}
		cancels := cmd.CancelTasks
		if cancels == nil {
			cancels = []string{}
		}
		out = append(out, revisionView{
			RevisionID: r.ID, Revision: r.Revision, BaseRevision: r.BaseRevision,
			Reason: r.Reason, AddTasks: tasks, AddLinks: links, CancelTasks: cancels,
			FinalTaskKey: cmd.FinalTaskKey, SourceRunID: r.SourceRunID, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// revisionFromRow 仅供测试与内部对读：把审计行还原成规范命令（空命令 = 解析失败）。
func revisionFromRow(r AgentPlanRevision) revisionCommand {
	var cmd revisionCommand
	_ = json.Unmarshal([]byte(r.CommandJSON), &cmd)
	return cmd
}
