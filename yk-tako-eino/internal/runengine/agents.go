// 本文件实现文件式子代理规格：agents/ 单目录，每个 *.md 文件定义一条子代理规格
// （YAML frontmatter 四字段 + 正文 = system prompt）。
//
// 装载时点 = run 起手的扫盘（BeforeAgent 每 Run 调一次）：改文件 → 下一条消息生效；run 中途不变。
// 装载路线 = BeforeAgent 换列表（不重建主 agent）：handler 逐条规格解析装配输入（工具池 / 模型 /
// 快照段）缓存进 subagentShell（静态壳，Info 直接给声明）后 append 进 runCtx.Tools；ADK 随后把
// 该列表重建进 toolInfos（声明面 model.WithTools）与 toolsNodeConf（执行面 WithToolList）。
// eino 实例（NewChatModelAgent）不在起手建：模型真正调用委派时才装配、每次委派新建即弃
// （动机与语义见 subagentShell 注释——#1177 保险，装载/观测面全部维持 run 级）。
// runCtx.Tools 每 run 从冻结基座克隆（chatmodel.go applyBeforeAgent），append 不跨 run 累积。
//
// T3-1b：model: 行按 ref 生效（19v6-05 §4 三种写法）——不写 / default|inherit|继承 = 继承主实例，
// 不进 ModelRegistry；其余 = ref 档，走 ModelResolver（进程内懒加载缓存表，实现归 config.ModelRegistry）。
// 装配同时产出「agent 名 → {生效 ref, translator}」表（agentModelTable，ctx 携带）：
// worker 写帧时查它取 model 值、drain 时按它选 translator。
//
// T3-3：委派工具出池前过 delegationTool 包壳——并发闸（sem，容量 = -subagent-concurrency）+
// 回传裁剪（capBytes = -subagent-return-cap）：模型侧回传有界、审计侧全量仍在子代理 runlog。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"gopkg.in/yaml.v3"
)

// AgentSpec 是一条子代理规格。Name 是唯一身份
// （= AgentTool 工具名 = runlog 的 agent 值；文件名不参与语义）。
// 工具面四态（操作定义真源 = doc/26 §3，本注释只给判据）：Tools 三态靠指针区分——
// nil = 缺席（继承父池）/ 指向空切片 = 空集 / 指向非空 = 白名单按名映射；第四态 =
// Tools 为 nil 且 ExcludeTools 非 nil = 继承父池减去列出的名字。两行同写在 parse 层拒载。
type AgentSpec struct {
	Name         string
	Description  string
	Tools        *[]string
	ExcludeTools *[]string // 继承父池减去这些名字（doc/26）；与 Tools 互斥
	Model        string    // frontmatter model: 行原文——parse 只认语法，写法分流在装配层（19v6-05 §4）
	Prompt       string    // 正文 = system prompt
}

// agentNamePattern 是助手名的唯一合法形状。名字不只是标签：它同时是 AgentTool 的工具名、
// runlog 的 agent 值、厂商请求里 tools[i].function.name——厂商只收 [A-Za-z0-9_-]，
// 一个空格就让这一租户装配出的每次请求被 400 打回（2026-10-23 走查的 P0）。
var agentNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// errBadAgentName 是「名字形状不合法」的哨兵：导入把解析错误统一说成「格式不对」，
// 只有这一种要翻成点名规则的文案。
var errBadAgentName = errors.New("bad agent name")

const agentNameRuleMessage = "助手名字只能用字母、数字、下划线、连字符（1–64 个字符）"

func validAgentName(name string) bool { return agentNamePattern.MatchString(name) }

// agentSpecFront 是 frontmatter 的 YAML 形状。Model 原样带出：写了/没写、是关键词还是 ref，
// 都在装配层分流——parse 层不做语义判断（19v6-05 §4）。
type agentSpecFront struct {
	Name         string    `yaml:"name"`
	Description  string    `yaml:"description"`
	Tools        *[]string `yaml:"tools"`
	ExcludeTools *[]string `yaml:"exclude_tools"`
	Model        string    `yaml:"model"`
}

// LoadAgentSpecs 扫 dir 下的 *.md（按文件名排序）逐个解析校验。拒载是逐条语义：
// 坏文件 log 一行、其余照载——单个规格绝不拖垮 run。目录不存在 = 0 规格（合法态：
// 移走文件/删目录 = 全部撤销委派，下一条消息生效）；只有目录本身读不了才返回 err，由调用方降级。
func LoadAgentSpecs(dir string) ([]AgentSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	var specs []AgentSpec
	seen := map[string]string{} // name → 来源文件（撞名报错指得出两处）
	for _, fn := range files {
		raw, err := os.ReadFile(filepath.Join(dir, fn))
		if err != nil {
			log.Printf("agents: %s: 拒载: %v", fn, err)
			continue
		}
		spec, err := parseAgentSpec(raw)
		if err != nil {
			log.Printf("agents: %s: 拒载: %v", fn, err)
			continue
		}
		if prev, dup := seen[spec.Name]; dup {
			log.Printf("agents: %s: 拒载: name %q 与 %s 撞名（文件名序在后者拒）", fn, spec.Name, prev)
			continue
		}
		seen[spec.Name] = fn
		specs = append(specs, spec)
	}
	return specs, nil
}

