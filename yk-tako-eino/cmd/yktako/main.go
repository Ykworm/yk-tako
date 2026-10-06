package main

import (
	"context"
	"flag"
	"log"
	"time"

	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/runengine"
)

// runliveTTL 是 run 结束后 runlive 键的回收期限（doc/19 §7 只写「设 TTL」未给数值；dev 取 300s）。
// 它不是正确性参数，只是回收参数：`done` 与 `gone` 收敛到同一条收口路径（planning §5 第 4 条）。
const runliveTTL = 300 * time.Second

func main() {
	engine := flag.String("engine", "127.0.0.1:8090", "listen address")
	batch := flag.Int("batch-window", 200, "per-batch limit for frames/log reads")
	dev := flag.Bool("dev", true, "register dev-only endpoints (reset)")
	redisAddr := flag.String("redis", "127.0.0.1:6390", "redis address holding the tk:* keyspace")
	flag.Parse()

	ctx := context.Background()

	// fail fast：模型配置不可用 = 引擎起不来，不留半个只能 409 的进程（config 自己向 stderr 报原因）。
	opened := config.MustOpen(ctx)
	log.Printf("model %s (%s)", opened.Ref, opened.Protocol)

	// fail fast 同理：Redis 不可达就不进监听——否则每个请求都 500，闸也无处可放。
	store := runengine.NewRedisStore(*redisAddr, runliveTTL)
	if err := store.Ping(ctx); err != nil {
		log.Fatalf("redis %s: %v", *redisAddr, err)
	}
	log.Printf("redis %s (runlive ttl %s)", *redisAddr, runliveTTL)

	worker := runengine.NewWorker(store, opened.Chat, runengine.TranslatorFor(string(opened.Protocol)), config.SystemPrompt())
	r := runengine.NewRouter(store, *batch, *dev, worker)
	log.Fatal(r.Run(*engine))
}
