package main

import (
	"context"

	"flag"
	"fmt"
	"github.com/cloudwego/eino/components/model"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/observability"
	"yk-tako-eino/internal/runengine"
)

// runliveTTL 是 run **正常结束**后 runlive 键的回收期限（19v6-03「生命周期」只写「设 TTL」未给数值；dev 取 300s）。
// 它不是正确性参数，只是回收参数：`done` 与 `gone` 收敛到同一条收口路径（planning §5 第 4 条）。
const runliveTTL = 300 * time.Second

// 闸租约（A7/A7-2；D1 落库后闸 = runs 活行）：抢闸的 INSERT 出生即带 lease_until（DB 时钟 now()+lease），
// tk:runlive / tk:runepoch 两键同笔出生、同租约（A7-2 的 Redis 半边）；worker watchdog 每 leaseRenew 续回。
// 进程死亡后没人续 → 活行按 lease 判死（读侧即回收、新 POST 清扫翻 error）、两键到点蒸发——同生同灭、会话自动解锁。
// 二者同为回收参数：30s/10s = 连续 3 次续约全失败才误放；默认值可被 .env 的 TAKO_LEASE_* 改（见 durationEnv）。
const (
	defaultLeaseTTL   = 30 * time.Second
	defaultLeaseRenew = 10 * time.Second
)

// defaultPGDSN 指向 dev 的独立 PG 实例（dev.sh 自举的一半；双 fail fast：PG 不在引擎起不来）。
const defaultPGDSN = "host=127.0.0.1 port=5433 dbname=tako sslmode=disable"

// modelRegistryResolver 把 config.ModelRegistry 适配成 runengine.ModelResolver（T3-1b）：
// 接口定义在消费侧（runengine 不 import config），适配收在 main——config 的 *ChatModel
// 摊平成 runengine.ModelInstance（协议名从强类型 ProtocolName 转字符串，跨过包边界即归一为原语）。
type modelRegistryResolver struct{ r *config.ModelRegistry }

func (a modelRegistryResolver) Get(ctx context.Context, ref string) (runengine.ModelInstance, error) {
	m, err := a.r.Get(ctx, ref)
	if err != nil {
		return runengine.ModelInstance{}, err
	}
	return runengine.ModelInstance{Chat: m.Chat, Ref: m.Ref, Protocol: string(m.Protocol)}, nil
}

func (a modelRegistryResolver) Build(ctx context.Context, cacheKey, ref string, env []string) (runengine.TenantBuilt, error) {
	got, err := a.r.GetFor(ctx, cacheKey, ref, config.PairsToEnv(env))
	if err != nil {
		return runengine.TenantBuilt{}, err
	}
	return runengine.TenantBuilt{
		Chat: got.Model.Chat, Ref: got.Model.Ref, Protocol: string(got.Model.Protocol),
		Provider: got.Provider, ModelID: got.ModelID, KeyFP: got.KeyFP, Vision: got.Vision,
	}, nil
}

// defaultCompactWindow 是上下文压缩的窗口 token 数（C 系列；doc/12 §2：查不到就用 256000）。
// 触发阈值 limit = window − min(window/5, 16384)（三期重裁，19v6-09 §9）；0 = 关压缩（验收对照跑法）。
const defaultCompactWindow = 256000

// durationEnv 取 .env/进程环境里的时长变量（TAKO_LEASE_* 约定同 TAKO_MODEL；进程环境非空时 godotenv 不覆盖它），
// 未设置回落 fallback；设置了但解析失败 = 启动即退（fail fast 一贯式，.env 手误不静默吞掉）。
func durationEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Fatalf("%s=%q 不是合法时长（如 30s、10s）：%v", key, raw, err)
	}
	return d
}

// intEnv 取 .env/进程环境里的非负整数变量（C5-A7 的 TAKO_MODEL_MAX_OUTPUT 约定）：
// 未设置回落 fallback；非整数或负值 = 启动即退（fail fast 一贯式，仿 durationEnv）。
func intEnv(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Fatalf("%s=%q 不是合法整数（正整数 tok；0 = 不顶）：%v", key, raw, err)
	}
	if n < 0 {
		log.Fatalf("%s=%d 不能为负（0 = 不顶）", key, n)
	}
	return n
}

// envOn 读长期记忆的三个开关（doc/29 §8）：未设置或空 = on，字面 "off" 才关（大小写不敏感）。
func envOn(key string) bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(key)), "off")
}