// parseAgentSpec 拆 frontmatter 与正文：文件须以独立一行 --- 开头，下一个独立一行 --- 收口；
// 中间是 YAML、之后是正文。KnownFields(true)：未知字段拒载——字段表是拍定的五字段
// （doc/26 加 exclude_tools 之前是四字段），拼写手误（如 tool:）没有「悄悄忽略」的通道。
func parseAgentSpec(raw []byte) (AgentSpec, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return AgentSpec{}, fmt.Errorf("缺 frontmatter（文件须以独立一行 --- 开头）")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return AgentSpec{}, fmt.Errorf("frontmatter 未闭合（缺收口的独立一行 ---）")
	}
	frontRaw := strings.TrimSpace(strings.Join(lines[1:end], "\n"))
	if frontRaw == "" {
		return AgentSpec{}, fmt.Errorf("frontmatter 为空")
	}
	var f agentSpecFront
	dec := yaml.NewDecoder(strings.NewReader(frontRaw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return AgentSpec{}, fmt.Errorf("frontmatter YAML 解析失败: %w", err)
	}
	f.Name = strings.TrimSpace(f.Name)
	f.Description = strings.TrimSpace(f.Description)
	if f.Name == "" {
		return AgentSpec{}, fmt.Errorf("缺必填字段 name")
	}
	if !validAgentName(f.Name) {
		return AgentSpec{}, fmt.Errorf("%w：%s", errBadAgentName, agentNameRuleMessage)
	}
	if f.Description == "" {
		return AgentSpec{}, fmt.Errorf("缺必填字段 description（模型决定委派与否的唯一入口）")
	}
	// 互斥在 parse 层判、不留给 resolveToolPool（doc/26 §3）：两行同写时「实际名单取决于代码里
	// 两个分支的先后」，改一次判断顺序就换一份能力面——那是护栏最不该有的性质。
	if f.Tools != nil && f.ExcludeTools != nil {
		return AgentSpec{}, fmt.Errorf("tools: 与 exclude_tools: 不能同时写（前者是「只给这些」，后者是「继承父池减去这些」）——二选一")
	}
	return AgentSpec{
		Name:         f.Name,
		Description:  f.Description,
		Tools:        f.Tools,
		ExcludeTools: f.ExcludeTools,
		Model:        f.Model,
		Prompt:       strings.TrimSpace(strings.Join(lines[end+1:], "\n")),
	}, nil
}

// ModelInstance 是解析器按 ref 交出的成品（T3-1b）：Chat = 客户端实例（子代理 loop 用它发请求）、
// Ref = 解析产物的规范 ref（帧/行的 model 值取它）、Protocol = 协议名（drain 按它选 translator——
// 子代理模型可能与主 worker 不同家）。
type ModelInstance struct {
	Chat     model.BaseChatModel
	Ref      string
	Protocol string
}

// ModelResolver 按 ref 解析模型实例。实现归 config.ModelRegistry（进程内懒加载缓存表，19v6-05 §4）；
// 接口定义在 runengine 侧、由 main 装配时适配——runengine 不 import config。
type ModelResolver interface {
	Get(ctx context.Context, ref string) (ModelInstance, error)
}

// isInheritRef 认「显式继承」的三种拼写（19v6-05 §4 写法二）：default / inherit / 继承；
// 比较前先 trim、ASCII 两词大小写不敏感（「继承」无大小写、原样比）。
func isInheritRef(ref string) bool {
	switch strings.ToLower(ref) {
	case "default", "inherit", "继承":
		return true
	}
	return false
}

// agentModelEntry 是一个 agent 的模型身份（T3-1b）。ref = 生效 ref（帧/行 model 字段的值）；
// translate = 该 agent 协议对应的 chunk 翻译器——drain 按它翻译该 agent 的输出流：
// 厂商差异全在 translator 层，子代理与主 worker 不同家时各用各的（现三家翻译器同读 Content，
// 某家字段一旦分叉、只改对应那一个）。
type agentModelEntry struct {
	ref       string
	translate Translator
}

// agentModelTable 是本 run 的「agent 名 → 模型身份」表：BeforeAgent 扫盘装配时逐个写入装载成功的
// 规格；root（""）条目由 worker 建表时预置（主 ref + 主 translator）。worker 写帧 / drain 时按
// event.AgentName 查。传递形态 = ctx 携带：填与读同 goroutine——agent.Run 内 getRunFunc →
// applyBeforeAgent 是**同步**先跑的（eino v0.9.21 chatmodel.go），事件流出时表已齐；锁只是兜底，
// 防 eino 后续版本把装载挪出同步路径。
type agentModelTable struct {
	mu      sync.RWMutex
	entries map[string]agentModelEntry
}

func newAgentModelTable(rootRef string, rootTranslate Translator) *agentModelTable {
	return &agentModelTable{entries: map[string]agentModelEntry{"": {ref: rootRef, translate: rootTranslate}}}
}

