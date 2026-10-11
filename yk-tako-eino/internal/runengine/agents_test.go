package runengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// agents_test.go 钉装载面最容易悄悄坏的三处：frontmatter 的 tools 三态（缺席/[]/名单——靠 *[]string
// 指针区分，yaml.v3 对 `tools: []` 必须给出非 nil 空切片）、逐条拒载语义（坏文件不拖垮其余）、
// model 三写法分流（不写/default|inherit|继承 不进注册表；ref 档查表、失败拒该条）。

func writeSpec(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgentSpecs(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "a_good.md", "---\nname: alpha\ndescription: 甲\ntools: [get_time]\n---\nAAA\n")
	writeSpec(t, dir, "b_nodesc.md", "---\nname: beta\n---\nB\n")
	writeSpec(t, dir, "c_dup.md", "---\nname: alpha\ndescription: 撞名\n---\nC\n")
	writeSpec(t, dir, "d_model.md", "---\nname: delta\ndescription: 丁\nmodel: gpt-x\n---\nD\n")
	writeSpec(t, dir, "e_empty.md", "---\nname: eps\ndescription: 空集\ntools: []\n---\nE\n")
	writeSpec(t, dir, "f_absent.md", "---\nname: phi\ndescription: 缺席\n---\nF\n")
	writeSpec(t, dir, "g_nofront.md", "没有 frontmatter\n")
	writeSpec(t, dir, "h_typo.md", "---\nname: eta\ndescription: 拼错\ntool: [get_time]\n---\nH\n")

	specs, err := LoadAgentSpecs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 4 {
		t.Fatalf("应剩 4 条（alpha/delta/eps/phi），实得 %d: %+v", len(specs), specs)
	}
	if specs[0].Name != "alpha" || specs[0].Prompt != "AAA" {
		t.Fatalf("首条应为 alpha/AAA: %+v", specs[0])
	}
	if specs[0].Tools == nil || len(*specs[0].Tools) != 1 || (*specs[0].Tools)[0] != "get_time" {
		t.Fatalf("alpha 应有名单 [get_time]: %+v", specs[0].Tools)
	}
	if specs[1].Name != "delta" || specs[1].Model != "gpt-x" {
		t.Fatalf("delta 的 model 写值应原样带出（parse 层只认语法、不判写法）: %+v", specs[1])
	}
	if specs[2].Name != "eps" || specs[2].Tools == nil || len(*specs[2].Tools) != 0 {
		t.Fatalf("eps 应为非 nil 空集: name=%s tools=%v", specs[2].Name, specs[2].Tools)
	}
	if specs[3].Name != "phi" || specs[3].Tools != nil {
		t.Fatalf("phi 应缺席（nil 指针 = 继承）: name=%s tools=%v", specs[3].Name, specs[3].Tools)
	}

	missing, err := LoadAgentSpecs(filepath.Join(dir, "不存在"))
	if err != nil || missing != nil {
		t.Fatalf("目录不存在应为 (nil, nil)：%v %v", missing, err)
	}
}

func TestResolveToolPool(t *testing.T) {
	ctx := context.Background()
	gt, err := newGetTimeTool()
	if err != nil {
		t.Fatal(err)
	}
	base := make([]tool.BaseTool, 1, 4)
	base[0] = gt

	inherit, err := resolveToolPool(ctx, AgentSpec{Name: "i"}, base)
	if err != nil || len(inherit) != 1 || inherit[0] != gt {
		t.Fatalf("缺席应继承父池单工具: %v %v", inherit, err)
	}
	if reflect.ValueOf(inherit).Pointer() == reflect.ValueOf(base).Pointer() {
		t.Fatal("继承必须拷贝切片——装载 handler 会就地 append 委派工具，共享底层数组即串扰")
	}

	empty := []string{}
	if pool, err := resolveToolPool(ctx, AgentSpec{Tools: &empty}, base); err != nil || len(pool) != 0 {
		t.Fatalf("空集档应为 len 0: %v %v", pool, err)
	}

	list := []string{"get_time"}
	if pool, err := resolveToolPool(ctx, AgentSpec{Tools: &list}, base); err != nil || len(pool) != 1 || pool[0] != gt {
		t.Fatalf("名单应映射到 get_time: %v %v", pool, err)
	}

	unknown := []string{"nope"}
	if _, err := resolveToolPool(ctx, AgentSpec{Tools: &unknown}, base); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("未知名应拒载并报出名字，实得: %v", err)
	}
}

// fakeModel 是够装进 ChatModelAgent 的空壳（构造只查非 nil，不调方法）。
type fakeModel struct{}

