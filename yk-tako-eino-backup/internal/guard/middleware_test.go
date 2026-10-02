// 本文件：守卫的两个工具包裹钩子。enhanced 工具（read_file 要回图片）只走
// WrapEnhancedInvokableToolCall，少包一层就等于少一道守卫，所以逐层验它拦得住。
package guard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// enhancedProbe 是假工具：记下每次被调用时的参数，回执固定。
type enhancedProbe struct {
	calls []string
	out   *schema.ToolResult
}

// endpoint 把 probe 包成真执行的那一层。
func (p *enhancedProbe) endpoint() adk.EnhancedInvokableToolCallEndpoint {
	return func(_ context.Context, arg *schema.ToolArgument, _ ...tool.Option) (*schema.ToolResult, error) {
		p.calls = append(p.calls, arg.Text)
		return p.out, nil
	}
}

// runEnhanced 走一遍包裹并执行，返回回执、错误和工具被调用的次数。
func runEnhanced(t *testing.T, wrap func(context.Context, adk.EnhancedInvokableToolCallEndpoint, *adk.ToolContext) (adk.EnhancedInvokableToolCallEndpoint, error), toolName, args string, probe *enhancedProbe) (*schema.ToolResult, error) {
	t.Helper()
	endpoint, err := wrap(context.Background(), probe.endpoint(), &adk.ToolContext{Name: toolName})
	if err != nil {
		t.Fatalf("包裹失败: %v", err)
	}
	return endpoint(context.Background(), &schema.ToolArgument{Text: args})
}

// resultText 把回执里的文本片段拼起来。
func resultText(result *schema.ToolResult) string {
	if result == nil {
		return ""
	}
	var text string
	for _, part := range result.Parts {
		if part.Type == schema.ToolPartTypeText {
			text += part.Text
		}
	}
	return text
}

// TestFilesystemEnhancedGuardsPaths 是路径守卫的 enhanced 一侧：敏感文件硬拒、
// 工作区外按配置处置、工作区内放行且参数原样传下去。
func TestFilesystemEnhancedGuardsPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("API_KEY=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "other.txt")
	if err := os.WriteFile(outside, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		options  Options
		path     string
		wantDeny bool
	}{
		{name: "工作区内的普通文件", options: Options{WorkspaceRoot: root}, path: filepath.Join(root, "note.txt")},
		{name: "工作区内的敏感文件", options: Options{WorkspaceRoot: root}, path: filepath.Join(root, ".env"), wantDeny: true},
		{name: "工作区外且配置为拒", options: Options{WorkspaceRoot: root, OutsideWorkspace: Deny}, path: outside, wantDeny: true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			handler := &filesystemHandler{options: item.options}
			probe := &enhancedProbe{out: &schema.ToolResult{}}
			args := `{"path":` + quoteJSON(item.path) + `}`
			result, err := runEnhanced(t, handler.WrapEnhancedInvokableToolCall, "read_file", args, probe)
			if err != nil {
				t.Fatalf("执行出错: %v", err)
			}
			if item.wantDeny {
				if len(probe.calls) != 0 {
					t.Fatalf("该拦的没拦，工具被调用了 %d 次", len(probe.calls))
				}
				if !strings.Contains(resultText(result), "permission denied") {
					t.Fatalf("拒绝文案不对: %q", resultText(result))
				}
				return
			}
			if len(probe.calls) != 1 || probe.calls[0] != args {
				t.Fatalf("放行时参数应原样传下去，实到 %v", probe.calls)
			}
		})
	}
}

// TestToolGuardEnhancedBlocksDangerous 是命令守卫的 enhanced 一侧：命中危险规则就不执行工具。
func TestToolGuardEnhancedBlocksDangerous(t *testing.T) {
	handler := &toolGuardHandler{engine: NewEngine(ModeBlock, nil)}
	probe := &enhancedProbe{out: &schema.ToolResult{}}

	result, err := runEnhanced(t, handler.WrapEnhancedInvokableToolCall, "bash", `{"command":"rm -rf /tmp/x"}`, probe)
	if err != nil {
		t.Fatalf("执行出错: %v", err)
	}
	if len(probe.calls) != 0 {
		t.Fatal("危险命令该被拦下，工具却执行了")
	}
	if !strings.Contains(resultText(result), "拦截") {
		t.Fatalf("拦截文案不对: %q", resultText(result))
	}

	if _, err := runEnhanced(t, handler.WrapEnhancedInvokableToolCall, "bash", `{"command":"ls -la"}`, probe); err != nil {
		t.Fatalf("执行出错: %v", err)
	}
	if len(probe.calls) != 1 {
		t.Fatalf("安全命令该放行，实到调用 %d 次", len(probe.calls))
	}
}

// TestPiiEnhancedMasksTextPartsOnly 是出口脱敏的 enhanced 一侧：文本片段打码，
// 图片片段一个字节都不能动（改了图就废了），而且不写穿工具原本的回执。
func TestPiiEnhancedMasksTextPartsOnly(t *testing.T) {
	secret := "sk-" + strings.Repeat("a", 24)
	b64 := "iVBORw0KGgo="
	original := &schema.ToolResult{Parts: []schema.ToolOutputPart{
		{Type: schema.ToolPartTypeText, Text: "key 是 " + secret},
		{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{
			MessagePartCommon: schema.MessagePartCommon{Base64Data: &b64, MIMEType: "image/png"},
		}},
	}}
	handler := &piiHandler{}
	probe := &enhancedProbe{out: original}

	result, err := runEnhanced(t, handler.WrapEnhancedInvokableToolCall, "read_file", `{"path":"a.txt"}`, probe)
	if err != nil {
		t.Fatalf("执行出错: %v", err)
	}
	masked := resultText(result)
	if strings.Contains(masked, secret) || !strings.Contains(masked, "key 是 ") {
		t.Fatalf("文本片段没打码: %q", masked)
	}
	image := result.Parts[1]
	if image.Image == nil || image.Image.Base64Data == nil || *image.Image.Base64Data != b64 {
		t.Fatal("图片片段被改动了")
	}
	if strings.Contains(original.Parts[0].Text, "*") {
		t.Fatal("打码写穿了工具原本的回执")
	}
}

// quoteJSON 把路径包成 JSON 字符串，省掉测试里手拼引号。
func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
