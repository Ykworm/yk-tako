// 本文件是持久 Task 的创建计划校验与输入构造（方案真源 = doc/70 §4.2/§6.1/§7.3；施工包 §6/§7）。
//
// 分工：这里只做**不碰数据库**的纯校验与推导——形状、大小、键规则、边合法性、防环、
// 最终节点唯一、规范化计划与 plan_hash、下游首条输入的文本。涉及「来源会话是不是本人的、
// 项目挂没挂对、执行者规格存不存在」的校验在 agent_task_store.go 的事务里做。
// HTTP handler 与模型工具都经统一创建服务进来，两边共享同一份校验（施工包 §6.1，不复制两套）。
package runengine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// planTask / planLink 是创建计划的原始节点与边（HTTP 请求体与模型工具入参共用形状）。
type planTask struct {
	Key      string `json:"key"`
	Content  string `json:"content"`
	Agent    string `json:"agent"`
	ModelRef string `json:"modelRef"`
	// PlanCheckpoint 省略时必须不进入规范化哈希字节：旧计划（没有这个字段）与显式 false
	// 的 plan_hash 保持一致（doc/72 §4.1）。
	PlanCheckpoint bool `json:"planCheckpoint,omitempty"`
}

type planLink struct {
	Parent string `json:"parent"`
	Child  string `json:"child"`
}

// CreateOrchestrationCommand 是统一创建服务的入参（施工包 §6.1）。
type CreateOrchestrationCommand struct {
	IdempotencyKey  string
	WorkspaceID     string
	SourceSessionID SessionID
	// SourceRunID 是模型工具创建时的当前 run（trace 对读）；产品 API 手工创建留空。
	SourceRunID string
	Goal        string
	Tasks       []planTask
	Links       []planLink
}

// CreateTaskCommand 是单件 Task 的入参；创建服务内部把它转成单节点编排命令。
type CreateTaskCommand struct {
	IdempotencyKey  string
	WorkspaceID     string
	SourceSessionID SessionID
	SourceRunID     string
	Content         string
	Agent           string
	ModelRef        string
	PlanCheckpoint  bool
}

// planError 是计划校验失败：API 层翻成 117101，data.reason 带具体原因（不含 SQL）。
type planError struct{ reason string }

func (e *planError) Error() string { return e.reason }

// taskKeyRe 是 Task key 的词法（施工包 §4.2）：字母数字开头，可含 _ 与 -，1–64 字符。
var taskKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// limits（施工包 §4.2 的数字都在这里，别处不写裸数字）。
const (
	taskGoalMaxRunes     = 8000
	taskContentMaxRunes  = 16000
	idempotencyMaxBytes  = 128
	taskSummaryMaxRunes  = 8000
	taskArtifactsMax     = 32
	taskMetadataMaxBytes = 16 << 10
	// singleTaskKey 是单件 Task 转成单节点编排时的组内 key（施工包未定名；取合规则的固定值，
	// 单节点组里 key 不参与任何语义，只满足 (orchestration_id, task_key) 唯一约束）。
	singleTaskKey = "main"
)

// orchPlan 是校验后的规范计划：Task 按 key 排序、边按 (parent, child) 排序。
// 它同时是 plan_hash 的哈希输入与建行的蓝图（finalTaskID、初始状态都从它推导）。
type orchPlan struct {
	SourceSessionID string     `json:"sourceSessionId"`
	WorkspaceID     string     `json:"workspaceId"`
	Goal            string     `json:"goal"`
	Tasks           []planTask `json:"tasks"`
	Links           []planLink `json:"links"`
}

