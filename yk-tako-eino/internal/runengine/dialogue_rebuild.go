package runengine

// 本文件是历史对话混合检索的版本化重建（doc/84 §6.4，施工包 §7 P3 第 2 条）：
// `yktako -rebuild-dialogue-retrieval` 一次性命令。它回答的问题是——outbox 只保证
// 「登记过的增量最终写出去」，而外部副本整体退化（维度变了、索引被手工清了、
// 大面积死信）时，增量修复没有抓手，必须有一台「从 PG 正本全量重放」的机器。
//
// 五步（§6.4 原文顺序，每步的输入是上一步的输出）：
//
//  1. 建新版本：新版本号 = 指针 index_version + 1，按基名派生
//     `<TAKO_ES_INDEX>-v<N>` / `<TAKO_MILVUS_COLLECTION>-v<N>`，mapping/schema 与
//     既有一致；embedding 真实探针定维度，与已存在 collection 不符 → 119004 不建；
//  2. 幂等回放：扫描 dialogue_chunks（keyset 分页，归属与正文联 runs），直接调
//     ES/Milvus 客户端 upsert（不写 outbox——重建是显式维护动作，不走投递链），
//     embedding 按既有批次/限流口径分批；
//  3. 校验：刷新 + 计数（新版本文档数 == PG chunk 数）+ 回放失败数 == 0
//     （有失败列出 run_id 中止切换）+ 抽样真实 passage 做 ES match 与 Milvus
//     语义检索冒烟；新版本里 PG 已没有的孤儿文档顺手清掉（重跑收敛）；
//  4. 一笔事务切指针（switchDialoguePointerRow：kill -9 下指针要么旧要么新）；
//  5. 旧版本保留不删，打印手工清理语句示例（只含索引名，不含凭据）。
//
// 重跑安全：指针已指向「基名-v当前版本」形态时，视为已是活跃版本——跳过建与切，
// 只做增量校验（数量 + 抽样），退出码 0。回放幂等（run_id 主键覆盖写）+ 校验门，
// 中途崩溃重跑从 PG 正本重来，新版本索引里的半成品被覆盖或清理。
//
// 共享实例纪律：只创建/读写本方案前缀（tako_dialogue_*）的索引与 collection，
// 绝不删除任何旧版本（回滚保险），清理语句只打印不执行。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 重建的固定口径。
const (
	dialogueRebuildBatch  = 100 // 每批回放多少个 run（keyset 分页）
	dialogueRebuildSample = 3   // 校验抽样条数（§6.4「抽样 3 条真实 passage」）
	dialogueRebuildTopK   = 10  // 冒烟检索的 topK（与消费查询口径一致）
)

// dialogueVersionedName 派生版本化的索引/collection 名：基名_v<版本号>。
// 后缀用下划线不用连字符——Milvus collection 名只允许字母/数字/下划线（活体
// rebuild1 实测 `-v2` 被 Invalid collection name 拒收），ES 下划线同样合法。
func dialogueVersionedName(base string, version int64) string {
	return fmt.Sprintf("%s_v%d", base, version)
}

// dialogueESRebuildIndex 是重建用的 ES 面（真实现 = dialogueESClient；单测打桩）。
// 与消费者面（dialogueESIndex）分开：重建多要建索引/刷新/计数/枚举，不该给消费链路。
type dialogueESRebuildIndex interface {
	EnsureIndex(ctx context.Context) error
	Refresh(ctx context.Context) error
	CountDocs(ctx context.Context) (int64, error)
	ListAllRunIDs(ctx context.Context, maxRuns int) (map[string]bool, bool, error)
	Upsert(ctx context.Context, doc dialogueESDoc) error
	Delete(ctx context.Context, runID string) error
	Search(ctx context.Context, query, tenantID, workspaceID, userID string, indexVersion int64, topK int) ([]dialogueLexHit, error)
}

