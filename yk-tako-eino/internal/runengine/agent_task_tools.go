// 本文件是持久 Task 的三个模型工具（方案真源 = doc/70 §5.3/§7.1；施工包 §6.3）：
//
//	delegate_task / create_orchestration —— 只进普通 mAgent 根池（NewWorker 冻结基座），
//	    子代理入口快照剔掉（agents.go dropAgentOrchestrationTools），Task 执行者装配也不给。
//	 submit_task_result —— 不进冻结基座；只在 Task 执行 run 的装配里 append（agent_task_runner.go），
//	    普通聊天 run 的调用面里没有它。
//
// 三把都只做「拆参数 → 调统一创建/提交服务 → 翻人话」，校验只有 store/ graph 层一份。
// 身份全部取自 run context（taskBox 的 session/run、journalTarget 的 epoch、agentTaskRunBox 的
// taskId、workspaceFrom 的项目根），不信任模型传来的任何 ID——模型入参里没有这些字段。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
)

// dropAgentOrchestrationTools 把 delegate_task / create_orchestration 从入口快照里剔掉
// （doc/70 §6.3）。剔在 base = 继承档、exclude_tools 档、tools 白名单档三档的共同来源——
// 一份过滤管三条路：继承档拿不到，白名单档写了名字按「未知名」拒载。
func dropAgentOrchestrationTools(ctx context.Context, tools []tool.BaseTool) []tool.BaseTool {
	reserved := map[string]bool{"delegate_task": true, "create_orchestration": true}
	out := make([]tool.BaseTool, 0, len(tools))
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			out = append(out, t) // 拿不到名的原样保留：过滤不该顺手改装配结果
			continue
		}
		if !reserved[info.Name] {
			out = append(out, t)
		}
	}
	return out
}

// agentTaskRunBox 是 Task 执行 run 的身份盒：装配时（agent_task_runner.go）由领取路径塞进
// ctx，submit_task_result 据此知道「我在为哪件 Task 干活」；worker.run 据此打 trace 归属
// 属性（doc/70 §10.3）。普通聊天 run 没有它，submit_task_result 调用即被拒——权限面不是
// 靠池子删干净，而是工具自己再看一眼 ctx。
type agentTaskRunBox struct {
	TaskID          string
	OrchestrationID string
	// SourceRunID 是创建这件编排的来源 run（trace 对读用）；产品 API 手工创建为空串。
	SourceRunID string
}

type agentTaskRunBoxKey struct{}

func withAgentTaskRunBox(ctx context.Context, b *agentTaskRunBox) context.Context {
	return context.WithValue(ctx, agentTaskRunBoxKey{}, b)
}

func agentTaskRunBoxFrom(ctx context.Context) *agentTaskRunBox {
	b, _ := ctx.Value(agentTaskRunBoxKey{}).(*agentTaskRunBox)
	return b
}

// planEnvFromProfile 从当前 run 的装配产物取模型 ref 校验口。没有 profile（单测）时
// resolveModel 为 nil，创建服务跳过模型校验——与 planEnv 的约定一致。
func planEnvFromProfile(ctx context.Context) planEnv {
	prof := profileFrom(ctx)
	if prof == nil || prof.Resolver == nil {
		return planEnv{}
	}
	r := prof.Resolver
	return planEnv{resolveModel: func(ctx context.Context, ref string) error {
		_, err := r.Get(ctx, ref)
		return err
	}}
}

// taskCreateErrText 把创建服务的业务错误翻成给模型看的句子。错误经 wrapToolErrors 变成
// 「错误：」开头的 tool 结果回注，模型可以改参数重试。
func taskCreateErrText(err error) string {
	var pe *planError
	switch {
	case errors.As(err, &pe):
		return "任务计划不合法：" + pe.reason
	case errors.Is(err, errPlanConflict):
		return "这个创建键已经用于另一份计划，换一个键或改回原计划"
	case errors.Is(err, errCreateSource):
		return "来源会话无效：不存在、已删除或不是本人的普通聊天会话"
	case errors.Is(err, errCreateWorkspace):
		return "项目无效：必须挂在来源会话上且本人可见"
	case errors.Is(err, errCreateAgent):
		return "找不到执行者规格（用当前可用的子代理名）：" + err.Error()
	case errors.Is(err, errCreateModel):
		return "模型不可用（留空继承或换一个已配置的 ref）：" + err.Error()
	default:
		return "创建任务失败：" + err.Error()
	}
}

// delegateTaskInput 是 delegate_task 的入参 schema（InferTool 反射生成）。
// 与看板工具 TaskCreate 明确分开（doc/70 §5.1）：那把记的是本 run 的待办步骤，
// 这把创建的是独立执行的持久 Task——执行者看不到本会话，content 必须自包含。
type delegateTaskInput struct {
	Content  string `json:"content" jsonschema:"description=任务全文：自包含、离开本会话也能独立执行的工作说明（目标、要写的文件、验收标准）"`
	Agent    string `json:"agent" jsonschema:"description=执行者规格名（当前可用的子代理名）"`
	ModelRef string `json:"modelRef,omitempty" jsonschema:"description=可选；缺省继承该执行者的模型配置"`
}

