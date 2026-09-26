// 本文件：REPL 的 sink——把 adkrun 产出的事件写成终端文本（呈现归消费方）。
package main

import (
	"fmt"
	"io"
	"strings"

	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/repl"
)

// newREPLSink 造一个写终端的事件消费函数，输出格式与重构前 adkrun 的 printMessage
// 逐字符一致（《10》§6 关口 2「REPL sink 打印格式不破」）。
//
// w 由调用方给：main 传 os.Stdout，测试传 bytes.Buffer——格式锁定测试靠它做字节级断言。
// root 是主 agent 名：非 root 的 agent 事件带「[名字] 」前缀（原逻辑搬来）。
// 落点选 cmd/yktako 的理由（P2 关闭）：格式是消费方自己的事；将来 HTTP sink 输出的是
// SSE JSON，与终端文本没有可共享的格式化，不必为此新开 internal/sink。
func newREPLSink(w io.Writer, root string) adkrun.Sink {
	return func(ev adkrun.Event) {
		prefix := ""
		if ev.Agent != "" && ev.Agent != root {
			prefix = "[" + ev.Agent + "] "
		}
		switch ev.Kind {
		case adkrun.EventToolCall:
			fmt.Fprintf(w, "  → 🔧 %s调用: %s\n", prefix, strings.Join(ev.ToolNames, ", "))
		case adkrun.EventBoard:
			// 账本工具的回执自带看板与催更块（taskboard/tools.go 的 withBoard），
			// 原样投影，不走 Clip 截断——截断会把看板截烂。看板原文自带
			// 「📋 任务看板」头，故终端显示「  📋 📋 …」两个标记——这是重构前的
			// 既有格式，本重构只搬不改，不许顺手去重。
			fmt.Fprintf(w, "  📋 %s%s\n", prefix, ev.Text)
		case adkrun.EventToolResult:
			fmt.Fprintf(w, "  ← 🔧 %s%s: %s\n", prefix, ev.ToolName, repl.Clip(ev.Text, 120))
		case adkrun.EventMessage:
			fmt.Fprintf(w, "\n🐙 %s%s\n\n", prefix, ev.Text)
		}
	}
}
