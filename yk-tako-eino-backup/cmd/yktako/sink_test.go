// 本文件：REPL sink 的格式锁定测试——逐字符断言终端输出，钉住重构前的
// printMessage 格式（《10》§6 关口 2）；改输出格式必须同步改这里。
package main

import (
	"bytes"
	"strings"
	"testing"

	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/taskboard"
)

// TestREPLSinkFormat 覆盖四种事件 + 子代理前缀（四类模板）+ 空 Agent + 未知 Kind
// + 长回执截断，逐字符锁定终端格式。
func TestREPLSinkFormat(t *testing.T) {
	long := strings.Repeat("a", 130)
	cases := []struct {
		name string
		ev   adkrun.Event
		want string
	}{
		{
			name: "工具调用",
			ev:   adkrun.Event{Kind: adkrun.EventToolCall, Agent: "yk-tako", ToolCalls: []adkrun.ToolCall{{Name: "read_file"}, {Name: "calculate"}}},
			want: "  → 🔧 调用: read_file, calculate\n",
		},
		{
			name: "工具调用带原始参数",
			ev:   adkrun.Event{Kind: adkrun.EventToolCall, Agent: "yk-tako", ToolCalls: []adkrun.ToolCall{{Name: "bash", Args: `{"command":"date"}`}}},
			want: "  → 🔧 调用: bash {\"command\":\"date\"}\n",
		},
		{
			name: "工具参数超长截断",
			ev:   adkrun.Event{Kind: adkrun.EventToolCall, Agent: "yk-tako", ToolCalls: []adkrun.ToolCall{{Name: "bash", Args: long}}},
			want: "  → 🔧 调用: bash " + strings.Repeat("a", 120) + "…\n",
		},
		{
			name: "工具调用带子代理前缀",
			ev:   adkrun.Event{Kind: adkrun.EventToolCall, Agent: "worker", ToolCalls: []adkrun.ToolCall{{Name: "read_file"}}},
			want: "  → 🔧 [worker] 调用: read_file\n",
		},
		{
			name: "工具回执",
			ev:   adkrun.Event{Kind: adkrun.EventToolResult, Agent: "yk-tako", ToolName: "read_file", Text: "第一行\n第二行"},
			want: "  ← 🔧 read_file: 第一行 第二行\n",
		},
		{
			name: "工具回执带子代理前缀",
			ev:   adkrun.Event{Kind: adkrun.EventToolResult, Agent: "worker", ToolName: "read_file", Text: "内容"},
			want: "  ← 🔧 [worker] read_file: 内容\n",
		},
		{
			name: "回执超长截断",
			ev:   adkrun.Event{Kind: adkrun.EventToolResult, Agent: "yk-tako", ToolName: "read_file", Text: long},
			want: "  ← 🔧 read_file: " + strings.Repeat("a", 120) + "…\n",
		},
		{
			// 看板原文自带「📋 任务看板」头（taskboard/tools.go 的 board），sink 又贴
			// 「  📋 」标记——重构前终端就是两个 📋，逐字符锁死，不许顺手去重。
			name: "看板回执不截断",
			ev:   adkrun.Event{Kind: adkrun.EventBoard, Agent: "yk-tako", ToolName: taskboard.ToolList, Text: "📋 看板\n第二行"},
			want: "  📋 📋 看板\n第二行\n",
		},
		{
			name: "看板带子代理前缀",
			ev:   adkrun.Event{Kind: adkrun.EventBoard, Agent: "worker", ToolName: taskboard.ToolList, Text: "📋 看板"},
			want: "  📋 [worker] 📋 看板\n",
		},
		{
			name: "正文",
			ev:   adkrun.Event{Kind: adkrun.EventMessage, Agent: "yk-tako", Text: "你好"},
			want: "\n🐙 你好\n\n",
		},
		{
			name: "子代理带前缀",
			ev:   adkrun.Event{Kind: adkrun.EventMessage, Agent: "worker", Text: "子代理说"},
			want: "\n🐙 [worker] 子代理说\n\n",
		},
		{
			name: "空 Agent 不加前缀",
			ev:   adkrun.Event{Kind: adkrun.EventMessage, Agent: "", Text: "匿名"},
			want: "\n🐙 匿名\n\n",
		},
		{
			name: "未知 Kind 不输出",
			ev:   adkrun.Event{Kind: adkrun.EventKind("unknown"), Agent: "yk-tako", Text: "x"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			newREPLSink(&buf, "yk-tako")(tc.ev)
			if got := buf.String(); got != tc.want {
				t.Fatalf("输出不符\nwant: %q\n got: %q", tc.want, got)
			}
		})
	}
}
