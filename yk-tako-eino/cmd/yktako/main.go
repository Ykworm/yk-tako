package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/runengine"
)

// runliveTTL 是 run **正常结束**后 runlive 键的回收期限（19v6-03「生命周期」只写「设 TTL」未给数值；dev 取 300s）。
// 它不是正确性参数，只是回收参数：`done` 与 `gone` 收敛到同一条收口路径（planning §5 第 4 条）。
const runliveTTL = 300 * time.Second

// 闸租约（A7/A7-2；D1 落库后闸 = runs 活行）：抢闸的 INSERT 出生即带 lease_until（DB 时钟 now()+lease），
// tk:draft / tk:runlive 两键同笔出生、同租约（A7-2 的 Redis 半边）；worker watchdog 每 leaseRenew 续回。
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

// defaultCompactWindow 是上下文压缩的窗口 token 数（C 系列；doc/12 §2：查不到就用 256000）。
// 触发阈值 limit = window × 4/5（doc/12 §9）；0 = 关压缩（验收对照跑法）。
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

func main() {
	// 先读 .env 再注册旗标：-lease-ttl/-lease-renew 的默认值取自 TAKO_LEASE_TTL/TAKO_LEASE_RENEW。
	// Open() 内那次调用照旧（godotenv 不覆盖已设变量、可重复调），这里只是把它提前一步。
	config.LoadDotEnv()

	engine := flag.String("engine", "127.0.0.1:8090", "listen address")
	batch := flag.Int("batch-window", 200, "per-batch limit for frames/log reads")
	dev := flag.Bool("dev", true, "register dev-only endpoints (reset)")
	redisAddr := flag.String("redis", "127.0.0.1:6390", "redis address holding the tk:runlive/tk:draft keys")
	pgDSN := flag.String("pg", defaultPGDSN, "postgres DSN (sessions/runs/run_model_messages/runlog_entries)")
	leaseTTL := flag.Duration("lease-ttl", durationEnv("TAKO_LEASE_TTL", defaultLeaseTTL), "gate lease TTL; watchdog keeps renewing it during generation (env TAKO_LEASE_TTL)")
	leaseRenew := flag.Duration("lease-renew", durationEnv("TAKO_LEASE_RENEW", defaultLeaseRenew), "watchdog renew interval (must be < lease-ttl; env TAKO_LEASE_RENEW)")
	compactWindow := flag.Int("compact-window", defaultCompactWindow, "context window in tokens for compaction; limit = window * 4/5; 0 disables")
	agentsDir := flag.String("agents-dir", "agents", "directory of sub-agent specs (*.md + YAML frontmatter); scanned at each run start (relative to CWD)")
	workspace := flag.String("workspace", "./workspace", "workspace root for first-party file tools; created at startup, tools are confined to it")
	subagentConcurrency := flag.Int("subagent-concurrency", 8, "max concurrent subagent delegations (process-wide semaphore; must be >= 1)")
	subagentReturnCap := flag.Int("subagent-return-cap", 51200, "max bytes of a subagent's return text handed back to the parent; excess is truncated with a notice (<=0 disables)")
	flag.Parse()

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
	log.Printf("redis %s + pg %s (runlive ttl %s, lease %s renew %s)", *redisAddr, *pgDSN, runliveTTL, *leaseTTL, *leaseRenew)

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

	// C5-A7：摘要预算帽的硬顶（正整数 tok；缺省/0 = 不顶；解析失败/负由 intEnv fail fast）。
	maxOutput := intEnv("TAKO_MODEL_MAX_OUTPUT", 0)

	// T3-1b：子代理 `model:` 写值档的模型客户端缓存（进程内懒加载，19v6-05 §4）——启动时为空，
	// 第一条用到某个 ref 的规格进来才建对应客户端；解析失败是逐条拒载（不 fail fast，
	// 与 TAKO_MODEL 的启动 fail fast 不同：主模型没有可降级对象，子代理规格有）。
	modelRegistry := config.NewModelRegistry(config.Environ())

	// agent 装配（T1）失败也是启动错误：装配不出来 = 主循环跑不了，fail fast 一贯式。
	// T3：agentsDir 只是规格目录位置（装载是 run 起手扫盘、失败降级为 log）——目录不存在不算启动错误。
	worker, err := runengine.NewWorker(store, opened.Chat, runengine.TranslatorFor(string(opened.Protocol)), config.SystemPrompt(), wsAbs, *agentsDir, *leaseRenew, *compactWindow, opened.Ref, maxOutput, *subagentConcurrency, *subagentReturnCap, modelRegistryResolver{modelRegistry}, counter)
	if err != nil {
		log.Fatalf("agent 装配: %v", err)
	}
	if *compactWindow > 0 {
		log.Printf("compact window %d tok (limit %d)", *compactWindow, *compactWindow*4/5)
	} else {
		log.Printf("compact off (-compact-window 0)")
	}
	log.Printf("subagents: concurrency %d, return cap %d B", *subagentConcurrency, *subagentReturnCap)
	r := runengine.NewRouter(store, *batch, *dev, worker)
	log.Fatal(r.Run(*engine))
}
