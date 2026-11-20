package runengine

// 本文件是历史对话混合检索的装配与错误映射（doc/84 §6.2/§6.4；施工包 §3.1）：
//
//   - 配置解析（TAKO_DIALOGUE_RETRIEVAL / TAKO_ES_* / TAKO_MILVUS_* /
//     TAKO_DIALOGUE_EMBEDDING_* / TAKO_REDPANDA_*），索引名前缀闸保护共享 demo 实例；
//   - 客户端错误类型 dialogueClientError 与机器可读错误分类词表（last_error 只存这些串，
//     不含正文与密钥——施工包禁令）；
//   - 业务码 119001–119005 的 HTTP 映射（P4 端点消费；本阶段先建映射与哨兵）；
//   - StartDialogueRetrieval：hybrid 模式的启动预检（ES mapping/_analyze、Milvus describe、
//     最小真实 embedding、Redpanda topic 元数据）与 relay + 两个消费者组的生命周期接线。
//     预检任何失败 = 启动失败（119001/119002/119003/119004 类），不静默降级。
//
// 模式语义（doc/84 §3）：
//   - pg_search（默认）：零新增行为——不登记 outbox、不起 relay、不起消费者；
//   - hybrid：TAKO_RAG=on 才成立（对话块与索引循环是它的输入）；RAG 关时按 pg_search
//     运行并记日志——hybrid 的输入根本不存在，硬跑只会空转。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 模式词表（doc/84 §3）。
const (
	DialogueModePGSearch = "pg_search"
	DialogueModeHybrid   = "hybrid"
)

// 机器可读错误分类（写 dialogue_index_jobs.last_error；施工包禁令：不含正文与密钥）。
const (
	dlgErrESUnavailable     = "es_unavailable" // 网络/超时/429/5xx（退避重试）
	dlgErrESPermanent       = "es_permanent"   // 其余 4xx、非法响应（死信）
	dlgErrEmbedUnavailable  = "embedding_unavailable"
	dlgErrEmbedBadResponse  = "embedding_bad_response"
	dlgErrEmbedDimMismatch  = "embedding_dimension_mismatch" // 119004 类
	dlgErrEmbedOverLimit    = "embedding_over_limit"         // 单块超 token 上限（不截断，死信）
	dlgErrMilvusUnavailable = "milvus_unavailable"
	dlgErrMilvusPermanent   = "milvus_permanent"
	dlgErrMilvusDimMismatch = "milvus_dimension_mismatch" // 119004 类

	dialogueErrPgRead            = "pg_read_failed"     // chunk/任务行读不出来（瞬时退避）
	dialogueErrInternalTransient = "internal_transient" // 未知错误一律按瞬时退避
	dialogueErrPointerMissing    = "pointer_missing"    // 指针行读不到（瞬时退避）
	dialogueErrProcessorContract = "processor_contract" // processor 返回形状被破坏（防呆）
)

// dialogueClientError 是三个外部客户端共用的错误类型：带机器可读分类与瞬时/永久
// 语义，消费者驱动据此写回退避或死信；Cause 只进日志，不进 last_error。
type dialogueClientError struct {
	Class     string
	Permanent bool
	Cause     error
}

func (e *dialogueClientError) Error() string {
	if e.Cause != nil {
		return e.Class + ": " + e.Cause.Error()
	}
	return e.Class
}

func (e *dialogueClientError) Unwrap() error { return e.Cause }

// ── 业务码 119xxx 与哨兵（doc/84 §6.4；词表登记在 auth.go）───────────────────

var (
	ErrDialogueLexicalUnavailable   = errors.New("历史对话词法索引不可用")
	ErrDialogueVectorUnavailable    = errors.New("历史对话向量索引不可用")
	ErrDialogueEmbeddingUnavailable = errors.New("历史对话向量生成服务不可用")
	ErrDialogueDimensionMismatch    = errors.New("向量模型或维度已变化，请重建对话索引")
	ErrDialogueRetrievalOff         = errors.New("当前未启用历史对话混合检索")
)