// dialogueMilvusRebuildIndex 是重建用的 Milvus 面（真实现 = dialogueMilvusClient）。
type dialogueMilvusRebuildIndex interface {
	EnsureCollection(ctx context.Context) error
	Flush(ctx context.Context) error
	CountDocs(ctx context.Context) (int64, error)
	ListAllRunIDs(ctx context.Context, maxRuns int) (map[string]bool, bool, error)
	Upsert(ctx context.Context, rows []dialogueMilvusRow) error
	DeleteByRunIDs(ctx context.Context, runIDs []string) error
	Search(ctx context.Context, vec []float32, expr string, topK int) ([]dialogueVecHit, error)
}

var (
	_ dialogueESRebuildIndex     = (*dialogueESClient)(nil)
	_ dialogueMilvusRebuildIndex = (*dialogueMilvusClient)(nil)
)

// dialogueRebuildDeps 是重建的依赖注入（生产 = 环境配置 + 真客户端；单测 = 假服务）。
type dialogueRebuildDeps struct {
	store    *Store
	esBase   string // TAKO_ES_INDEX（版本后缀的基名）
	milBase  string // TAKO_MILVUS_COLLECTION
	newES    func(index string) (dialogueESRebuildIndex, error)
	newMil   func(collection string, dim int) (dialogueMilvusRebuildIndex, error)
	probeDim func(ctx context.Context) (int, error) // 真实 embedding 探针（生产 = ProbeDim）
	embed    dialogueEmbedderAPI                    // 回放与抽样用
	counter  TokenCounter                           // nil = 按条数分批（无 token 上限保护，与消费路径同一降级）
	embCfg   dialogueEmbeddingConfig
	batch    int
}

// dialogueRebuildFailure 是回放单条失败（run_id + 错误分类；正文与密钥不进日志）。
type dialogueRebuildFailure struct {
	RunID string
	Arm   string // es | milvus
	Class string // 错误分类词表（last_error 同源）
}

func (f dialogueRebuildFailure) String() string {
	return f.RunID + "(" + f.Arm + ":" + f.Class + ")"
}

