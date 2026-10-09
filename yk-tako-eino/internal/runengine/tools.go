// 本文件是 T2 的首个工具（get_time）：无副作用、可 curl 造，专为逼出工具环的全部契约面
// （tool_call/tool_result 帧、runlog 扩展行、mh 的 tool_calls/tool_call_id 两列）而选。
//
// 装配在 NewWorker（ToolsConfig）——工具环本体是 ADK 内建的：模型给出 tool_calls → ToolsNode
// 执行 → 结果消息回注 → 模型续答；本文件只负责「工具是什么」（名字/描述/schema/执行体）。
package runengine

import (
	"context"
	"fmt"
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