// dialogueRetrievalHTTPError 把检索链路的错误映射成 {status, code, message}
// （doc/84 §6.4：503 119001/119002/119003，409 119004/119005；失败 data 恒 null，
// 不写底层堆栈）。ok=false 表示不是本链路的错误。P4 的查询/诊断端点消费这个映射。
func dialogueRetrievalHTTPError(err error) (status, code int, message string, ok bool) {
	switch {
	case errors.Is(err, ErrDialogueRetrievalOff):
		return http.StatusConflict, codeDialogueRetrievalOff, ErrDialogueRetrievalOff.Error(), true
	case errors.Is(err, ErrDialogueDimensionMismatch):
		return http.StatusConflict, codeDialogueDimensionMismatch, ErrDialogueDimensionMismatch.Error(), true
	}
	var ce *dialogueClientError
	if errors.As(err, &ce) {
		switch {
		case ce.Class == dlgErrMilvusDimMismatch || ce.Class == dlgErrEmbedDimMismatch:
			return http.StatusConflict, codeDialogueDimensionMismatch, ErrDialogueDimensionMismatch.Error(), true
		case strings.HasPrefix(ce.Class, "es_"):
			return http.StatusServiceUnavailable, codeDialogueLexicalUnavailable, ErrDialogueLexicalUnavailable.Error(), true
		case strings.HasPrefix(ce.Class, "embedding_"):
			return http.StatusServiceUnavailable, codeDialogueEmbeddingUnavailable, ErrDialogueEmbeddingUnavailable.Error(), true
		case strings.HasPrefix(ce.Class, "milvus_"):
			return http.StatusServiceUnavailable, codeDialogueVectorUnavailable, ErrDialogueVectorUnavailable.Error(), true
		}
	}
	return 0, 0, "", false
}

// ── 配置解析 ────────────────────────────────────────────────────────────────

// ParseDialogueRetrievalMode 解析检索模式：空/未设置 = pg_search；非法值启动即退。
func ParseDialogueRetrievalMode(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return DialogueModePGSearch, nil
	}
	if v == DialogueModePGSearch || v == DialogueModeHybrid {
		return v, nil
	}
	return "", fmt.Errorf("TAKO_DIALOGUE_RETRIEVAL=%q 不是合法值（pg_search|hybrid）", v)
}

// DialogueRetrievalConfig 是 hybrid 的完整连接口径（凭据只从环境读，不落库不进日志）。
type DialogueRetrievalConfig struct {
	Mode string

	ESURL      string
	ESUsername string
	ESPassword string
	ESIndex    string

	MilvusURI        string
	MilvusToken      string
	MilvusDB         string
	MilvusCollection string
	MilvusUseTLS     bool

	Embedding dialogueEmbeddingConfig

	// 队列口径（broker/凭据与 memory 共用一组环境变量；两个消费者组共用）。
	QueueBrokers  []string
	QueueUser     string
	QueuePassword string
	QueueMech     string

	// 覆盖对账器的轮询间隔（TAKO_DIALOGUE_RETRIEVAL=hybrid 时解析，缺省 5 分钟）。
	ReconcileInterval time.Duration
}

// dialogueEnvInt 取正整数环境变量（缺省 fallback；非法值启动即退）。
func dialogueEnvInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q 必须是正整数", key, raw)
	}
	return n, nil
}

// dialogueEnvDuration 取正时长环境变量（缺省 fallback；非法值启动即退）。
func dialogueEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s=%q 必须是正时长（如 30s）", key, raw)
	}
	return d, nil
}

