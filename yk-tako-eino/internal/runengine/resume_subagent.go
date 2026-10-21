// resume_subagent 让父代理继续一场已经完整结束的子代理（doc/39 S2）。
// 先读档案、校验，通过之后才占闸、开新档案。四个拒绝都回错误文本，父 run 保持 complete，原文件不动。
package runengine

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

const resumeSubagentToolName = "resume_subagent"

// subagentArchiveTTL 是 -archive-ttl 的默认值。生产路径读 agentSpecHandler.archiveTTL，
// 那个字段由 NewWorker 从旗标灌进来。这里只给没经过 NewWorker 的单测当窗口。
const subagentArchiveTTL = 168 * time.Hour

const (
	msgArchiveMissing    = "找不到该子代理档案，请重新委派"
	msgArchiveIncomplete = "该实例未完成或已中断，请重新委派"
	msgArchiveExpired    = "档案已过期"
	msgArchiveCorrupt    = "档案损坏"
	msgSpecMissing       = "找不到该子代理规格，请重新委派"
)

const resumeSubagentDesc = "继续一场已经完整结束的子代理。id 与 agent 二选一：id 填回传里的实例 id（sa_ 开头），agent 填规格名并取该规格最近一份有终态的档案。两个都填时以 id 为准。instruction 必填，是这次要它继续做的事。档案未完成、已过期、找不到或损坏时，结果里会说明原因。"

// resumeSubagentArgs 是模型填的参数。id 与 agent 至少给一个。instruction 必填。
type resumeSubagentArgs struct {
	ID          string `json:"id,omitempty" jsonschema_description:"实例 id，形如 sa_ 加 ULID。与 agent 二选一；两个都填时以 id 为准"`
	Agent       string `json:"agent,omitempty" jsonschema_description:"子代理规格名。不填 id 时取该规格最近一份完整档案"`
	Instruction string `json:"instruction" jsonschema_description:"这次要子代理继续做的事"`
}

// subagentAssembly 是这一场 run 起手时的入口快照。handler 跨 run 共用，不能把这些记在结构体上。
type subagentAssembly struct {
	base  []tool.BaseTool
	snap  string
	specs []AgentSpec
}

type subagentAssemblyKey struct{}

func withSubagentAssembly(ctx context.Context, a *subagentAssembly) context.Context {
	return context.WithValue(ctx, subagentAssemblyKey{}, a)
}

func subagentAssemblyFrom(ctx context.Context) *subagentAssembly {
	a, _ := ctx.Value(subagentAssemblyKey{}).(*subagentAssembly)
	return a
}

// resumeArchive 是一份通过校验的完整档案。messages 不含 terminal 行。
type resumeArchive struct {
	meta     wireMetadata
	messages []*schema.Message
}

type resumeSubagentTool struct {
	h *agentSpecHandler
}

func newResumeSubagentTool(h *agentSpecHandler) *resumeSubagentTool {
	return &resumeSubagentTool{h: h}
}

var _ tool.InvokableTool = (*resumeSubagentTool)(nil)

func (t *resumeSubagentTool) archiveWindow() time.Duration {
	if t != nil && t.h != nil && t.h.archiveTTL > 0 {
		return t.h.archiveTTL
	}
	return subagentArchiveTTL
}

func (t *resumeSubagentTool) Info(context.Context) (*schema.ToolInfo, error) {
	return utils.GoStruct2ToolInfo[resumeSubagentArgs](resumeSubagentToolName, resumeSubagentDesc)
}

