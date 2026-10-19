// 本文件：overflow guard 的单测——B1 观测（既有几条原样回归）+ B2 救援链 + B3 交接（槽/staged）。
// 裸 model 替身直调 guardModel——不打引擎、不碰库，钉住行为面：
//
//	判据四条件（零帧不救 / window=0 不救 / 闸花不救 / 全真才救）；折败上抛原始错误（闸已花）；
//	重调再拒上抛第二次错误；happy path 的重调输入形状（system 原样 + 折后行、无思考残留）、
//	rescue 行格式与交接槽形状（折后行 + 救援点行数）；base 换代不在 guard（归采用点 1）。Generate 直通。
package runengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// stubChatModel 是 guard 单测的模型替身：Stream 按 errs（按调用序）分流——第 i 次调用取
// errs[i]（越界回落 streamErr），非空直接错、否则吐 chunks 流；每次调用的输入进 inputs
// （B2 重调断言的转录）。Generate 回 stub 值——guard 的成功面与错误面各自有靶。
type stubChatModel struct {
	genMsg     *schema.Message
	genErr     error
	chunks     []*schema.Message
	streamErr  error                                 // 缺省错误：每个调用都用它（既有单调用测试的形态）
	errs       []error                               // 按调用序覆盖（nil 项 = 该次成功）
	inputs     [][]*schema.Message                   // 每次调用的输入转录
	lastStream *schema.StreamReader[*schema.Message] // 最近一次返回的流（原流传递断言的同一性靶）
}

func (s *stubChatModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return s.genMsg, s.genErr
}

func (s *stubChatModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	s.inputs = append(s.inputs, input)
	i := len(s.inputs) - 1
	err := s.streamErr
	if i < len(s.errs) {
		err = s.errs[i]
	}
	if err != nil {
		return nil, err
	}
	s.lastStream = schema.StreamReaderFromArray(s.chunks)
	return s.lastStream, nil
}

// overflowErr128k 是 OpenAI 系包壳形状的溢出报错（fixtures 同款；真值窗 = 128000）。
func overflowErr128k() error {
	return errors.New(`error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 128000 tokens. However, you requested 150000 tokens (149984 in the messages, 16 in the completion).`)
}

// wrapWithGuard 按生产默认装配包装（计数 = 字节数、摘要恒 "SUM"、fileEffects = 第一方表；
// B2 救援链的走位由各测试自己的 fixture 决定，这里只给可跑的默认件）。
func wrapWithGuard(t *testing.T, s *stubChatModel, window int) model.BaseChatModel {
	t.Helper()
	return wrapWithGuardDeps(t, s, window, fakeCounter{func(text string) int { return len(text) }},
		func(context.Context, string) (string, error) { return "SUM", nil }, firstPartyFileEffects)
}

func wrapWithGuardDeps(t *testing.T, s *stubChatModel, window int, counter TokenCounter,
	summarize func(context.Context, string) (string, error), fileEffects map[string]string) model.BaseChatModel {
	t.Helper()
	wrapped, err := newOverflowGuard(window, counter, summarize, fileEffects).WrapModel(context.Background(), s, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("WrapModel: %v", err)
	}
	return wrapped
}

// captureLog 捕获本测试期间的 log 输出（观测行/rescue 行走 log 全局口）；t.Cleanup 恢复。
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