func (t *agentModelTable) set(name string, e agentModelEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[name] = e
}

// entryFor 按 agent 名取条目（"" = root）；查不到回落 root 条目——子代理事件只可能来自装载成功的
// 规格（表条目与 AgentTool 同点写入），查不到即装配异常：translator 回落保帧不炸（异常态本身
// 仍可见于装载日志——该 agent 名不在 loaded 名单里）。
func (t *agentModelTable) entryFor(agent string) agentModelEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if e, ok := t.entries[agent]; ok {
		return e
	}
	return t.entries[""]
}

// lookupStrict 按 agent 名取生效 ref，查不到不回落（ok=false）——web_search 的严格读
// （doc/22 §4）：表坏时走 entryFor 的回落会把子代理的搜索静默打去主模型厂商。
func (t *agentModelTable) lookupStrict(agent string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[agent]
	return e.ref, ok
}

// agentModelTableKey 是 ctx 携带身份表的私有 key 类型（context key 的防撞车惯例）。
type agentModelTableKey struct{}

func withAgentModelTable(ctx context.Context, t *agentModelTable) context.Context {
	return context.WithValue(ctx, agentModelTableKey{}, t)
}

func agentModelTableFrom(ctx context.Context) *agentModelTable {
	t, _ := ctx.Value(agentModelTableKey{}).(*agentModelTable)
	return t
}

// agentSpecHandler 是装载 handler：BeforeAgent 每 Run 扫一次盘，把规格的装配输入解析进壳
// （subagentShell，实例推迟到每次委派）追加进 runCtx.Tools。
// 入口快照 base = runCtx.Tools：此刻池里只有基座工具（本 handler 是唯一往池里加委派工具的），
// 子代理继承父池时取它 = 天然禁嵌套。
// T3-3 起委派工具出池前过 delegationTool 包壳（并发闸 + 回传裁剪）——sem/returnCap 由 NewWorker 造好注入。
// M5 起规格正文后接笔记快照段（Instruction = spec.Prompt + "\n\n" + 快照段，与主代理同一份，
// 见 BeforeAgent 顶部的读一次说明）。
type agentSpecHandler struct {
	*adk.BaseChatModelAgentMiddleware
	dir       string
	store     *Store              // 笔记快照读口（M5；listLiveNotes）
	workspace string              // 笔记分区键（workspace 根 Abs）
	model     model.BaseChatModel // 主 worker 实例：写法一/二（不写 / default|inherit|继承）直接用
	modelRef  string              // 主 ref：继承档的生效 ref（帧 model 字段）
	translate Translator          // 主 translator：继承档的 drain 分发
	resolver  ModelResolver       // 写法三（具体 ref）：进程内注册表
	sem       chan struct{}       // 委派并发闸（容量 = -subagent-concurrency）：全部规格共享一个
	returnCap int                 // 回传上限（字节；<=0 = 不裁剪）：-subagent-return-cap
	// 子代理折叠链（doc/17 §6，台账 17-子代理折叠 A1）：与主 run 同一条链的四件套，
	// BeforeAgent 装配子代理时按规格名造 agentFoldHandler 挂上。
	foldCounter   TokenCounter
	foldWindow    int                                           // = compactWindow（foldInput 的 trim 预算）
	foldLimit     int                                           // = compactLimit
	foldSummarize func(context.Context, string) (string, error) // = w.sum.summarize
	foldEffects   map[string]string                             // = firstPartyFileEffects
	archiveTTL    time.Duration                                 // -archive-ttl，resume 过期与清扫器同一个数
}

// newAgentSpecHandler 组装载 handler。dir 是规格目录（相对 CWD 或绝对路径）；
// store/workspace 是笔记快照读口（M5）；m / mainRef / mainTranslate 是主 worker 的三件套
// （继承档取用）；r 是模型注册表（ref 档取用）；sem/returnCap 是 T3-3 的预算面
// （并发闸 + 回传裁剪），装配点 = NewWorker；counter/window/limit/summarize/fileEffects
// 是子代理折叠链（doc/17 §6）——全部沿用主 run 的实例，不另设旗标。
func newAgentSpecHandler(dir string, store *Store, workspace string, m model.BaseChatModel, mainRef string, mainTranslate Translator,
	r ModelResolver, sem chan struct{}, returnCap int,
	counter TokenCounter, window, limit int, summarize func(context.Context, string) (string, error), fileEffects map[string]string, archiveTTL time.Duration) *agentSpecHandler {
	return &agentSpecHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		dir: dir, store: store, workspace: workspace, model: m, modelRef: mainRef, translate: mainTranslate,
		resolver: r, sem: sem, returnCap: returnCap,
		foldCounter: counter, foldWindow: window, foldLimit: limit, foldSummarize: summarize, foldEffects: fileEffects,
		archiveTTL: archiveTTL}
}

