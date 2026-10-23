// waittools.go 是「等人」工具族（doc/47 §3.1）：工具的 handler 阻塞在 run_questions 的一行上，
// run 保持 running（watchdog 照续租约、不烧 token），人在任意入口答题/批准后工具返回，
// 模型同一轮继续。现场全在 PG：进程死了，resume 按 journal intent + 行重建（Q4）。
//
// ask_user 的三拍（doc/47 §3.3）：
//  1. 落行（kind=question，pending；tool_call_id 让 resume 能把 intent 与行对上）；
//  2. 发 kind=question 帧（name=工具名、args=原始参数 JSON、text=问题全文、messageId=行 id）；
//  3. 轮询该行直到 answered，答案原文作为 tool_result 返回。
package runengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
)

// questionPollInterval 是挂起期读行的节奏（doc/47 §3.3：1–2s）。var 而非 const：
// 单测可调小，生产值不动。
var questionPollInterval = 1500 * time.Millisecond

type askUserInput struct {
	Question string   `json:"question" jsonschema:"description=要问用户的问题（一句话，说清你在哪两个选择之间拿不准）"`
	Options  []string `json:"options,omitempty" jsonschema:"description=可选：2–4 个候选答案，前端渲染成快捷按钮；没有明确候选取值就不要给"`
}

// newAskUserTool 造 ask_user。options 只是答案形态提示：点快捷按钮与自由文本
// 提交的都是同一个 answer 字符串，表与端点无结构差异（doc/47 §5.4）。
func newAskUserTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("ask_user",
		"遇到歧义、缺关键信息（目标文件/范围/环境），或不可逆动作的意图不明时，把问题交给用户确认："+
			"run 会挂起等人回答（不烧 token），答案作为这次工具调用的结果回到同一轮，你继续干活。"+
			"**不要替用户猜**。一次只问一个问题；有明确候选取值时用 options 给 2–4 个快捷选项。",
		func(ctx context.Context, in askUserInput) (string, error) {
			return handleAskUser(ctx, store, in)
		})
}

func handleAskUser(ctx context.Context, store *Store, in askUserInput) (string, error) {
	question := strings.TrimSpace(in.Question)
	if question == "" {
		return "", fmt.Errorf("ask_user 的 question 不能为空")
	}
	row, err := openWaitRow(ctx, store, questionKindQuestion, "ask_user", in, question)
	if err != nil {
		return "", err
	}
	answered, err := waitForAnswer(ctx, store, row.ID)
	if err != nil {
		return "", err
	}
	return answered.Answer, nil
}

// openWaitRow 是 ask_user 与 submit_plan 共用的前两拍：落行（kind 区分问题/方案）→ 发
// kind=question 帧。text 是人读全文（问题 / 方案），args 是原始参数 JSON（前端解析 options）。
func openWaitRow(ctx context.Context, store *Store, kind, toolName string, payload any, text string) (RunQuestion, error) {
	box := taskBoxFrom(ctx)
	if box == nil {
		return RunQuestion{}, fmt.Errorf("%s 不可用（没有 run 身份）", toolName)
	}
	target, ok := journalTargetFrom(ctx)
	if !ok {
		return RunQuestion{}, fmt.Errorf("%s 不可用（没有 run 账本坐标）", toolName)
	}
	p, ok := principalFromCtx(ctx)
	if !ok {
		return RunQuestion{}, fmt.Errorf("%s 不可用（没有租户身份）", toolName)
	}
	ws, err := store.RunWorkspace(ctx, box.run)
	if err != nil {
		return RunQuestion{}, fmt.Errorf("读取 run 项目失败: %w", err)
	}
	args, err := json.Marshal(payload)
	if err != nil {
		return RunQuestion{}, err
	}
	row, err := store.InsertQuestion(ctx, RunQuestion{
		TenantID: p.TenantID, Workspace: ws, RunID: box.run, SessionID: box.session,
		ToolCallID: compose.GetToolCallID(ctx), Kind: kind,
		Question: text, Args: string(args), Status: questionStatusPending,
	})
	if err != nil {
		return RunQuestion{}, fmt.Errorf("问题落库失败: %w", err)
	}
	agent, _ := callerNameFrom(ctx)
	if err := emitQuestionFrame(ctx, store, box.run, target.epoch, agent, row); err != nil {
		return RunQuestion{}, err
	}
	return row, nil
}

