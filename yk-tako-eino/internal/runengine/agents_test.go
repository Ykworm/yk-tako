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
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// agents_test.go 钉装载面最容易悄悄坏的五处：frontmatter 的 tools 三态（缺席/[]/名单——靠 *[]string
// 指针区分，yaml.v3 对 `tools: []` 必须给出非 nil 空切片）、exclude_tools 第四态（继承父池减排除项，
// doc/26）与它和 tools 的互斥、逐条拒载语义（坏文件不拖垮其余）、model 三写法分流（不写/
// default|inherit|继承 不进注册表；ref 档查表、失败拒该条）、以及**出厂规格的有效名单金样**
// （TestShippedAgentSpecToolGuards，读 ../../agents 真文件）。

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

// TestResolveToolPoolExclude 钉 doc/26 的第四态（exclude_tools = 继承父池减去列出的名字）。三态在上面
// 那支；第四态单独一支是因为它有三处独有性质：① 结果**顺序是池序、不是排除项序**（写成 [c a] 与
// 写成 [a c] 同结果）；② 元素与父池是同一个工具对象（不换壳，与下面 H4 那支同判据）；③ 空列表 =
// 与不写同结果（显式「已看过、一个不排除」是合法表态，不该报错）。
// 外加一条拒载：排除一个父池没有的名字 ⇒ 报错并报出那个名字。不报就是护栏最哑的失效方式——
// bash 拼成 bashh 被悄悄忽略，规格文件看起来已经挡住了，子代理照旧能跑命令。
func TestResolveToolPoolExclude(t *testing.T) {
	ctx := context.Background()
	base := []tool.BaseTool{
		&fakeInvokableTool{info: &schema.ToolInfo{Name: "a"}},
		&fakeInvokableTool{info: &schema.ToolInfo{Name: "b"}},
		&fakeInvokableTool{info: &schema.ToolInfo{Name: "c"}},
	}
	names := func(t *testing.T, pool []tool.BaseTool) []string {
		t.Helper()
		out := make([]string, 0, len(pool))
		for _, x := range pool {
			info, err := x.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, info.Name)
		}
		return out
	}

	drop := []string{"b"}
	pool, err := resolveToolPool(ctx, AgentSpec{ExcludeTools: &drop}, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, pool); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatalf("排除 b 应得池序 [a c]，实得 %v", got)
	}
	if pool[0] != base[0] || pool[1] != base[2] {
		t.Fatal("第四态必须原对象搬运（不换壳）——池内元素与父池不是同一个接口值")
	}

	reversed := []string{"c", "a"}
	pool, err = resolveToolPool(ctx, AgentSpec{ExcludeTools: &reversed}, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, pool); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("排除 [c a] 应得 [b]（结果序是池序不是排除项序），实得 %v", got)
	}

	zero := []string{}
	inherited, err := resolveToolPool(ctx, AgentSpec{}, base)
	if err != nil {
		t.Fatal(err)
	}
	pool, err = resolveToolPool(ctx, AgentSpec{ExcludeTools: &zero}, base)
	if err != nil {
		t.Fatalf("exclude_tools: [] 应与不写同结果、不报错，实得 %v", err)
	}
	if !reflect.DeepEqual(names(t, pool), names(t, inherited)) {
		t.Fatalf("空排除应与继承同结果: %v vs %v", names(t, pool), names(t, inherited))
	}
	if reflect.ValueOf(pool).Pointer() == reflect.ValueOf(base).Pointer() {
		t.Fatal("空排除也必须拷贝切片——装载 handler 会就地 append 委派工具，共享底层数组即串扰")
	}

	typo := []string{"bashh"}
	if _, err := resolveToolPool(ctx, AgentSpec{ExcludeTools: &typo}, base); err == nil || !strings.Contains(err.Error(), "bashh") {
		t.Fatalf("排除未知名应拒载并报出名字，实得: %v", err)
	}
}