// subagentShell 是委派工具的静态壳（doc/25 §7 翻案，2026-10-23 拍板落地）：run 起手只缓存
// 装配输入（规格 / 工具池 / 模型 / 快照段），不建 eino 实例；模型真正调用时（InvokableRun）
// 才 NewChatModelAgent + NewAgentTool，跑完即弃。装载时点、run 内冻结、拒载时机、
// agent tools / agent system 观测面全部维持 run 级不动；变的是实例生命周期——每场委派一份
// 实例 ⇒ 实例级共享可写状态（msgConf.cancelCtx，上游 #1177）不跨委派存活，将来启用 adk 取消
// 作用域也没有并发同名委派的写字竞态。形态照 resume 的 buildResumeAgent（resume_subagent.go）：
// 调用点装配的先例已在库里（resume 因 GenModelInput 按调用参数化而被迫如此，普通委派这里
// 是主动选择——两者共用 assembleSubagentTool 一个装配核）。
type subagentShell struct {
	h           *agentSpecHandler   // 装配工厂：handler 链 / 折叠链 / 模型从它拿
	spec        AgentSpec           // 规格原文（Name / Description / Prompt）
	pool        []tool.BaseTool     // run 起手定死的工具池（入口快照解析产物，run 内冻结）
	subModel    model.BaseChatModel // run 起手解析的模型
	instruction string              // = spec.Prompt + "\n\n" + 快照段（run 内冻结）
	wantSearch  bool                // 规格显式要 tool_search 才挂目录分级（2026-10-20 拍）
	toolInfo    *schema.ToolInfo    // 静态工具声明：壳不建实例，Info 不再从实例取
}

// subagentShellRequestParams 复刻 eino 的 defaultAgentToolParam（agent_tool.go 私有，壳的
// Info 不经过实例，得自己给）：委派工具唯一入参 = 必填 string request。形状漂移会改模型可见
// 的声明字节（活体验收的契约面），改它要跟 eino 版本对一遍。
func subagentShellRequestParams() *schema.ParamsOneOf {
	return schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		"request": {Desc: "request to be processed", Required: true, Type: schema.String},
	})
}

func (s *subagentShell) Info(context.Context) (*schema.ToolInfo, error) { return s.toolInfo, nil }

// InvokableRun：调用点装配（每次委派新实例）→ 内层照跑。装配失败回错误文本（与 resume 同政策），
// 壳在 delegationTool/runSubagentGuarded 内层，占闸、开档案、回传裁剪都在更外层，语义不变。
// opts 原样透传：EmitInternalEvents 的子代理事件转发与 cancel 派生都走 opts（delegationTool 注释）。
func (s *subagentShell) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	inner, errText := s.h.assembleSubagentTool(ctx, s.spec, s.subModel, s.pool, s.instruction, s.wantSearch, nil)
	if errText != "" {
		return errText, nil
	}
	return inner.InvokableRun(ctx, args, opts...)
}

// assembleSubagentTool 拿解析好的装配输入造一场委派用的子代理实例并包成可调用工具，
// 壳（每次委派）与 resume（每次续跑）共用这一个装配核。genInput 非 nil = resume 形态
// （档案历史 + 新指令作模型输入）；nil = 默认模型输入。失败回 (nil, 错误文本)——
// 调用方这时还没占闸、也还没开档案。
func (h *agentSpecHandler) assembleSubagentTool(ctx context.Context, spec AgentSpec, subModel model.BaseChatModel, pool []tool.BaseTool, instruction string, wantSearch bool, genInput adk.GenModelInput) (tool.InvokableTool, string) {
	cfg := &adk.ChatModelAgentConfig{
		Name:          spec.Name,
		Description:   spec.Description,
		Instruction:   instruction,
		Model:         subModel,
		// 子代理与主代理同一条上限（agentMaxIterations，2026-10-16 用户裁定放开到 50）。
		MaxIterations: agentMaxIterations,
		ToolsConfig:   adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: pool}},
		// handler 每次调用新造（callerStamp / toolSearch / wireAppend / fold 链上均无
		// per-delegation 字段——2026-10-16 禁令二，新造无副作用）。
		Handlers: h.subagentHandlers(spec.Name, wantSearch),
	}
	if genInput != nil {
		cfg.GenModelInput = genInput
	}
	agent, err := adk.NewChatModelAgent(ctx, cfg)
	if err != nil {
		return nil, "错误：子代理 " + spec.Name + " 当前无法装配：" + err.Error()
	}
	inner, ok := adk.NewAgentTool(ctx, agent).(tool.InvokableTool)
	if !ok {
		return nil, "错误：子代理 " + spec.Name + " 当前无法装配：内层不可调用"
	}
	return inner, ""
}