// boxForRescue 造 B2 救援用的容器：base 齐 + 帧数齐 + 起手估算故意填 999999——rescue 行若
// 误读 box.countBase() 而不是被拒输入的现算估算，各测试对精确估算值的断言会立刻抓住。
func boxForRescue(base []ModelMessage, frames int) *foldBox {
	box := newFoldBox(SessionID("s1"), RunID("r_cur"))
	box.setBase(base)
	box.setBaseCount(999999)
	box.addFrames(frames)
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

// ---- B2 救援 happy path：四条件全真 = 折被拒输入 + 重调一次 ----

// 料：sys + r1/r2 两个老 run（各一条 500 字符 user 行）+ 当前 run 的 user/工具往返。
// 折叠走位（fakeCounter=字节数）：limit' = C_rej×4/5 ≈ 870 → phase1 n=1 档（drop r1、keep r2）
// 即收敛——重调输入 = [sys] + [摘要行, r2, 当前 user, 工具往返]，共 6 条。
func TestGuardRescueHappyPath(t *testing.T) {
	buf := captureLog(t)
	x500, y500 := strings.Repeat("X", 500), strings.Repeat("Y", 500)
	base := []ModelMessage{
		{RunID: "r1", Seq: 1, Role: "user", Text: x500},
		{RunID: "r2", Seq: 1, Role: "user", Text: y500},
		{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
	}
	asst := schema.AssistantMessage("", getTimeCall("c1"))
	asst.ReasoningContent = "上一枪的思考（重调输入不该带它）"
	input := []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage(x500), schema.UserMessage(y500),
		schema.UserMessage("问"), asst, schema.ToolMessage("12:00", "c1")}
	// C_rej 的期望值 = 现役口径按被拒输入转录现算（与 guard 内部同一条 countInput）——
	// 顺带把「误读 box.baseCount」挡死（那是 999999）。
	counter := fakeCounter{func(text string) int { return len(text) }}
	histWant := []ModelMessage{
		base[0], base[1], base[2],
		{RunID: "r_cur", Seq: 2, Role: "assistant", ToolCalls: getTimeCall("c1")},
		{RunID: "r_cur", Seq: 3, Role: "tool", Text: "12:00", ToolCallID: "c1"},
	}
	wantEst := countInput(counter, "sys", histWant)

	e1 := overflowErr128k()
	stub := &stubChatModel{errs: []error{e1, nil}, chunks: []*schema.Message{
		schema.AssistantMessage("接着答", nil)}}
	box := boxForRescue(base, 1)
	wrapped := wrapWithGuardDeps(t, stub, 131072, counter,
		func(context.Context, string) (string, error) { return "SUM", nil }, firstPartyFileEffects)

	sr, err := wrapped.Stream(withFoldBox(context.Background(), box), input)
	if err != nil {
		t.Fatalf("救援未接住：%v", err)
	}
	if got := drainStream(t, sr); !slices.Equal(got, []string{"接着答"}) {
		t.Fatalf("重调流内容: %v", got)
	}
	// ① 重调恰一次、输入形状 = [system 原样] + 折后行。
	if len(stub.inputs) != 2 {
		t.Fatalf("inner 调用次数 = %d, want 2（首拒 + 重调）", len(stub.inputs))
	}
	if got := stub.inputs[0]; len(got) != len(input) {
		t.Fatalf("首调输入被改动: %d 条", len(got))
	}
	assertContents(t, stub.inputs[1], "sys", "SUM", y500, "问", "", "12:00")
	// ② 重调输入无思考残留（折后行由 mh 形状重建，本不带 ReasoningContent）。
	for i, m := range stub.inputs[1] {
		if m.ReasoningContent != "" {
			t.Fatalf("重调输入第 %d 条带思考残留: %q", i, m.ReasoningContent)
		}
	}
	// ③ rescue 行格式钉死；估算 = C_rej（不是 box.baseCount 的 999999）。
	rescueLine := fmt.Sprintf("overflow guard rescue: provider 拒收（估算 %d tok；真值 128000）；强折 1 个 run，摘要 3 tok；重试", wantEst)
	if !strings.Contains(buf.String(), rescueLine) {
		t.Fatalf("rescue 行缺失/格式不对:\nwant 含 %q\ngot %q", rescueLine, buf.String())
	}
	// ④ 闸已花（一次性，D5）。
	if box.tryMarkRecovered() {
		t.Fatal("救援后闸还能花（应已花）")
	}
	// ⑤ B3 交接：staged 进现役列表、槽写齐（折后行 + 救援点行数 = 被拒输入条数）；base /
	// baseCount 仍不动（base 换代归采用点 1，不在 guard）。
	assertRows(t, box.baseRows(), base)
	if got := box.stagedList(); len(got) != 1 || got[0].AttachRun != "r1" || got[0].Summary != "SUM" {
		t.Fatalf("staged = %+v, want 1 条挂 r1、摘要 SUM", got)
	}
	if box.countBase() != 999999 {
		t.Fatalf("baseCount 被改: %d", box.countBase())
	}
	slot, slotLen := box.takeAdopted()
	assertRows(t, slot, []ModelMessage{
		{RunID: "r1", Seq: 0, Role: "user", Text: "SUM"},
		{RunID: "r2", Seq: 1, Role: "user", Text: y500},
		{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
		{RunID: "r_cur", Seq: 2, Role: "assistant", ToolCalls: getTimeCall("c1")},
		{RunID: "r_cur", Seq: 3, Role: "tool", Text: "12:00", ToolCallID: "c1"},
	})
	if slotLen != len(input) {
		t.Fatalf("救援点行数 = %d, want %d（被拒输入条数）", slotLen, len(input))
	}
	if again, _ := box.takeAdopted(); again != nil {
		t.Fatal("槽取走应清空（一次救援只有一个采用点消费）")
	}
}

// ---- 判据四条件：不救的三条 + 闸花的观测行 ----

// ① 零帧不救：让位 A2（run-start 拒收走整条重跑链）——静默上抛、零重调、闸也不花（A2 要用）。
func TestGuardZeroFramesSilent(t *testing.T) {
	buf := captureLog(t)
	overflowErr := overflowErr128k()
	stub := &stubChatModel{streamErr: overflowErr}
	wrapped := wrapWithGuard(t, stub, 131072)
	box := boxForRescue(nil, 0)
	_, err := wrapped.Stream(withFoldBox(context.Background(), box), nil)
	if err != overflowErr {
		t.Fatalf("错误非同一对象: %v", err)
	}
	if len(stub.inputs) != 1 {
		t.Fatalf("零帧不该重调: inner 调用 %d 次", len(stub.inputs))
	}
	if strings.Contains(buf.String(), "overflow guard") {
		t.Fatalf("零帧应静默: %q", buf.String())
	}
	if !box.tryMarkRecovered() {
		t.Fatal("零帧花了闸（A2 谓词要它未花）")
	}
}

// ② window=0 不救：本层关——溢出错误也不打行、原样上抛、不重调、闸不花。
func TestGuardWindowOffPassthrough(t *testing.T) {
	buf := captureLog(t)
	overflowErr := overflowErr128k()
	stub := &stubChatModel{streamErr: overflowErr}
	wrapped := wrapWithGuard(t, stub, 0)
	box := boxForRescue(nil, 3)
	_, err := wrapped.Stream(withFoldBox(context.Background(), box), nil)
	if err != overflowErr {
		t.Fatalf("错误非同一对象: %v", err)
	}
	if len(stub.inputs) != 1 {
		t.Fatalf("window=0 不该重调: inner 调用 %d 次", len(stub.inputs))
	}
	if strings.Contains(buf.String(), "overflow guard") {
		t.Fatalf("window=0 应直通: %q", buf.String())
	}
	if !box.tryMarkRecovered() {
		t.Fatal("window=0 花了闸")
	}
}

// ③ 闸花不救：本 run 已救过一次——保留 B1 观测行原样上抛（同一闸换载体后，此前救援的痕迹）。
func TestGuardGateSpentLogsObservation(t *testing.T) {
	buf := captureLog(t)
	overflowErr := overflowErr128k()
	stub := &stubChatModel{streamErr: overflowErr}
	wrapped := wrapWithGuard(t, stub, 131072)
	box := boxForRescue(nil, 1)
	box.tryMarkRecovered() // 模拟此前已救一次（guard 救援或 A3 强折闸都花的是它）
	_, err := wrapped.Stream(withFoldBox(context.Background(), box), nil)
	if err != overflowErr {
		t.Fatalf("错误非同一对象: %v", err)
	}
	if len(stub.inputs) != 1 {
		t.Fatalf("闸花不该重调: inner 调用 %d 次", len(stub.inputs))
	}
	if !strings.Contains(buf.String(), "overflow guard: 中途拒收（观测；本拍不救）") {
		t.Fatalf("观测行缺失/格式不对: %q", buf.String())
	}
	if strings.Contains(buf.String(), "rescue") {
		t.Fatalf("闸花不该有 rescue 行: %q", buf.String())
	}
}

// ---- 负路径：折败 / 重调再拒 ----

// 折败：无可折原料（历史只有当前 run 的 user 行 → errStillOver）→ 原始错误对象上抛、
// 闸已花、零重调、无 rescue 行（救援链自身失败 = 原始错误走既有 error 出口）。
func TestGuardRescueFoldFailThrowsOriginal(t *testing.T) {
	buf := captureLog(t)
	overflowErr := overflowErr128k()
	stub := &stubChatModel{streamErr: overflowErr}
	wrapped := wrapWithGuard(t, stub, 131072)
	base := []ModelMessage{{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"}}
	box := boxForRescue(base, 1)
	input := []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("问")}
	_, err := wrapped.Stream(withFoldBox(context.Background(), box), input)
	if err != overflowErr {
		t.Fatalf("折败应上抛原始错误对象: %v", err)
	}
	if len(stub.inputs) != 1 {
		t.Fatalf("折败不该重调: inner 调用 %d 次", len(stub.inputs))
	}
	if box.tryMarkRecovered() {
		t.Fatal("折败也应闸已花（闸先花）")
	}
	if strings.Contains(buf.String(), "rescue") {
		t.Fatalf("折败无 rescue 行: %q", buf.String())
	}
}

// 重调再拒：上抛第二次的错误（首次拒已随 rescue 行进 guard 日志；闸已花不再救）。
func TestGuardRescueRetryRejectsSecondError(t *testing.T) {
	buf := captureLog(t)
	x500, y500 := strings.Repeat("X", 500), strings.Repeat("Y", 500)
	base := []ModelMessage{
		{RunID: "r1", Seq: 1, Role: "user", Text: x500},
		{RunID: "r2", Seq: 1, Role: "user", Text: y500},
		{RunID: "r_cur", Seq: 1, Role: "user", Text: "问"},
	}
	input := []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage(x500), schema.UserMessage(y500),
		schema.UserMessage("问"), schema.AssistantMessage("", getTimeCall("c1")), schema.ToolMessage("12:00", "c1")}
	e1 := overflowErr128k()
	e2 := errors.New(`error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 128000 tokens. However, you requested 131000 tokens (130984 in the messages, 16 in the completion).`)
	stub := &stubChatModel{errs: []error{e1, e2}}
	box := boxForRescue(base, 1)
	wrapped := wrapWithGuard(t, stub, 131072)

	_, err := wrapped.Stream(withFoldBox(context.Background(), box), input)
	if err != e2 {
		t.Fatalf("应上抛第二次的错误对象: %v", err)
	}
	if len(stub.inputs) != 2 {
		t.Fatalf("inner 调用次数 = %d, want 2（首拒 + 重调）", len(stub.inputs))
	}
	if !strings.Contains(buf.String(), "overflow guard rescue: provider 拒收（估算 ") {
		t.Fatalf("rescue 行缺失（首次拒的日志痕迹）: %q", buf.String())
	}
	if box.tryMarkRecovered() {
		t.Fatal("重调再拒后闸应已花")
	}
	// B3：交接写在重调之前（折成即写）——重调再拒时 run 走 error、EndRun 零写入，容器写作废（无害）。
	if len(box.stagedList()) != 1 {
		t.Fatalf("折成后 staged 应已写（重调再拒不回收）: %+v", box.stagedList())
	}
	if slot, _ := box.takeAdopted(); slot == nil {
		t.Fatal("折成后槽应已写（重调再拒不回收）")
	}
}

// ---- B1 既有行为回归（观测行升级后原样保留的四条） ----

// 成功流零扰动：guard 返回的 reader 与内层同一对象、chunk 逐条原样。
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
	if len(stub.inputs) != 1 {
		t.Fatalf("成功面不该重调: inner 调用 %d 次", len(stub.inputs))
	}
}

// 非溢出错误直通：即使有帧也不动（命中表/排除表判定在 overflow_test，这里只验 guard 不误伤）。
func TestGuardNonOverflowErrorPassthrough(t *testing.T) {
	buf := captureLog(t)
	plainErr := errors.New("dial tcp 127.0.0.1:8080: connection refused")
	stub := &stubChatModel{streamErr: plainErr}
	wrapped := wrapWithGuard(t, stub, 131072)
	box := boxForRescue(nil, 1)
	_, err := wrapped.Stream(withFoldBox(context.Background(), box), nil)
	if err != plainErr {
		t.Fatalf("错误非同一对象: %v", err)
	}
	if len(stub.inputs) != 1 {
		t.Fatalf("非溢出不该重调: inner 调用 %d 次", len(stub.inputs))
	}
	if strings.Contains(buf.String(), "overflow guard") {
		t.Fatalf("非溢出错误应直通: %q", buf.String())
	}
}

// 无容器直通：ctx 没带 foldBox（旁路调用）——不 panic、静默上抛。
func TestGuardNoBoxPassthrough(t *testing.T) {
	buf := captureLog(t)
	overflowErr := overflowErr128k()
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

// Generate 直通：成功值与错误都原样（主循环走 Stream，此方法仅为接口完备）。
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
