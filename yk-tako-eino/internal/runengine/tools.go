// 本文件是 T2 的首个工具（get_time）：无副作用、可 curl 造，专为逼出工具环的全部契约面
// （tool_call/tool_result 帧、runlog 扩展行、mh 的 tool_calls/tool_call_id 两列）而选。
//
// 装配在 NewWorker（ToolsConfig）——工具环本体是 ADK 内建的：模型给出 tool_calls → ToolsNode
// 执行 → 结果消息回注 → 模型续答；本文件只负责「工具是什么」（名字/描述/schema/执行体），
// 外加工具错误的统一回注包装（wrapToolErrors，F1 起在装配点包住全部第一方工具）。
package runengine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// getTimeInput 是 get_time 的入参 schema：由 InferTool 按 struct tag 反射生成
// （eino-contrib/jsonschema：带 omitempty 的字段非必填）。描述写清楚给模型看。
type getTimeInput struct {
	Timezone string `json:"timezone,omitempty" jsonschema:"description=IANA 时区名，如 Asia/Shanghai；缺省取服务器本地时区"`
}

// newGetTimeTool 造 get_time 工具。返回文本形如 "2026-10-08 15:04:05 +0800 CST"——
// 纯文本直出（字符串输出不套 JSON 引号），可被 curl 一眼验证。
func newGetTimeTool() (tool.BaseTool, error) {
	return utils.InferTool("get_time", "查当前时间。可给 IANA 时区名（如 Asia/Shanghai）；返回该时区的当前时间文本。",
		func(_ context.Context, in getTimeInput) (string, error) {
			loc := time.Local
			if in.Timezone != "" {
				l, err := time.LoadLocation(in.Timezone)
				if err != nil {
					return "", fmt.Errorf("未知时区 %q: %w", in.Timezone, err)
				}
				loc = l
			}
			return time.Now().In(loc).Format("2006-01-02 15:04:05 -0700 MST"), nil
		})
}

// wrapToolErrors 把第一方工具包成「错误 → 结果」。为什么必须有这一层（2026-10-11 F1 实读
// eino v0.9.21 源码，纠正 19v6-08 §2 原「ADK 内建语义」表述）：compose.ToolsNode 对工具返回的
// error 是**直接失败**——非 interrupt 一律 `failed to invoke tool[name:id]` 上抛（tool_node.go
// Invoke/Stream 两条路径同式），run 随之 error；而工具环契约（19v6-08 §3）要求错误作为 tool
// 结果回注模型、run 照跑。WrapToolWithErrorHandler 是 eino 为此提供的出口：工具照常返回
// Go error，包装层转成字符串结果、error 不再上抛（interrupt 类错误原样透传，不挡未来 HITL）。
// 统一在装配点包（NewWorker 的池内 wrap、子代理继承的也是包装后的实例），一条语义管全部工具。
func wrapToolErrors(t tool.BaseTool) tool.BaseTool {
	return utils.WrapToolWithErrorHandler(t, toolErrorText)
}

// toolErrorText 是错误回注文本的唯一出口：契约「错误文本一律『错误：』开头」（19v6-08 §3）
// 在这一处机械保证——工具侧错误消息照常写、不重复带前缀，也不会漏写。
// 另剥一层 eino 外壳：InferTool 会在函数错误外包「[LocalFunc] failed to invoke tool,
// toolName=…, err=…」（invokable_func.go:199 等，v0.9.21 固定格式；Enhanced 前缀同理）——
// 这段实现噪声明文进的是模型输入与 runlog 展示行（「tool_result 行 text 可直接展示」，F1 活体
// curl 实测所见），在这里剥掉、只留原因句；剥不出来（eino 将来改格式）则原文照发，不误伤。
func toolErrorText(_ context.Context, err error) string {
	msg := err.Error()
	for _, prefix := range []string{"[LocalFunc] ", "[EnhancedLocalFunc] "} {
		if strings.HasPrefix(msg, prefix) {
			if i := strings.Index(msg, ", err="); i >= 0 {
				msg = msg[i+len(", err="):]
			}
			break
		}
	}
	if strings.HasPrefix(msg, "错误：") {
		return msg
	}
	return "错误：" + msg
}
