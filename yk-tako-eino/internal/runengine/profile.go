package runengine

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
)

// RunProfile 是一场 run 在抢闸前定下来的装配结果。newRunAgent 只消费它，不再回退到进程里的模型和密钥。
type RunProfile struct {
	mu          sync.Mutex
	TenantID    string
	UserID      string
	AgentID     string
	ModelRef    string
	Model       model.BaseChatModel
	Translator  Translator
	Instruction string
	Tools       []tool.BaseTool
	// SkillPolicy 是这只 manager 的技能作用域（E1）：目录按它过滤，worker.Run 塞进 ctx
	// 供三个技能工具在解析时判。零值 = inherit（不写就是全量）。
	SkillPolicy SkillPolicy
	ConfigVer   string
	KeyFP       string
	// Provider 是目录里的厂商 id。图片 token 目前只有 deepseek 有计算器。
	Provider string
	// Vision 是这场 root 模型在目录里的收图标记。mediaRestoreHandler 每次调用读它。
	Vision   bool
	Resolver ModelResolver
	conns    []*mcpServerConn
}

func (p *RunProfile) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		if c != nil && c.session != nil {
			_ = c.session.Close()
		}
	}
	p.conns = nil
}

type runProfileKey struct{}

func withRunProfile(ctx context.Context, p *RunProfile) context.Context {
	return context.WithValue(ctx, runProfileKey{}, p)
}

func profileFrom(ctx context.Context) *RunProfile {
	p, _ := ctx.Value(runProfileKey{}).(*RunProfile)
	return p
}

type assembleError struct {
	Status  int
	Code    int
	Message string
	Err     error
}

func (e *assembleError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Message
}

func failAssemble(status, code int, message string, err error) *assembleError {
	return &assembleError{Status: status, Code: code, Message: message, Err: err}
}

// tenantResolver 让子代理的显式 model ref 走同一份租户环境，不走进程注册表。
type tenantResolver struct {
	models   *TenantModels
	tenantID string
	raw      string
	secrets  map[string]string
}

func (r tenantResolver) Get(ctx context.Context, ref string) (ModelInstance, error) {
	built, _, err := r.models.Resolve(ctx, r.tenantID, ref, r.raw, r.secrets)
	if err != nil {
		return ModelInstance{}, err
	}
	return ModelInstance{Chat: built.Chat, Ref: built.Ref, Protocol: built.Protocol}, nil
}

// assembleRun 在 AcquireRun 之前把模型、工具和 MCP 定下来。
// 失败时调用方还没建 run。连上的 MCP 由返回的 profile 持有，抢闸失败时调用方关掉。
func (s *Server) assembleRun(ctx context.Context, p principal, agentID string) (*RunProfile, *assembleError) {
	if s.worker == nil || s.worker.models == nil {
		return nil, failAssemble(http.StatusConflict, codeModelUnavailable, "这个工作区的模型还不可用", errors.New("tenant models not configured"))
	}
	var agent EffectiveAgent
	if agentID != "" {
		row, err := s.store.ResolveRunAgent(ctx, p.TenantID, p.UserID, agentID)
		if err != nil {
			if errors.Is(err, ErrAgentNotFound) {
				return nil, failAssemble(http.StatusNotFound, codeAgentMissing, "找不到这个助手", err)
			}
			return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		}
		if row.Kind == "subagent" {
			return nil, failAssemble(http.StatusNotFound, codeAgentMissing, "找不到这个助手", ErrAgentNotFound)
		}
		agent = row
	}
	cfg, raw, err := s.store.RuntimeConfigRecord(ctx, p.TenantID)
	if err != nil {
		return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
	}
	secrets, err := unsealSecretMap(cfg.Secrets)
	if err != nil {
		log.Printf("assemble unseal tenant=%s: %v", p.TenantID, err)
		return nil, failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
	}
	ref := effectiveModelRef(agent.ModelRef, cfg.ModelRef, s.worker.models.RootRef())
	built, cacheKey, err := s.worker.models.Resolve(ctx, p.TenantID, ref, raw, secrets)
	if err != nil {
		log.Printf("assemble model tenant=%s ref=%s: %v", p.TenantID, ref, err)
		return nil, failAssemble(http.StatusConflict, codeModelUnavailable, "这个工作区的模型还不可用", err)
	}
	log.Printf("assemble model tenant=%s ref=%s provider=%s keyfp=%s cache=%s", p.TenantID, built.Ref, built.Provider, built.KeyFP, cacheKey)

	pool := append([]tool.BaseTool{}, s.worker.tools...)
	conns, mcpTools, aerr := s.connectRunMCP(ctx, p, cfg, secrets)
	if aerr != nil {
		closeMCP(conns)
		return nil, aerr
	}
	pool = append(pool, mcpTools...)

	filtered, aerr := filterByPolicy(ctx, pool, parseToolPolicy(agent.ToolPolicy), "根助手")
	if aerr != nil {
		closeMCP(conns)
		return nil, aerr
	}
	if aerr := s.checkSubagentBounds(ctx, p, filtered, pool, secrets, raw); aerr != nil {
		closeMCP(conns)
		return nil, aerr
	}

	instruction := s.worker.system
	if agent.ID != "" {
		instruction = agent.Spec
	}
	// 技能目录拼在 manager 的 Instruction 之后（doc/52 §2.2）：只 manager 有——
	// 子代理的 Instruction 来自规格文件，装配在 agents.go，不经这里。
	// 读库失败只记日志、不拦 run：目录是「看得见有哪些技能」，少它不该让整场跑不起来
	// （与记忆召回同一条线：横切支撑不是主流程硬依赖）。
	skillPolicy := parseSkillPolicy(agent.SkillPolicy)
	if catalog := s.skillCatalogBlock(ctx, p.TenantID, skillPolicy); catalog != "" {
		instruction = instruction + "\n\n" + catalog
	}
	prof := &RunProfile{
		TenantID:    p.TenantID,
		UserID:      p.UserID,
		AgentID:     agent.ID,
		ModelRef:    built.Ref,
		Model:       built.Chat,
		Translator:  TranslatorFor(built.Protocol),
		Instruction: instruction,
		Tools:       filtered,
		SkillPolicy: skillPolicy,
		ConfigVer:   configVersion(raw),
		KeyFP:       built.KeyFP,
		Provider:    built.Provider,
		Vision:      built.Vision,
		Resolver: tenantResolver{
			models: s.worker.models, tenantID: p.TenantID, raw: raw, secrets: secrets,
		},
		conns: conns,
	}
	return prof, nil
}