// DialogueRetrievalConfigFromEnv 读全部配置。hybrid 模式下任何缺项/非法项都报错
// （启动即退），pg_search 模式不校验外部依赖项（它们不参与运行）。
func DialogueRetrievalConfigFromEnv() (DialogueRetrievalConfig, error) {
	mode, err := ParseDialogueRetrievalMode(os.Getenv("TAKO_DIALOGUE_RETRIEVAL"))
	if err != nil {
		return DialogueRetrievalConfig{}, err
	}
	cfg := DialogueRetrievalConfig{
		Mode:             mode,
		ESURL:            strings.TrimSpace(os.Getenv("TAKO_ES_URL")),
		ESUsername:       strings.TrimSpace(os.Getenv("TAKO_ES_USERNAME")),
		ESPassword:       os.Getenv("TAKO_ES_PASSWORD"),
		ESIndex:          strings.TrimSpace(os.Getenv("TAKO_ES_INDEX")),
		MilvusURI:        strings.TrimSpace(os.Getenv("TAKO_MILVUS_URI")),
		MilvusToken:      os.Getenv("TAKO_MILVUS_TOKEN"),
		MilvusDB:         strings.TrimSpace(os.Getenv("TAKO_MILVUS_DB")),
		MilvusCollection: strings.TrimSpace(os.Getenv("TAKO_MILVUS_COLLECTION")),
		MilvusUseTLS:     strings.EqualFold(strings.TrimSpace(os.Getenv("TAKO_MILVUS_USE_TLS")), "true"),
	}
	if cfg.ESIndex == "" {
		cfg.ESIndex = "tako_dialogue_v1"
	}
	if cfg.MilvusCollection == "" {
		cfg.MilvusCollection = "tako_dialogue_v1"
	}
	if cfg.MilvusDB == "" {
		cfg.MilvusDB = "default"
	}
	qcfg := dialogueQueueConfigFromEnv("")
	cfg.QueueBrokers, cfg.QueueUser, cfg.QueuePassword, cfg.QueueMech =
		qcfg.Brokers, qcfg.User, qcfg.Password, qcfg.Mech

	embTimeout, err := dialogueEnvDuration("TAKO_DIALOGUE_EMBEDDING_TIMEOUT", 30*time.Second)
	if err != nil {
		return DialogueRetrievalConfig{}, err
	}
	embBatch, err := dialogueEnvInt("TAKO_DIALOGUE_EMBEDDING_BATCH_SIZE", dialogueEmbeddingBatchSize)
	if err != nil {
		return DialogueRetrievalConfig{}, err
	}
	embMaxTok, err := dialogueEnvInt("TAKO_DIALOGUE_EMBEDDING_MAX_INPUT_TOKENS", dialogueEmbeddingMaxInputTokens)
	if err != nil {
		return DialogueRetrievalConfig{}, err
	}
	cfg.Embedding = dialogueEmbeddingConfig{
		BaseURL:        strings.TrimRight(strings.TrimSpace(os.Getenv("TAKO_DIALOGUE_EMBEDDING_BASE_URL")), "/"),
		APIKey:         os.Getenv("TAKO_DIALOGUE_EMBEDDING_API_KEY"),
		Model:          strings.TrimSpace(os.Getenv("TAKO_DIALOGUE_EMBEDDING_MODEL")),
		Timeout:        embTimeout,
		BatchSize:      embBatch,
		MaxInputTokens: embMaxTok,
	}
	if mode == DialogueModeHybrid {
		recInterval, err := dialogueEnvDuration(dialogueCoverageIntervalEnv, dialogueCoverageInterval)
		if err != nil {
			return DialogueRetrievalConfig{}, err
		}
		cfg.ReconcileInterval = recInterval
		if err := cfg.validate(); err != nil {
			return DialogueRetrievalConfig{}, err
		}
	}
	return cfg, nil
}