// emitQuestionFrame 写一条 kind=question 帧：openWaitRow（首挂）与 resume 悬空重发（Q4）共用。
// 帧字面量写成 `Frame{Kind: "question", …}` 单行——契约快闸的词表测试按这个形状扫。
func emitQuestionFrame(ctx context.Context, store *Store, runID RunID, epoch int64, agent string, row RunQuestion) error {
	accepted, err := store.AppendFrame(ctx, runID, epoch, Frame{Kind: "question", Agent: agent, Name: questionToolName(row.Kind), Args: row.Args, Text: row.Question, MessageID: row.ID})
	if err != nil {
		return fmt.Errorf("问题帧写入失败: %w", err)
	}
	if !accepted {
		return fmt.Errorf("问题帧写入失败：本执行者已不是当前一代")
	}
	return nil
}

// questionToolName 从行 kind 还原发起工具名（帧 name）。kind 与工具当前是 1:1，表不另存列
// （doc/47 §5.1）。
func questionToolName(kind string) string {
	if kind == questionKindPlan {
		return "submit_plan"
	}
	return "ask_user"
}

type submitPlanInput struct {
	Plan string `json:"plan" jsonschema:"description=完整方案：目标、步骤、要改哪些文件/执行什么命令；用户会原样看到并可直接编辑"`
}

// newSubmitPlanTool 造 submit_plan（doc/47 §3.4）：复用 ask_user 的三拍（kind=plan 行），
// 批准侧除注回文本外还置 runs.plan_mode=false 并解除本 run 的守卫——批准即放行。
func newSubmitPlanTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("submit_plan",
		"把方案提交给用户审批：run 会挂起等人（不烧 token），批准后工具返回到同一轮，"+
			"修改类工具从此放行。计划模式里侦察清楚后就调用它。",
		func(ctx context.Context, in submitPlanInput) (string, error) {
			plan := strings.TrimSpace(in.Plan)
			if plan == "" {
				return "", fmt.Errorf("submit_plan 的 plan 不能为空")
			}
			gate := planGateFrom(ctx)
			target, ok := journalTargetFrom(ctx)
			if gate == nil || !ok {
				return "", fmt.Errorf("submit_plan 不可用（没有 run 状态）")
			}
			row, err := openWaitRow(ctx, store, questionKindPlan, "submit_plan", in, plan)
			if err != nil {
				return "", err
			}
			answered, err := waitForAnswer(ctx, store, row.ID)
			if err != nil {
				return "", err
			}
			// 驳回 = 方案没被接受：plan_mode 保持开启、守卫不解，模型改完重新提交。
			if answered.Decision == questionDecisionRevise {
				return planRevisionText(answered.Answer), nil
			}
			// 先写 PG 真源再松进程内快照：写失败 = 不能声称已批准（守卫保持关闭）。
			if err := store.SetPlanMode(ctx, target.runID, target.epoch, false); err != nil {
				return "", fmt.Errorf("批准后解除计划模式失败: %w", err)
			}
			gate.on.Store(false)
			return planApprovalText(answered.Answer), nil
		})
}

// planApprovalText 是批准行的注回文本（doc/47 §5.4）：resume 对账（Q4）复用同一函数，
// 保证「当场批准」与「批准后崩溃再续跑」模型读到的字节一致。
func planApprovalText(answer string) string {
	if strings.TrimSpace(answer) == "" {
		return "方案已批准，按方案执行。"
	}
	return "方案已批准，按方案执行。\n用户的修改：\n" + answer
}

// planRevisionText 是驳回行的注回文本（U1）：明确「没执行、计划模式还在、改完重交」，
// 否则模型容易把驳回读成批准。feedback 是人写的修改意见，可空。
func planRevisionText(feedback string) string {
	text := "方案被驳回，尚未执行；计划模式仍然开启。" +
		"请根据用户意见修改方案，再调用 submit_plan 重新提交；在此之前不要执行修改类操作。"
	if strings.TrimSpace(feedback) != "" {
		text += "\n用户的意见：\n" + feedback
	}
	return text
}

