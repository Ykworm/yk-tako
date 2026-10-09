// 本文件实现文件式子代理规格：agents/ 单目录，每个 *.md 文件定义一条子代理规格
// （YAML frontmatter 四字段 + 正文 = system prompt）。
//
// 装载时点 = run 起手的扫盘（BeforeAgent 每 Run 调一次）：改文件 → 下一条消息生效；run 中途不变。
// 装载路线 = BeforeAgent 换列表（不重建主 agent）：handler 把每条规格装配成 ChatModelAgent、
// 包成 adk.NewAgentTool 后 append 进 runCtx.Tools；ADK 随后把该列表重建进 toolInfos
// （声明面 model.WithTools）与 toolsNodeConf（执行面 WithToolList）——两处都按本次调用生效。
// runCtx.Tools 每 run 从冻结基座克隆（chatmodel.go applyBeforeAgent），append 不跨 run 累积。
package runengine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"gopkg.in/yaml.v3"
)

// AgentSpec 是一条子代理规格。Name 是唯一身份
// （= AgentTool 工具名 = runlog 的 agent 值；文件名不参与语义）。
// Tools 三态靠指针区分：nil = 缺席（继承父池）/ 指向空切片 = 空集 / 指向非空 = 白名单按名映射。
type AgentSpec struct {
	Name        string
	Description string
	Tools       *[]string
	Prompt      string // 正文 = system prompt
}

// agentSpecFront 是 frontmatter 的 YAML 形状。Model 保留字段是为了区分「写了」与「没写」——
// 写值在 LoadAgentSpecs 拒载（未实现），缺席 = 继承主 worker 的模型实例。
type agentSpecFront struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Tools       *[]string `yaml:"tools"`
	Model       string    `yaml:"model"`
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
// 中间是 YAML、之后是正文。KnownFields(true)：未知字段拒载——字段表是拍定的四字段，
// 拼写手误（如 tool:）没有「悄悄忽略」的通道。
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
	if f.Description == "" {
		return AgentSpec{}, fmt.Errorf("缺必填字段 description（模型决定委派与否的唯一入口）")
	}
	if strings.TrimSpace(f.Model) != "" {
		return AgentSpec{}, fmt.Errorf("model 写值未实现（只支持缺席 = 继承主 worker 模型实例）")
	}
	return AgentSpec{
		Name:        f.Name,
		Description: f.Description,
		Tools:       f.Tools,
		Prompt:      strings.TrimSpace(strings.Join(lines[end+1:], "\n")),
	}, nil
}

// agentSpecHandler 是装载 handler：BeforeAgent 每 Run 扫一次盘，把规格装配成委派工具追加进 runCtx.Tools。
// 入口快照 base = runCtx.Tools：此刻池里只有基座工具（本 handler 是唯一往池里加委派工具的），
// 子代理继承父池时取它 = 天然禁嵌套。
type agentSpecHandler struct {
	*adk.BaseChatModelAgentMiddleware
	dir   string
	model model.BaseChatModel
}

// newAgentSpecHandler 组装载 handler。dir 是规格目录（相对 CWD 或绝对路径）；
// m 是子代理共享的模型实例（字段表：model 缺席 = 继承主 worker 实例）。
func newAgentSpecHandler(dir string, m model.BaseChatModel) *agentSpecHandler {
	return &agentSpecHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}, dir: dir, model: m}
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
// 本函数具体步骤：扫盘 → 三态解析工具池（resolveToolPool）→ 装配子代理 → 包 AgentTool → append runCtx.Tools，
// 最后打一行装载日志（agents: N spec(s): <names>）。
//
// 白名单档的名称空间 = base 本身：tools: [a, b] 里的 a、b 其实已经在 base tools 里面——
// resolveToolPool 只按名从 base 挑子集（挑不到 = 拒该条并列出可选名单），只收窄、不扩容。
//
// 本 handler 不改 instruction、不返回错误：装载面全部降级为 log 拒该条——run 本身照跑（最差 = 无委派工具）。
func (h *agentSpecHandler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	specs, err := LoadAgentSpecs(h.dir)
	if err != nil {
		log.Printf("agents: 扫盘 %s 失败: %v", h.dir, err)
		return ctx, runCtx, nil
	}
	base := runCtx.Tools // 入口快照：继承档取它（此刻无任何委派工具 = 禁嵌套）
	var loaded []string
	for _, spec := range specs {
		pool, err := resolveToolPool(ctx, spec, base)
		if err != nil {
			log.Printf("agents: %s: 拒载: %v", spec.Name, err)
			continue
		}
		sub, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
			Name:        spec.Name,
			Description: spec.Description,
			Instruction: spec.Prompt,
			Model:       h.model,
			ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: pool}},
		})
		if err != nil {
			log.Printf("agents: %s: 拒载: 子代理装配失败: %v", spec.Name, err)
			continue
		}
		runCtx.Tools = append(runCtx.Tools, adk.NewAgentTool(ctx, sub))
		loaded = append(loaded, spec.Name)
	}
	if len(loaded) == 0 {
		log.Printf("agents: 0 spec(s)")
	} else {
		log.Printf("agents: %d spec(s): %s", len(loaded), strings.Join(loaded, ", "))
	}
	return ctx, runCtx, nil
}

// resolveToolPool 按 Tools 指针的三态解析子代理的工具池：
//   - Tools == nil（没写 tools 行）→ 继承父池快照（拷贝一份，调用方后续 append 不串扰）
//   - Tools 指向空切片（写了 tools: []）→ 空集
//   - Tools 指向非空切片（写了 tools: [a, b]）→ 按名映射，未知名报错带可选名单
func resolveToolPool(ctx context.Context, spec AgentSpec, base []tool.BaseTool) ([]tool.BaseTool, error) {
	if spec.Tools == nil {
		return append([]tool.BaseTool(nil), base...), nil
	}
	if len(*spec.Tools) == 0 {
		return nil, nil
	}
	byName := make(map[string]tool.BaseTool, len(base))
	var names []string
	for _, t := range base {
		info, err := t.Info(ctx)
		if err != nil {
			continue
		}
		byName[info.Name] = t
		names = append(names, info.Name)
	}
	pool := make([]tool.BaseTool, 0, len(*spec.Tools))
	for _, name := range *spec.Tools {
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("tools 列了未知名 %q（可选: %s）", name, strings.Join(names, ", "))
		}
		pool = append(pool, t)
	}
	return pool, nil
}