func (f *fakeModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, nil
}

func (f *fakeModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

// fakeResolver 脚本化 ModelResolver：记录每次 Get 的 ref（钉「关键词档不进注册表」），
// fail 里的 ref 返回错误（钉逐条拒载语义）。
type fakeResolver struct {
	calls []string
	fail  map[string]error
	ok    map[string]ModelInstance
}

func (f *fakeResolver) Get(_ context.Context, ref string) (ModelInstance, error) {
	f.calls = append(f.calls, ref)
	if err := f.fail[ref]; err != nil {
		return ModelInstance{}, err
	}
	return f.ok[ref], nil
}

// TestAgentSpecModelBranches 钉 model 三写法分流（19v6-05 §4）：
// 不写 / default|inherit|继承 = 继承主实例（resolver 零调用、表条目取主身份）；
// ref 档 = resolver.Get（表条目取解析产物的规范 ref 与该协议 translator）；解析失败拒该条、其余照载。
func TestAgentSpecModelBranches(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "a_absent.md", "---\nname: absent\ndescription: 不写\n---\nA\n")
	writeSpec(t, dir, "b_inherit.md", "---\nname: inherits\ndescription: inherit\ntools: []\nmodel: inherit\n---\nB\n")
	writeSpec(t, dir, "c_default.md", "---\nname: capped\ndescription: DEFAULT 大小写\ntools: []\nmodel: DEFAULT\n---\nC\n")
	writeSpec(t, dir, "d_zh.md", "---\nname: zh\ndescription: 继承\ntools: []\nmodel: 继承\n---\nD\n")
	writeSpec(t, dir, "e_ok.md", "---\nname: okref\ndescription: ref 档\ntools: []\nmodel: gpt-x\n---\nE\n")
	writeSpec(t, dir, "f_bad.md", "---\nname: badref\ndescription: 解析失败\ntools: []\nmodel: nope/x\n---\nF\n")

	sub := &fakeModel{}
	resolver := &fakeResolver{
		fail: map[string]error{"nope/x": errors.New("模型 nope/x 不在已配置厂商的目录里")},
		ok:   map[string]ModelInstance{"gpt-x": {Chat: sub, Ref: "prov/gpt-x", Protocol: "openai"}},
	}
	gt, err := newGetTimeTool()
	if err != nil {
		t.Fatal(err)
	}
	mainTranslate := claudeChunkText
	h := newAgentSpecHandler(dir, &fakeModel{}, "main/ref", mainTranslate, resolver, make(chan struct{}, 8), 51200)

	ctx := withAgentModelTable(context.Background(), newAgentModelTable("main/ref", mainTranslate))
	runCtx := &adk.ChatModelAgentContext{Tools: []tool.BaseTool{gt}}
	if _, _, err := h.BeforeAgent(ctx, runCtx); err != nil {
		t.Fatalf("BeforeAgent 不返回错误（装载面全部降级为 log）: %v", err)
	}

	// 只有 ref 档进注册表，顺序 = 文件名序；关键词档一次都不调。
	if !reflect.DeepEqual(resolver.calls, []string{"gpt-x", "nope/x"}) {
		t.Fatalf("resolver 调用应为 [gpt-x nope/x]（关键词档不得进注册表）, 实得 %v", resolver.calls)
	}

	// 池 = 基座 1 + 装载成功 5（badref 拒载）。
	if len(runCtx.Tools) != 6 {
		t.Fatalf("工具池应 1+5=6，实得 %d", len(runCtx.Tools))
	}
	names := map[string]bool{}
	for _, tl := range runCtx.Tools {
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		names[info.Name] = true
	}
	for _, want := range []string{"get_time", "absent", "inherits", "capped", "zh", "okref"} {
		if !names[want] {
			t.Fatalf("池里应有 %q: %v", want, names)
		}
	}
	if names["badref"] {
		t.Fatal("badref 应被拒载，委派工具不得入池")
	}

	// 身份表：关键词档取主身份；ref 档取解析产物（规范 ref + 该协议 translator）；拒载档无条目。
	table := agentModelTableFrom(ctx)
	sameFunc := func(a, b Translator) bool {
		return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
	}
	for _, name := range []string{"absent", "inherits", "capped", "zh"} {
		e := table.entryFor(name)
		if e.ref != "main/ref" || !sameFunc(e.translate, mainTranslate) {
			t.Fatalf("%s 应继承主身份（主 ref + 主 translator）: %+v", name, e)
		}
	}
	e := table.entryFor("okref")
	if e.ref != "prov/gpt-x" || !sameFunc(e.translate, TranslatorFor("openai")) {
		t.Fatalf("okref 应取解析产物（prov/gpt-x + openai translator）: %+v", e)
	}
	if _, ok := table.entries["badref"]; ok {
		t.Fatal("badref 拒载后不得留表条目")
	}
}

