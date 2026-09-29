// 本文件：压缩循环。算法在 doc/12 第 9 节。这里不调用模型，摘要由 Deps.Summarize 注入。
package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

const (
	// KeepN 是一开始原样保留的已结束轮数。未完成的这一轮不占这个数。
	KeepN = 20
	// maxPlanLoops 防止计数永远不小于 limit 时把摘要模型叫个不停。
	maxPlanLoops = 40
)

// ErrStillOver 表示将要发送的内容仍然不少于窗口大小的 80%。
// 调用方不 Stage，主模型也不被调用。这一轮没有助手回复。
var ErrStillOver = errors.New("压缩之后仍然不少于窗口大小的 80%，这一轮没有发给模型")

// Input 是这一次主模型调用之前手里的消息。
type Input struct {
	// Messages 是即将送给主模型的消息，含已经结束的轮和未完成的这一轮。
	Messages []*schema.Message
	// RunIDs 与已结束轮对齐，来自 Memory.CompletedRunIDs。
	RunIDs []string
	// Window 是该模型一次请求能装下的 token 数。
	Window int
	// Query 是这一轮用户原话，用来做那一次检索。不是整段未完成的工具结果。
	Query string
}

// Output 是这一次循环的结果。Compacted 为假时 Messages 只是原消息的副本，调用方不 Stage。
type Output struct {
	Messages      []*schema.Message
	KeptRunIDs    []string
	DroppedRunIDs []string
	CombinedUser  string
	Compacted     bool
}

// Deps 是循环要用的外部动作。计数和摘要都可以换成测试里的假实现。
type Deps struct {
	// CountPrompt 按第 3 节数将要发送的消息。没有超过 limit 就不进循环。
	CountPrompt func([]*schema.Message) int
	// CountMessages 忽略 Usage，只数这份新消息。循环里的比较用它。
	CountMessages func([]*schema.Message) int
	// Summarize 是一次摘要 Generate。失败则整轮停止，不 Stage。
	Summarize func(ctx context.Context, transcript string) (string, error)
	// WriteRAG 保存离开保留集合的轮。失败只记日志。
	WriteRAG func(runID, body string) error
	// SearchRAG 在同一次压缩里只被调用一次。失败当空结果。
	SearchRAG func(query string) (string, error)
	// Notify 在进入循环时告诉界面正在压缩。
	Notify func(text string)
	// Logf 把压缩过程、摘要正文，以及检索或写入的失败打到调用方的日志。为空则不打。
	Logf func(format string, args ...any)
}

// Limit 是窗口大小的 token 数乘以 4 再除以 5。窗口大小是 256000 时，结果是 204800。
// 大于它才开始压缩。循环要看到严格小于它才停。
func Limit(window int) int {
	if window <= 0 {
		window = FallbackWindow
	}
	return window * 4 / 5
}