func closeMCP(conns []*mcpServerConn) {
	for _, c := range conns {
		if c != nil && c.session != nil {
			_ = c.session.Close()
		}
	}
}

// connectRunMCP 把系统 mcp.json 和租户 mcpServers 合成一份。同名时租户盖住系统，盖住的那条算自定义。
// 自定义连不上就整场失败；系统连不上就少它的工具，run 还能继续。
func (s *Server) connectRunMCP(ctx context.Context, p principal, cfg runtimeConfig, secrets map[string]string) ([]*mcpServerConn, []tool.BaseTool, *assembleError) {
	sys, err := loadMcpConfig(s.mcpConfPath)
	if err != nil {
		log.Printf("mcp: 系统配置拒载，本场没有系统 MCP: %v", err)
		sys = McpConfig{}
	}
	tenantServers, err := materializeServers(cfg.McpServers, secrets)
	if err != nil {
		return nil, nil, failAssemble(http.StatusBadRequest, codeMcpIllegal, err.Error(), err)
	}
	connect := s.mcpConnect
	if connect == nil {
		connect = connectMcpServer
	}
	var conns []*mcpServerConn
	var tools []tool.BaseTool
	tenantRoot := tenantStorageRoot(s.mediaRoot, p.TenantID)
	for _, srv := range mergeServers(sys.McpServers, tenantServers) {
		conn, cerr := connect(ctx, srv.name, srv.conf, mcpFenceRoot(srv, tenantRoot))
		if cerr != nil {
			if srv.custom {
				log.Printf("mcp: custom %s 启动失败: %v", srv.name, cerr)
				return conns, nil, failAssemble(http.StatusServiceUnavailable, codeMcpDown, "自定义 MCP 连不上", cerr)
			}
			log.Printf("mcp: system %s 启动失败，本场降级: %v", srv.name, cerr)
			continue
		}
		conns = append(conns, conn)
		for _, t := range conn.tools {
			tools = append(tools, wrapToolErrors(t))
		}
		log.Printf("mcp: connected %s custom=%v tools=%d", srv.name, srv.custom, len(conn.toolNames))
	}
	return conns, tools, nil
}

type mergedServer struct {
	name   string
	conf   McpServerConfig
	custom bool
}

func mergeServers(system, tenant map[string]McpServerConfig) []mergedServer {
	names := map[string]struct{}{}
	for n := range system {
		names[n] = struct{}{}
	}
	for n := range tenant {
		names[n] = struct{}{}
	}
	order := make([]string, 0, len(names))
	for n := range names {
		order = append(order, n)
	}
	sort.Strings(order)
	out := make([]mergedServer, 0, len(order))
	for _, n := range order {
		if sc, ok := tenant[n]; ok {
			out = append(out, mergedServer{name: n, conf: sc, custom: true})
			continue
		}
		out = append(out, mergedServer{name: n, conf: system[n], custom: false})
	}
	return out
}

func materializeServers(servers map[string]McpServerConfig, secrets map[string]string) (map[string]McpServerConfig, error) {
	out := make(map[string]McpServerConfig, len(servers))
	for name, sc := range servers {
		env, err := materializeMcpEnv(sc.Env, secrets)
		if err != nil {
			return nil, err
		}
		sc.Env = env
		if sc.Args == nil {
			sc.Args = []string{}
		}
		out[name] = sc
	}
	return out, nil
}

