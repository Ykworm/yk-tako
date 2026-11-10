// 本文件是 pipeline 记忆的**运行配置与观测口**（doc/65 §0/§5）。
//
// 新行结构在 memory_types.go，新数据层（视图、可见性谓词、状态迁移、证据时间、
// 索引）在 memory_store.go。本文件只留三件事：
//
//   - MemoryConfig：实例级的记忆运行模式与开关（pipeline / markdown）；
//   - ListMemoryItems / ListMemoryJobs：管理面与观测口的读侧；
//   - 少量共用小工具（clipMemoryText / sleepCtx）。
//
// **旧的 PG 轮询抽取循环（doc/29 线一）已退役**：pipeline 抽取器按 doc/65 的新形态
// 实现在 memory_pipeline.go（三段流 + Redpanda 消费者）。旧循环依赖已取消的
// kind/topic 去重语义与 episode 行，随旧表结构一起下线。
package runengine

import (
	"context"
	"time"

	"github.com/cloudwego/eino/components/model"
	"gorm.io/gorm"
)

// MemoryConfig 是两线的运行配置（doc/29 §8）。TenantID 一期恒 "default"（doc/28 落地后只改
// 这里）；Workspace 与笔记/文件工具同一个根；三个开关同时管后台循环与召回：
//   - Extract：run 终态是否登记抽取 job（关 = 线一召回照旧读已有记忆）；
//   - Recall：线一是否召回；
//   - RAG：线二是否索引与召回。
//
// Mode 是**实例级**的记忆运行模式（pipeline / markdown，doc/65 §0）：markdown 模式下
// 这三个开关一律按关处理（只装配文件工具）。Mode 空 = pipeline。
//
// 放在 Store 上而不是散在三处：EndRun（同事务插 job）、Worker（注入）、Server（预览端点）
// 读同一份值，开关不会出现「端点开了、注入没开」的假象。
type MemoryConfig struct {
	TenantID  string
	Workspace string
	Extract   bool
	Recall    bool
	RAG       bool
	Mode      MemoryMode
}

// ConfigureMemory 在启动装配期调一次（workspace 绝对化之后）。TenantID 空 = default。
func (s *Store) ConfigureMemory(cfg MemoryConfig) {
	if cfg.TenantID == "" {
		cfg.TenantID = "default"
	}
	s.memoryCfg = cfg
}

// MemoryConfig 返回当前配置（只读快照）。
func (s *Store) MemoryConfig() MemoryConfig { return s.memoryCfg }

// 记忆任务的生命周期常量（doc/29 §4.5）。任务行结构与状态迁移在
// memory_types.go / memory_store.go；这里只留租约与重试参数。
const (
	memoryExtractTick = 2 * time.Second // 空轮询间隔
	memoryJobLease    = 2 * time.Minute // running 租约：进程崩溃后别的副本按它捡回
	memoryMaxAttempts = 3               // 三次后 failed（run 早已 complete）
)

// memoryRetryBackoff[i] 是第 i+1 次失败后的退避（1min / 5min；第三次直接 failed）。
var memoryRetryBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// MemoryModelFunc 按需解析抽取模型（doc/29 §8）：TAKO_MEMORY_MODEL 为空时返回主模型客户端；
// 非空时走 ModelRegistry 懒解析。解析失败只让当前 job 失败重试，不影响启动与 run。
type MemoryModelFunc func(ctx context.Context) (model.BaseChatModel, error)