// reconcileResumeWaits 是 resume 的第三分支（doc/47 §3.3）：把账本里缺结果的「等人」调用
// 接回现场。answered 行已由 replayJournal 换成答案/批准文，这里补两件有副作用的：
//  1. 已答 plan 行还欠的「退出 plan mode」——崩溃发生在工具醒来之前（真源写失败 = 报错，
//     不拿「已批准」继续跑）；
//  2. pending 行：先全部重发 question 帧（seedResume 已清空 runlive，天然幂等），再逐行等；
//     答案回来后把重放里该调用的 tool 行换成答案/批准文。
//
// 调用点在 watchdog 起跑之后：继续等人期间租约照续，进程仍可再次被 kill、下次 resume 再接。
func reconcileResumeWaits(ctx context.Context, store *Store, runID RunID, epoch int64, resume *journalReplay) error {
	approve := func() error {
		if err := store.SetPlanMode(ctx, runID, epoch, false); err != nil {
			return fmt.Errorf("批准后解除计划模式失败: %w", err)
		}
		if gate := planGateFrom(ctx); gate != nil {
			gate.on.Store(false)
		}
		return nil
	}
	if resume.PlanApproved {
		if err := approve(); err != nil {
			return err
		}
	}
	for _, row := range resume.Waits {
		if err := emitQuestionFrame(ctx, store, runID, epoch, "", row); err != nil {
			return err
		}
	}
	for _, row := range resume.Waits {
		answered, err := waitForAnswer(ctx, store, row.ID)
		if err != nil {
			return fmt.Errorf("恢复等待 %s: %w", row.ID, err)
		}
		text := answered.Answer
		if row.Kind == questionKindPlan {
			if answered.Decision == questionDecisionRevise {
				text = planRevisionText(answered.Answer)
			} else {
				text = planApprovalText(answered.Answer)
				if err := approve(); err != nil {
					return err
				}
			}
		}
		patchReplayToolText(resume, row.ToolCallID, text)
	}
	return nil
}

// patchReplayToolText 把重放里那条工具行换成等人结果（Rows 与 Turn 是同一份数据的两个投影）。
func patchReplayToolText(r *journalReplay, callID, text string) {
	for i := range r.Rows {
		if r.Rows[i].Role == "tool" && r.Rows[i].ToolCallID == callID {
			r.Rows[i].Text = text
		}
	}
	for i := range r.Turn {
		if r.Turn[i].Role == "tool" && r.Turn[i].ToolCallID == callID {
			r.Turn[i].Text = text
		}
	}
}

// waitForAnswer 是第三拍：读行直到 answered，返回整行（答案 + 方案的 approve/revise）。
// ctx 取消（stopRun / 进程收摊）立即返回，行留 pending（清扫器在 run 终态后收 stale）；
// 进程直接死亡时行也留在库里，resume 对账。
func waitForAnswer(ctx context.Context, store *Store, id string) (RunQuestion, error) {
	ticker := time.NewTicker(questionPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return RunQuestion{}, ctx.Err()
		case <-ticker.C:
			row, err := store.Question(ctx, id)
			if err != nil {
				return RunQuestion{}, fmt.Errorf("读取问题行失败: %w", err)
			}
			switch row.Status {
			case questionStatusAnswered:
				return row, nil
			case questionStatusStale:
				return RunQuestion{}, fmt.Errorf("这个问题已经失效（run 已结束或被清扫）")
			}
		}
	}
}

// askUserPrompt 是追问约定的系统提示段（doc/47 §3.3）。与工具一起装：
// 「提示词和工具，一个都不能少」。
const askUserPrompt = `## 追问澄清

遇到歧义、缺关键信息（目标文件、范围、环境），或不可逆动作的意图不明时，先调 ask_user 问清楚，不要替用户猜。
- 一次只问一个问题，把不确定的点说具体。
- 有明确候选取值（例如两个文件、两套环境）时用 options 给 2–4 个快捷选项；没有就只给问题，让用户自由回答。
- 用户答完，答案会作为工具结果回到这一轮，你继续干活。`
