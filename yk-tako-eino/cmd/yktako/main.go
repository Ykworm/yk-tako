package main

import (
	"context"
	"flag"
	"log"
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
// 二者同为回收参数：30s/5s = 连续 6 次续约全失败才误放。
const (
	defaultLeaseTTL   = 30 * time.Second
	defaultLeaseRenew = 5 * time.Second
)

// defaultPGDSN 指向 dev 的独立 PG 实例（dev.sh 自举的一半；双 fail fast：PG 不在引擎起不来）。
const defaultPGDSN = "host=127.0.0.1 port=5433 dbname=tako sslmode=disable"

// defaultCompactWindow 是上下文压缩的窗口 token 数（C 系列；doc/12 §2：查不到就用 256000）。
// 触发阈值 limit = window × 4/5（doc/12 §9）；0 = 关压缩（验收对照跑法）。
const defaultCompactWindow = 256000

func main() {
	engine := flag.String("engine", "127.0.0.1:8090", "listen address")
	batch := flag.Int("batch-window", 200, "per-batch limit for frames/log reads")
	dev := flag.Bool("dev", true, "register dev-only endpoints (reset)")
	redisAddr := flag.String("redis", "127.0.0.1:6390", "redis address holding the tk:runlive/tk:draft keys")
	pgDSN := flag.String("pg", defaultPGDSN, "postgres DSN (sessions/runs/run_model_messages/runlog_entries)")
	leaseTTL := flag.Duration("lease-ttl", defaultLeaseTTL, "gate lease TTL; watchdog keeps renewing it during generation")
	leaseRenew := flag.Duration("lease-renew", defaultLeaseRenew, "watchdog renew interval (must be < lease-ttl)")
	compactWindow := flag.Int("compact-window", defaultCompactWindow, "context window in tokens for compaction; limit = window * 4/5; 0 disables")
	flag.Parse()

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
	worker := runengine.NewWorker(store, opened.Chat, runengine.TranslatorFor(string(opened.Protocol)), config.SystemPrompt(), *leaseRenew, *compactWindow, counter)
	if *compactWindow > 0 {
		log.Printf("compact window %d tok (limit %d)", *compactWindow, *compactWindow*4/5)
	} else {
		log.Printf("compact off (-compact-window 0)")
	}
	r := runengine.NewRouter(store, *batch, *dev, worker)
	log.Fatal(r.Run(*engine))
}