// Plan 按第 9 节决定这一次主模型调用要带哪些消息。
// 返回错误时调用方保持原来的 state.Messages，也不 Stage。
func Plan(ctx context.Context, in Input, deps Deps) (Output, error) {
	if deps.CountPrompt == nil || deps.CountMessages == nil {
		return Output{}, fmt.Errorf("compact: 缺少计数")
	}
	limit := Limit(in.Window)
	count := deps.CountPrompt(in.Messages)
	if count <= limit {
		return Output{Messages: deepCopy(in.Messages, false)}, nil
	}
	logf(deps, "compact: 开始。窗口大小 %d，阈值 %d，CountPrompt %d", in.Window, limit, count)
	if deps.Notify != nil {
		deps.Notify("正在压缩")
	}
	completed, unfinished := split(in.Messages, in.RunIDs)
	n := KeepN
	summary := ""
	ragText := ""
	searched := false
	written := map[string]struct{}{}
	drainFrom := -1
	for step := 0; step < maxPlanLoops; step++ {
		if err := ctx.Err(); err != nil {
			return Output{}, err
		}
		keep, drop := cut(completed, n)
		for _, item := range drop {
			if _, ok := written[item.id]; ok {
				continue
			}
			written[item.id] = struct{}{}
			if deps.WriteRAG == nil {
				continue
			}
			if err := deps.WriteRAG(item.id, renderTurn(item)); err != nil {
				logf(deps, "compact: 写入检索失败: %v", err)
			}
		}
		if !searched {
			searched = true
			if deps.SearchRAG != nil {
				text, err := deps.SearchRAG(in.Query)
				if err != nil {
					logf(deps, "compact: 检索失败: %v", err)
				} else {
					ragText = text
				}
			}
		}
		if summary == "" && len(drop) == 0 {
			if n == 0 {
				logDrain(deps, drainFrom, 0)
				logf(deps, "compact: 没有已结束的轮可换，未完成的这一轮仍然不少于阈值 %d，这一轮不发给模型", limit)
				return Output{}, ErrStillOver
			}
			if drainFrom < 0 {
				drainFrom = n
			}
			n--
			continue
		}
		logDrain(deps, drainFrom, n)
		drainFrom = -1
		logf(deps, "compact: n=%d，保留 %s，换掉 %s", n, formatIDs(turnIDs(keep)), formatIDs(turnIDs(drop)))
		transcript, showRAG := summaryInput(summary, ragText, drop, n)
		if deps.Summarize == nil {
			return Output{}, fmt.Errorf("compact: 缺少摘要")
		}
		logf(deps, "compact: 生成摘要，输入 %d 个字", len([]rune(transcript)))
		next, err := deps.Summarize(ctx, transcript)
		if err != nil {
			logf(deps, "compact: 摘要失败: %v", err)
			return Output{}, err
		}
		next = strings.TrimSpace(next)
		if next == "" {
			logf(deps, "compact: 摘要为空")
			return Output{}, fmt.Errorf("compact: 摘要为空")
		}
		summary = next
		logf(deps, "compact: 摘要正文：\n%s", summary)
		assembled := assemble(summary, showRAG, ragText, keep, unfinished)
		assembledCount := deps.CountMessages(assembled)
		if assembledCount < limit {
			logf(deps, "compact: 拼装后 CountMessages %d < 阈值 %d，结束。留下 %s，换掉 %s",
				assembledCount, limit, formatIDs(turnIDs(keep)), formatIDs(turnIDs(drop)))
			return Output{
				Messages:      assembled,
				KeptRunIDs:    turnIDs(keep),
				DroppedRunIDs: turnIDs(drop),
				CombinedUser:  CombineUser(summary, in.Query),
				Compacted:     true,
			}, nil
		}
		if n == 0 && showRAG {
			// 已结束的轮少于 20 时，第一次摘要就落在 n 为 0。这一档摘要的是被换掉的原文，
			// 检索块也可能还单独占一条消息，还不是「摘要本身」。再走一圈：输入改成摘要，检索块不再单独放入。
			logf(deps, "compact: 拼装后 CountMessages %d，仍不少于阈值 %d。这一档摘要的是原文，再压一次摘要本身", assembledCount, limit)
			continue
		}
		if n == 0 {
			logf(deps, "compact: 拼装后 CountMessages %d，仍不少于阈值 %d，这一轮不发给模型", assembledCount, limit)
			return Output{}, ErrStillOver
		}
		logf(deps, "compact: 拼装后 CountMessages %d，仍不少于阈值 %d，n 改为 %d", assembledCount, limit, n-1)
		n--
	}
	logf(deps, "compact: 循环达到 %d 次，这一轮不发给模型", maxPlanLoops)
	return Output{}, ErrStillOver
}

// logDrain 把连续好几档都没有轮可换合成一行。n 从 20 逐档下降时不再各打一行。
func logDrain(deps Deps, from, to int) {
	if from < 0 {
		return
	}
	logf(deps, "compact: n 从 %d 降到 %d，这些档没有已结束的轮可换", from, to)
}

// logf 在 Logf 为空时什么都不做。
func logf(deps Deps, format string, args ...any) {
	if deps.Logf == nil {
		return
	}
	deps.Logf(format, args...)
}

// formatIDs 把轮次 id 连成一行。没有 id 时写「无」，避免终端上出现空括号。
func formatIDs(ids []string) string {
	if len(ids) == 0 {
		return "无"
	}
	return strings.Join(ids, ",")
}

// cut 按当前的 n 把已结束轮分成保留和换掉。n 为 0 时一轮都不留。
func cut(completed []turn, n int) (keep, drop []turn) {
	if n <= 0 {
		return nil, completed
	}
	keepCount := n
	if keepCount > len(completed) {
		keepCount = len(completed)
	}
	cutAt := len(completed) - keepCount
	return completed[cutAt:], completed[:cutAt]
}

// summaryInput 决定这一次摘要看什么。
// 还没有摘要时看被换掉的轮，并允许把检索结果单独放进拼装。
// 已经有摘要、n 还没到 0 时，看上一份摘要和那一次检索结果，检索结果写进新摘要。
// n 为 0 时只再压摘要本身。
func summaryInput(summary, ragText string, drop []turn, n int) (transcript string, showRAG bool) {
	if summary == "" {
		return renderTurns(drop), true
	}
	if n == 0 || ragText == "" {
		return summary, false
	}
	return summary + "\n" + ragText, false
}

// assemble 拼出这一次要发送的消息。保留轮和未完成轮都是新对象，Usage 被清掉。
func assemble(summary string, showRAG bool, ragText string, keep []turn, unfinished []*schema.Message) []*schema.Message {
	var out []*schema.Message
	if summary != "" {
		out = append(out, mark(schema.UserMessage(SummaryHeading+"\n"+summary), "summary"))
	}
	if showRAG && ragText != "" {
		out = append(out, mark(schema.UserMessage(ragHeading+"\n"+ragText), "rag"))
	}
	for _, item := range keep {
		out = append(out, deepCopy(item.msgs, true)...)
	}
	out = append(out, deepCopy(unfinished, true)...)
	return out
}

// turnIDs 按原顺序取出轮次 id。
func turnIDs(turns []turn) []string {
	if len(turns) == 0 {
		return nil
	}
	out := make([]string, len(turns))
	for i, item := range turns {
		out[i] = item.id
	}
	return out
}