func main() {
	// 先读 .env 再注册旗标：-lease-ttl/-lease-renew 的默认值取自 TAKO_LEASE_TTL/TAKO_LEASE_RENEW。
	// Open() 内那次调用照旧（godotenv 不覆盖已设变量、可重复调），这里只是把它提前一步。
	config.LoadDotEnv()
	observability.Setup(context.Background())

	engine := flag.String("engine", "127.0.0.1:8090", "listen address")
	batch := flag.Int("batch-window", 200, "per-batch limit for frames/log reads")
	dev := flag.Bool("dev", true, "register dev-only endpoints (reset)")
	bootstrapAdmin := flag.String("bootstrap-admin-email", os.Getenv("TAKO_BOOTSTRAP_ADMIN_EMAIL"), "existing user email to mark system admin at startup")
	redisAddr := flag.String("redis", "127.0.0.1:6390", "redis address holding the tk:runlive/tk:runepoch keys")
	pgDSN := flag.String("pg", defaultPGDSN, "postgres DSN (sessions/runs/run_model_messages/runlog_entries)")
	leaseTTL := flag.Duration("lease-ttl", durationEnv("TAKO_LEASE_TTL", defaultLeaseTTL), "gate lease TTL; watchdog keeps renewing it during generation (env TAKO_LEASE_TTL)")
	leaseRenew := flag.Duration("lease-renew", durationEnv("TAKO_LEASE_RENEW", defaultLeaseRenew), "watchdog renew interval (must be < lease-ttl; env TAKO_LEASE_RENEW)")
	compactWindow := flag.Int("compact-window", defaultCompactWindow, "context window in tokens for compaction; limit = window - min(window/5, 16384); 0 disables")
	resumeMax := flag.Int("resume-max", 3, "max resumes per run (doc/30); resumeCount = lease_epoch-1, resumable while below it")
	archiveTTL := flag.Duration("archive-ttl", 168*time.Hour, "how long compacted transcripts and soft-deleted session archives stay before the sweeper removes them")
	agentsDir := flag.String("agents-dir", "agents", "directory of sub-agent specs (*.md + YAML frontmatter); scanned at each run start (relative to CWD)")
	workspace := flag.String("workspace", "./workspace", "workspace root for first-party file tools; created at startup, tools are confined to it")
	systemSkillsDir := flag.String("system-skills-dir", "", "system skills root (doc/52 §4.2); default <workspace>/system-skills; the app only reads it, the release pipeline writes it")
	subagentConcurrency := flag.Int("subagent-concurrency", 8, "max concurrent subagent delegations (process-wide semaphore; must be >= 1)")
	subagentReturnCap := flag.Int("subagent-return-cap", 51200, "max bytes of a subagent's return text handed back to the parent; excess is truncated with a notice (<=0 disables)")
	rebuildDialogue := flag.Bool("rebuild-dialogue", false, "clear dialogue_chunks at startup and let the indexer rebuild them from complete runs (requires TAKO_RAG=on)")
	migrateTo := flag.String("memory-migrate-to", "", "offline memory mode migration: markdown|pipeline; stop all engine replicas first; does not start HTTP/worker/consumer (doc/65 §6.2)")
	flag.Parse()

	// 离线互切形态（doc/65 §6.2）：不起 HTTP / Worker / 消费者 / 定时器，只连 PG 与工作区。
	// 放在所有引擎校验之前：lease/subagent/resume 这些参数与迁移无关，不该挡住迁移。
	if *migrateTo != "" {
		if *migrateTo != runengine.MemoryMigrateToMarkdown && *migrateTo != runengine.MemoryMigrateToPipeline {
			fmt.Fprintf(os.Stderr, "memory-migrate-to=%q 不是合法值（markdown|pipeline）\n", *migrateTo)
			flag.Usage()
			os.Exit(2)
		}
		wsAbs, err := filepath.Abs(*workspace)
		if err != nil {
			log.Fatalf("workspace %q 绝对化: %v", *workspace, err)
		}
		if err := runengine.RunMemoryModeMigration(context.Background(), *migrateTo, *pgDSN, wsAbs); err != nil {
			log.Fatalf("memory mode migration: %v", err)
		}
		return
	}

	// 必须 leaseRenew 严格小于 leaseTTL：
	//   ① renew >= ttl → 第一次 watchdog 触发前租约已过期 = 进程稳定地自己放闸，
	//      其他副本能抢同 session，并发写 runlog/model_messages 把帧序和历史写乱。
	//   ② renew == ttl 容错余量为 0，一次 ticker 抖动 / GC / 存储瞬断就踩线。
	//   默认 30s/10s 给 2 次漏续余量（连续 3 拍全丢才误放）。
	// .env 的 TAKO_LEASE_* 是新增的配置面，手误把两个值输反是常见坑，fail fast 起不来比跑起来自毁好。
	if *leaseRenew >= *leaseTTL {
		log.Fatalf("lease-renew %s ≥ lease-ttl %s：必须严格小于（默认 30s/10s = 2 次漏续余量）", *leaseRenew, *leaseTTL)
	}
	// 委派并发闸容量 0 = 信号量为空通道：每笔委派都永久等闸（直到 run ctx 取消）——同类手误，启动即退。
	if *subagentConcurrency < 1 {
		log.Fatalf("subagent-concurrency %d < 1：并发闸至少要 1 个位（0 = 所有委派永久等闸）", *subagentConcurrency)
	}
	// 续跑窗口 0 = 所有 run 都不可续（resumable 恒 false），留着只会让配置看起来开了、实际没开——
	// 手误与默认值混淆同款，启动即退（与上面的闸容量同一处置）。
	if *resumeMax < 1 {
		log.Fatalf("resume-max %d < 1：至少要 1 次续跑（0 = 崩溃后永远不可续）", *resumeMax)
	}
	if *archiveTTL <= 0 {
		log.Fatalf("archive-ttl %s 必须大于 0（默认 168h）", *archiveTTL)
	}

	ctx := context.Background()

	// fail fast：模型配置不可用 = 引擎起不来，不留半个只能 409 的进程（config 自己向 stderr 报原因）。
	opened := config.MustOpen(ctx)
	log.Printf("model %s (%s)", opened.Ref, opened.Protocol)

	// fail fast 同理：Redis 与 PG 任一不可达就不进监听——四个端点全要动存储（闸在 PG、临时态在 Redis），
	// 半死的进程只会给每个请求回 500。
	store, err := runengine.OpenStore(ctx, *redisAddr, *pgDSN, runliveTTL, *leaseTTL)
	if err != nil {
		log.Fatal(err)
	}
	if err := store.SeedAgentDefaults(ctx, *agentsDir); err != nil {
		log.Fatalf("agent defaults: %v", err)
	}
	log.Printf("redis %s + pg %s (runlive ttl %s, lease %s renew %s)", *redisAddr, *pgDSN, runliveTTL, *leaseTTL, *leaseRenew)
	// 密钥缺失在这里退，不让后面的抽取循环先跑起来再被 Fatal 掐掉。
	auth, err := runengine.NewAuth(store, *dev)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	if email := strings.TrimSpace(*bootstrapAdmin); email != "" {
		found, err := store.BootstrapSystemAdmin(ctx, strings.ToLower(email))
		if err != nil {
			log.Fatalf("bootstrap admin: %v", err)
		}
		if !found {
			log.Printf("bootstrap admin %s 不存在，跳过", email)
		} else {
			log.Printf("bootstrap admin %s", email)
		}
	}

	// 压缩计数器同走 fail fast（§10.3 第 10 条）：词表取不到就别起——超窗后才发现「压不动」更糟。
	// 首次获取从 openaipublic 拉词表并落盘缓存，之后启动即命中（缓存目录 TIKTOKEN_CACHE_DIR/系统临时目录）。
	var counter runengine.TokenCounter
	if *compactWindow > 0 {
		if counter, err = runengine.NewCl100kCounter(); err != nil {
			log.Fatalf("compact counter (cl100k): %v", err)
		}
	}
	// workspace（F1）：起点即绝对化——越界判定是纯字符串前缀检查（resolveInWorkspace），
	// 相对根会让检查结果依赖 CWD 的偶然值；启动 MkdirAll 保证根存在（工具只负责树内创建）。
	wsAbs, err := filepath.Abs(*workspace)
	if err != nil {
		log.Fatalf("workspace %q 绝对化: %v", *workspace, err)
	}
	if err := os.MkdirAll(wsAbs, 0o755); err != nil {
		log.Fatalf("workspace %q 创建: %v", wsAbs, err)
	}
	log.Printf("workspace %s", wsAbs)
	log.Printf("bash sandbox: %s", runengine.SandboxStatus())
	// 系统技能根（doc/52 §4.2）：默认 <workspace>/system-skills。
	// 旗标指到别处时用它（验收要给隔离实例指不同的根）。
	//
	// **根目录本身要建出来（可以是空的）**：rescan 读到「根不存在」按「这个归属下还没装
	// 技能」处理（视同空），但读到「根读失败」按「盘没挂上」处理、整次重扫中止。
	// 目录不存在与目录读不出来，在 unix 上是两件不同的事——前者是正常状态，后者是故障。
	sysSkills := strings.TrimSpace(*systemSkillsDir)
	if sysSkills == "" {
		sysSkills = filepath.Join(wsAbs, "system-skills")
	}
	sysSkillsAbs, err := filepath.Abs(sysSkills)
	if err != nil {
		log.Fatalf("system-skills-dir %q 绝对化: %v", sysSkills, err)
	}
	if err := os.MkdirAll(sysSkillsAbs, 0o755); err != nil {
		log.Fatalf("system-skills-dir %q 创建: %v", sysSkillsAbs, err)
	}
	log.Printf("system skills %s", sysSkillsAbs)
	if n, err := runengine.ClearSessionMedia(wsAbs); err != nil {
		log.Fatalf("清理会话旧媒体: %v", err)
	} else if n > 0 {
		log.Printf("清理了 %d 个会话 .media 目录", n)
	}
	if host, herr := os.Hostname(); herr == nil {
		marker := filepath.Join(wsAbs, ".tako-replica")
		prev, rerr := os.ReadFile(marker)
		if rerr != nil {
			_ = os.WriteFile(marker, []byte(host+"\n"), 0o644)
		} else if strings.TrimSpace(string(prev)) != host {
			log.Printf("workspace 警告: %s 上次由主机 %s 使用，本机是 %s。多副本必须把 -workspace 指到同一块共享盘，否则另一边读不到文件", wsAbs, strings.TrimSpace(string(prev)), host)
		}
	}

	// 长期记忆配置（doc/29 §8）：三个开关默认 on，值 "off" 才关；tenant 一期恒 default。
	// 配置写入 Store 后，EndRun（登记 job）、Worker（注入）、Server（预览）读同一份。
	//
	// 记忆**模式**（doc/65 §0 / §5）：实例级开关，pipeline（默认）或 markdown。
	// 非法值启动即退——「以为切了模式、其实没切」这种半状态不能活到线上。
	memMode, err := runengine.ParseMemoryMode(os.Getenv("TAKO_MEMORY_MODE"))
	if err != nil {
		log.Fatalf("TAKO_MEMORY_MODE: %v", err)
	}
	memCfg := runengine.MemoryConfig{
		TenantID:  "default",
		Workspace: wsAbs,
		Extract:   envOn("TAKO_MEMORY_EXTRACT"),
		Recall:    envOn("TAKO_MEMORY_RECALL"),
		RAG:       envOn("TAKO_RAG"),
		Mode:      memMode,
	}
	// markdown 模式：只装配文件工具——抽取消费者、memory_jobs、postrun、每回合 BM25 召回
	// 全不启动。环境变量即使还写着 on 也不生效（下面那行日志会显示 extract=false recall=false）。
	if memCfg.Mode == runengine.MemoryModeMarkdown {
		memCfg.Extract = false
		memCfg.Recall = false
	}
	store.ConfigureMemory(memCfg)
	// 线二的 pg_search 索引（doc/29 §5.4）：RAG 开且 preload/扩展缺失 = 启动即退；
	// RAG 关完全不碰扩展（建表由 OpenStore 的 AutoMigrate 照常完成）。
	if err := store.EnsureDialogueIndex(ctx); err != nil {
		log.Fatalf("dialogue index: %v", err)
	}
	// 线二重建（doc/29 §11 D3）：显式清空派生副本，索引循环从 complete 的 runs 反查重建。
	// RAG 关时索引循环不启动——清空后没人重建，所以旗标与开关互斥，手误启动即退。
	if *rebuildDialogue {
		if !memCfg.RAG {
			log.Fatalf("rebuild-dialogue 需要 TAKO_RAG=on：索引循环未启动，清空后没人重建")
		}
		n, err := store.ResetDialogueChunks(ctx)
		if err != nil {
			log.Fatalf("rebuild-dialogue: %v", err)
		}
		log.Printf("rebuild-dialogue: 清空 dialogue_chunks %d 行，等待索引循环重建", n)
	}
	log.Printf("memory: mode=%s extract=%v recall=%v rag=%v", memCfg.Mode, memCfg.Extract, memCfg.Recall, memCfg.RAG)

	// C5-A7：摘要预算上限的硬顶（正整数 tok；缺省/0 = 不顶；解析失败/负由 intEnv fail fast）。
	maxOutput := intEnv("TAKO_MODEL_MAX_OUTPUT", 0)

	// T3-1b：子代理 `model:` 写值档的模型客户端缓存（进程内懒加载，19v6-05 §4）——启动时为空，
	// 第一条用到某个 ref 的规格进来才建对应客户端；解析失败是逐条拒载（不 fail fast，
	// 与 TAKO_MODEL 的启动 fail fast 不同：主模型没有可降级对象，子代理规格有）。
	modelRegistry := config.NewModelRegistry(config.Environ())
	tenantModels := runengine.NewTenantModels(modelRegistryResolver{modelRegistry}, os.Environ(), opened.Ref)
	store.SetTenantModels(tenantModels)

	// 记忆层的存量迁移（doc/65 §6.1）：notes 表的数据搬进 memory_items 的 resident 层、
	// 旧 memory_items 行迁成 recall/workspace、旧列与旧表清掉。**可重入**——第二次启动
	// 整步跳过（每一步都以「输入还在不在」为闸），不会重复搬、不会把 teamInstructions 追加两遍。
	// 迁移失败 = 启动失败：带着半迁移的库继续跑，两种模式的记忆语义就是错的。
	if err := store.MigrateMemorySchema(ctx); err != nil {
		log.Fatalf("memory migrate: %v", err)
	}

	// 记忆的视图与 BM25 索引（doc/65 §4.1/§4.4）：pipeline 且 Extract/Recall 任一开才需要；
	// pg_search 缺失启动即退。**与 dialogue 的索引检查分开执行**——记忆检查的错误文案
	// 不建议「设 TAKO_RAG=off」（TAKO_RAG 从此只控制 dialogue_chunks，施工包 §5.1）。
	if memCfg.Mode == runengine.MemoryModePipeline && (memCfg.Extract || memCfg.Recall) {
		if err := store.EnsureMemoryIndexes(ctx); err != nil {
			log.Fatalf("memory index: %v", err)
		}
	}

	// 记忆抽取的消费者与对账器（doc/65 §4.6）：pipeline 且 Extract=on 才起；
	// broker 不可达 / 认证失败 / topic 不存在 = 启动即退（fail fast）。
	// Extract=off 不要求 broker（没有登记方，任务侧本来就没有流量）。
	// markdown 模式两者都不碰（上面 ConfigureMemory 已把开关按关处理）。
	var memoryWake runengine.MemoryWakeFunc
	if memCfg.Mode == runengine.MemoryModePipeline && memCfg.Extract {
		memoryModelRef := strings.TrimSpace(os.Getenv("TAKO_MEMORY_MODEL"))
		getModel := func(ctx context.Context) (model.BaseChatModel, error) {
			if memoryModelRef == "" {
				return opened.Chat, nil
			}
			inst, err := modelRegistryResolver{modelRegistry}.Get(ctx, memoryModelRef)
			if err != nil {
				return nil, err
			}
			return inst.Chat, nil
		}
		stop, wake, err := runengine.StartMemoryConsumer(ctx, store, getModel, counter)
		if err != nil {
			log.Fatalf("memory consumer: %v", err)
		}
		memoryWake = wake
		_ = stop // 进程生命周期与 main 一致（log.Fatal 退出）；显式停服路径归 API 层
	}

	// 线二索引循环（doc/29 §5.2）：RAG 关时不启动；反查「complete 且未入 dialogue_chunks」，
	// 删行/漏写由下一轮自愈，不建 outbox。
	if memCfg.RAG {
		go store.RunDialogueIndexer(ctx)
	}

	// agent 装配（T1）失败也是启动错误：装配不出来 = 主循环跑不了，fail fast 一贯式。
	// T3：agentsDir 只是规格目录位置（装载是 run 起手扫盘、失败降级为 log）——目录不存在不算启动错误。
	mediaSend := envOn("TAKO_MEDIA_SEND")
	log.Printf("media: send=%v", mediaSend)
	worker, err := runengine.NewWorker(store, opened.Chat, runengine.TranslatorFor(string(opened.Protocol)), config.SystemPrompt(), wsAbs, *agentsDir, *leaseRenew, *compactWindow, opened.Ref, maxOutput, *subagentConcurrency, *subagentReturnCap, modelRegistryResolver{modelRegistry}, counter, mediaSend, *archiveTTL, memoryWake)
	if err != nil {
		log.Fatalf("agent 装配: %v", err)
	}
	worker.SetTenantModels(tenantModels)
	if *compactWindow > 0 {
		log.Printf("compact window %d tok (limit %d)", *compactWindow, runengine.CompactLimitFor(*compactWindow))
	} else {
		log.Printf("compact off (-compact-window 0)")
	}
	log.Printf("subagents: concurrency %d, return cap %d B", *subagentConcurrency, *subagentReturnCap)
	log.Printf("archive ttl %s", *archiveTTL)
	r := runengine.NewRouter(store, *batch, *dev, worker, *resumeMax, auth, sysSkillsAbs)
	log.Fatal(r.Run(*engine))
}
