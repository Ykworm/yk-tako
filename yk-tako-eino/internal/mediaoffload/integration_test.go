// 本文件：走真实 ChatModelAgent 图的端到端测试——read_file 把图片读进来，
// 中间件在同一轮里第二次调模型前把它换成占位符，下一轮一进来就是占位符。
// 假模型记下每次 Generate 收到的消息，断言的就是「模型每一眼看到了什么」。
package mediaoffload

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/guard"
	"yk-tako-eino/internal/tools"
)

// recordingModel 是按剧本吐消息、同时记下每次输入的假模型。
//
// 实现接口：model.ToolCallingChatModel —— github.com/cloudwego/eino/components/model
// 包 interface.go:99（eino v0.9.21），覆盖 Generate / Stream / WithTools。
type recordingModel struct {
	mu      sync.Mutex
	replies []*schema.Message
	seen    [][]*schema.Message
}

// 编译期校验 recordingModel 实现了 model.ToolCallingChatModel。
var _ model.ToolCallingChatModel = (*recordingModel)(nil)

// Generate 实现 model.ToolCallingChatModel 的 Generate：先记下这次收到的消息，再弹一条剧本回复。
func (m *recordingModel) Generate(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, msgs)
	if len(m.replies) == 0 {
		return nil, fmt.Errorf("剧本已用完")
	}
	reply := m.replies[0]
	m.replies = m.replies[1:]
	return reply, nil
}

// Stream 把一条回复拆成单块流。本测试的 Runner 关掉了流式，接口要求实现。
func (m *recordingModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reply, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{reply}), nil
}

// WithTools 实现 model.ToolCallingChatModel 的 WithTools：测试不关心工具绑定，原样返回。
func (m *recordingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// readCall 造一条调用 read_file 的助手消息。
func readCall(id, path string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:   id,
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "read_file",
				Arguments: fmt.Sprintf(`{"path":%q}`, path),
			},
		}},
	}
}

// imageParts 数一批消息里还留着几个图像片段。
func imageParts(msgs []*schema.Message) int {
	n := 0
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, part := range msg.UserInputMultiContent {
			if part.Image != nil {
				n++
			}
		}
	}
	return n
}

// onlyImage 取出这批消息里唯一一个图像片段的字节。调用前先断言过个数。
func onlyImage(t *testing.T, msgs []*schema.Message) []byte {
	t.Helper()
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, part := range msg.UserInputMultiContent {
			if part.Image == nil || part.Image.Base64Data == nil {
				continue
			}
			raw, err := decodeBase64(*part.Image.Base64Data)
			if err != nil {
				t.Fatalf("解不开图像片段: %v", err)
			}
			return raw
		}
	}
	return nil
}

// placeholders 数一批消息里的占位符条数。
func placeholders(msgs []*schema.Message) int {
	n := 0
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, part := range msg.UserInputMultiContent {
			if part.Type == schema.ChatMessagePartTypeText && strings.Contains(part.Text, "已落盘") {
				n++
			}
		}
	}
	return n
}