// newDelegateTaskTool 造 delegate_task：创建一件单节点编排组（持久 Task），立即返回
// {taskId, orchestrationId, status}——不在这次调用里等执行完。幂等键由运行时派生
// （当前 run + 工具调用 id）：每次调用是一次独立的创建意图，resume 重放不会重执行工具，
// 因此不需要模型自己发明键。
func newDelegateTaskTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("delegate_task",
		"把一件工作委派给独立执行者异步完成：立即返回任务号，不等结果；完成/失败会以持久通知回到本会话。"+
			"content 必须自包含（执行者看不到本会话的任何历史，也看不到本次对话）。"+delegationWaitHint,
		func(ctx context.Context, in delegateTaskInput) (string, error) {
			return handleDelegateTask(ctx, store, in)
		})
}

func handleDelegateTask(ctx context.Context, store *Store, in delegateTaskInput) (string, error) {
	box := taskBoxFrom(ctx)
	if box == nil {
		return "", fmt.Errorf("delegate_task 不可用（没有 run 身份）")
	}
	p, ok := principalFromCtx(ctx)
	if !ok {
		return "", fmt.Errorf("delegate_task 不可用（没有租户身份）")
	}
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", fmt.Errorf("delegate_task 不可用（没有工具调用坐标）")
	}
	ws, err := store.RunWorkspace(ctx, box.run)
	if err != nil || ws == "" {
		return "", fmt.Errorf("读取 run 项目失败: %w", err)
	}
	resp, err := store.CreateAgentTask(ctx, p, CreateTaskCommand{
		IdempotencyKey:  string(box.run) + ":" + callID,
		WorkspaceID:     ws,
		SourceSessionID: box.session,
		SourceRunID:     string(box.run),
		Content:         in.Content,
		Agent:           in.Agent,
		ModelRef:        in.ModelRef,
	}, planEnvFromProfile(ctx))
	if err != nil {
		return "", fmt.Errorf("%s", taskCreateErrText(err))
	}
	b, _ := json.Marshal(map[string]string{
		"taskId": resp.TaskID, "orchestrationId": resp.OrchestrationID, "status": resp.Status,
	})
	return string(b), nil
}

// orchestrationTaskInput / orchestrationLinkInput 是 create_orchestration 的节点与边入参。
// 不复用 planTask：那份的 json tag 是 plan_hash 规范化的哈希输入（不能动 omitempty），
// 工具 schema 的形状在这里独立声明、创建前转回 planTask。
type orchestrationTaskInput struct {
	Key      string `json:"key" jsonschema:"description=组内唯一标识（字母数字开头的 1–64 位字母数字、_ 或 -）；依赖边用 key 引用"`
	Content  string `json:"content" jsonschema:"description=任务全文：自包含、离开本会话也能独立执行的工作说明"`
	Agent    string `json:"agent" jsonschema:"description=执行者规格名（当前可用的子代理名）"`
	ModelRef string `json:"modelRef,omitempty" jsonschema:"description=可选；缺省继承该执行者的模型配置"`
}

type orchestrationLinkInput struct {
	Parent string `json:"parent" jsonschema:"description=上游 Task 的 key"`
	Child  string `json:"child" jsonschema:"description=下游 Task 的 key（上游成功后才开跑）"`
}

type createOrchestrationInput struct {
	Goal  string                   `json:"goal" jsonschema:"description=整个编排要达成的目标，写清最终要交付什么"`
	Tasks []orchestrationTaskInput `json:"tasks" jsonschema:"description=全部节点（1–32 件），恰好一个没有下游的节点是最终交付"`
	Links []orchestrationLinkInput `json:"links,omitempty" jsonschema:"description=依赖边：parent 成功后 child 才具备执行资格"`
}

// newCreateOrchestrationTool 造 create_orchestration：一次提交整张初始 DAG，原子建组——
// 不存在「先建 A 再补 B」的中间态。建好后调度程序按依赖与容量自动推进。
func newCreateOrchestrationTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("create_orchestration",
		"一次提交整张固定的任务依赖图（例如并行调研 A、B 后汇总成报告）：原子创建整组，立即返回编排组号；"+
			"各节点完成后按依赖自动推进，整组完成/失败以持久通知回到本会话。"+delegationWaitHint,
		func(ctx context.Context, in createOrchestrationInput) (string, error) {
			return handleCreateOrchestration(ctx, store, in)
		})
}

