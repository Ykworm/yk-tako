// 本文件：溢出修复层二期 B1——WrapModel 装饰观测层（19v6-09 §8 的缝先验活拍）。
//
// 病灶（19v6-09 §8）：一期救援的零帧谓词救不了「读大文件读到一半被拒」——第一轮
// tool_call/tool_result 帧已发、工具副作用已在场，整条 agent.Run 重跑 = 帧双份 + 副作用两遍；
// worker 级没有别的出路。二期把救援点下移到**模型调用点**：拦得住当次流式调用的错误、
// 折掉被拒输入、重调一次——工具不重执行、帧不重发（被拒调用一帧未出）。
//
// B1 只装眼睛不动手：命中「中途拒收」（溢出错误 ∧ 已有帧）打一行观测、错误原样上抛，
// 行为与一期逐字节一致——先在真栈上验活三条缝假设（拦得到错误 / ctx 里有容器 / 错误路径
// 无副作用），救援动作是 B2 的面。
//
// 缝实读（eino v0.9.21，2026-10-12）：WrapModel 在每次模型调用点逐层包裹（wrappers.go:1129
// 逆序包裹、first registered = outermost）——guard 挂 Handlers 链尾 = 最内 user wrapper，
// 模型调用错误冒泡最先到它；eventSender 在用户链之外再包一层，其对错误 `return nil, err`
// 不发事件（wrappers.go Stream/Generate 错误分支）——被拒调用在事件面上无声，B2 的重调
// 不会造成事件双发。子代理装配（agents.go）不含本 guard：装配面没有 Handlers 字段，
// 天然排除（§10 的边界，零排除代码）。
package runengine

import (
	"context"
	"log"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// overflowGuard 是挂在中介链尾的观测中间件（D13）：只覆写 WrapModel，把内层模型包成
// guardModel。windowOn 取自操作者窗标（compactWindow > 0；窗未声明 = 本层关）。
type overflowGuard struct {
	*adk.BaseChatModelAgentMiddleware
	windowOn bool
}

func newOverflowGuard(compactWindow int) *overflowGuard {
	return &overflowGuard{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		windowOn:                     compactWindow > 0,
	}
}

func (g *overflowGuard) WrapModel(_ context.Context, m model.BaseChatModel, _ *adk.ModelContext) (model.BaseChatModel, error) {
	return &guardModel{inner: m, windowOn: g.windowOn}, nil
}

// guardModel 是装饰后的模型：Generate 直通（主循环走 Stream，直通只为接口完备——
// BaseModel 只有这两个方法）；Stream 拦错误分支。
type guardModel struct {
	inner    model.BaseChatModel
	windowOn bool
}

func (m *guardModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.inner.Generate(ctx, input, opts...)
}

// Stream：成功 = 原流原样返回。错误路径——命中（windowOn ∧ IsOverflowError）且容器在
// ctx 里且已有帧 → 打观测行，然后**原样上抛**（B1 行为零变化：上游看到的还是同一个 error
// 对象）；命中但零帧 → 静默上抛（让位 A2：run-start 拒收走整条重跑链，一期形状原样）；
// 其余错误直通。观测行出现 = 缝活体证据：既证错误拦得到、ctx 里拿得到容器（D12 缝勘察
// 第 5 条）、又证已有帧（「中途」而非 run-start）。
func (m *guardModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	stream, err := m.inner.Stream(ctx, input, opts...)
	if err != nil && m.windowOn && IsOverflowError(err) {
		if box := foldBoxFrom(ctx); box != nil && box.frameCount() > 0 {
			log.Printf("overflow guard: 中途拒收（观测；本拍不救）")
		}
	}
	return stream, err
}

// 编译期锚：接口一缺，编译不过（中间件其余方法由 Base 兜底；guardModel 两方法 = BaseModel 全部）。
var _ adk.ChatModelAgentMiddleware = (*overflowGuard)(nil)
var _ model.BaseChatModel = (*guardModel)(nil)