// TestOffloadLoopThroughAgent 是整个 4b 的回路：
// 同一轮里第一次读图 → 模型看到字节；第二次调模型 → 换成占位符；
// 同一轮里再 read_file 读另一张图 → 模型重新看到字节；
// 下一轮一进来 → 两张图都只剩占位符。
//
// 两次 read_file 读的是不同的文件：同一张图在同一轮里重复出现按去重处理
// （见 TestRewriteSameImageTwiceInTurn），这里要验的是「新读到的图会再给模型看一次」。
func TestOffloadLoopThroughAgent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pngHead := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	rawA := append(append([]byte{}, pngHead...), bytes.Repeat([]byte{0x42}, DefaultMinBytes)...)
	rawB := append(append([]byte{}, pngHead...), bytes.Repeat([]byte{0x43}, DefaultMinBytes)...)
	imageA := filepath.Join(dir, "a.png")
	imageB := filepath.Join(dir, "b.png")
	if err := os.WriteFile(imageA, rawA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imageB, rawB, 0o644); err != nil {
		t.Fatal(err)
	}

	fileTools, err := tools.Files(ctx)
	if err != nil {
		t.Fatalf("tools.Files: %v", err)
	}
	fake := &recordingModel{}
	engine := guard.NewEngine(guard.ModeBlock, nil)
	// 落盘中间件在守卫外面：守卫要看到模型真正会执行的那次调用，落盘只改历史。
	handlers := []adk.ChatModelAgentMiddleware{
		NewHandler(Config{Store: NewStore(filepath.Join(dir, DefaultDir), 0)}),
	}
	handlers = append(handlers, guard.Handlers(engine, guard.Options{WorkspaceRoot: dir})...)
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "tako",
		Description: "测试用",
		Instruction: "你是测试里的 agent。",
		Model:       fake,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: fileTools, ExecuteSequentially: true},
		},
		Handlers:      handlers,
		MaxIterations: 6,
	})
	if err != nil {
		t.Fatalf("NewChatModelAgent: %v", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, EnableStreaming: false, CheckPointStore: adkrun.NewStore()})

	// 第一轮：读两张不同的图，中间不给模型别的事做。
	fake.replies = []*schema.Message{
		readCall("call-1", imageA),
		readCall("call-2", imageB),
		schema.AssistantMessage("看完了", nil),
	}
	history, err := adkrun.Run(ctx, runner, agent, "turn-1", []*schema.Message{schema.UserMessage("看下这张图")}, nil, nil)
	if err != nil {
		t.Fatalf("第一轮: %v", err)
	}

	fake.mu.Lock()
	seen := append([][]*schema.Message(nil), fake.seen...)
	fake.mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("第一轮该调模型 3 次，实到 %d 次", len(seen))
	}
	// 第 2 次：第一次读到的图，模型该看到字节。
	if got := imageParts(seen[1]); got != 1 {
		t.Fatalf("第 2 次调模型时图像片段有 %d 个，想要 1（本轮第一次看到）", got)
	}
	if got := onlyImage(t, seen[1]); !bytes.Equal(got, rawA) {
		t.Fatal("第 2 次调模型时看到的不是第一张图的字节")
	}
	// 第 3 次：第一张换成占位符，刚读回来的第二张留着字节。
	if got := imageParts(seen[2]); got != 1 {
		t.Fatalf("第 3 次调模型时图像片段有 %d 个，想要 1（刚读回来的那张）", got)
	}
	if got := onlyImage(t, seen[2]); !bytes.Equal(got, rawB) {
		t.Fatal("第 3 次调模型时看到的不是第二张图的字节")
	}
	if got := placeholders(seen[2]); got != 1 {
		t.Fatalf("第 3 次调模型时占位符有 %d 条，想要 1（本轮已看过的那张）", got)
	}

	// 两张图都落盘了，字节和原图一致。
	for _, raw := range [][]byte{rawA, rawB} {
		cached, err := os.ReadFile(filepath.Join(dir, DefaultDir, SHA256(raw)+".png"))
		if err != nil {
			t.Fatalf("读缓存文件: %v", err)
		}
		if !bytes.Equal(cached, raw) {
			t.Fatal("缓存里的字节和原图不一致")
		}
	}

	// 第二轮：上一轮的两张图都该只剩占位符。
	fake.replies = []*schema.Message{schema.AssistantMessage("还记得", nil)}
	input := append(append([]*schema.Message{}, history...), schema.UserMessage("图里是什么"))
	if _, err := adkrun.Run(ctx, runner, agent, "turn-2", input, nil, nil); err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	fake.mu.Lock()
	last := fake.seen[len(fake.seen)-1]
	fake.mu.Unlock()
	if got := imageParts(last); got != 0 {
		t.Fatalf("第二轮还留着 %d 个图像片段，想要 0", got)
	}
	if got := placeholders(last); got != 2 {
		t.Fatalf("第二轮占位符有 %d 条，想要 2", got)
	}
}