func (t *resumeSubagentTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	var in resumeSubagentArgs
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "错误：resume_subagent 参数无法解析", nil
	}
	instruction := strings.TrimSpace(in.Instruction)
	if instruction == "" {
		return "错误：instruction 不能为空", nil
	}
	ref, ok := workspaceFrom(ctx)
	if !ok {
		return msgArchiveMissing, nil
	}
	dir, err := subagentWireDir(ref)
	if err != nil {
		return msgArchiveMissing, nil
	}
	arch, reject := locateSubagentArchiveWindow(dir, strings.TrimSpace(in.ID), strings.TrimSpace(in.Agent), time.Now().UTC(), t.archiveWindow())
	if reject != "" {
		log.Printf("resume_subagent: %s", reject)
		return reject, nil
	}
	asm := subagentAssemblyFrom(ctx)
	if asm == nil {
		log.Printf("resume_subagent: %s", msgSpecMissing)
		return msgSpecMissing, nil
	}
	spec, ok := specByName(asm.specs, arch.meta.AgentName)
	if !ok {
		log.Printf("resume_subagent: %s agent=%s", msgSpecMissing, arch.meta.AgentName)
		return msgSpecMissing, nil
	}
	inner, errText := t.h.buildResumeAgent(ctx, spec, asm, arch.messages, instruction)
	if errText != "" {
		log.Printf("resume_subagent: %s", errText)
		return errText, nil
	}
	req, err := json.Marshal(struct {
		Request string `json:"request"`
	}{Request: instruction})
	if err != nil {
		return "错误：instruction 无法编码", nil
	}
	return runSubagentGuarded(ctx, t.h.store, t.h.sem, t.h.returnCap, spec.Name, arch.meta.SaID, func(runCtx context.Context) (string, error) {
		return inner.InvokableRun(runCtx, string(req), opts...)
	})
}

func specByName(specs []AgentSpec, name string) (AgentSpec, bool) {
	for _, spec := range specs {
		if spec.Name == name {
			return spec, true
		}
	}
	return AgentSpec{}, false
}

// buildResumeAgent 按当前规格装配子代理，模型输入是档案里的消息再加一条新的 user。
// 装配失败回错误文本，调用方这时还没占闸、也还没开新档案。
func (h *agentSpecHandler) buildResumeAgent(ctx context.Context, spec AgentSpec, asm *subagentAssembly, history []*schema.Message, instruction string) (tool.InvokableTool, string) {
	pool, err := resolveToolPool(ctx, spec, asm.base)
	if err != nil {
		return nil, "错误：子代理 " + spec.Name + " 当前无法装配：" + err.Error()
	}
	subModel, entry, err := h.resolveSpecModel(ctx, spec)
	if err != nil {
		return nil, "错误：子代理 " + spec.Name + " 当前无法装配：" + err.Error()
	}
	if table := agentModelTableFrom(ctx); table != nil {
		table.set(spec.Name, entry)
	}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          spec.Name,
		Description:   spec.Description,
		Instruction:   spec.Prompt + "\n\n" + asm.snap,
		Model:         subModel,
		MaxIterations: agentMaxIterations,
		ToolsConfig:   adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: pool}},
		Handlers:      h.subagentHandlers(spec.Name, specWantsToolSearch(spec)),
		GenModelInput: func(context.Context, string, *adk.AgentInput) ([]*schema.Message, error) {
			return resumeModelInput(history, instruction)
		},
	})
	if err != nil {
		return nil, "错误：子代理 " + spec.Name + " 当前无法装配：" + err.Error()
	}
	inner, ok := adk.NewAgentTool(ctx, agent).(tool.InvokableTool)
	if !ok {
		return nil, "错误：子代理 " + spec.Name + " 当前无法装配：内层不可调用"
	}
	return inner, ""
}

// resumeModelInput 是 resume 喂给模型的整表：档案消息原样在前，新指令作最后一条 user。
// 不另加一条 system。档案里已经有上一场的 system，再加一条就会变成两套人设。
func resumeModelInput(history []*schema.Message, instruction string) ([]*schema.Message, error) {
	out := make([]*schema.Message, len(history), len(history)+1)
	copy(out, history)
	out = append(out, schema.UserMessage(instruction))
	return out, nil
}

// locateSubagentArchive 按 id 或 agent 找一份可 resume 的档案。
// id 非空时只认这一份，忽略 agent。拒绝时第二个返回值是给父对话的原文。
func locateSubagentArchive(dir, id, agent string, now time.Time) (*resumeArchive, string) {
	return locateSubagentArchiveWindow(dir, id, agent, now, subagentArchiveTTL)
}