// delegationTool 是委派工具的预算包壳（T3-3）：并发闸（sem）+ 回传裁剪（capBytes），
// 套在委派壳（subagentShell）外面。它必须实现与 AgentTool 相同的调用面（Info + InvokableRun
// 带 opts），且把 ctx/args/opts 原样递给 inner——EmitInternalEvents 的子代理事件转发靠 opts 里
// 携带的 event generator（eino v0.9.21 agent_tool.go 的 getEmitGeneratorAndEnableStreaming 从
// opts 读它），扣下 opts 等于把子代理的帧全掐掉；cancel 派生（extractAndDeriveAgentToolCancelCtx）
// 同走 opts，一并原样传。
type delegationTool struct {
	inner    tool.BaseTool // *adk.typedAgentTool：Info + InvokableRun
	sem      chan struct{} // 池级并发闸：容量 = -subagent-concurrency，所有规格共享
	capBytes int           // 回传上限（字节）；<=0 = 不裁剪
	name     string        // 规格名（错误话术用）
	store    *Store        // 租户子代理槽；nil 时只走进程内 sem（单测）
}

func (d *delegationTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return d.inner.Info(ctx)
}

// InvokableRun：取闸（满则等，ctx 取消可中断——不无限吊住父循环）→ 内层照跑 → 回传超帽截断。
// 内层错误分流（A2，doc/17 §6 + planning D2）：折叠链错误（handler 的 foldChainErrorMark 前缀，
// graph 层的 [NodeRunError] 壳不影响文本匹配）与厂商溢出拒收（IsOverflowError，待拍①已裁转）
// 同属「终稿出不来」一类——转成错误文本当这次委派的工具结果，父 run 保持 complete；
// 其余错误（含手动停止的 ctx 取消）维持裸上抛。
func (d *delegationTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	inner, ok := d.inner.(tool.InvokableTool)
	if !ok {
		return "", fmt.Errorf("委派工具 %s: 内层不可调用（装配错误）", d.name)
	}
	return runSubagentGuarded(ctx, d.store, d.sem, d.capBytes, d.name, "", func(ctx context.Context) (string, error) {
		return inner.InvokableRun(ctx, args, opts...)
	})
}