// validate 是 hybrid 的配置闸。索引名前缀（tako_dialogue_*）保护共享 demo ES 实例：
// 前缀不对宁可不起，绝不往别人的索引里写。
func (c DialogueRetrievalConfig) validate() error {
	if c.ESURL == "" {
		return errors.New("TAKO_ES_URL 未配置（hybrid 模式必需）")
	}
	if c.ESUsername == "" || c.ESPassword == "" {
		return errors.New("TAKO_ES_USERNAME / TAKO_ES_PASSWORD 未配置（hybrid 模式必需）")
	}
	if !strings.HasPrefix(c.ESIndex, "tako_dialogue_") {
		return fmt.Errorf("TAKO_ES_INDEX=%q 必须以 tako_dialogue_ 开头（共享 ES 实例保护，只允许创建本方案前缀的索引）", c.ESIndex)
	}
	if c.MilvusURI == "" {
		return errors.New("TAKO_MILVUS_URI 未配置（hybrid 模式必需）")
	}
	if c.MilvusToken == "" {
		return errors.New("TAKO_MILVUS_TOKEN 未配置（hybrid 模式必需）")
	}
	if !strings.HasPrefix(c.MilvusCollection, "tako_dialogue") {
		return fmt.Errorf("TAKO_MILVUS_COLLECTION=%q 必须以 tako_dialogue 开头（共享实例保护）", c.MilvusCollection)
	}
	if c.Embedding.BaseURL == "" || c.Embedding.Model == "" {
		return errors.New("TAKO_DIALOGUE_EMBEDDING_BASE_URL / TAKO_DIALOGUE_EMBEDDING_MODEL 未配置（hybrid 模式必需）")
	}
	if c.Embedding.APIKey == "" {
		return errors.New("TAKO_DIALOGUE_EMBEDDING_API_KEY 未配置（hybrid 模式必需）")
	}
	if c.Embedding.BatchSize < 1 || c.Embedding.BatchSize > 16 {
		return fmt.Errorf("TAKO_DIALOGUE_EMBEDDING_BATCH_SIZE=%d 超出 1–16", c.Embedding.BatchSize)
	}
	if c.Embedding.MaxInputTokens < 128 {
		return fmt.Errorf("TAKO_DIALOGUE_EMBEDDING_MAX_INPUT_TOKENS=%d 太小（至少 128）", c.Embedding.MaxInputTokens)
	}
	if len(c.QueueBrokers) == 0 {
		return fmt.Errorf("%s 未配置（hybrid 模式必需）", dialogueQueueEnvKey)
	}
	return nil
}

// ── 启动预检 ────────────────────────────────────────────────────────────────

// dialoguePrecheckText 是启动预检的分词用例（doc/84 §5.1「覆盖中文、英文、数字、
// 生造词及中英混排」）；P0 已在该实例上实测通过（见 tmp/84-历史对话混合检索/p0-实测.md
// 补测表），启动预检对同一实例重放并断言关键 token。
const dialoguePrecheckText = "数据库端口 5433 用 go1.27 编译，发布窗口改到周六凌晨"

// dialoguePrecheckAnalyzers 断言集：P0 实测字节（ik_max_word("数据库端口") 含 数据库+端口、
// 数字 5433 独立成 token；ik_smart("周末") 含 周末）。断言的是「分词器真实生效」，
// 不是穷举词表——ik 插件被卸载/换版本导致切词结果变化时启动失败并报告。
func dialoguePrecheckAnalyzers(es *dialogueESClient) error {
	mw, err := es.Analyze(context.Background(), "ik_max_word", "数据库端口 5433")
	if err != nil {
		return fmt.Errorf("ik_max_word: %w", err)
	}
	mwSet := make(map[string]bool, len(mw))
	for _, t := range mw {
		mwSet[t] = true
	}
	if !mwSet["数据库"] || !mwSet["端口"] {
		return fmt.Errorf("ik_max_word 预检失败：%q 切出 %v，缺 数据库/端口（ik 插件未生效？）", "数据库端口 5433", mw)
	}
	if !mwSet["5433"] {
		return fmt.Errorf("ik_max_word 预检失败：数字 5433 未独立成 token（切出 %v）", mw)
	}
	sm, err := es.Analyze(context.Background(), "ik_smart", "周末")
	if err != nil {
		return fmt.Errorf("ik_smart: %w", err)
	}
	smOK := false
	for _, t := range sm {
		if t == "周末" {
			smOK = true
		}
	}
	if !smOK {
		return fmt.Errorf("ik_smart 预检失败：%q 切出 %v，缺 周末", "周末", sm)
	}
	return nil
}