// normalizeOrchestrationPlan 做全部纯校验并产出规范计划。任何一条不过都回 *planError。
// 步骤即依赖：先验节点形状（key/content/agent），边校验需要节点集合，防环与最终节点
// 需要边集合——顺序反了会在半成品上推导。
func normalizeOrchestrationPlan(cmd CreateOrchestrationCommand) (*orchPlan, error) {
	if strings.TrimSpace(cmd.IdempotencyKey) == "" {
		return nil, &planError{"idempotencyKey 必填"}
	}
	if len(cmd.IdempotencyKey) > idempotencyMaxBytes {
		return nil, &planError{"idempotencyKey 超过 128 字节"}
	}
	if strings.TrimSpace(cmd.Goal) == "" {
		return nil, &planError{"goal 必填"}
	}
	if utf8Len(cmd.Goal) > taskGoalMaxRunes {
		return nil, &planError{"goal 超过 8000 字"}
	}
	if len(cmd.Tasks) == 0 || len(cmd.Tasks) > agentTaskMaxCount {
		return nil, &planError{"一件编排组需要 1–32 件 Task"}
	}
	seen := make(map[string]bool, len(cmd.Tasks))
	for _, t := range cmd.Tasks {
		if !taskKeyRe.MatchString(t.Key) {
			return nil, &planError{"Task key 只允许字母数字开头的 1–64 位字母数字、_ 或 -：" + t.Key}
		}
		if seen[t.Key] {
			return nil, &planError{"Task key 在组内重复：" + t.Key}
		}
		seen[t.Key] = true
		if strings.TrimSpace(t.Content) == "" {
			return nil, &planError{"Task 内容必填：" + t.Key}
		}
		if utf8Len(t.Content) > taskContentMaxRunes {
			return nil, &planError{"Task 内容超过 16000 字：" + t.Key}
		}
		if strings.TrimSpace(t.Agent) == "" {
			return nil, &planError{"Task 执行者必填：" + t.Key}
		}
	}
	linkSeen := make(map[planLink]bool, len(cmd.Links))
	childOf := make(map[string][]string, len(cmd.Tasks))
	for _, l := range cmd.Links {
		if !seen[l.Parent] || !seen[l.Child] {
			return nil, &planError{"依赖边引用了不存在的 Task：" + l.Parent + " -> " + l.Child}
		}
		if l.Parent == l.Child {
			return nil, &planError{"依赖边不允许自依赖：" + l.Parent}
		}
		if linkSeen[l] {
			return nil, &planError{"依赖边重复：" + l.Parent + " -> " + l.Child}
		}
		linkSeen[l] = true
		childOf[l.Parent] = append(childOf[l.Parent], l.Child)
	}
	keys := make([]string, 0, len(cmd.Tasks))
	for _, t := range cmd.Tasks {
		keys = append(keys, t.Key)
	}
	if err := detectCycle(keys, cmd.Links); err != nil {
		return nil, err
	}
	// 最终节点 = 出度为 0（没有任何 child）。恰好一个，程序不猜该交付哪份结果（doc/70 §2 第 5 条）。
	finals := 0
	for _, t := range cmd.Tasks {
		if len(childOf[t.Key]) == 0 {
			finals++
		}
	}
	if finals != 1 {
		return nil, &planError{"一张编排图必须恰好有一个无子节点的最终 Task，现在有 " + fmt.Sprint(finals) + " 个"}
	}
	plan := &orchPlan{
		SourceSessionID: string(cmd.SourceSessionID),
		WorkspaceID:     cmd.WorkspaceID,
		Goal:            cmd.Goal,
		Tasks:           append([]planTask(nil), cmd.Tasks...),
		Links:           append([]planLink(nil), cmd.Links...),
	}
	sort.Slice(plan.Tasks, func(i, j int) bool { return plan.Tasks[i].Key < plan.Tasks[j].Key })
	sort.Slice(plan.Links, func(i, j int) bool {
		if plan.Links[i].Parent != plan.Links[j].Parent {
			return plan.Links[i].Parent < plan.Links[j].Parent
		}
		return plan.Links[i].Child < plan.Links[j].Child
	})
	return plan, nil
}

// detectCycle 用 Kahn 拓扑排序防环：能排完所有节点 = 无环；排不完 = 有环。
// keys 是全部节点（含没出现在任何边上的孤立节点——单节点编排没有边，孤立节点也是合法成员，
// 漏掉它们会把合法 DAG 误判成环）。不用 DFS 递归——32 个节点内两者都够，但 Kahn 的
// 「已处理数」一眼对得上测试断言。
func detectCycle(keys []string, links []planLink) error {
	indegree := make(map[string]int, len(keys))
	next := make(map[string][]string, len(keys))
	for _, l := range links {
		indegree[l.Child]++
		next[l.Parent] = append(next[l.Parent], l.Child)
	}
	queue := make([]string, 0, len(keys))
	for _, k := range keys {
		if indegree[k] == 0 {
			queue = append(queue, k)
		}
	}
	processed := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		processed++
		for _, c := range next[cur] {
			indegree[c]--
			if indegree[c] == 0 {
				queue = append(queue, c)
			}
		}
	}
	if processed != len(keys) {
		return &planError{"依赖边成环，无法拓扑排序"}
	}
	return nil
}