// runSubagentGuarded 是委派和 resume 共用的后半段：占闸、开新档案、跑内层、截断后接实例 id、写 terminal。
// 调用方要在进这里之前做完会拒绝的检查。拒绝路径不占闸、不开档案。
// resumedFrom 空字符串是普通委派；resume 填原 sa_id。agentName 是规格名，写进新档案的 agent_name。
func runSubagentGuarded(ctx context.Context, store *Store, sem chan struct{}, capBytes int, agentName, resumedFrom string, call func(context.Context) (string, error)) (string, error) {
	// 有 Redis 时租户槽就是闸：进程级 sem 会让 A 占满本进程后，B 即使自己还有额度也在这里等。
	// 没有 Redis（单测）才用进程级 sem。
	useProcessSem := store == nil || store.rdb == nil
	if store != nil {
		ok, err := store.trySubagentSlot(ctx)
		if err != nil {
			return fmt.Sprintf("错误：子代理额度检查失败：%v", err), nil
		}
		if !ok {
			return "错误：这个工作区的子代理额度用完了", nil
		}
		defer store.releaseSubagentSlot(context.WithoutCancel(ctx))
	}
	if useProcessSem {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	// 档案在内层开跑前铸。开档失败时 wire 为 nil，回传不带实例 id，调用照旧。
	ctx, wire := beginDelegationWire(ctx, agentName, resumedFrom)
	if wire != nil {
		defer wire.close()
	}
	out, err := call(ctx)
	if err != nil {
		if strings.Contains(err.Error(), foldChainErrorMark) {
			text := withSubagentInstance(fmt.Sprintf("错误：子代理 %s 上下文折叠失败：%v", agentName, err), subagentWireID(wire))
			wire.finish("error", text)
			return text, nil
		}
		if IsOverflowError(err) {
			text := withSubagentInstance(fmt.Sprintf("错误：子代理 %s 上下文窗口溢出：%v", agentName, err), subagentWireID(wire))
			wire.finish("error", text)
			return text, nil
		}
		// ctx 取消、panic 转成的事件错误、其余上抛：不写 terminal，文件留成半成品。
		return "", err
	}
	// 先按帽截断正文，再把实例 id 接在末尾，帽不会把 id 切掉。
	text := withSubagentInstance(truncateDelegationReturn(out, capBytes), subagentWireID(wire))
	wire.finish("complete", text)
	return text, nil
}

// delegationTruncationNotice 是超帽回传的尾缀模板（照 pi：50 KB 上限、UTF-8 安全 + omitted 提示；
// 全量在子代理 runlog，19v6-05 §4）。%d = 被省略的字节数。
const delegationTruncationNotice = "[Output truncated: %d bytes omitted. Full output preserved in subagent runlog.]"

// truncateDelegationReturn 把超过 capBytes 的回传截成「UTF-8 完整前缀 + 提示尾缀」。
// 截点回退到 rune 边界（绝不劈开多字节字符）；N = 总字节 − 保留字节（含尾缀与原文重合部分不算省略——
// 提示说的是「没进回传的字节数」）。capBytes <= 0 = 不裁剪。
func truncateDelegationReturn(s string, capBytes int) string {
	if capBytes <= 0 || len(s) <= capBytes {
		return s
	}
	kept := capBytes
	for kept > 0 && !utf8.RuneStart(s[kept]) {
		kept--
	}
	return s[:kept] + fmt.Sprintf(delegationTruncationNotice, len(s)-kept)
}

// BeforeAgent：每 run 起手执行一次，整体流程是：
//
//	      进程启动时（只执行一次）
//	      NewWorker
//	         │
//	         ▼
//	冻结基座：[getTime]           ← 真源，写死在代码里（ToolsConfig.Tools），不是扫目录
//	 （保存在 ADK 内部，不改）
//	         │
//	┌────────┴────────┐
//	│  每条 run 克隆  │     ← 所以下面 append 进去的子代理工具不跨 run 累积
//	▼                 ▼
//
// runCtx.Tools     runCtx.Tools
//
//	= [getTime]       = [getTime]
//	 │                 │
//	 ▼                 ▼
//	入口快照 base      入口快照 base   ← 这就是 resolveToolPool 的 base 参数
//	 = [getTime]       = [getTime]
//	 │                 │   （此刻无任何委派工具，子代理继承它 = 天然禁嵌套）
//	 │  扫盘 agents/   │  扫盘 agents/
//	 │  装配子代理     │  装配子代理
//	 ▼                 ▼
//
// runCtx.Tools +=   runCtx.Tools +=
//
//	[AgentTool_A,     [AgentTool_A,
//	 AgentTool_B]      AgentTool_B]
//
// 本函数具体步骤：扫盘 → 读笔记快照（M5）→ 三态解析工具池（resolveToolPool）→ 三写法解析模型
// （继承档用主实例 / ref 档查注册表）→ 装配子代理（Instruction = 规格正文 + 快照段）→
// 包 AgentTool → append runCtx.Tools + 写身份表（agentModelTable），最后打装载日志与逐条 system 日志。
//
// 白名单档的名称空间 = base + 保留名 tool_search：tools: [a, b] 里的 a、b 其实已经在 base
// tools 里面——resolveToolPool 只按名从 base 挑子集（挑不到 = 拒该条并列出可选名单），只收窄。
// 唯一例外是 tool_search：它不在 base（入口快照早于根 handler 入池），由子代理自己的
// toolSearchHandler 在 BeforeAgent 末尾装入并绑定该 agent 的目录；写进 tools: 才是显式要。
//
// 装载面错误（坏规格 / 解析失败 / 装配失败）全部降级为 log 拒该条——run 本身照跑（最差 = 无委派工具）。
// 唯一不降级的 = 笔记快照读库失败走 error：笔记层不可信不静默，与主代理 noteSnapshotHandler 同政策
// （链上紧随其后、对同一次读的失败同判，所以持久故障下 run 必死、瞬时故障下最多差这一场装载）。
func (h *agentSpecHandler) loadSpecs(ctx context.Context) ([]AgentSpec, error) {
	if p, ok := principalFromCtx(ctx); ok && p.TenantID != "" && h.store != nil {
		return h.store.EffectiveSubagentSpecs(ctx, p.TenantID, p.UserID)
	}
	return LoadAgentSpecs(h.dir)
}

func (h *agentSpecHandler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	specs, err := h.loadSpecs(ctx)
	if err != nil {
		log.Printf("agents: 装载规格失败: %v", err)
		return ctx, runCtx, nil
	}
	// 笔记快照（M5，planning §5 子代理快照缝）：装载时点 = run 起手，此刻读一次未替代行、渲染
	// 快照段，全部规格共用同一份（run 内冻结语义与主代理相同——工具写库不回写，下一场才可见）。
	snap := ""
	if len(specs) > 0 {
		snap, err = renderRunNotes(ctx, h.store, h.workspace)
		if err != nil {
			return ctx, runCtx, fmt.Errorf("agents: 笔记快照读库失败: %w", err)
		}
	}
	table := agentModelTableFrom(ctx) // 本 run 的身份表（worker.Run 建并塞 ctx）；写帧/drain 读它，见 worker.go
	// 拷一份入口快照。后面往 runCtx.Tools 追加的委派工具和 resume_subagent 不在这份里，
	// 子代理继承它 = 拿不到委派，也拿不到 resume。
	base := append([]tool.BaseTool(nil), runCtx.Tools...)
	ctx = withSubagentAssembly(ctx, &subagentAssembly{base: base, snap: snap, specs: specs})
	var loaded []string
	for _, spec := range specs {
		// 存量行也要挡：写入口校验是 2026-10-23 才加的，之前落库的坏名字（如带空格）仍在表里，
		// 装进池就是这一租户每场 run 都被厂商 400——一条坏规格只该失效它自己。
		if !validAgentName(spec.Name) {
			log.Printf("agents: %q: 拒载: %s", spec.Name, agentNameRuleMessage)
			continue
		}
		pool, err := resolveToolPool(ctx, spec, base)
		if err != nil {
			log.Printf("agents: %s: 拒载: %v", spec.Name, err)
			continue
		}
		// model 三写法分流（19v6-05 §4）：写法一/二（不写 / default|inherit|继承，大小写不敏感）
		// = 继承主实例，不进注册表；写法三 = ref 档 → ModelResolver 解析，失败拒该条、其余照载。
		subModel, entry, err := h.resolveSpecModel(ctx, spec)
		if err != nil {
			log.Printf("agents: %s: 拒载: %v", spec.Name, err)
			continue
		}
		instruction := spec.Prompt + "\n\n" + snap
		// 子代理的目录分级（T4/T5 A2）：只有显式写进 tools: 才挂自己的 toolSearchHandler——
		// 每条规格一份完整目录/激活集，tool_search 只搜自己的池。继承档与 exclude_tools 档
		// 不自动加（2026-10-20 已拍），其 MCP 工具照旧全部 direct 声明。
		wantSearch := specWantsToolSearch(spec)
		// 装载观测面（doc/26 §6）：这条规格这一场**实际**拿到的有效名单。exclude_tools 形状下规格
		// 文件里只写着例外，实际名单是「父池减去排除项」算出来的、父池一变就变——没这行，想知道某条
		// 子代理能干什么只能人脑跑一遍四态，护栏验收就退化成模型自觉（doc/21 §5 明写不靠模型自觉）。
		// 壳只缓存装配输入，不再起实例 ⇒ 不存在「装配成功才打日志」的先后关系，拒载只有解析面
		// （坏池/坏模型）一种；NewChatModelAgent 的失败面移到调用点（错误文本，见 subagentShell）。
		_, toolNames := poolIndex(ctx, pool)
		if wantSearch {
			toolNames = append(toolNames, toolSearchToolName) // handler 在 BeforeAgent 末尾装入，排在池尾
		}
		log.Printf("agent tools %s: %s", spec.Name, strings.Join(toolNames, ", "))
		// 子代理 system 全文日志（M5 观测面，转正点 planning §7）：装载时打拼接后 Instruction——
		// 快照句在规格正文之后的活体证据就靠它。instruction 在 run 起手冻结，调用点装配用的就是
		// 这一份，日志与实跑一致。
		log.Printf("agent system %s:\n%s", spec.Name, instruction)
		// 委派工具出池前过预算包壳（T3-3）：并发闸 + 回传裁剪；内层 = 壳（静态声明 + 调用点装配）。
		runCtx.Tools = append(runCtx.Tools, &delegationTool{
			inner: &subagentShell{
				h: h, spec: spec, pool: pool, subModel: subModel,
				instruction: instruction, wantSearch: wantSearch,
				toolInfo: &schema.ToolInfo{
					Name:        spec.Name,
					Desc:        spec.Description,
					ParamsOneOf: subagentShellRequestParams(),
				},
			},
			sem: h.sem, capBytes: h.returnCap, name: spec.Name, store: h.store,
		})
		table.set(spec.Name, entry)
		loaded = append(loaded, spec.Name)
	}
	if len(loaded) == 0 {
		log.Printf("agents: 0 spec(s)")
	} else {
		log.Printf("agents: %d spec(s): %s", len(loaded), strings.Join(loaded, ", "))
	}
	// resume 挂在入口快照之后，子代理池里没有它。账本包壳在本 handler 之后，根上的这次调用会计账。
	runCtx.Tools = append(runCtx.Tools, newResumeSubagentTool(h))
	return ctx, runCtx, nil
}

// resolveSpecModel 按规格的 model 行选出这一场要用的模型。
// 不写、或 default|inherit|继承：用当前 run 的主模型（租户 profile 优先于进程启动时的那份）。
// 写了具体 ref：问当前解析器要实例。解析失败把原因交回去，调用方决定拒载还是回错误文本。
func (h *agentSpecHandler) resolveSpecModel(ctx context.Context, spec AgentSpec) (model.BaseChatModel, agentModelEntry, error) {
	subModel := h.model
	entry := agentModelEntry{ref: h.modelRef, translate: h.translate}
	resolver := h.resolver
	if prof := profileFrom(ctx); prof != nil {
		if prof.Model != nil {
			subModel = prof.Model
			entry = agentModelEntry{ref: prof.ModelRef, translate: prof.Translator}
		}
		if prof.Resolver != nil {
			resolver = prof.Resolver
		}
	}
	ref := strings.TrimSpace(spec.Model)
	if ref == "" || isInheritRef(ref) {
		return subModel, entry, nil
	}
	if resolver == nil {
		return nil, agentModelEntry{}, fmt.Errorf("model %q: 没有模型注册表", ref)
	}
	inst, err := resolver.Get(ctx, ref)
	if err != nil {
		return nil, agentModelEntry{}, fmt.Errorf("model %q: %v", ref, err)
	}
	return inst.Chat, agentModelEntry{ref: inst.Ref, translate: TranslatorFor(inst.Protocol)}, nil
}

// subagentHandlers 是一条规格的 middleware。wire 紧挨折叠之前：折叠会换短 state.Messages，
// 先把折前新增行追加进档案。这些 handler 无调用态，写句柄在 ctx。
func (h *agentSpecHandler) subagentHandlers(name string, wantSearch bool) []adk.ChatModelAgentMiddleware {
	handlers := []adk.ChatModelAgentMiddleware{
		&callerStampMiddleware{
			BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}, name: name,
		},
	}
	if wantSearch {
		handlers = append(handlers, newToolSearchHandler())
	}
	handlers = append(handlers, newWireAppendHandler())
	handlers = append(handlers, newAgentFoldHandler(name, h.foldCounter, h.foldWindow, h.foldLimit,
		h.foldSummarize, h.foldEffects))
	return handlers
}