func materializeMcpEnv(env, secrets map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		switch {
		case strings.HasPrefix(v, "secret://"):
			key := strings.TrimPrefix(v, "secret://")
			plain := secrets[key]
			if strings.TrimSpace(key) == "" || plain == "" {
				return nil, errors.New("有一项 MCP 密钥还没配")
			}
			out[k] = plain
		case isSealedValue(v):
			return nil, errors.New("MCP 环境不能直接放密文")
		default:
			out[k] = v
		}
	}
	return out, nil
}

func filterByPolicy(ctx context.Context, tools []tool.BaseTool, policy ToolPolicy, who string) ([]tool.BaseTool, *assembleError) {
	mode := policy.Mode
	if mode == "" {
		mode = "inherit"
	}
	names := policy.Names
	switch mode {
	case "inherit":
		if len(names) > 0 {
			return nil, failAssemble(http.StatusBadRequest, codeUnknownTool, "工具策略里有不认识的工具", errors.New(who+" inherit with names"))
		}
		return append([]tool.BaseTool{}, tools...), nil
	case "none":
		if len(names) > 0 {
			return nil, failAssemble(http.StatusBadRequest, codeUnknownTool, "工具策略里有不认识的工具", errors.New(who+" none with names"))
		}
		return []tool.BaseTool{}, nil
	case "allow", "exclude":
	default:
		return nil, failAssemble(http.StatusBadRequest, codeBadInput, "工具策略不合法", errors.New(who+" bad mode"))
	}
	byName, known := poolIndex(ctx, tools)
	for _, name := range names {
		if _, ok := byName[name]; !ok {
			log.Printf("assemble tool %s: %s 不在有效池（%s）", who, name, strings.Join(known, ","))
			return nil, failAssemble(http.StatusBadRequest, codeUnknownTool, "工具策略里有不认识的工具", errors.New(name))
		}
	}
	if mode == "allow" {
		want := map[string]bool{}
		for _, name := range names {
			want[name] = true
		}
		out := make([]tool.BaseTool, 0, len(names))
		seen := map[string]bool{}
		for _, t := range tools {
			info, err := t.Info(ctx)
			if err != nil || info == nil || !want[info.Name] || seen[info.Name] {
				continue
			}
			seen[info.Name] = true
			out = append(out, t)
		}
		return out, nil
	}
	skip := map[string]bool{}
	for _, name := range names {
		skip[name] = true
	}
	out := make([]tool.BaseTool, 0, len(tools))
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil || info == nil || skip[info.Name] {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// checkSubagentBounds 用根助手过滤后的池当上界。
// 名字不在过滤前的有效池里：400，不建 run。这是写错了的工具名。
// 租户自己写的 allow 点了根池里没有的工具：400。默认子代理不挡这场 run，装载时按根池拒掉那一条。
func (s *Server) checkSubagentBounds(ctx context.Context, p principal, rootTools, fullTools []tool.BaseTool, secrets map[string]string, raw string) *assembleError {
	rows, err := s.store.ListEffectiveAgents(ctx, p.TenantID, p.UserID, "subagent")
	if err != nil {
		return failAssemble(http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
	}
	_, fullNames := poolIndex(ctx, fullTools)
	full := map[string]bool{}
	for _, name := range fullNames {
		full[name] = true
	}
	_, rootNames := poolIndex(ctx, rootTools)
	root := map[string]bool{}
	for _, name := range rootNames {
		root[name] = true
	}
	for _, row := range rows {
		if ref := strings.TrimSpace(row.ModelRef); ref != "" && !isInheritRef(ref) {
			if _, _, err := s.worker.models.Resolve(ctx, p.TenantID, ref, raw, secrets); err != nil {
				log.Printf("assemble subagent %s model %s: %v", row.Name, ref, err)
				return failAssemble(http.StatusConflict, codeModelUnavailable, "这个工作区的模型还不可用", err)
			}
		}
		policy := parseToolPolicy(row.ToolPolicy)
		switch policy.Mode {
		case "allow":
			for _, name := range policy.Names {
				// tool_search 是保留名：装配期池里没有它（由子代理自己的 toolSearchHandler
				// 在 BeforeAgent 末尾装入），不能按普通工具名拒绝。
				if name == toolSearchToolName {
					continue
				}
				if !full[name] {
					log.Printf("assemble subagent %s allow unknown %s", row.Name, name)
					return failAssemble(http.StatusBadRequest, codeUnknownTool, "工具策略里有不认识的工具", errors.New(name))
				}
				if !root[name] && row.Source != "default" {
					log.Printf("assemble subagent %s allow exceeds root %s", row.Name, name)
					return failAssemble(http.StatusBadRequest, codeUnknownTool, "子代理不能使用根助手没有的工具", errors.New(name))
				}
			}
		case "exclude":
			for _, name := range policy.Names {
				if name == toolSearchToolName {
					continue // 同上：保留名不在装配期池里；本档也不自动给 tool_search
				}
				if !full[name] {
					log.Printf("assemble subagent %s exclude unknown %s", row.Name, name)
					return failAssemble(http.StatusBadRequest, codeUnknownTool, "工具策略里有不认识的工具", errors.New(name))
				}
			}
		}
	}
	return nil
}
