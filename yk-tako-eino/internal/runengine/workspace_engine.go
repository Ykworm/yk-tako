// 本文件是隔离工作区的引擎装配层（方案真源 = doc/74 §5.3/§9）：把 Store 的 PG/作业根
// 接成 Provider 需要的组句柄与配额口，并在领取/恢复时调用 Provider 准备工作现场。
//
// 分工：引擎裁决 run/epoch、结果版本、发布准入与 Task 状态；Provider 只做文件/Git 操作。
// shared 与 isolated 都走这里（doc/76 §4）：shared 共用 work/shared，isolated 每 Task 一个
// work/<taskId>；两种模式的结果都经接受与发布才进用户内容根。
package runengine

import (
	"context"
	"fmt"
)

// workspaceGroupHandle 按组行装配句柄：路径全部由 tenant/orchestration/workspace ID 推导，
// 锁是 PG advisory + 本机 flock 的串联（跨副本互斥），租户身份钉在组行上（见 groupTenantLocker）。
// 作业根未配置（单测直接构造 Store）= 错误。
func (s *Store) workspaceGroupHandle(o AgentOrchestration) (*GroupHandle, error) {
	if s.workspaceRoot == "" {
		return nil, fmt.Errorf("引擎未配置作业根，无法装配组工作区")
	}
	// 发布目标与基线都是用户内容根 content（doc/76 §6.1），运行资源在 runtime。
	projectRoot, _, err := ensureProjectLayout(s.workspaceRoot, o.TenantID, o.WorkspaceID)
	if err != nil {
		return nil, err
	}
	tenantRoot := tenantStorageRoot(s.workspaceRoot, o.TenantID)
	locker := &groupTenantLocker{
		ref:   WorkspaceRef{TenantID: o.TenantID, Root: tenantRoot, TenantRoot: tenantRoot},
		inner: newChainFileLocker(newPGFileLocker(s.db, 0), newFlockFileLocker(s.workspaceRoot)),
	}
	return newGroupHandle(s.workspaceRoot, o, projectRoot, locker)
}

// groupTenantLocker 把组锁的租户身份钉在编排行上，不吃调用方 ctx。
//
// 同一把组锁会被多条路径取：发布快路径（run ctx，带租户）、调度慢路径／领取／清理
// （循环 ctx，无租户）、retry-publication（登录身份，无工作区引用）。PG 键与 flock
// 锁目录若按 ctx 推导，同一把锁会在不同路径落成两个键 → 互斥失效：2026-11-12 活体
// 里快慢两路同时发布，一路 merge 失败落 blocked，项目却已被另一路写成。
// 租户是编排行上的事实，装配时钉死，所有路径取到的都是同一把锁。
type groupTenantLocker struct {
	ref   WorkspaceRef
	inner FileLocker
}

func (l *groupTenantLocker) Lock(ctx context.Context, keys []string) (func(), error) {
	return l.inner.Lock(withWorkspace(ctx, l.ref), keys)
}

// workspaceReserve 把租户存储配额接成 Provider 的配额口（doc/74 §10.7）：
// 基线、工作目录、结果与发布记录都计入整棵租户目录。maxBytes <= 0 = 不限额。
func (s *Store) workspaceReserve() storageReserveFunc {
	return func(ctx context.Context, tenantID string, adding int64) (func(), error) {
		_, _, _, maxBytes, err := s.TenantQuota(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		if maxBytes <= 0 {
			return func() {}, nil
		}
		return s.ReserveStorage(ctx, tenantID, adding, maxBytes, func() int64 {
			return directoryBytes(tenantStorageRoot(s.workspaceRoot, tenantID))
		})
	}
}

// workspaceProvider 选 Provider：state.json 已固定类型则以它为准（项目形态变化不静默换），
// 否则按原项目真实形态探测（第一次准备）。两个 Provider 共用同一配额口。
func (s *Store) workspaceProvider(g *GroupHandle) (WorkspaceProvider, error) {
	st, err := readWorkspaceGroupState(g.Dir)
	if err != nil {
		return nil, err
	}
	kind := ""
	if st != nil {
		kind = st.Provider
	} else {
		kind = detectWorkspaceProvider(g.ProjectRoot)
	}
	reserve := s.workspaceReserve()
	switch kind {
	case workspaceProviderGit:
		return &gitProvider{reserve: reserve}, nil
	case workspaceProviderDirectory:
		return &directoryProvider{reserve: reserve}, nil
	default:
		return nil, fmt.Errorf("未知的工作区 Provider：%q", kind)
	}
}

// workspaceParentRefs 读这件 Task 已成功父的结果引用（按 taskKey 排序，doc/74 §7.1/§8.2）。
// 父 succeeded 必有 resultRevision；Provider 据此解析内部引用（Git commit / Directory 清单）。
func (s *Store) workspaceParentRefs(ctx context.Context, taskID string) ([]ResultRef, error) {
	var parents []AgentTask
	if err := s.db.WithContext(ctx).Raw(`
SELECT t.* FROM agent_task_links l JOIN agent_tasks t ON t.id = l.parent_task_id
 WHERE l.child_task_id = ? AND t.status = ? ORDER BY t.task_key`,
		taskID, agentTaskStatusSucceeded).Scan(&parents).Error; err != nil {
		return nil, err
	}
	refs := make([]ResultRef, 0, len(parents))
	for _, p := range parents {
		refs = append(refs, ResultRef{TaskID: p.ID, ResultRevision: p.ResultRevision})
	}
	return refs, nil
}

// prepareTaskWork 在领取/恢复时准备工作现场（doc/74 §7.1/§8.1；doc/76 §4）：
// shared 与 isolated 都先 PrepareGroup（幂等固定 Provider 与基线）再 PrepareTask
// （幂等：现场已存在直接返回，不重新应用父结果/merge）。shared 的现场是同组共用的
// work/shared，isolated 是每件 Task 的 work/<taskId>。
// 第二个返回值是 Provider 类型（进 trace）。
// 所有文件/Git 操作都在 PG 事务外，由调用方在领取事务提交后调用。
func (s *Store) prepareTaskWork(ctx context.Context, taskID string) (TaskWork, string, error) {
	t, o, err := s.agentTaskRow(ctx, taskID)
	if err != nil {
		return TaskWork{}, "", err
	}
	g, err := s.workspaceGroupHandle(o)
	if err != nil {
		return TaskWork{}, "", err
	}
	provider, err := s.workspaceProvider(g)
	if err != nil {
		return TaskWork{}, "", err
	}
	if err := provider.PrepareGroup(ctx, g); err != nil {
		return TaskWork{}, "", err
	}
	parents, err := s.workspaceParentRefs(ctx, taskID)
	if err != nil {
		return TaskWork{}, "", err
	}
	work, err := provider.PrepareTask(ctx, g, t, parents)
	if err != nil {
		return TaskWork{}, "", err
	}
	st, err := readWorkspaceGroupState(g.Dir)
	if err != nil || st == nil {
		return TaskWork{}, "", fmt.Errorf("组状态在准备后不可读：%v", err)
	}
	return work, st.Provider, nil
}