// TestParseAgentSpecRejectsToolsAndExclude 钉两行同写的互斥落在 **parse 层**，不留给 resolveToolPool：
// 若放到 resolve，实际名单就取决于代码里两个分支的先后——改一次判断顺序换一份能力面，
// 那是护栏最不该有的性质。用例名带 ParseAgentSpec 是有意的（run pattern 靠它命中）。
func TestParseAgentSpecRejectsToolsAndExclude(t *testing.T) {
	both := []byte("---\nname: x\ndescription: 丁\ntools:\n  - get_time\nexclude_tools:\n  - bash\n---\n正文\n")
	if _, err := parseAgentSpec(both); err == nil || !strings.Contains(err.Error(), "二选一") {
		t.Fatalf("两行同写应在 parse 层拒载、文案含「二选一」，实得: %v", err)
	}
	// 单写 exclude_tools 放行，且指针非 nil——空列表与「没写」靠指针区分（与 tools 三态同法）。
	empty := []byte("---\nname: x\ndescription: 丁\nexclude_tools: []\n---\n正文\n")
	spec, err := parseAgentSpec(empty)
	if err != nil || spec.ExcludeTools == nil || len(*spec.ExcludeTools) != 0 {
		t.Fatalf("exclude_tools: [] 应为非 nil 空切片: %v %v", spec.ExcludeTools, err)
	}
	absent := []byte("---\nname: x\ndescription: 丁\n---\n正文\n")
	if spec, err = parseAgentSpec(absent); err != nil || spec.ExcludeTools != nil {
		t.Fatalf("不写 exclude_tools 应为 nil 指针（继承路）: %v %v", spec.ExcludeTools, err)
	}
}