func locateSubagentArchiveWindow(dir, id, agent string, now time.Time, ttl time.Duration) (*resumeArchive, string) {
	if id != "" {
		if !validSubagentID(id) {
			return nil, msgArchiveMissing
		}
		b, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, msgArchiveMissing
			}
			return nil, msgArchiveCorrupt
		}
		return validateSubagentArchive(b, now, ttl)
	}
	if agent == "" {
		return nil, msgArchiveMissing
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return nil, msgArchiveMissing
	}
	var best []byte
	var bestMeta wireMetadata
	var bestAt time.Time
	found := false
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		meta, ok := terminalCandidate(b)
		if !ok || meta.AgentName != agent {
			continue
		}
		at := wireCreatedAt(meta.CreatedAt)
		if !found || at.After(bestAt) || (at.Equal(bestAt) && meta.SaID > bestMeta.SaID) {
			best, bestMeta, bestAt, found = b, meta, at, true
		}
	}
	if !found {
		return nil, msgArchiveMissing
	}
	return validateSubagentArchive(best, now, ttl)
}

// validSubagentID 只接受 sa_ 加 Crockford base32。挡住路径段，档案只能落在会话目录里。
func validSubagentID(id string) bool {
	if !strings.HasPrefix(id, "sa_") || len(id) == len("sa_") {
		return false
	}
	for _, r := range id[len("sa_"):] {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'A' && r <= 'Z' && r != 'I' && r != 'L' && r != 'O' && r != 'U':
		default:
			return false
		}
	}
	return true
}

func wireCreatedAt(s string) time.Time {
	t, err := parseWireTime(s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseWireTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

func splitJSONL(b []byte) [][]byte {
	var lines [][]byte
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, []byte(line))
	}
	return lines
}

// dropPartialTail 丢掉文件末尾没写完的半行。只丢最后一行，前面的坏行留给校验判损坏。
func dropPartialTail(lines [][]byte) [][]byte {
	if len(lines) == 0 {
		return lines
	}
	if !json.Valid(lines[len(lines)-1]) {
		return lines[:len(lines)-1]
	}
	return lines
}

// terminalCandidate 判断这份文件在丢掉末尾半行之后，是不是一份带终态的档案。
// 按规格名挑「最新一份」时用它。过期和消息损坏留到选中之后再判。
func terminalCandidate(b []byte) (wireMetadata, bool) {
	lines := dropPartialTail(splitJSONL(b))
	if len(lines) == 0 {
		return wireMetadata{}, false
	}
	var meta wireMetadata
	if json.Unmarshal(lines[0], &meta) != nil || meta.Type != "metadata" || meta.SaID == "" {
		return wireMetadata{}, false
	}
	var term wireTerminal
	if json.Unmarshal(lines[len(lines)-1], &term) != nil || term.Type != "terminal" {
		return wireMetadata{}, false
	}
	return meta, true
}

// validateSubagentArchive 对已经定位到的那一份做拒绝判断。
// 顺序：无终态、过期、消息逐条可解析。末行 JSON 解析失败则丢掉，更早的坏行算损坏。
func validateSubagentArchive(b []byte, now time.Time, ttl time.Duration) (*resumeArchive, string) {
	lines := dropPartialTail(splitJSONL(b))
	if len(lines) == 0 {
		return nil, msgArchiveCorrupt
	}
	var meta wireMetadata
	if json.Unmarshal(lines[0], &meta) != nil || meta.Type != "metadata" || meta.SaID == "" {
		return nil, msgArchiveCorrupt
	}
	var term wireTerminal
	if json.Unmarshal(lines[len(lines)-1], &term) != nil || term.Type != "terminal" {
		return nil, msgArchiveIncomplete
	}
	if meta.Protocol != subagentWireProtocol {
		return nil, msgArchiveCorrupt
	}
	finished, err := parseWireTime(term.FinishedAt)
	if err != nil {
		return nil, msgArchiveCorrupt
	}
	if ttl <= 0 {
		ttl = subagentArchiveTTL
	}
	if finished.Add(ttl).Before(now) {
		return nil, msgArchiveExpired
	}
	msgs := make([]*schema.Message, 0, len(lines)-2)
	for i := 1; i < len(lines)-1; i++ {
		var line wireMessageLine
		if json.Unmarshal(lines[i], &line) != nil || line.Type != "message" || line.Msg == nil {
			return nil, msgArchiveCorrupt
		}
		msgs = append(msgs, line.Msg)
	}
	return &resumeArchive{meta: meta, messages: msgs}, ""
}
