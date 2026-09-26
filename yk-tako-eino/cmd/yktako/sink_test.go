// 本文件：REPL sink 的格式锁定测试——逐字符断言终端输出，钉住重构前的
// printMessage 格式（《10》§6 关口 2）；改输出格式必须同步改这里。
package main

import (
	"bytes"
	"strings"
	"testing"

	"yk-tako-eino/internal/adkrun"
)

// TestREPLSinkFormat 覆盖四种事件 + 子代理前缀 + 长回执截断六种输出。
func TestREPLSinkFormat(t *testing.T) {
	long := strings.Repeat("a", 130)
	cases := []struct {
		name string
		ev   adkrun.Event
		want string
	}{
		{
			name: "工具调用",
			ev:   adkrun.Event{Kind: adkrun.EventToolCall, Agent: "yk-tako", ToolNames: []string{"read_file", "calculate"}},
			want: "  → 🔧 调用: read_file, calculate\n",
		},
		{
			name: "工具回执",
			ev:   adkrun.Event{Kind: adkrun.EventToolResult, Agent: "yk-tako", ToolName: "read_file", Text: "第一行\n第二行"},
			want: "  ← 🔧 read_file: 第一行 第二行\n",
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
			ev:   adkrun.Event{Kind: adkrun.EventBoard, Agent: "yk-tako", ToolName: "TaskList", Text: "📋 看板\n第二行"},
			want: "  📋 📋 看板\n第二行\n",
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
