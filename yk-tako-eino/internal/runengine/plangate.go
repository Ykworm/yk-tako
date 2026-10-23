// plangate.go 是计划模式（doc/47 §3.2）：模型自己判断该不该进、运行时硬保证进了就写不了。
//
// 分工：
//   - enter_plan_mode 工具把 runs.plan_mode 置 true（PG 是跨副本真源），并把本 run 的
//     planGate 置位（进程内快照，工具调用热路径只读它）；
//   - planGuardHandler 注册在 handler 链**最外**（journalWrapHandler 之后 append）：
//     进模式后任何 isMutatingTool 命中的调用在 journal 记账之前就被拒，工具不执行、账本无 intent；
//   - 批准放行由 submit_plan 的答案侧完成（P2）：置 plan_mode=false 后同一轮继续。
//
// 拒绝文本作为普通 tool_result 回到模型（不是错误上抛）：模型能读到原因并改走只读侦察 /
// 提交方案，run 不因一次被拒而中断。
package runengine

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
)

// planPrompt 是计划模式的系统提示段（v1 硬编码，tenant 可配登记为后续拍）。
const planPrompt = `## 计划模式（plan mode）
遇到多步、破坏性或不可逆的任务（批量改文件、删除/移动、执行 shell、改配置），先调用 enter_plan_mode 进入计划模式。
计划模式里只能做只读侦察（读文件、列目录、搜索）；write_file/edit_file/move_file/create_directory/bash 等修改类工具会被运行时拒绝。
侦察清楚后，先把方案的步骤用 TaskCreate 建成看板（≥3 步才建），再调用 submit_plan 提交方案（目标、步骤、要改哪些文件），等用户批准；批准后修改类工具立即放行，按看板逐条执行、用 TaskUpdate 推进状态。
用户驳回方案时，按用户意见改方案后重新 submit_plan；批准前不要尝试绕过拒绝，也不要重复调用同一个被拒工具。`

// planGate 是一場 run 的计划模式快照。进程内只读；真源在 runs.plan_mode，
// resume 时由 Worker.run 从 PG 重新播种。
type planGate struct{ on atomic.Bool }

type planGateKey struct{}

func withPlanGate(ctx context.Context, g *planGate) context.Context {
	return context.WithValue(ctx, planGateKey{}, g)
}

func planGateFrom(ctx context.Context) *planGate {
	g, _ := ctx.Value(planGateKey{}).(*planGate)
	return g
}

// isMutatingTool 是 fail-closed 的只读白名单：叫得出名字的读类工具放行，其余一律当修改类
// （新工具、委派工具、MCP 工具默认被挡）。黑名单式判断在「新增写工具忘了登记」时静默放行，
// 白名单式判断的失败方向是「新工具在计划模式里不可用」——由金样单测逼人当场表态。
func isMutatingTool(name string) bool {
	if name == "" || strings.HasPrefix(name, "mcp__") {
		return true
	}
	switch name {
	case "get_time",
		"read_text_file", "read_file", "read_pdf_text", "read_media_file",
		"list_directory", "directory_tree", "get_file_info", "search_files",
		"search_history", "web_search", "tool_search",
		// 看板是方案的一部分：批准前把步骤写成任务，批准后按它执行。
		// 它只改 run_tasks，不动工作区，不属于 plan gate 要拦的写。
		"TaskList", "TaskGet", "TaskCreate", "TaskUpdate",
		"ask_user", "submit_plan", "enter_plan_mode":
		return false
	}
	return true
}

// planGuardDenies 是守卫与 dev 探针共用的唯一判定式。
func planGuardDenies(planMode bool, name string) bool {
	return planMode && isMutatingTool(name)
}

func planDenyText(name string) string {
	return fmt.Sprintf("错误：当前处于计划模式，工具 %s 会修改状态，已被运行时拒绝执行。请只做只读侦察，用 submit_plan 提交方案，等用户批准后再动手。", name)
}

// planGuardHandler 必须 append 在 journalWrapHandler 之后：eino 按注册序应用 BeforeAgent，
// 后注册的包在更外层——运行时顺序是 guard(journal(tool))，拒绝发生在记账与执行之前。
type planGuardHandler struct {
	*adk.BaseChatModelAgentMiddleware
}

