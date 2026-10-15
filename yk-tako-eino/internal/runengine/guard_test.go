// 本文件：overflow guard 的单测（B1）。裸 model 替身直调 guardModel——不打引擎、不碰库，
// 钉住四条行为：成功流原样（同一 reader 对象）；溢出 ∧ 有帧恒上抛（同一 error 对象）且
// 观测行格式钉死；零帧 / window=0 / 非溢出 / 无容器四种让位均静默上抛；Generate 直通。
package runengine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"slices"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// stubChatModel 是 guard 单测的模型替身：Stream 按 streamErr 分流（非空直接错、否则吐
// chunks 流）；Generate 回 stub 值——guard 的成功面与错误面各自有靶。
type stubChatModel struct {
	genMsg     *schema.Message
	genErr     error
	chunks     []*schema.Message
	streamErr  error
	lastStream *schema.StreamReader[*schema.Message] // 最近一次返回的流（guard 原流传递的同一性靶）
}

func (s *stubChatModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return s.genMsg, s.genErr
}

func (s *stubChatModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	s.lastStream = schema.StreamReaderFromArray(s.chunks)
	return s.lastStream, nil
}

func wrapWithGuard(t *testing.T, s *stubChatModel, window int) model.BaseChatModel {
	t.Helper()
	wrapped, err := newOverflowGuard(window).WrapModel(context.Background(), s, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("WrapModel: %v", err)
	}
	return wrapped
}

// captureLog 捕获本测试期间的 log 输出（观测行走 log.Printf 全局口）；t.Cleanup 恢复。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

func boxWithFrames(n int) *foldBox {
	box := newFoldBox(SessionID("s1"), RunID("r1"))
	box.addFrames(n)
	return box
}

func drainStream(t *testing.T, sr *schema.StreamReader[*schema.Message]) []string {
	t.Helper()
	var got []string
	for {
		m, err := sr.Recv()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, m.Content)
	}
}

// ① 成功流零扰动：guard 返回的 reader 与内层同一对象、chunk 逐条原样。
func TestGuardStreamPassthrough(t *testing.T) {
	stub := &stubChatModel{chunks: []*schema.Message{
		schema.AssistantMessage("甲", nil), schema.AssistantMessage("乙", nil)}}
	wrapped := wrapWithGuard(t, stub, 131072)
	sr, err := wrapped.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if sr != stub.lastStream {
		t.Fatal("guard 未原流传递（换了 reader 对象）")
	}
	if got := drainStream(t, sr); !slices.Equal(got, []string{"甲", "乙"}) {
		t.Fatalf("chunk 被扰动: %v", got)
	}
}

// ② 观测行钉死 + 错误同对象上抛：溢出 ∧ 有帧 = 命中；B1 行为零变化的判据 = 观测行之外
// 无任何差异（错误对象同一）。
func TestGuardMidRunOverflowObserved(t *testing.T) {
	buf := captureLog(t)
	overflowErr := errors.New("This model's maximum context length is 131072 tokens. However, you requested 1400065 tokens.")
	stub := &stubChatModel{streamErr: overflowErr}
	wrapped := wrapWithGuard(t, stub, 131072)
	_, err := wrapped.Stream(withFoldBox(context.Background(), boxWithFrames(1)), nil)
	if err != overflowErr {
		t.Fatalf("错误非同一对象: %v", err)
	}
	if !strings.Contains(buf.String(), "overflow guard: 中途拒收（观测；本拍不救）") {
		t.Fatalf("观测行缺失/格式不对: %q", buf.String())
	}
}

// ③ 零帧静默：让位 A2（run-start 拒收走整条重跑链）——上抛但**不打**观测行。
func TestGuardZeroFramesSilent(t *testing.T) {
	buf := captureLog(t)
	overflowErr := errors.New("maximum context length is 131072 tokens")
	stub := &stubChatModel{streamErr: overflowErr}
	wrapped := wrapWithGuard(t, stub, 131072)
	_, err := wrapped.Stream(withFoldBox(context.Background(), boxWithFrames(0)), nil)
	if err != overflowErr {
		t.Fatalf("错误非同一对象: %v", err)
	}
	if strings.Contains(buf.String(), "overflow guard") {
		t.Fatalf("零帧应静默: %q", buf.String())
	}
}

// ④ window=0 直通：本层关——溢出错误也不打行、原样上抛。
func TestGuardWindowOffPassthrough(t *testing.T) {
	buf := captureLog(t)
	overflowErr := errors.New("maximum context length is 131072 tokens")
	stub := &stubChatModel{streamErr: overflowErr}
	wrapped := wrapWithGuard(t, stub, 0)
	_, err := wrapped.Stream(withFoldBox(context.Background(), boxWithFrames(3)), nil)
	if err != overflowErr {
		t.Fatalf("错误非同一对象: %v", err)
	}
	if strings.Contains(buf.String(), "overflow guard") {
		t.Fatalf("window=0 应直通: %q", buf.String())
	}
}

// ⑤ 非溢出错误直通：即使有帧也不打行（命中表/排除表判定在 worker 层已有单测，这里只验
// guard 不误伤）。
func TestGuardNonOverflowErrorPassthrough(t *testing.T) {
	buf := captureLog(t)
	plainErr := errors.New("dial tcp 127.0.0.1:8080: connection refused")
	stub := &stubChatModel{streamErr: plainErr}
	wrapped := wrapWithGuard(t, stub, 131072)
	_, err := wrapped.Stream(withFoldBox(context.Background(), boxWithFrames(1)), nil)
	if err != plainErr {
		t.Fatalf("错误非同一对象: %v", err)
	}
	if strings.Contains(buf.String(), "overflow guard") {
		t.Fatalf("非溢出错误应直通: %q", buf.String())
	}
}

// ⑥ 无容器直通：ctx 没带 foldBox（旁路调用）——不 panic、静默上抛。
func TestGuardNoBoxPassthrough(t *testing.T) {
	buf := captureLog(t)
	overflowErr := errors.New("maximum context length is 131072 tokens")
	stub := &stubChatModel{streamErr: overflowErr}
	wrapped := wrapWithGuard(t, stub, 131072)
	_, err := wrapped.Stream(context.Background(), nil)
	if err != overflowErr {
		t.Fatalf("错误非同一对象: %v", err)
	}
	if strings.Contains(buf.String(), "overflow guard") {
		t.Fatalf("无容器应静默: %q", buf.String())
	}
}

// ⑦ Generate 直通：成功值与错误都原样（主循环走 Stream，此方法仅为接口完备）。
func TestGuardGeneratePassthrough(t *testing.T) {
	stub := &stubChatModel{genMsg: schema.AssistantMessage("ok", nil)}
	wrapped := wrapWithGuard(t, stub, 131072)
	msg, err := wrapped.Generate(context.Background(), nil)
	if err != nil || msg != stub.genMsg {
		t.Fatalf("Generate 成功面被扰动: %v, %v", msg, err)
	}
	genErr := errors.New("gen boom")
	stub.genErr = genErr
	if _, err = wrapped.Generate(context.Background(), nil); err != genErr {
		t.Fatalf("Generate 错误面非同一对象: %v", err)
	}
}
