// 本文件：进程内的模型客户端缓存 ModelRegistry（19v6-05 §4）——按 ref 懒加载并缓存的子代理模型
// HTTP 客户端句柄表。它缓存的是建好的客户端句柄，不是规格登记表：多副本各持一份、进程重启即
// 空表重填，不共享不持久化（判断依据 = 19v6-05 §4 的四物对比表：数据放共享存储、连接句柄放进程内）。
package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"strings"
	"sync"
)

// ModelRegistry 是懒加载缓存表：key = ref（trim 后原文），value = 建好的成品。
// 启动时为空——写了 20 条规格只用到 3 个 ref，就只建 3 个客户端（「晚解析」）。
type ModelRegistry struct {
	mu       sync.Mutex
	env      Env
	cache    map[string]*ChatModel
	forCache map[string]BuiltModel
}

// BuiltModel 是按调用方给的环境建成的客户端。KeyFP 是密钥的短指纹，日志和验收只看它，不看密钥本身。
type BuiltModel struct {
	Model    *ChatModel
	Provider string
	ModelID  string
	KeyFP    string
}

// NewModelRegistry 建一张空表。env 是进程环境快照（API Key / BaseURL 都从这里读，
// 与主 worker 的 TAKO_MODEL 走的是同一份环境）：main 在 MustOpen 之后调一次。
func NewModelRegistry(env Env) *ModelRegistry {
	return &ModelRegistry{env: env, cache: map[string]*ChatModel{}}
}

// Get 按 ref 取模型实例：表中已有 → 直接复用；没有 → 走与 TAKO_MODEL 同一条解析链
// （ResolveModelWithRef → build）建好入表，同 ref 再来直接拿同一实例。
// 解析失败 / 建客户端失败**不缓存**——失败不是可缓存的状态，下次 Get 重新解析重新报错。
// 每次解析打一行日志（新建 / 缓存命中），验收可观测（T3-1b 验收③）。
func (r *ModelRegistry) Get(ctx context.Context, ref string) (*ChatModel, error) {
	ref = strings.TrimSpace(ref)
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.cache[ref]; ok {
		log.Printf("modelregistry: %s: 缓存命中", ref)
		return m, nil
	}
	resolved, err := ResolveModelWithRef(r.env, ref)
	if err != nil {
		return nil, err
	}
	chat, err := build(ctx, resolved)
	if err != nil {
		return nil, err
	}
	m := &ChatModel{Chat: chat, Protocol: resolved.Provider.Protocol, Ref: resolved.Ref}
	r.cache[ref] = m
	log.Printf("modelregistry: %s: 新建", ref)
	return m, nil
}

// GetFor 用调用方给的环境建客户端，缓存键由调用方决定。
// 租户 run 的键必须带 tenant 和配置版本；这里不读进程里的 API Key。
// 解析失败不入缓存。日志只留 provider、model、ref 和密钥指纹。
func (r *ModelRegistry) GetFor(ctx context.Context, cacheKey, ref string, env Env) (BuiltModel, error) {
	ref = strings.TrimSpace(ref)
	if cacheKey == "" {
		cacheKey = ref
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forCache == nil {
		r.forCache = map[string]BuiltModel{}
	}
	if m, ok := r.forCache[cacheKey]; ok {
		log.Printf("modelregistry: tenant cache hit ref=%s provider=%s keyfp=%s", ref, m.Provider, m.KeyFP)
		return m, nil
	}
	resolved, err := ResolveModelWithRef(env, ref)
	if err != nil {
		return BuiltModel{}, err
	}
	chat, err := build(ctx, resolved)
	if err != nil {
		return BuiltModel{}, err
	}
	built := BuiltModel{
		Model:    &ChatModel{Chat: chat, Protocol: resolved.Provider.Protocol, Ref: resolved.Ref},
		Provider: resolved.Provider.ID,
		ModelID:  resolved.ModelID,
		KeyFP:    keyFingerprint(resolved.Provider.APIKey),
	}
	r.forCache[cacheKey] = built
	log.Printf("modelregistry: tenant new ref=%s provider=%s model=%s keyfp=%s", built.Model.Ref, built.Provider, built.ModelID, built.KeyFP)
	return built, nil
}

func keyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// PairsToEnv 把 KEY=VALUE 列表收成解析器要的 map。后出现的同名键盖住前面的。
func PairsToEnv(pairs []string) Env {
	env := Env{}
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if ok {
			env[k] = v
		}
	}
	return env
}