// hash 计算规范化计划的 SHA-256 十六进制。输入数组顺序不同但逻辑计划相同 → 同哈希；
// 任何内容字节不同 → 不同哈希（幂等比对就靠它，施工包 §4.2）。
func (p *orchPlan) hash() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum), nil
}

// finalKey 返回最终 Task 的 key（出度为 0 的那一个；校验已保证恰好一个）。
func (p *orchPlan) finalKey() string {
	hasChild := make(map[string]bool, len(p.Links))
	for _, l := range p.Links {
		hasChild[l.Parent] = true
	}
	for _, t := range p.Tasks {
		if !hasChild[t.Key] {
			return t.Key
		}
	}
	return ""
}

// hasParents 报告这个 key 是否有依赖边（决定初始状态：无 parents = ready，否则 pending）。
func (p *orchPlan) hasParents(key string) bool {
	for _, l := range p.Links {
		if l.Child == key {
			return true
		}
	}
	return false
}

// singleTaskCommand 把单件 Task 命令转成单节点编排命令（施工包 §6.1）：
// goal 从 content 派生（截 8000 rune，纯截断不加标记——goal 是展示与输入字段，不是折叠摘要）。
func singleTaskCommand(cmd CreateTaskCommand) CreateOrchestrationCommand {
	return CreateOrchestrationCommand{
		IdempotencyKey:  cmd.IdempotencyKey,
		WorkspaceID:     cmd.WorkspaceID,
		SourceSessionID: cmd.SourceSessionID,
		SourceRunID:     cmd.SourceRunID,
		Goal:            truncatePlain(cmd.Content, taskGoalMaxRunes),
		Tasks: []planTask{{
			Key: singleTaskKey, Content: cmd.Content, Agent: cmd.Agent,
			ModelRef: cmd.ModelRef, PlanCheckpoint: cmd.PlanCheckpoint,
		}},
	}
}

// truncatePlain 按 rune 截断，不加任何标记（区别于 compact.go 的 truncateRunes）。
func truncatePlain(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// utf8Len 是 rune 计数（施工包 §4.2 的 8000/16000 都按字数不按字节）。
func utf8Len(s string) int { return len([]rune(s)) }

// buildTaskFirstInput 构造 Task 执行 run 的首条用户输入（施工包 §7.3）。输入四段固定：
// 原始 goal → 自己的 key 与 content → 每个 succeeded parent 的结果摘要（按 key 排序，可重复）
// → 执行约定。parents 由领取事务现读（不复制父 Task 的聊天或工具历史）。
func buildTaskFirstInput(goal string, t AgentTask, parents []AgentTask) string {
	var b strings.Builder
	b.WriteString("你是一件持久 Task 的执行者。\n\n## 编排目标\n")
	b.WriteString(goal)
	b.WriteString("\n\n## 本件任务\nkey: ")
	b.WriteString(t.TaskKey)
	b.WriteString("\n")
	b.WriteString(t.Content)
	sorted := append([]AgentTask(nil), parents...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TaskKey < sorted[j].TaskKey })
	if len(sorted) > 0 {
		b.WriteString("\n\n## 上游结果\n")
		for _, p := range sorted {
			b.WriteString("\n### ")
			b.WriteString(p.TaskKey)
			b.WriteString("（taskId=")
			b.WriteString(p.ID)
			b.WriteString("，resultRevision=")
			b.WriteString(fmt.Sprint(p.ResultRevision))
			b.WriteString("）\n")
			if r := parseTaskResult(p.Result); r != nil {
				b.WriteString("summary: ")
				b.WriteString(r.Summary)
				b.WriteString("\n")
				if len(r.Artifacts) > 0 {
					b.WriteString("artifacts:\n")
					for _, a := range r.Artifacts {
						b.WriteString("- " + a.Path + "（kind=" + a.Kind + "）\n")
					}
				}
				if len(r.Metadata) > 0 {
					if mb, err := json.Marshal(r.Metadata); err == nil {
						b.WriteString("metadata: ")
						b.Write(mb)
						b.WriteString("\n")
					}
				}
			}
		}
	}
	b.WriteString("\n## 执行约定\n")
	b.WriteString("只读取这些结果和授权项目文件；完成后调用 submit_task_result 提交结果，然后结束本轮。")
	b.WriteString("不要把工具回执当作已经交付：只有 submit_task_result 的回执才算提交。\n")
	return b.String()
}
