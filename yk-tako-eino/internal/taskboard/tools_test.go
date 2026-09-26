// 本文件：四个记账工具的输出格式测试——回执、看板、催更块，以及非法输入。
// 单测直接调工具函数，ctx 里没有 runCtx，session 镜像是静默空操作
// （adk.AddSessionValue 在无 session 时直接返回，adk/runctx.go 的 AddSessionValue），
// 镜像的验证在 integration_test.go 的真实 run 里做。
package taskboard

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

// invoke 按名字找到工具并执行，返回输出文本。
// 工具经 utils.InferTool 生成，实现 tool.InvokableTool
// （github.com/cloudwego/eino/components/tool 包 interface.go:42）。
func invoke(t *testing.T, tools []tool.BaseTool, name, args string) string {
	t.Helper()
	for _, bt := range tools {
		info, err := bt.Info(context.Background())
		if err != nil || info.Name != name {
			continue
		}
		it, ok := bt.(tool.InvokableTool)
		if !ok {
			t.Fatalf("%s 未实现 tool.InvokableTool", name)
		}
		out, err := it.InvokableRun(context.Background(), args)
		if err != nil {
			t.Fatalf("%s 执行: %v", name, err)
		}
		return out
	}
	t.Fatalf("工具 %s 不存在", name)
	return ""
}

// newTools 建 Store 并生成四个工具，测试入口。
func newTools(t *testing.T) (*Store, []tool.BaseTool) {
	t.Helper()
	s := NewStore()
	tools, err := makeTools(s)
	if err != nil {
		t.Fatalf("makeTools: %v", err)
	}
	if len(tools) != 4 {
		t.Fatalf("应有 4 个工具，实际 %d", len(tools))
	}
	return s, tools
}

// TestCreateOutput 验证 TaskCreate 输出 = 回执（含 taskId）+ 看板 + 催更块。
func TestCreateOutput(t *testing.T) {
	_, tools := newTools(t)
	out := invoke(t, tools, ToolCreate, `{"subject":"查时间","activeForm":"查时间"}`)
	for _, want := range []string{"已创建任务 #1", "查时间", "📋 任务看板（0/1 已完成）", "⚠️ 还有 1 项未完成"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出应含 %q:\n%s", want, out)
		}
	}
}

// TestUpdateOutput 验证 TaskUpdate 后看板反映新状态；唯一任务完成后催更块消失。
func TestUpdateOutput(t *testing.T) {
	_, tools := newTools(t)
	invoke(t, tools, ToolCreate, `{"subject":"查时间"}`)
	out := invoke(t, tools, ToolUpdate, `{"taskId":"1","status":"in_progress"}`)
	if !strings.Contains(out, "🔄 #1 查时间") {
		t.Fatalf("看板应显示 in_progress:\n%s", out)
	}
	out = invoke(t, tools, ToolUpdate, `{"taskId":"1","status":"completed"}`)
	if !strings.Contains(out, "✅ #1 查时间") || !strings.Contains(out, "1/1 已完成") {
		t.Fatalf("看板应显示 completed:\n%s", out)
	}
	if strings.Contains(out, "⚠️") {
		t.Fatalf("全部完成后不应再有催更块:\n%s", out)
	}
}

// TestUpdateInvalidStatusReturnsError 验证非法 status 作为错误回给模型（不是静默忽略）。
func TestUpdateInvalidStatusReturnsError(t *testing.T) {
	_, tools := newTools(t)
	invoke(t, tools, ToolCreate, `{"subject":"查时间"}`)
	for _, bt := range tools {
		info, _ := bt.Info(context.Background())
		if info.Name != ToolUpdate {
			continue
		}
		_, err := bt.(tool.InvokableTool).InvokableRun(context.Background(), `{"taskId":"1","status":"doing"}`)
		if err == nil || !strings.Contains(err.Error(), "非法 status") {
			t.Fatalf("非法 status 应报错: %v", err)
		}
		return
	}
	t.Fatal("TaskUpdate 不存在")
}

// TestListFiltersDeleted 验证 TaskList 输出不含 deleted 任务。
func TestListFiltersDeleted(t *testing.T) {
	_, tools := newTools(t)
	invoke(t, tools, ToolCreate, `{"subject":"一"}`)
	invoke(t, tools, ToolCreate, `{"subject":"二"}`)
	invoke(t, tools, ToolUpdate, `{"taskId":"1","status":"deleted"}`)
	out := invoke(t, tools, ToolList, `{}`)
	if strings.Contains(out, "#1") || !strings.Contains(out, "#2 二") {
		t.Fatalf("List 应过滤 deleted:\n%s", out)
	}
}

// TestGetOutput 验证 TaskGet 输出含描述与状态。
func TestGetOutput(t *testing.T) {
	_, tools := newTools(t)
	invoke(t, tools, ToolCreate, `{"subject":"查时间","description":"调用时钟工具"}`)
	out := invoke(t, tools, ToolGet, `{"taskId":"1"}`)
	for _, want := range []string{"#1", "查时间", "调用时钟工具", "pending"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Get 输出应含 %q:\n%s", want, out)
		}
	}
}

// TestUnknownTaskIDReturnsError 验证操作不存在的 id 报错。
func TestUnknownTaskIDReturnsError(t *testing.T) {
	_, tools := newTools(t)
	for _, name := range []string{ToolUpdate, ToolGet} {
		for _, bt := range tools {
			info, _ := bt.Info(context.Background())
			if info.Name != name {
				continue
			}
			if _, err := bt.(tool.InvokableTool).InvokableRun(context.Background(), `{"taskId":"99"}`); err == nil {
				t.Fatalf("%s 操作不存在的 id 应报错", name)
			}
		}
	}
}

// TestIsBoardTool 验证四个工具名被认出，其他名字不认。
func TestIsBoardTool(t *testing.T) {
	for _, name := range []string{ToolCreate, ToolUpdate, ToolList, ToolGet} {
		if !IsBoardTool(name) {
			t.Fatalf("%s 应被认作账本工具", name)
		}
	}
	if IsBoardTool("bash") || IsBoardTool("write_todos") {
		t.Fatal("非账本工具不应被认出")
	}
}