// TestTruncateDelegationReturn 钉回传裁剪（T3-3）：未过帽原样、过帽 = UTF-8 完整前缀 + 提示尾缀
// （N = 总字节 − 保留字节）、截点回退到 rune 边界、cap<=0 = 不裁剪。
func TestTruncateDelegationReturn(t *testing.T) {
	notice := func(n int) string { return fmt.Sprintf(delegationTruncationNotice, n) }

	short := "短"
	if got := truncateDelegationReturn(short, 100); got != short {
		t.Fatalf("未过帽应原样: %q", got)
	}
	if got := truncateDelegationReturn(strings.Repeat("结", 20), 0); got != strings.Repeat("结", 20) {
		t.Fatalf("cap 0 应为不裁剪: %q", got)
	}

	ascii := strings.Repeat("a", 50)
	if got, want := truncateDelegationReturn(ascii, 10), "aaaaaaaaaa"+notice(40); got != want {
		t.Fatalf("ASCII 截断 = %q, want %q", got, want)
	}

	// 每字 3 字节：cap 10 落在第 4 个字的中间 → 回退到 9（3 个整字）。
	zh := strings.Repeat("结", 20)
	got := truncateDelegationReturn(zh, 10)
	if want := strings.Repeat("结", 3) + notice(51); got != want {
		t.Fatalf("UTF-8 截断 = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断结果必须仍是合法 UTF-8: %q", got)
	}
}

// fakeInvokableTool 记录收到的 opts 与调用次数（钉包壳透传与闸门）。
type fakeInvokableTool struct {
	info  *schema.ToolInfo
	opts  []tool.Option
	calls int
	out   string
}

func (f *fakeInvokableTool) Info(context.Context) (*schema.ToolInfo, error) { return f.info, nil }
func (f *fakeInvokableTool) InvokableRun(_ context.Context, _ string, opts ...tool.Option) (string, error) {
	f.calls++
	f.opts = opts
	return f.out, nil
}

// markerOpt 是装的「实现私有 option」（复刻 eino 用 opts 传 event generator 的通道）：
// 内层读得到它，才证明 delegationTool 没有扣下 opts（扣下 = EmitInternalEvents 的子代理帧全灭）。
type markerOpt struct{ v string }

// TestDelegationToolForwardsOptsAndGates 钉包壳的两条承重面：opts 原样递内层；满闸时 ctx 取消可中断
// （不无限吊住父循环、内层不被调）；Info 透传（声明字节不变）。
func TestDelegationToolForwardsOptsAndGates(t *testing.T) {
	inner := &fakeInvokableTool{info: &schema.ToolInfo{Name: "researcher"}, out: "报告"}
	d := &delegationTool{inner: inner, sem: make(chan struct{}, 1), capBytes: 0, name: "researcher"}

	info, err := d.Info(context.Background())
	if err != nil || info.Name != "researcher" {
		t.Fatalf("Info 应透传: %v %v", info, err)
	}

	opt := tool.WrapImplSpecificOptFn(func(o *markerOpt) { o.v = "x" })
	if out, err := d.InvokableRun(context.Background(), `{"request":"q"}`, opt); err != nil || out != "报告" {
		t.Fatalf("InvokableRun = (%q, %v), want (报告, nil)", out, err)
	}
	if len(inner.opts) != 1 {
		t.Fatalf("opts 必须原样递内层，实得 %d 个", len(inner.opts))
	}
	if seen := tool.GetImplSpecificOptions[markerOpt](nil, inner.opts...); seen.v != "x" {
		t.Fatalf("内层应能从 opts 读到 markerOpt: %+v", seen)
	}

	// 闸满 + ctx 已取消：立即返回、内层不被调。
	d.sem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.InvokableRun(ctx, "{}"); !errors.Is(err, context.Canceled) {
		t.Fatalf("满闸 + ctx 取消应回 context.Canceled，实得 %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("等闸中被取消，内层不得被调: calls=%d", inner.calls)
	}
	<-d.sem

	// 放闸后照常跑通（闸不是一次性）。
	if _, err := d.InvokableRun(context.Background(), "{}"); err != nil {
		t.Fatalf("放闸后应可再调: %v", err)
	}
}
