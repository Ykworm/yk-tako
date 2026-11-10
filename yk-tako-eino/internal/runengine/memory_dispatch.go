package runengine

// 本文件：pipeline 模式的 master 记忆写入工具——**只登记派工，不直接写记忆**。
//
// 为什么不是一个直接写库的按钮（doc/65 §4.5）：
//
//	master 调 memory → 程序登记一条 inrun 任务（memory_jobs）→ 提交后发一条 {job_id} 叫醒
//	→ 抽取器比较全部当前记忆 → 在一个事务里写 memory_items
//
// 分成两段是因为「记住 / 改口 / 明确删除」这三件事的判定需要**比全部当前记忆**才能做：
// 只有 master 看到的那点上下文，它不知道哪条旧记忆被这次的话取代了。派工工具只负责
// 把「用户原话 + 目标层」如实记下来，判定留给后台抽取器。
//
// 工具返回的是「已受理」而不是「已写成功」——master 不许对用户说「已经记住了」，
// 因为写库发生在它看不见的另一条链路上（doc/65 §4.5）。
//
// 工具名仍是 `memory`（与 markdown 模式同名、不同形）；**只挂 master**，普通 subagent
// 不得继承（subagent 只能读当场冻结的快照）。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// memoryDispatchDesc 是 pipeline 模式 memory 工具的说明文案。
// 刻意比 markdown 版短：这条工具只登记，不做判断——描述里多说一句「怎么记」反而会诱导模型
// 替抽取器做判定（它看不到全部当前记忆）。
const memoryDispatchDesc = `把用户明确说过的规矩记下来，以后每一场 run 都要遵守。
targetLayer=resident 表示这条规矩每场都要看到；targetLayer=recall 表示事实类、需要时再检索。
quote 必须是用户原话（不要转述、不要加解释）。
只登记，后台会与当前已有记忆比对后写入——所以本轮不会立刻生效，也**不要**对用户说「已经记住了」。`

// memoryDispatchInput 是 pipeline 模式 memory 工具的入参。
// 全部字段 omitempty（schema 层非必填）：条件必填（targetLayer 合法、quote 非空）在工具里校验——
// schema 层必填会让「模型漏填」直接被 provider 拒掉，用户看不到为什么。
type memoryDispatchInput struct {
	TargetLayer string `json:"targetLayer,omitempty" jsonschema:"description=resident=每场 run 都要遵守的规矩；recall=事实类，需要时再检索到"`
	Quote       string `json:"quote,omitempty" jsonschema:"description=用户原话（逐字，不要转述）；用户没说过的不要记"`
}

// newMemoryDispatchTool 造 pipeline 模式的 memory 工具（只登记派工）。
//
// wake 是登记**提交后**的叫醒函数（doc/65 §4.5：提交后才发），由装配处传：
// 消费者起不来（没配 broker）时留空——对账器 30 秒内会补投。
// **装配点在 worker.go 的工具池注册处**；变参是为了让现有装配点
// 不传 wake 也编译——那不是「可选功能」，是「装配顺序上后到」。
//
// 底座故障（DB 断）上抛给 wrapToolErrors 兜底（装配点在 NewWorker，与其余第一方工具同款）。
// 业务拒收（目标层不合法、quote 为空、含密钥）格式化成结果文本回注模型——不进 Go error 通道，
// 让模型能当场改正再试一次。
func newMemoryDispatchTool(store *Store, wake ...MemoryWakeFunc) (tool.BaseTool, error) {
	return utils.InferTool("memory", memoryDispatchDesc, func(ctx context.Context, in memoryDispatchInput) (string, error) {
		box := noteBoxFrom(ctx)
		if box == nil {
			return "", errors.New("memory 工具缺 run 上下文（noteBox 无 source run/seq）")
		}
		layer := strings.TrimSpace(in.TargetLayer)
		switch layer {
		case MemoryLayerResident, MemoryLayerRecall:
		case "":
			return "错误：targetLayer 必填——resident（每场都要遵守的规矩）或 recall（事实类，需要时再检索）。", nil
		default:
			return "错误：targetLayer 只能是 resident 或 recall。", nil
		}
		quote := strings.TrimSpace(in.Quote)
		if quote == "" {
			return "错误：quote 是空的——没有用户原话就不记。", nil
		}
		// 隐形字符与密钥：拒在这一步，别让毒行/密钥进到后台抽取的输入里
		// （markdown 模式是同一条规矩，见 memory_text_guard.go）。
		if id := scanMemoryInvisible(quote); id != "" {
			return fmt.Sprintf("错误：quote 含人眼不可见字符（%s）——它在页面上看不见，却会进以后每一场的系统提示；删掉该字符或用可见写法再记。", id), nil
		}
		if guard := guardMemoryText(quote); guard.Secret {
			return "错误：quote 里有关键词（API Key / 密码）——这类东西不写进记忆。", nil
		}
		// 幂等键：同一场 run 的同一句原话重复登记 → 复用同一行，不堆重复任务。
		dedup := fmt.Sprintf("inrun:%s:%d:%s", box.run, box.userSeq, layer)
		jobID, err := store.RegisterMemoryJob(ctx, MemoryJobSpec{
			RunID:       box.run,
			SessionID:   box.session,
			TenantID:    tenantOf(ctx),
			UserID:      userOf(ctx),
			Workspace:   workspaceOf(ctx),
			Kind:        MemoryJobKindInrun,
			TargetLayer: layer,
			Quote:       quote,
			SourceSeq:   box.userSeq,
			DedupKey:    &dedup,
		})
		if err != nil {
			return "", err
		}
		// 提交后发叫醒（doc/65 §4.5）。发不发得出都不改变「已受理」：
		// 发送失败只记日志（在 wake 内部），对账器 30 秒内会补投。
		for _, w := range wake {
			w(ctx, &MemoryJob{
				ID:        jobID,
				SessionID: box.session,
				TenantID:  tenantOf(ctx),
				Workspace: workspaceOf(ctx),
				Kind:      MemoryJobKindInrun,
			})
		}
		// 回执只说「已受理」——写库发生在另一条链路上（doc/65 §4.5）。
		return fmt.Sprintf("已受理：jobId=%s，targetLayer=%s。后台会与当前已有记忆比对后写入，本轮不会立刻生效——不要对用户说「已经记住了」。", jobID, layer), nil
	})
}

// tenantOf / userOf / workspaceOf 从 ctx 取当前身份与项目。
//
// 取不到就返回空串：工具的登记不因为身份缺失而失败——真正的可见性判定在读侧
// （统一谓词）与写侧（写库前复核）各做一次，登记这一步只负责把来源记下来。
func tenantOf(ctx context.Context) string {
	if ref, ok := workspaceFrom(ctx); ok && ref.TenantID != "" {
		return ref.TenantID
	}
	if p, ok := principalFromCtx(ctx); ok {
		return p.TenantID
	}
	return ""
}

func userOf(ctx context.Context) string {
	if p, ok := principalFromCtx(ctx); ok {
		return p.UserID
	}
	return ""
}

// workspaceOf 取当前项目 id（不是作业根路径——新结构的 workspace 列是项目 id）。
func workspaceOf(ctx context.Context) string {
	if ref, ok := workspaceFrom(ctx); ok {
		return ref.WorkspaceID
	}
	return ""
}