// TestShippedAgentSpecToolGuards 钉**仓库出厂配置**（`agents/*.md`）与 doc/26 的一致性——机制在上面
// 两支，这支盯的是配置。它把三件「文档里写了很久、删掉却没有任何东西会红」的事转成检查：
//   - doc/22 §7「researcher 的名单必须含 web_search」→ 金样逐名逐序比对，把它写进排除项就红；
//   - doc/26 §2 的哑失败方向（父池加一把新工具 ⇒ 排除路让它自动到达 researcher）→ 名单变长即红，
//     逼人当场决定这把给不给（给 ⇒ 改金样；不给 ⇒ 往 exclude_tools 加一行、金样不动）；
//   - 名单里的手误（websearch / bashh）→ resolveToolPool 报错即红，文案说明运行时整条规格会拒载
//     （今天只有跑起来才在日志里看得见，run 本身不失败——最难查的一类）。
//
// ⚠️ `agents/*.md` 是设计成给运维改的（改文件 ⇒ 下一条消息生效）。**有意改配置就同步改金样**：
// 给 researcher 加回 bash、给 coder 添一把工具、新加一条规格，都在这张表里表态，不要删测试。
// 盲区一处：下面的名字宇宙派生自 firstPartyFileEffects 的键 ∪ {get_time}，将来某把新工具
// **不进注册表**（像 get_time 那样）就不在金样视野里 ⇒「新工具必须登记 file_effect（显式 none 也算）」
// 这条既有惯例（doc/22 §3）从此多一个理由。
func TestShippedAgentSpecToolGuards(t *testing.T) {
	specs, err := LoadAgentSpecs(filepath.Join("..", "..", "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) == 0 {
		t.Fatal("读不到出厂规格（../../agents 空或不存在）——这支测试空转就等于没有护栏")
	}

	// 父池装配序（= worker.go 的入池顺序）**显式写死**：firstPartyFileEffects 是 Go map、遍历键序随机，
	// 而 researcher 走排除路按池序输出，不钉死顺序则逐名逐序比对会 flaky。
	poolOrder := []string{
		"get_time", "write_file", "read_text_file", "read_file", "edit_file", "move_file",
		"create_directory", "list_directory", "directory_tree", "get_file_info", "search_files",
		"read_media_file", "read_pdf_text", "bash", "memory", "search_history", "web_search",
	}
	// 名字宇宙从注册表派生（不另抄一份名单，两份会各自腐化）：父池加一把登记过 file_effect 的
	// 新工具 ⇒ 集合与写死的池序不等 ⇒ 红。get_time 按 fileops.go:86 的说明不进注册表，显式并进来。
	universe := make(map[string]bool, len(firstPartyFileEffects)+1)
	for name := range firstPartyFileEffects {
		universe[name] = true
	}
	universe["get_time"] = true
	if len(universe) != len(poolOrder) {
		t.Fatalf("注册表派生的名字宇宙 %d 名 ≠ 写死的池序 %d 名——父池加/减了工具？两处同步改（doc/26 §7）",
			len(universe), len(poolOrder))
	}
	for _, name := range poolOrder {
		if !universe[name] {
			t.Fatalf("池序里的 %q 不在 firstPartyFileEffects ∪ {get_time} 里——它没登记 file_effect", name)
		}
	}
	base := make([]tool.BaseTool, 0, len(poolOrder))
	for _, name := range poolOrder {
		base = append(base, &fakeInvokableTool{info: &schema.ToolInfo{Name: name}})
	}

	// 金样 = 每条出厂规格今天应有的**有效名单**（逐名逐序；researcher 走排除路 = 池序，
	// coder 走白名单路 = 它 frontmatter 里写的顺序，两序今天只差 memory/bash 的先后）。
	// researcher 的金样 = 父池去掉 bash，逐名写死；不写总数——数字会随父池变化而腐化。
	// 给 researcher read_pdf_text，是因为它本来就能读 workspace 里的文件，PDF 文本属于同一类读。
	golden := map[string][]string{
		"researcher": {"get_time", "write_file", "read_text_file", "read_file", "edit_file", "move_file",
			"create_directory", "list_directory", "directory_tree", "get_file_info", "search_files",
			"read_media_file", "read_pdf_text", "memory", "search_history", "web_search"},
		"coder": {"write_file", "read_text_file", "edit_file", "list_directory", "search_files", "memory", "bash"},
	}
	// inheritOK：出厂规格默认必须写 tools: 或 exclude_tools: 之一——两行都不写 = 整份继承，
	// 继承正是 doc/26 §2 那条「新工具自动到达」的通道。哪条规格有意整份继承，在这里显式表态，
	// 不靠「不写那一行」承载语义。
	inheritOK := map[string]bool{}

	for _, spec := range specs {
		want, registered := golden[spec.Name]
		if !registered {
			t.Errorf("出厂规格 %q 没登记金样——新规格的作者必须当场写下它的有效名单（通用规则，不是特例）", spec.Name)
			continue
		}
		if spec.Tools == nil && spec.ExcludeTools == nil && !inheritOK[spec.Name] {
			t.Errorf("出厂规格 %q 两行都不写 = 整份继承父池（doc/26 §2 的通道）——写 tools:/exclude_tools: 之一，或在 inheritOK 里显式放行", spec.Name)
		}
		pool, err := resolveToolPool(context.Background(), spec, base)
		if err != nil {
			t.Errorf("规格 %q 的有效名单解析失败（运行时整条规格会拒载、这场 run 里没有这个子代理）: %v", spec.Name, err)
			continue
		}
		got := make([]string, 0, len(pool))
		for _, x := range pool {
			info, ierr := x.Info(context.Background())
			if ierr != nil {
				t.Fatal(ierr)
			}
			got = append(got, info.Name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("规格 %q 的有效名单与金样不符（doc/26）\n期望: %s\n实得: %s",
				spec.Name, strings.Join(want, ", "), strings.Join(got, ", "))
		}
	}
}

// TestSubagentPoolKeepsRealSearchHistory 钉 H4（「拒绝壳」裁定已推翻，见 tmp/21-历史查找 planning §8）：
// 子代理池里的 search_history 就是父池里那一个真工具——继承路（tools 缺席 = Tools 指针 nil）与
// 白名单路（tools: [search_history]）两路都只做「原对象搬运」，不换壳。真工具的判据两条：
// 同一性（池内元素与父池对象是同一个接口值）+ 行为（挂上 noteBox 真查临时库，出真命中不是拒绝句）。
// 哪天有人把壳塞回来（同名同声明的替代实现），第一条断言就红。
//
// 注意这里 name 写着 researcher，用的却是**手造的 AgentSpec 值**（Tools 为 nil = 继承路），
// 不读 agents/ 里的出厂文件；doc/26 之后出厂的 researcher 走 exclude_tools 排除路，这支照旧绿、
// 照旧钉它本来钉的东西（不换壳）。出厂配置与 doc/26 的一致性由 TestShippedAgentSpecToolGuards 钉。
func TestSubagentPoolKeepsRealSearchHistory(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, time.Minute, time.Minute)

	// 造一行「上一场已提交」的历史：词模式真实查询的靶（属于 s_h4、不属于本场 r_cur）。
	rowID, err := newRowID("rl")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.WithContext(ctx).Create(&RunlogEntry{
		ID: rowID, SessionID: "s_h4", Seq: 1, RunID: "r_prev", Role: "user",
		Text: "上一场留下的错误码 E_DECK_17。", CreatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}

	sh, err := newSearchHistoryTool(store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gt, err := newGetTimeTool()
	if err != nil {
		t.Fatal(err)
	}
	base := []tool.BaseTool{gt, sh}

	inherit, err := resolveToolPool(ctx, AgentSpec{Name: "researcher"}, base)
	if err != nil || len(inherit) != 2 || inherit[1] != sh {
		t.Fatalf("继承路应原样带出父池两件（search_history 同对象）: %v %v", inherit, err)
	}
	whitelist := []string{"search_history"}
	listed, err := resolveToolPool(ctx, AgentSpec{Name: "researcher", Tools: &whitelist}, base)
	if err != nil || len(listed) != 1 || listed[0] != sh {
		t.Fatalf("白名单路应只挑出 search_history 且同对象: %v %v", listed, err)
	}

	for route, pool := range map[string][]tool.BaseTool{"继承路": inherit, "白名单路": listed} {
		var got tool.BaseTool
		for _, tl := range pool {
			info, err := tl.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.Name == "search_history" {
				got = tl
			}
		}
		if got == nil {
			t.Fatalf("%s: 池内应有 search_history，实得 %d 件", route, len(pool))
		}
		if got != sh {
			t.Fatalf("%s: 池内 search_history 与父池不是同一对象（被换成了替代实现）", route)
		}
		inv, ok := got.(tool.InvokableTool)
		if !ok {
			t.Fatalf("%s: search_history 应实现 InvokableTool（真工具），实得 %T", route, got)
		}
		out, err := inv.InvokableRun(withNoteBox(ctx, newNoteBox("s_h4", "r_cur", 0)), `{"query":"E_DECK_17"}`)
		if err != nil {
			t.Fatalf("%s: 真查应成功，实得错误: %v", route, err)
		}
		for _, want := range []string{"命中 1 条", "seq=1", "E_DECK_17"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s: 应出真实命中（不是拒绝句），缺 %q：%q", route, want, out)
			}
		}
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
	h := newAgentSpecHandler(dir, newTestStore(t, time.Minute, time.Minute), "/tmp/agents-test-ws", &fakeModel{}, "main/ref", mainTranslate, resolver, make(chan struct{}, 8), 51200,
		fakeCounter{f: func(string) int { return 0 }}, 0, 0, nil, nil, subagentArchiveTTL)

	ctx := withAgentModelTable(context.Background(), newAgentModelTable("main/ref", mainTranslate))
	runCtx := &adk.ChatModelAgentContext{Tools: []tool.BaseTool{gt}}
	gotCtx, _, err := h.BeforeAgent(ctx, runCtx)
	if err != nil {
		t.Fatalf("BeforeAgent 不返回错误（装载面全部降级为 log）: %v", err)
	}

	// 只有 ref 档进注册表，顺序 = 文件名序；关键词档一次都不调。
	if !reflect.DeepEqual(resolver.calls, []string{"gpt-x", "nope/x"}) {
		t.Fatalf("resolver 调用应为 [gpt-x nope/x]（关键词档不得进注册表）, 实得 %v", resolver.calls)
	}

	// 池 = 基座 1 + 装载成功 5（badref 拒载）+ resume_subagent。
	if len(runCtx.Tools) != 7 {
		t.Fatalf("工具池应 1+5+resume=7，实得 %d", len(runCtx.Tools))
	}
	names := map[string]bool{}
	for _, tl := range runCtx.Tools {
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		names[info.Name] = true
	}
	for _, want := range []string{"get_time", "absent", "inherits", "capped", "zh", "okref", resumeSubagentToolName} {
		if !names[want] {
			t.Fatalf("池里应有 %q: %v", want, names)
		}
	}
	asm := subagentAssemblyFrom(gotCtx)
	if asm == nil || len(asm.base) != 1 {
		t.Fatalf("子代理入口快照应只有基座: %#v", asm)
	}
	baseInfo, err := asm.base[0].Info(ctx)
	if err != nil || baseInfo.Name != "get_time" {
		t.Fatalf("入口快照 = %v %v，应是 get_time，不含委派和 resume", baseInfo, err)
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

// TestDelegationRedisSkipsProcessSem：Redis 租户槽在时，进程 sem 满了也不能挡住另一个租户。
// 没有 Redis 的单测仍走进程 sem，见上一个用例。
func TestDelegationRedisSkipsProcessSem(t *testing.T) {
	st := newTestStore(t, time.Minute, time.Minute)
	user, tenant, _, err := st.Signup(context.Background(), "slot@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(withPrincipal(context.Background(), principal{
		UserID: user.ID, TenantID: tenant.ID, Role: "owner",
	}), 2*time.Second)
	defer cancel()
	inner := &fakeInvokableTool{info: &schema.ToolInfo{Name: "researcher"}, out: "报告"}
	d := &delegationTool{
		inner: inner, sem: make(chan struct{}, 1), capBytes: 0, name: "researcher", store: st,
	}
	d.sem <- struct{}{}
	out, err := d.InvokableRun(ctx, "{}")
	if err != nil || out != "报告" || inner.calls != 1 {
		t.Fatalf("租户槽应放行、进程 sem 不应挡住，得到 out=%q err=%v calls=%d", out, err, inner.calls)
	}
}