// resolveToolPool 按规格的工具面四态解析子代理的工具池（操作定义真源 doc/26 §3）：
//   - Tools == nil 且 ExcludeTools == nil（两行都没写）→ 继承父池快照（拷贝一份，调用方后续 append 不串扰）
//   - Tools 指向空切片（写了 tools: []）→ 空集
//   - Tools 指向非空切片（写了 tools: [a, b]）→ 按名映射，未知名报错带可选名单
//   - ExcludeTools 非 nil（写了 exclude_tools: [a]；与 tools: 互斥，同写已在 parse 层拒）→
//     继承父池减去列出的名字，见 excludeFromPool
//
// 名称空间四态同一个 = base（入口快照，此刻还没有任何委派工具 ⇒ 天然禁嵌套，
// 写 exclude_tools: [coder] 是未知名、拒载）。白名单档额外认保留名 tool_search：
// 它由子代理自己的 toolSearchHandler 装入，本函数只负责不把它当未知名。
func resolveToolPool(ctx context.Context, spec AgentSpec, base []tool.BaseTool) ([]tool.BaseTool, error) {
	if spec.Tools == nil {
		if spec.ExcludeTools == nil {
			return append([]tool.BaseTool(nil), base...), nil
		}
		return excludeFromPool(ctx, base, *spec.ExcludeTools)
	}
	if len(*spec.Tools) == 0 {
		return nil, nil
	}
	byName, names := poolIndex(ctx, base)
	pool := make([]tool.BaseTool, 0, len(*spec.Tools))
	for _, name := range *spec.Tools {
		if name == toolSearchToolName {
			continue // 保留名：不在 base，由子代理自己的 toolSearchHandler 装入
		}
		t, ok := byName[name]
		if !ok {
			options := append(append([]string(nil), names...), toolSearchToolName)
			return nil, fmt.Errorf("tools 列了未知名 %q（可选: %s）", name, strings.Join(options, ", "))
		}
		pool = append(pool, t)
	}
	return pool, nil
}