// ListMemoryItems 是 GET /api/memory/items 的读侧（观测口）：按层读**视图**
// （resident_notes / recall_items），layer 空 = 两层都读，limit 默认 50、上限 200。
//
// state 空 = 只回 active 且未过期（当前态）；历史（superseded/removed/archived）必须显式传
// state——默认列表混进历史行，读侧会把「已被替代的旧说法」当成当前事实。
// 显式 state=active 与默认同口径（仍滤过期）；过期行没有观测口，它们不是历史链的一环。
//
// 可见性沿用观测口的旧口径（租户 + 人；scope=tenant 给管理员看本租户全员）——
// 应用侧的普通读取走 memory_store.go 的可见性谓词（user / workspace 两分支），
// 两条读口刻意分开：观测口看的是「这个租户里有什么」，应用读的是「这个人现在能看见什么」。
//
// markdown 模式**没有**这张表：记忆在两份文件里。这个观测口在那个模式下返回
// ErrMemoryModeUnsupported（server.go 的 memoryPipelineOnly 把它映射成 409 / 118001），
// 不返回空数组——空数组会被读成「这个租户一条记忆都没有」，而真相是
// 「记忆在文件里，观测口看不见」。
func (s *Store) ListMemoryItems(ctx context.Context, layer, state string, limit int) ([]MemoryItem, error) {
	if s.markdownMode() {
		return nil, ErrMemoryModeUnsupported
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	sc := scopeFrom(ctx)
	// 观测口读视图（普通应用读口也只打视图，doc/65 §4.1）。
	// 两层都要时用 UNION ALL：GORM 的 Table() 不接受子查询别名，这里走 Raw。
	from := `SELECT * FROM resident_notes`
	switch layer {
	case MemoryLayerResident:
	case MemoryLayerRecall:
		from = `SELECT * FROM recall_items`
	default:
		from = `SELECT * FROM resident_notes UNION ALL SELECT * FROM recall_items`
	}
	stateSQL := `state = 'active' AND (expires_at IS NULL OR expires_at > now())`
	args := memoryScopeArgs(sc)
	if state != "" && state != MemoryStateActive {
		stateSQL = `state = ?`
		args = append(args, state)
	}
	// 无身份的旧调用（单测）scope 片段为空，state 过滤自带 WHERE。
	where := ` WHERE ` + stateSQL
	if scopeSQL := applyMemoryScopeSQL(sc); scopeSQL != "" {
		where = scopeSQL + ` AND ` + stateSQL
	}
	var items []MemoryItem
	err := s.db.WithContext(ctx).Raw(
		`SELECT * FROM (`+from+`) m`+where+` ORDER BY created_at DESC, id DESC LIMIT ?`,
		append(args, limit)...).Scan(&items).Error
	if items == nil {
		items = []MemoryItem{}
	}
	return items, err
}

// ListMemoryJobs 是 GET /api/memory/jobs 的读侧（doc/29 §7.2②）：runId 空 = 最近 20 条。
//
// markdown 模式没有抽取器、没有 memory_jobs：同 ListMemoryItems，返回哨兵错误而不是空列表。
func (s *Store) ListMemoryJobs(ctx context.Context, runID RunID) ([]MemoryJob, error) {
	if s.markdownMode() {
		return nil, ErrMemoryModeUnsupported
	}
	q := s.db.WithContext(ctx).Order("created_at DESC, id DESC").Limit(20)
	q = applyMemoryScope(q, scopeFrom(ctx))
	if runID != "" {
		q = q.Where("run_id = ?", runID)
	}
	var jobs []MemoryJob
	err := q.Find(&jobs).Error
	if jobs == nil {
		jobs = []MemoryJob{}
	}
	return jobs, err
}

// applyMemoryScope 给观测口加租户和人。没有身份的旧调用（单测）不加过滤。
// scope=tenant 只限制租户，给管理员看本工作区全员；其余只看本人。
func applyMemoryScope(db *gorm.DB, sc dataScope) *gorm.DB {
	if !scopeActive(sc) {
		return db
	}
	if sc.Scope == "tenant" {
		return db.Where("tenant_id = ?", sc.TenantID)
	}
	return db.Where("tenant_id = ? AND user_id = ?", sc.TenantID, sc.UserID)
}

// applyMemoryScopeSQL 是 applyMemoryScope 的 Raw 版（同样的口径，参数顺序固定）。
// 观测口读视图时走 Raw，所以需要这个片段——两条实现必须同口径，改一处要改两处。
func applyMemoryScopeSQL(sc dataScope) string {
	if !scopeActive(sc) {
		return ""
	}
	if sc.Scope == "tenant" {
		return ` WHERE tenant_id = ?`
	}
	return ` WHERE tenant_id = ? AND user_id = ?`
}

// memoryScopeArgs 是 applyMemoryScopeSQL 的参数（顺序与片段里的 ? 一致）。
func memoryScopeArgs(sc dataScope) []any {
	if !scopeActive(sc) {
		return nil
	}
	if sc.Scope == "tenant" {
		return []any{sc.TenantID}
	}
	return []any{sc.TenantID, sc.UserID}
}

// clipRunes 按 rune 截断、无标记（与 compact 的 truncateRunes 不同：这里的文本会进抽取
// 输入/校验，标记串只会污染模型输出与长度判定）。
func clipMemoryText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// sleepCtx 等一拍；ctx 取消返回 false（循环退出）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