// RunDialogueRebuild 是 main 的入口：配置（要求 hybrid）→ 裸 PG（不起 Redis/HTTP，
// 与离线迁移同款）→ 组装真依赖 → 跑核心流程。错误带 119xxx 业务码前缀（可映射时），
// 命令行直接可读。
func RunDialogueRebuild(ctx context.Context, pgDSN string) error {
	cfg, err := DialogueRetrievalConfigFromEnv()
	if err != nil {
		return err
	}
	if cfg.Mode != DialogueModeHybrid {
		return fmt.Errorf("TAKO_DIALOGUE_RETRIEVAL=%s：重建只对 hybrid 有意义（外部索引/collection 只有 hybrid 在维护）", cfg.Mode)
	}
	if strings.TrimSpace(pgDSN) == "" {
		return errors.New("PG DSN 为空（-pg）")
	}

	db, err := gorm.Open(postgres.Open(pgDSN), &gorm.Config{
		TranslateError: true,
		Logger:         logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return fmt.Errorf("连 PG: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("取 PG 句柄: %w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("PG ping: %w", err)
	}
	if err := autoMigrate(db); err != nil {
		return fmt.Errorf("表结构自检: %w", err)
	}

	counter, cerr := NewCl100kCounter()
	if cerr != nil {
		// 与消费路径同一降级：counter 为 nil 时按条数分批，没有单块 token 上限保护。
		log.Printf("rebuild-dialogue-retrieval: token 计数器不可用（%v），按条数分批（无单块 token 上限保护）", cerr)
		counter = nil
	}
	embedder := newDialogueEmbedder(cfg.Embedding)
	d := dialogueRebuildDeps{
		store:   &Store{db: db},
		esBase:  cfg.ESIndex,
		milBase: cfg.MilvusCollection,
		newES: func(index string) (dialogueESRebuildIndex, error) {
			return newDialogueESClient(cfg.ESURL, cfg.ESUsername, cfg.ESPassword, index), nil
		},
		newMil: func(collection string, dim int) (dialogueMilvusRebuildIndex, error) {
			return newDialogueMilvusClient(cfg.MilvusURI, cfg.MilvusToken, cfg.MilvusDB, collection, cfg.MilvusUseTLS, dim)
		},
		probeDim: embedder.ProbeDim,
		embed:    embedder,
		counter:  counter,
		embCfg:   cfg.Embedding,
		batch:    dialogueRebuildBatch,
	}
	if err := runDialogueRebuild(ctx, d); err != nil {
		if status, code, message, ok := dialogueRetrievalHTTPError(err); ok {
			return fmt.Errorf("[HTTP %d code %d] %s：%w", status, code, message, err)
		}
		return err
	}
	return nil
}

// dialogueRebuildPage 扫描 dialogue_chunks（联 runs 取项目归属与完成时间），
// keyset 按 run_id 分页——重建期间重跑/续跑都从同一把尺子出发，不漂移。
func (s *Store) dialogueRebuildPage(ctx context.Context, afterRunID string, limit int) ([]dialogueOwnershipRow, error) {
	var rows []dialogueOwnershipRow
	err := s.db.WithContext(ctx).Raw(`
		SELECT c.run_id, c.session_id, c.tenant_id, c.user_id,
		       COALESCE(r.workspace_id, '') AS workspace_id,
		       c.passage, c.indexed_at, r.finished_at
		  FROM dialogue_chunks c
		  LEFT JOIN runs r ON r.id = c.run_id
		 WHERE c.run_id > ?
		 ORDER BY c.run_id
		 LIMIT ?`, afterRunID, limit).Scan(&rows).Error
	return rows, err
}

// runDialogueRebuild 是重建核心流程（五步；重跑收敛见文件头注）。
func runDialogueRebuild(ctx context.Context, d dialogueRebuildDeps) error {
	logf := func(format string, args ...any) { log.Printf("rebuild-dialogue-retrieval: "+format, args...) }
	if d.batch <= 0 {
		d.batch = dialogueRebuildBatch
	}

	ptr, err := d.store.dialoguePointer(ctx)
	if err != nil {
		return err
	}
	newVersion := ptr.IndexVersion + 1

	// 重跑收敛判定：指针已经指向「基名-当前版本」形态 → 前一轮重建已完成切换，
	// 本轮不建不切，只做增量校验（数量 + 抽样），退出码 0。
	if ptr.EsIndex == dialogueVersionedName(d.esBase, ptr.IndexVersion) &&
		ptr.MilvusCollection == dialogueVersionedName(d.milBase, ptr.IndexVersion) {
		logf("指针已是版本化形态（v%d，es=%s mil=%s）——已是活跃版本，做增量校验",
			ptr.IndexVersion, ptr.EsIndex, ptr.MilvusCollection)
		dim, err := d.probeDim(ctx)
		if err != nil {
			return fmt.Errorf("维度探针: %w", err)
		}
		es, err := d.newES(ptr.EsIndex)
		if err != nil {
			return fmt.Errorf("连 ES: %w", err)
		}
		mil, err := d.newMil(ptr.MilvusCollection, dim)
		if err != nil {
			return fmt.Errorf("连 Milvus: %w", err)
		}
		if err := mil.EnsureCollection(ctx); err != nil {
			return fmt.Errorf("Milvus collection 校验: %w", err)
		}
		if _, err := dialogueRebuildVerify(ctx, d, es, mil, ptr.IndexVersion, logf); err != nil {
			return err
		}
		logf("已是活跃版本 v%d，增量校验通过（本轮未建新版本、未切指针）", ptr.IndexVersion)
		return nil
	}

	// 第 1 步：建新版本。先真实探针定维度，并先过 Milvus 的维度校验（与已存在
	// collection 不符 → 119004 永久失败）——维度问题是本轮的死因，一个字节都别建。
	dim, err := d.probeDim(ctx)
	if err != nil {
		return fmt.Errorf("embedding 维度探针: %w", err)
	}
	logf("建新版本 v%d（es=%s mil=%s，embedding dim=%d）",
		newVersion, dialogueVersionedName(d.esBase, newVersion), dialogueVersionedName(d.milBase, newVersion), dim)
	mil, err := d.newMil(dialogueVersionedName(d.milBase, newVersion), dim)
	if err != nil {
		return fmt.Errorf("连 Milvus: %w", err)
	}
	if err := mil.EnsureCollection(ctx); err != nil {
		return fmt.Errorf("建 Milvus collection %s: %w", dialogueVersionedName(d.milBase, newVersion), err)
	}
	es, err := d.newES(dialogueVersionedName(d.esBase, newVersion))
	if err != nil {
		return fmt.Errorf("连 ES: %w", err)
	}
	if err := es.EnsureIndex(ctx); err != nil {
		return fmt.Errorf("建 ES 索引 %s: %w", dialogueVersionedName(d.esBase, newVersion), err)
	}

	// 第 2 步：幂等回放（直接客户端 upsert，不写 outbox）。
	failures, err := dialogueRebuildReplay(ctx, d, es, mil, newVersion, logf)
	if err != nil {
		return fmt.Errorf("回放: %w", err)
	}
	if len(failures) > 0 {
		// 第 3 步前置门：有失败列出 run_id 并中止切换——指针不动，旧版本继续服务，
		// 修好原因（如 embedding 服务恢复）后重跑，幂等续做。
		return fmt.Errorf("回放有 %d 条失败，中止切换（run_id 清单）：%s（+%d 条未列出）",
			len(failures), strings.Join(dialogueRebuildFailList(failures, 20), ", "), len(failures)-20)
	}

	// 第 3 步：校验（计数 == PG chunk 数 + 抽样冒烟；新版本孤儿清理在 verify 内）。
	checked, err := dialogueRebuildVerify(ctx, d, es, mil, newVersion, logf)
	if err != nil {
		return err
	}

	// 第 4 步：一笔事务切指针（switchDialoguePointerRow：kill -9 下要么旧要么新）。
	if err := d.store.SwitchDialogueRetrievalPointer(ctx, newVersion,
		dialogueVersionedName(d.esBase, newVersion), dialogueVersionedName(d.milBase, newVersion)); err != nil {
		return fmt.Errorf("切换指针: %w", err)
	}
	logf("指针已切到 v%d（es=%s mil=%s）；抽样 %d 条全部命中",
		newVersion, dialogueVersionedName(d.esBase, newVersion), dialogueVersionedName(d.milBase, newVersion), checked)

	// 第 5 步：旧版本保留 + 打印手工清理示例（只含索引名，不含凭据；不自动执行）。
	dialogueRebuildPrintCleanup(logf, ptr, d)
	return nil
}

func dialogueRebuildFailList(failures []dialogueRebuildFailure, max int) []string {
	out := make([]string, 0, max)
	for i, f := range failures {
		if i >= max {
			break
		}
		out = append(out, f.String())
	}
	return out
}

// dialogueRebuildReplay 第 2 步：扫描 chunks 分批回放。单条失败不中断整轮
// （失败数与 run_id 记下来，切换门统一处理）；幂等 = run_id 主键覆盖写，
// 崩溃重跑把同一批再写一遍，状态收敛。
func dialogueRebuildReplay(ctx context.Context, d dialogueRebuildDeps, es dialogueESRebuildIndex, mil dialogueMilvusRebuildIndex, version int64, logf func(string, ...any)) ([]dialogueRebuildFailure, error) {
	var failures []dialogueRebuildFailure
	done := 0
	after := ""
	for {
		rows, err := d.store.dialogueRebuildPage(ctx, after, d.batch)
		if err != nil {
			return failures, err
		}
		if len(rows) == 0 {
			break
		}
		// ES 臂：逐条 upsert（_id = run_id，天然幂等）。
		for _, row := range rows {
			if err := es.Upsert(ctx, dialogueESDoc{
				RunID:          row.RunID,
				SessionID:      row.SessionID,
				TenantID:       row.TenantID,
				UserID:         row.UserID,
				WorkspaceID:    row.WorkspaceID,
				Passage:        MaskPIIKeepUserMasked(row.Passage),
				IndexVersion:   version,
				EmbeddingModel: d.embCfg.Model,
				RunFinishedAt:  row.FinishedAt,
				IndexedAt:      row.IndexedAt,
			}); err != nil {
				failures = append(failures, dialogueRebuildFailure{RunID: row.RunID, Arm: "es", Class: dialogueRebuildErrClass(err)})
			}
		}
		// Milvus 臂：批量 embed（既有批次/限流口径）→ 一次 upsert。
		milFailures, err := dialogueRebuildMilvusArm(ctx, d, mil, rows, version)
		if err != nil {
			return failures, err
		}
		failures = append(failures, milFailures...)

		done += len(rows)
		after = rows[len(rows)-1].RunID
		logf("回放进度：已处理 %d，失败 %d（最后 run_id=%s）", done, len(failures), after)
		if ctx.Err() != nil {
			return failures, ctx.Err()
		}
	}
	logf("回放完成：共 %d，失败 %d", done, len(failures))
	return failures, nil
}

// dialogueRebuildMilvusArm 处理一批的向量侧：脱敏 → 分组限流 → 真实 embed → upsert。
// 超 token 上限的整条永久失败（与消费者同一口径，不截断）；embed/upsert 失败按
// run_id 记入失败清单。
func dialogueRebuildMilvusArm(ctx context.Context, d dialogueRebuildDeps, mil dialogueMilvusRebuildIndex, rows []dialogueOwnershipRow, version int64) ([]dialogueRebuildFailure, error) {
	texts := make([]string, len(rows))
	for i, row := range rows {
		texts[i] = MaskPIIKeepUserMasked(row.Passage)
	}
	groups, overLimit := dialogueEmbedGroups(d.counter, texts, d.embCfg.BatchSize, d.embCfg.MaxInputTokens)
	failures := make([]dialogueRebuildFailure, 0, len(overLimit))
	for _, idx := range overLimit {
		failures = append(failures, dialogueRebuildFailure{RunID: rows[idx].RunID, Arm: "milvus", Class: dlgErrEmbedOverLimit})
	}
	vecByRun := make(map[string][]float32, len(rows))
	for _, group := range groups {
		gtexts := make([]string, len(group))
		for i, idx := range group {
			gtexts[i] = texts[idx]
		}
		vecs, err := d.embed.EmbedBatch(ctx, gtexts)
		if err != nil {
			class := dialogueRebuildErrClass(err)
			for _, idx := range group {
				failures = append(failures, dialogueRebuildFailure{RunID: rows[idx].RunID, Arm: "milvus", Class: class})
			}
			continue
		}
		if len(vecs) != len(group) {
			return failures, fmt.Errorf("embedding 返回 %d 条 != 请求 %d 条（内部契约破坏）", len(vecs), len(group))
		}
		for i, idx := range group {
			vecByRun[rows[idx].RunID] = vecs[i]
		}
	}
	if len(vecByRun) == 0 {
		return failures, nil
	}
	upsertRows := make([]dialogueMilvusRow, 0, len(vecByRun))
	for i, row := range rows {
		vec, ok := vecByRun[row.RunID]
		if !ok {
			continue // 该 run 已在失败清单（embed 失败/超限）
		}
		fin := int64(0)
		if row.FinishedAt != nil {
			fin = row.FinishedAt.Unix()
		}
		upsertRows = append(upsertRows, dialogueMilvusRow{
			RunID:          row.RunID,
			SessionID:      row.SessionID,
			TenantID:       row.TenantID,
			UserID:         row.UserID,
			WorkspaceID:    row.WorkspaceID,
			Passage:        texts[i],
			IndexVersion:   version,
			EmbeddingModel: d.embCfg.Model,
			RunFinishedAt:  fin,
			IndexedAt:      row.IndexedAt.Unix(),
			Embedding:      vec,
		})
	}
	if len(upsertRows) > 0 {
		if err := mil.Upsert(ctx, upsertRows); err != nil {
			class := dialogueRebuildErrClass(err)
			for _, r := range upsertRows {
				failures = append(failures, dialogueRebuildFailure{RunID: r.RunID, Arm: "milvus", Class: class})
			}
		}
	}
	return failures, nil
}

// dialogueRebuildVerify 第 3 步：刷新 → 计数 == PG chunk 数 → 新版本孤儿清理 →
// 抽样冒烟（ES match + Milvus 语义检索都要命中自身 run_id）。返回抽样条数。
// dialogueRebuildVerify 第 3 步：孤儿清理 + 计数门 + 抽样冒烟。version 是本索引绑定
// 的代次（增量校验 = 指针当前值；新建 = newVersion），抽样检索带同款代次过滤，
// 与消费查询口径一致。
func dialogueRebuildVerify(ctx context.Context, d dialogueRebuildDeps, es dialogueESRebuildIndex, mil dialogueMilvusRebuildIndex, version int64, logf func(string, ...any)) (int, error) {
	if err := es.Refresh(ctx); err != nil {
		return 0, fmt.Errorf("ES 刷新: %w", err)
	}
	if err := mil.Flush(ctx); err != nil {
		// flush 只是让 row_count 尽量收敛的一致性辅助；serverless 实例可能不支持，
		// 失败只记日志（计数门本身仍会如实报告差异）。
		logf("Milvus flush 失败（继续计数，如差异请稍后重跑校验）: %v", err)
	}

	// 新版本孤儿清理（重跑收敛）：回放可能把「此后被删的 chunk」也写进了新版本，
	// 或上一轮崩溃留下半成品——PG 现在没有的 run_id 直接删掉，再计数比对。
	pgIDs, truncated, err := d.store.dialogueChunkRunIDs(ctx, dialogueReconcileMaxRuns)
	if err != nil {
		return 0, fmt.Errorf("PG chunk 枚举: %w", err)
	}
	if truncated {
		logf("PG chunk 超过枚举上限 %d，孤儿清理与计数按截断集进行", dialogueReconcileMaxRuns)
	}
	for _, lister := range []struct {
		name  string
		list  dialogueRunIDLister
		clean func(ctx context.Context, ids []string) error
	}{
		{"ES", es, func(ctx context.Context, ids []string) error {
			for _, id := range ids {
				if err := es.Delete(ctx, id); err != nil {
					return err
				}
			}
			return nil
		}},
		{"Milvus", mil, func(ctx context.Context, ids []string) error {
			return mil.DeleteByRunIDs(ctx, ids)
		}},
	} {
		extIDs, _, err := lister.list.ListAllRunIDs(ctx, dialogueReconcileMaxRuns)
		if err != nil {
			return 0, fmt.Errorf("%s run_id 枚举: %w", lister.name, err)
		}
		var stale []string
		for id := range extIDs {
			if !pgIDs[id] {
				stale = append(stale, id)
			}
		}
		if len(stale) > 0 {
			logf("新版本 %s 有 %d 条 PG 已不存在的孤儿文档，清理", lister.name, len(stale))
			if err := lister.clean(ctx, stale); err != nil {
				return 0, fmt.Errorf("%s 孤儿清理: %w", lister.name, err)
			}
		}
	}
	if err := es.Refresh(ctx); err != nil {
		return 0, fmt.Errorf("ES 刷新: %w", err)
	}
	if ferr := mil.Flush(ctx); ferr != nil {
		logf("Milvus flush 失败（继续计数）: %v", ferr)
	}

	pgCount, err := d.store.dialogueChunkCount(ctx)
	if err != nil {
		return 0, fmt.Errorf("PG chunk 计数: %w", err)
	}
	esCount, err := es.CountDocs(ctx)
	if err != nil {
		return 0, fmt.Errorf("ES 计数: %w", err)
	}
	milCount, err := mil.CountDocs(ctx)
	if err != nil {
		return 0, fmt.Errorf("Milvus 计数: %w", err)
	}
	logf("计数：PG=%d ES=%d Milvus=%d", pgCount, esCount, milCount)
	if esCount != pgCount {
		return 0, fmt.Errorf("校验失败：ES 文档数 %d ≠ PG chunk 数 %d，中止切换（重跑本命令可增量收敛）", esCount, pgCount)
	}
	if milCount != pgCount {
		return 0, fmt.Errorf("校验失败：Milvus 行数 %d ≠ PG chunk 数 %d，中止切换（重跑本命令可增量收敛）", milCount, pgCount)
	}

	// 抽样冒烟：取 3 条真实 passage（确定性取样：run_id 最小的 3 条），用脱敏后的
	// 正文头部做 ES match 与 Milvus 语义检索，期望命中自身 run_id。
	samples, err := d.store.dialogueRebuildPage(ctx, "", dialogueRebuildSample)
	if err != nil {
		return 0, fmt.Errorf("抽样读取: %w", err)
	}
	if len(samples) == 0 {
		logf("PG 无 chunk，跳过抽样冒烟（计数 0==0 已过）")
		return 0, nil
	}
	for _, sample := range samples {
		query := clipMemoryText(MaskPIIKeepUserMasked(sample.Passage), 120)
		if err := dialogueRebuildSampleCheck(ctx, d, es, mil, sample, query, version); err != nil {
			return 0, fmt.Errorf("抽样 run=%s: %w", sample.RunID, err)
		}
	}
	logf("抽样冒烟 %d 条全部命中", len(samples))
	return len(samples), nil
}

// dialogueRebuildSampleCheck 对一条抽样跑两个臂的冒烟检索（归属过滤与代次过滤与消费查询同口径）。
func dialogueRebuildSampleCheck(ctx context.Context, d dialogueRebuildDeps, es dialogueESRebuildIndex, mil dialogueMilvusRebuildIndex, sample dialogueOwnershipRow, query string, version int64) error {
	lexHits, err := es.Search(ctx, query, sample.TenantID, sample.WorkspaceID, sample.UserID, version, dialogueRebuildTopK)
	if err != nil {
		return fmt.Errorf("ES match 冒烟: %w", err)
	}
	if !dialogueRebuildHitRun(lexRunIDs(lexHits), sample.RunID) {
		return fmt.Errorf("ES match 未命中自身（top%d %d 条）", dialogueRebuildTopK, len(lexHits))
	}
	vecs, err := d.embed.EmbedBatch(ctx, []string{query})
	if err != nil {
		return fmt.Errorf("embedding 冒烟: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return fmt.Errorf("embedding 冒烟返回 %d 条向量（期望 1 条非空）", len(vecs))
	}
	vecHits, err := mil.Search(ctx, vecs[0], dialogueMilvusFilter(sample.TenantID, sample.WorkspaceID, sample.UserID, version), dialogueRebuildTopK)
	if err != nil {
		return fmt.Errorf("Milvus 语义检索冒烟: %w", err)
	}
	vecIDs := make([]string, len(vecHits))
	for i, h := range vecHits {
		vecIDs[i] = h.RunID
	}
	if !dialogueRebuildHitRun(vecIDs, sample.RunID) {
		return fmt.Errorf("Milvus 语义检索未命中自身（top%d %d 条）", dialogueRebuildTopK, len(vecHits))
	}
	return nil
}

func lexRunIDs(hits []dialogueLexHit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.RunID
	}
	return ids
}

func dialogueRebuildHitRun(ids []string, runID string) bool {
	for _, id := range ids {
		if id == runID {
			return true
		}
	}
	return false
}

// dialogueRebuildPrintCleanup 第 5 步：旧版本保留 + 手工清理示例（只含索引名，
// 不含凭据；本命令绝不自动删除旧版本——它是回滚保险）。
func dialogueRebuildPrintCleanup(logf func(string, ...any), oldPtr *DialogueRetrievalPointer, d dialogueRebuildDeps) {
	logf("旧版本（v%d，es=%s mil=%s）保留用于回滚；确认不再回滚后可手工清理：", oldPtr.IndexVersion, oldPtr.EsIndex, oldPtr.MilvusCollection)
	logf("  回滚（把指针切回旧版本）：UPDATE dialogue_retrieval_pointer SET index_version=%d, es_index='%s', milvus_collection='%s', updated_at=now() WHERE id=1;", oldPtr.IndexVersion, oldPtr.EsIndex, oldPtr.MilvusCollection)
	logf("  ES 清理：DELETE /%s（凭据用你自己的 -u）", oldPtr.EsIndex)
	logf("  Milvus 清理：drop collection %s（控制台或 SDK）", oldPtr.MilvusCollection)
	logf("  另：重启各引擎副本以重绑新索引/collection（客户端在启动期绑定绑定名）。")
}

// dialogueRebuildErrClass 把任意错误归到词表类（last_error 同源，不进正文与密钥）。
func dialogueRebuildErrClass(err error) string {
	var ce *dialogueClientError
	if errors.As(err, &ce) {
		return ce.Class
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return dialogueErrInternalTransient
	}
	return dialogueErrInternalTransient
}