// specWantsToolSearch 是子代理要 tool_search 的唯一判据：显式写进 tools:。
// 继承档与 exclude_tools 档不自动加——入口快照 base 本就不含 tool_search，
// 这类子代理拿不到检索工具，其 MCP 工具照旧全部 direct 声明，不出现「有目录无检索」的半边天。
func specWantsToolSearch(spec AgentSpec) bool {
	if spec.Tools == nil {
		return false
	}
	for _, name := range *spec.Tools {
		if name == toolSearchToolName {
			return true
		}
	}
	return false
}

// poolIndex 按 Info().Name 给父池建索引，同时保序带出名字表（两条「未知名」报错的可选名单用它）。
// 第三态与第四态共用这一份——两处各写一遍 Info() 循环就会各自腐化。
// Info 失败的工具跳过（与第三态既有行为一致：它进不了可选名单，也就无从被挑中或排除）。
func poolIndex(ctx context.Context, base []tool.BaseTool) (map[string]tool.BaseTool, []string) {
	byName := make(map[string]tool.BaseTool, len(base))
	names := make([]string, 0, len(base))
	for _, t := range base {
		info, err := t.Info(ctx)
		if err != nil {
			continue
		}
		byName[info.Name] = t
		names = append(names, info.Name)
	}
	return byName, names
}

// excludeFromPool 是第四态：父池减去 drop 里的名字，其余**按父池顺序**原样带出
// （结果序是池序不是排除项序），元素与父池是同一个工具对象——不换壳，与 doc/21 H4 同判据。
// 空列表 = 零排除 = 与不写同结果（显式表态「已看过、一个不排除」是合法配置，不报错）。
// 排除项里有一个名字父池没有 → 报错：不报就是护栏最哑的失效方式——bash 拼成 bashh 被悄悄忽略，
// 规格文件看起来已经挡住了，子代理照旧能跑命令。
func excludeFromPool(ctx context.Context, base []tool.BaseTool, drop []string) ([]tool.BaseTool, error) {
	if len(drop) == 0 {
		return append([]tool.BaseTool(nil), base...), nil
	}
	byName, names := poolIndex(ctx, base)
	skip := make(map[string]bool, len(drop))
	for _, name := range drop {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("exclude_tools 列了未知名 %q（可选: %s）", name, strings.Join(names, ", "))
		}
		skip[name] = true
	}
	pool := make([]tool.BaseTool, 0, len(base))
	for _, t := range base {
		info, err := t.Info(ctx)
		if err != nil || skip[info.Name] {
			continue
		}
		pool = append(pool, t)
	}
	return pool, nil
}