func handleCreateOrchestration(ctx context.Context, store *Store, in createOrchestrationInput) (string, error) {
	box := taskBoxFrom(ctx)
	if box == nil {
		return "", fmt.Errorf("create_orchestration 不可用（没有 run 身份）")
	}
	p, ok := principalFromCtx(ctx)
	if !ok {
		return "", fmt.Errorf("create_orchestration 不可用（没有租户身份）")
	}
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", fmt.Errorf("create_orchestration 不可用（没有工具调用坐标）")
	}
	ws, err := store.RunWorkspace(ctx, box.run)
	if err != nil || ws == "" {
		return "", fmt.Errorf("读取 run 项目失败: %w", err)
	}
	cmd := CreateOrchestrationCommand{
		IdempotencyKey:  string(box.run) + ":" + callID,
		WorkspaceID:     ws,
		SourceSessionID: box.session,
		SourceRunID:     string(box.run),
		Goal:            in.Goal,
	}
	for _, t := range in.Tasks {
		cmd.Tasks = append(cmd.Tasks, planTask{Key: t.Key, Content: t.Content, Agent: t.Agent, ModelRef: t.ModelRef})
	}
	for _, l := range in.Links {
		cmd.Links = append(cmd.Links, planLink{Parent: l.Parent, Child: l.Child})
	}
	resp, err := store.CreateAgentOrchestration(ctx, p, cmd, planEnvFromProfile(ctx))
	if err != nil {
		return "", fmt.Errorf("%s", taskCreateErrText(err))
	}
	b, _ := json.Marshal(map[string]string{
		"orchestrationId": resp.OrchestrationID, "status": resp.Status,
	})
	return string(b), nil
}

// delegationWaitHint 是两把创建工具共有的行为提示：结果怎么回来（持久通知 + 恢复 run），
// 以及「不要在 content 里要求执行者向你汇报」——执行者只向 submit_task_result 交结果。
const delegationWaitHint = "调用后继续做你自己的事，不要等待；任务失败、中断或整组完成时你会收到通知，" +
	"收到后再决定下一步。"

// submitTaskArtifactInput 是结果里一个产物引用的入参形状。
type submitTaskArtifactInput struct {
	Path string `json:"path" jsonschema:"description=产物文件路径（项目根的相对路径，必须是已经写出的真实文件）"`
	Kind string `json:"kind" jsonschema:"description=产物种类标签，如 report / code / data"`
}

// submitTaskResultInput 是 submit_task_result 的入参 schema（doc/70 §7.1 的载荷形状）。
type submitTaskResultInput struct {
	Summary   string                    `json:"summary" jsonschema:"description=交接摘要：完成状态、关键结论与产物位置；下游执行者只看它续工，不看你这次的过程"`
	Artifacts []submitTaskArtifactInput `json:"artifacts,omitempty" jsonschema:"description=产物清单：写进项目根的文件（相对路径）与种类"`
	Metadata  map[string]any            `json:"metadata,omitempty" jsonschema:"description=可选：给下游的结构化补充信息（键值对）"`
}

// submitErrText 把提交服务的业务错误翻成给模型看的句子。stale / not_running 是终态拒绝：
// 这场执行已经没有资格交付，重试无意义。
func submitErrText(err error) string {
	switch {
	case errors.Is(err, errSubmitStale):
		return "这次执行已经不是当前一代（执行者被接管或已收尾），提交被拒绝，结果不会生效"
	case errors.Is(err, errSubmitNotRunning):
		return "任务不在运行中（已取消或已被对账收尾），提交被拒绝"
	default:
		return err.Error()
	}
}

// newSubmitTaskResultTool 造 submit_task_result（doc/70 §7.1）。taskId / run / epoch 全部取自
// run context；提交只写结果列，Task 翻 succeeded 由对账裁决——所以返回体里的 status 恒是 running
// （此刻的真实状态），模型不凭这个回执宣布「已交付」。
func newSubmitTaskResultTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("submit_task_result",
		"提交这件任务的最终结果（交接摘要 + 产物清单 + 可选补充信息）。完成约定工作、把产物写进项目根之后调用它，"+
			"然后结束本轮。不要把工具回执当作已经成功交付：结果通过程序对账裁决，成功与否以任务状态为准。",
		func(ctx context.Context, in submitTaskResultInput) (string, error) {
			return handleSubmitTaskResult(ctx, store, in)
		})
}

func handleSubmitTaskResult(ctx context.Context, store *Store, in submitTaskResultInput) (string, error) {
	box := taskBoxFrom(ctx)
	if box == nil {
		return "", fmt.Errorf("submit_task_result 不可用（没有 run 身份）")
	}
	jt, ok := journalTargetFrom(ctx)
	if !ok {
		return "", fmt.Errorf("submit_task_result 不可用（没有执行代次）")
	}
	tb := agentTaskRunBoxFrom(ctx)
	if tb == nil {
		return "", fmt.Errorf("submit_task_result 只在任务执行 run 里可用")
	}
	ref, ok := workspaceFrom(ctx)
	if !ok || ref.Root == "" {
		return "", fmt.Errorf("submit_task_result 不可用（没有项目根）")
	}
	result := TaskResult{Summary: in.Summary, Metadata: in.Metadata}
	for _, a := range in.Artifacts {
		result.Artifacts = append(result.Artifacts, TaskArtifact{Path: a.Path, Kind: a.Kind})
	}
	revision, err := store.SubmitTaskResult(ctx, tb.TaskID, jt.runID, jt.epoch, result, ref.Root)
	if err != nil {
		return "", fmt.Errorf("%s", submitErrText(err))
	}
	b, _ := json.Marshal(map[string]any{
		"taskId": tb.TaskID, "resultRevision": revision, "status": agentTaskStatusRunning,
	})
	return string(b), nil
}
