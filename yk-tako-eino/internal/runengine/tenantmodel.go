package runengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/model"
)

// TenantBuilt 是一次租户模型解析的成品。KeyFP 是密钥指纹，不是密钥。
type TenantBuilt struct {
	Chat     model.BaseChatModel
	Ref      string
	Protocol string
	Provider string
	ModelID  string
	KeyFP    string
}

// TenantModelSource 按调用方给的环境和缓存键建客户端。实现在 config.ModelRegistry，main 做适配。
type TenantModelSource interface {
	Build(ctx context.Context, cacheKey, ref string, env []string) (TenantBuilt, error)
}

// TenantModels 是一场租户 run、子代理、记忆抽取、摘要共用的解析口。
// 平台进程里的 *_API_KEY 不会进它造出来的环境。
type TenantModels struct {
	source   TenantModelSource
	platform []string
	rootRef  string
}

func NewTenantModels(source TenantModelSource, platform []string, rootRef string) *TenantModels {
	return &TenantModels{source: source, platform: append([]string{}, platform...), rootRef: rootRef}
}

func (m *TenantModels) RootRef() string {
	if m == nil {
		return ""
	}
	return m.rootRef
}

// Resolve 用租户解开的 secrets 盖住平台环境里的密钥，再按 tenant + ref + 配置版本取客户端。
func (m *TenantModels) Resolve(ctx context.Context, tenantID, ref, configRaw string, secrets map[string]string) (TenantBuilt, string, error) {
	ref = strings.TrimSpace(ref)
	ver := configVersion(configRaw)
	cacheKey := tenantID + "\x00" + ref + "\x00" + ver
	built, err := m.source.Build(ctx, cacheKey, ref, tenantEnvPairs(m.platform, secrets))
	if err != nil {
		return TenantBuilt{}, "", err
	}
	sum := sha256.Sum256([]byte(cacheKey))
	return built, hex.EncodeToString(sum[:]), nil
}

func configVersion(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func isAPIKeyEnv(name string) bool {
	return strings.HasSuffix(strings.ToUpper(name), "_API_KEY")
}

// tenantEnvPairs 丢掉平台进程里的全部 *_API_KEY，再叠上租户自己的 secrets。
// 平台的 base URL、模型目录这类非密钥配置留着。租户没给的密钥不会从进程环境补上。
func tenantEnvPairs(platform []string, secrets map[string]string) []string {
	out := make([]string, 0, len(platform)+len(secrets))
	for _, pair := range platform {
		k, _, ok := strings.Cut(pair, "=")
		if !ok || isAPIKeyEnv(k) {
			continue
		}
		out = append(out, pair)
	}
	extras := make([]string, 0, len(secrets))
	for k, v := range secrets {
		if strings.TrimSpace(k) == "" || v == "" {
			continue
		}
		extras = append(extras, k+"="+v)
	}
	sort.Strings(extras)
	return append(out, extras...)
}

func unsealSecretMap(in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range in {
		plain, err := unsealSecret(v)
		if err != nil {
			return nil, err
		}
		if plain != "" {
			out[k] = plain
		}
	}
	return out, nil
}

func effectiveModelRef(agentRef, tenantRef, rootRef string) string {
	if s := strings.TrimSpace(agentRef); s != "" && !isInheritRef(s) {
		return s
	}
	if s := strings.TrimSpace(tenantRef); s != "" && !isInheritRef(s) {
		return s
	}
	return strings.TrimSpace(rootRef)
}