// ── 装配入口（cmd/yktako/main.go 调用）──────────────────────────────────────

// StartDialogueRetrieval 装配 hybrid 混合检索（doc/84 P2）：
//
//  1. 配置解析 + 前缀闸；
//  2. 启动预检逐项**真实执行**：ES 建索引 + mapping + ik 双分析器 _analyze 断言；
//     embedding 最小真实调用锁维度指纹；Milvus describe collection 比对维度（缺则建）；
//     Redpanda topic 元数据（队列构造时探）——任何失败 = 启动失败，不静默降级；
//  3. 指针播种（登记的代次真源）；
//  4. relay + 两个消费者组 + 各自 due 补投循环 + 覆盖对账器（doc/84 §6.4）起循环；
//
// 返回 stop（幂等；停 relay、消费者、对账器并收连接）。mode=pg_search 或
// TAKO_RAG=off 时返回 (nil, nil)：零新增行为。
func StartDialogueRetrieval(parent context.Context, store *Store, counter TokenCounter) (stop func(), err error) {
	cfg, err := DialogueRetrievalConfigFromEnv()
	if err != nil {
		return nil, err
	}
	if cfg.Mode != DialogueModeHybrid {
		log.Printf("dialogue retrieval: mode=%s（outbox/relay/消费者不启动）", cfg.Mode)
		return nil, nil
	}
	if !store.memoryCfg.RAG {
		log.Printf("dialogue retrieval: TAKO_DIALOGUE_RETRIEVAL=hybrid 但 TAKO_RAG=off——对话块与索引循环不存在，按 pg_search 运行（要启用请开 TAKO_RAG）")
		return nil, nil
	}

	// ES 预检：索引 + mapping 幂等创建；ik 双分析器真实 _analyze 断言。
	es := newDialogueESClient(cfg.ESURL, cfg.ESUsername, cfg.ESPassword, cfg.ESIndex)
	if err := es.EnsureIndex(parent); err != nil {
		return nil, fmt.Errorf("ES 词法索引预检: %w", err)
	}
	if err := dialoguePrecheckAnalyzers(es); err != nil {
		return nil, fmt.Errorf("ES 分词预检: %w", err)
	}
	log.Printf("dialogue retrieval: ES index=%s 预检通过（ik_max_word/ik_smart）", cfg.ESIndex)

	// embedding 预检：最小真实调用，锁维度指纹（doc/84 §6.2「以真实响应为准」）。
	embedder := newDialogueEmbedder(cfg.Embedding)
	dim, err := embedder.ProbeDim(parent)
	if err != nil {
		return nil, fmt.Errorf("embedding 维度预检: %w", err)
	}
	log.Printf("dialogue retrieval: embedding model=%s dim=%d", cfg.Embedding.Model, dim)

	// Milvus 预检：describe collection 比对维度（不存在则按指纹创建）。
	mil, err := newDialogueMilvusClient(cfg.MilvusURI, cfg.MilvusToken, cfg.MilvusDB, cfg.MilvusCollection, cfg.MilvusUseTLS, dim)
	if err != nil {
		return nil, fmt.Errorf("Milvus 连接: %w", err)
	}
	if err := mil.EnsureCollection(parent); err != nil {
		mil.Close()
		return nil, fmt.Errorf("Milvus collection 预检: %w", err)
	}
	log.Printf("dialogue retrieval: Milvus collection=%s dim=%d 预检通过", cfg.MilvusCollection, dim)

	// 指针播种：登记的代次真源（幂等；首次 version=1）。
	if _, err := store.EnsureDialogueRetrievalPointer(parent, cfg.ESIndex, cfg.MilvusCollection); err != nil {
		mil.Close()
		return nil, fmt.Errorf("对话检索指针: %w", err)
	}

	// 队列三实例：relay 一份（producer），两个消费者组各一份（producer+consumer）。
	// topic 探测在构造内完成（broker 不可达/认证失败/topic 不存在 = 启动失败）。
	qcfg := dialogueQueueConfigFromEnv("")
	relayQ, err := newDialogueRedpandaQueue(qcfg)
	if err != nil {
		mil.Close()
		return nil, fmt.Errorf("dialogue relay 队列: %w", err)
	}
	esQ, err := newDialogueRedpandaQueue(dialogueQueueConfigFromEnv(dialogueESGroup))
	if err != nil {
		relayQ.Close()
		mil.Close()
		return nil, fmt.Errorf("dialogue ES 消费者队列: %w", err)
	}
	vecQ, err := newDialogueRedpandaQueue(dialogueQueueConfigFromEnv(dialogueVectorGroup))
	if err != nil {
		relayQ.Close()
		esQ.Close()
		mil.Close()
		return nil, fmt.Errorf("dialogue 向量消费者队列: %w", err)
	}

	// 预检全过、队列就绪：查询面运行时先挂上（Recall 线二与 dev 诊断端点从这里
	// 拿词法/向量/embedding 与 lag 口），再登记 hybrid 开关——开关翻开时查询面已在位。
	store.attachDialogueRuntime(&dialogueHybridRuntime{
		cfg:     cfg,
		es:      es,
		mil:     mil,
		embed:   embedder,
		esQ:     esQ,
		vecQ:    vecQ,
		esIndex: cfg.ESIndex,
		milColl: cfg.MilvusCollection,
	})
	// 登记口从此知道 hybrid 与 embedding 模型
	// （indexDialogueRun / DeleteWorkspaceCascade / SetSessionWorkspace 据此写 outbox）。
	store.ConfigureDialogueRetrieval(true, cfg.Embedding.Model)

	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	relay := newDialogueOutboxRelay(store, relayQ)
	esCons := newDialogueConsumer(store, esQ, newDialogueESProcessor(store, es))
	milCons := newDialogueConsumer(store, vecQ, newDialogueMilvusProcessor(store, mil, embedder, counter, cfg.Embedding))
	reconciler := newDialogueReconciler(store, es, mil, esQ, vecQ, cfg.ReconcileInterval)
	wg.Add(6)
	go func() {
		defer wg.Done()
		relay.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		if cerr := esCons.Run(ctx); cerr != nil && ctx.Err() == nil {
			log.Printf("dialogue consumer(%s): 退出：%v", DialogueTargetES, cerr)
		}
	}()
	go func() {
		defer wg.Done()
		esCons.reconcileLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		if cerr := milCons.Run(ctx); cerr != nil && ctx.Err() == nil {
			log.Printf("dialogue consumer(%s): 退出：%v", DialogueTargetMilvus, cerr)
		}
	}()
	go func() {
		defer wg.Done()
		milCons.reconcileLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		reconciler.Run(ctx)
	}()
	log.Printf("dialogue retrieval: hybrid 已启动（relay + %s/%s 两组消费者 + 覆盖对账器（间隔 %s），topic=%s）",
		dialogueESGroup, dialogueVectorGroup, cfg.ReconcileInterval, dialogueIndexTopic)
	stop = func() {
		// 先摘查询面：此后 Recall 的 hybrid 分支按 fail closed 处理（不注入、不退回
		// pg_search），在途调用拿旧客户端报错也只是「本次不注入」。
		store.attachDialogueRuntime(nil)
		cancel()
		relayQ.Close()
		esQ.Close()
		vecQ.Close()
		mil.Close()
		wg.Wait()
	}
	return stop, nil
}