func (h *planGuardHandler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	for i, t := range runCtx.Tools {
		if inner, ok := t.(tool.InvokableTool); ok {
			runCtx.Tools[i] = &planGuardTool{inner: inner}
		}
	}
	return ctx, runCtx, nil
}

type planGuardTool struct {
	inner tool.InvokableTool
}

func (t *planGuardTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *planGuardTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	gate := planGateFrom(ctx)
	if gate == nil {
		return t.inner.InvokableRun(ctx, argumentsInJSON, opts...)
	}
	info, err := t.inner.Info(ctx)
	if err != nil {
		return "", err
	}
	if planGuardDenies(gate.on.Load(), info.Name) {
		return planDenyText(info.Name), nil
	}
	return t.inner.InvokableRun(ctx, argumentsInJSON, opts...)
}

type enterPlanModeInput struct {
	Task string `json:"task,omitempty" jsonschema:"description=一句话说明这次任务的目标（可省略）"`
}

// newEnterPlanModeTool 造 enter_plan_mode：置 PG 真源 + 进程内快照，返回给模型的指引文本。
// 幂等——已经在计划模式里再调一次不会报错。
func newEnterPlanModeTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("enter_plan_mode",
		"进入计划模式：先只读侦察、把方案交给用户批准，批准前修改类工具会被运行时拒绝。"+
			"遇到多步、破坏性或不可逆的任务时先调用它。",
		func(ctx context.Context, in enterPlanModeInput) (string, error) {
			gate := planGateFrom(ctx)
			target, ok := journalTargetFrom(ctx)
			if gate == nil || !ok {
				return "", fmt.Errorf("enter_plan_mode 不可用（没有 run 状态）")
			}
			if err := store.SetPlanMode(ctx, target.runID, target.epoch, true); err != nil {
				return "", fmt.Errorf("计划模式置位失败: %w", err)
			}
			gate.on.Store(true)
			return "已进入计划模式。现在只能做只读侦察；修改类工具会被拒绝。请理解任务后先用 TaskCreate 把步骤建成看板，再用 submit_plan 提交方案，等待用户批准。", nil
		})
}

// PlanMode 读 runs.plan_mode（resume 播种用）。
func (s *Store) PlanMode(ctx context.Context, runID RunID) (bool, error) {
	var rows []bool
	err := s.db.WithContext(ctx).Raw(`SELECT plan_mode FROM runs WHERE id = ?`, runID).Scan(&rows).Error
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, fmt.Errorf("run %s 不存在", runID)
	}
	return rows[0], nil
}

// SetPlanMode 带 epoch 围栏写 plan_mode：被接管的旧执行者写不进去（与账本同一套围栏）。
func (s *Store) SetPlanMode(ctx context.Context, runID RunID, epoch int64, on bool) error {
	res := s.db.WithContext(ctx).Exec(`UPDATE runs SET plan_mode = ? WHERE id = ? AND lease_epoch = ?`, on, runID, epoch)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errJournalGateLost
	}
	return nil
}

// devPlanGateCheck（-dev only，只读诊断口）= 用真判定式回答「这把工具在计划模式里会不会被拒」，
// 不依赖模型、不写状态（doc/47 §6 Q5 收口后保留的形态）。
func (s *Server) devPlanGateCheck(c *gin.Context) {
	if _, ok := s.requirePrincipal(c); !ok {
		return
	}
	var req struct {
		Tool     string `json:"tool"`
		PlanMode *bool  `json:"planMode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Tool) == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	planMode := true
	if req.PlanMode != nil {
		planMode = *req.PlanMode
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		PlanMode bool `json:"planMode"`
		Deny     bool `json:"deny"`
	}{PlanMode: planMode, Deny: planGuardDenies(planMode, req.Tool)})
}

// seedPlanGate 在 run 起手把 PG 真源读进进程内快照。读失败只记日志、按 false 起——
// 与 quota/workspace 的起手读同一处置；正常路径（行刚被 claim）不会失败。
func seedPlanGate(ctx context.Context, store *Store, runID RunID) *planGate {
	gate := &planGate{}
	on, err := store.PlanMode(ctx, runID)
	if err != nil {
		log.Printf("planmode %s: %v", runID, err)
		return gate
	}
	gate.on.Store(on)
	return gate
}
