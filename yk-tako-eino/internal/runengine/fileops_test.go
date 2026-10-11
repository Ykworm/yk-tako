// fileOps 提取与分类的 fixture 网（C5-A4；每条规则一个用例，与 19v6-07 §5 钉死的规则表一一对应）。
// 池内暂时没有 file 工具（e2e 活体转正点 = 首个 file 工具那拍），所以 tool_calls JSON 直接造。
package runengine

import (
	"fmt"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// fileCall 造一条工具调用（提取不看 call id，固定值即可）。
func fileCall(name, args string) schema.ToolCall {
	return schema.ToolCall{ID: "call_1", Function: schema.FunctionCall{Name: name, Arguments: args}}
}

// fileCallMsg 造一条带工具调用的 assistant 行（extractFileOps 的输入单位）。
func fileCallMsg(run string, calls ...schema.ToolCall) ModelMessage {
	return ModelMessage{RunID: RunID(run), Seq: 1, Role: "assistant", ToolCalls: calls}
}

func assertEntries(t *testing.T, label string, got, want []fileOpEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d 条 %+v，want %d 条 %+v", label, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %+v, want %+v", label, i, got[i], want[i])
		}
	}
}

// fixture 1 桶与条目：三桶收集 + 三元组（path/tool/runID）——三个工具走声明位。
func TestFileOpsExtractThreeBuckets(t *testing.T) {
	registry := map[string]string{"write_file": "write", "edit_file": "edit", "read_file": "read"}
	msgs := []ModelMessage{fileCallMsg("r_1",
		fileCall("write_file", `{"path":"/w/deck.html"}`),
		fileCall("edit_file", `{"path":"/w/spec.md"}`),
		fileCall("read_file", `{"path":"/w/readme.md"}`),
	)}
	ops := extractFileOps(msgs, registry)
	assertEntries(t, "written", ops.written, []fileOpEntry{{path: "/w/deck.html", tool: "write_file", runID: "r_1"}})
	assertEntries(t, "edited", ops.edited, []fileOpEntry{{path: "/w/spec.md", tool: "edit_file", runID: "r_1"}})
	assertEntries(t, "read", ops.read, []fileOpEntry{{path: "/w/readme.md", tool: "read_file", runID: "r_1"}})
}

// 同清单内按 path 去重：保留先进的一条（来源注记 = 第一次碰它的那次调用）。
func TestFileOpsListDedupeKeepsFirst(t *testing.T) {
	registry := map[string]string{"read_file": "read"}
	msgs := []ModelMessage{
		fileCallMsg("r_1", fileCall("read_file", `{"path":"/w/a.md"}`)),
		fileCallMsg("r_2", fileCall("read_file", `{"path":"/w/a.md"}`)),
	}
	ops := extractFileOps(msgs, registry)
	assertEntries(t, "read", ops.read, []fileOpEntry{{path: "/w/a.md", tool: "read_file", runID: "r_1"}})
}

// fixture 2 归并：同路径又读又写，resolve 后只在「改过」；未写的读保留。
func TestFileOpsResolveReadWrittenMerge(t *testing.T) {
	registry := map[string]string{"read_file": "read", "write_file": "write"}
	msgs := []ModelMessage{fileCallMsg("r_1",
		fileCall("read_file", `{"path":"/w/deck.html"}`),
		fileCall("write_file", `{"path":"/w/deck.html"}`),
		fileCall("read_file", `{"path":"/w/other.md"}`),
	)}
	res := resolveOps(extractFileOps(msgs, registry))
	assertEntries(t, "changed", res.changed, []fileOpEntry{{path: "/w/deck.html", tool: "write_file", runID: "r_1"}})
	assertEntries(t, "read", res.read, []fileOpEntry{{path: "/w/other.md", tool: "read_file", runID: "r_1"}})
}

// fixture 3 渲染行：带 provenance（工具 + run id，全角标点即契约字节）。
func TestFileOpsRenderLineProvenance(t *testing.T) {
	got := renderFileOpLine(fileOpEntry{path: "/w/deck.html", tool: "edit_file", runID: "r_01"})
	want := "- /w/deck.html（edit_file，r_01）"
	if got != want {
		t.Fatalf("渲染行漂移：got %q want %q", got, want)
	}
}

// fixture 4 声明优先于名字：名字命中的桶被声明覆盖（read_wrapper 声明 edit、write_sensor 声明 read）。
func TestFileOpsDeclarationOverridesName(t *testing.T) {
	registry := map[string]string{"read_wrapper": "edit", "write_sensor": "read"}
	msgs := []ModelMessage{fileCallMsg("r_1",
		fileCall("read_wrapper", `{"path":"/w/a.md"}`),
		fileCall("write_sensor", `{"path":"/w/b.md"}`),
	)}
	ops := extractFileOps(msgs, registry)
	assertEntries(t, "edited", ops.edited, []fileOpEntry{{path: "/w/a.md", tool: "read_wrapper", runID: "r_1"}})
	assertEntries(t, "read", ops.read, []fileOpEntry{{path: "/w/b.md", tool: "write_sensor", runID: "r_1"}})
	assertEntries(t, "written", ops.written, nil)
}

// fixture 5 不写 = 空串 = 按 none 处理（用户 2026-10-11 裁定）：在注册表但字段空 →
// 无记录、不进启发式——就算参数带 path 也不记（对比 fixture 12：不在注册表才有歧义归改过）。
func TestFileOpsUndeclaredNoRecord(t *testing.T) {
	registry := map[string]string{"get_time": "", "fetch_url": ""}
	msgs := []ModelMessage{fileCallMsg("r_1",
		fileCall("get_time", `{"timezone":"UTC"}`),
		fileCall("fetch_url", `{"path":"/tmp/x"}`),
	)}
	ops := extractFileOps(msgs, registry)
	assertEntries(t, "read", ops.read, nil)
	assertEntries(t, "written", ops.written, nil)
	assertEntries(t, "edited", ops.edited, nil)
}

// fixture 6 lint：目录名单 + 空声明 → 启动报错。
func TestFileOpsLintDirectoryNameNoDeclaration(t *testing.T) {
	if err := lintFileEffects(map[string]string{"write_file": ""}); err == nil {
		t.Fatal("目录名单名字 + 空声明应拦（启动报错）")
	}
}

// fixture 7 显式 none 豁免：lint 放行，且提取也不记录（表态的两半都生效）。
func TestFileOpsLintExplicitNoneExempt(t *testing.T) {
	registry := map[string]string{"write_file": fileEffectNone}
	if err := lintFileEffects(registry); err != nil {
		t.Fatalf("显式 none 应豁免 lint: %v", err)
	}
	ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("write_file", `{"path":"/w/a.html"}`))}, registry)
	assertEntries(t, "written", ops.written, nil)
}

// fixture 8 前缀相似非精确名（read_sensor）不受 lint 管辖（宁漏不误杀：lint 误报 = 阻断启动）。
func TestFileOpsLintPrefixSimilarNotCaught(t *testing.T) {
	if err := lintFileEffects(map[string]string{"read_sensor": ""}); err != nil {
		t.Fatalf("read_sensor 非精确名单，不应拦: %v", err)
	}
}

// fixture 9 目录外名字空声明不拦。
func TestFileOpsLintOutsideNameNotCaught(t *testing.T) {
	if err := lintFileEffects(map[string]string{"get_time": ""}); err != nil {
		t.Fatalf("目录外名字不应拦: %v", err)
	}
}

// fixture 10 前缀表大小写不敏感：Read_File 小写后命中 read。
func TestFileOpsPrefixCaseInsensitive(t *testing.T) {
	ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("Read_File", `{"path":"/w/a.md"}`))}, map[string]string{})
	assertEntries(t, "read", ops.read, []fileOpEntry{{path: "/w/a.md", tool: "Read_File", runID: "r_1"}})
}

// fixture 11 写侧前缀 → written。
func TestFileOpsPrefixWriteSide(t *testing.T) {
	ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("write_doc", `{"path":"/w/a.md"}`))}, nil)
	assertEntries(t, "written", ops.written, []fileOpEntry{{path: "/w/a.md", tool: "write_doc", runID: "r_1"}})
}

// fixture 12 歧义归改过：两侧不命中 + 有 path → written（唯一触发点）。
func TestFileOpsAmbiguousPathToWritten(t *testing.T) {
	ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("handle_thing", `{"path":"/w/x"}`))}, nil)
	assertEntries(t, "written", ops.written, []fileOpEntry{{path: "/w/x", tool: "handle_thing", runID: "r_1"}})
}

// fixture 13 两侧不命中 + 无 path → 无记录。
func TestFileOpsAmbiguousNoPathNoRecord(t *testing.T) {
	ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("handle_thing", `{"other":1}`))}, nil)
	assertEntries(t, "read", ops.read, nil)
	assertEntries(t, "written", ops.written, nil)
	assertEntries(t, "edited", ops.edited, nil)
}

// fixture 14 shell 重定向：> / >> / 2> / 2>> 各取下一词。
func TestFileOpsShellRedirect(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{"cat a.md > /tmp/x.md", "/tmp/x.md"},
		{"echo hi >> /tmp/y.md", "/tmp/y.md"},
		{"ls 2> /tmp/err.log", "/tmp/err.log"},
		{"cmd 2>> /tmp/app.log", "/tmp/app.log"},
	} {
		ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("bash", fmt.Sprintf(`{"command":%q}`, tc.cmd)))}, nil)
		assertEntries(t, tc.cmd, ops.written, []fileOpEntry{{path: tc.want, tool: "bash", runID: "r_1"}})
	}
}

// fixture 15 引号：引号内空格不断词，收路径时剥掉首尾引号。
func TestFileOpsShellQuotedPath(t *testing.T) {
	ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("bash", `{"command":"echo hi >> '/tmp/y file'"}`))}, nil)
	assertEntries(t, "written", ops.written, []fileOpEntry{{path: "/tmp/y file", tool: "bash", runID: "r_1"}})
}

// fixture 16 写命令表：cp/mv 末参、touch/mkdir 多非旗标参、sed 独立 -i 末参、dd of=。
func TestFileOpsShellWriteCommands(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want []string
	}{
		{"cp a b", []string{"b"}},
		{"mv old new", []string{"new"}},
		{"touch d1 d2", []string{"d1", "d2"}},
		{"mkdir -p d1 d2", []string{"d1", "d2"}},
		{"sed -i s/a/b/ f.md", []string{"f.md"}},
		{"dd if=in of=out", []string{"out"}},
	} {
		ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("bash", fmt.Sprintf(`{"command":%q}`, tc.cmd)))}, nil)
		want := make([]fileOpEntry, 0, len(tc.want))
		for _, p := range tc.want {
			want = append(want, fileOpEntry{path: p, tool: "bash", runID: "r_1"})
		}
		assertEntries(t, tc.cmd, ops.written, want)
	}
}

// fixture 17 sed 的 -ie 与 -i.bak 不是独立 -i token，不碰。
func TestFileOpsShellInlineEditVariants(t *testing.T) {
	for _, cmd := range []string{"sed -ie s/a/b/ f.md", "sed -i.bak s/a/b/ f.md"} {
		ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("bash", fmt.Sprintf(`{"command":%q}`, cmd)))}, nil)
		assertEntries(t, cmd, ops.written, nil)
	}
}

// fixture 18 一命令多写全记：cp 的参数范围到下个分隔词为止，后续子命令各自抓。
func TestFileOpsShellMultipleWrites(t *testing.T) {
	ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("bash", `{"command":"cp a b && > c"}`))}, nil)
	assertEntries(t, "written", ops.written, []fileOpEntry{
		{path: "b", tool: "bash", runID: "r_1"},
		{path: "c", tool: "bash", runID: "r_1"},
	})
}

// fixture 19 只抓写：无重定向的纯读命令不产生记录。
func TestFileOpsShellPureRead(t *testing.T) {
	for _, cmd := range []string{"cat a.md", "grep foo bar.txt", "ls -l /tmp"} {
		ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("bash", fmt.Sprintf(`{"command":%q}`, cmd)))}, nil)
		assertEntries(t, cmd, ops.written, nil)
	}
}

// fixture 20 变量展开与命令替换不碰（收路径时按 $ / ` 跳过）。
func TestFileOpsShellVarExpansion(t *testing.T) {
	for _, cmd := range []string{"echo hi > $OUT", "echo hi > $(mktemp)", "echo hi > `mktemp`"} {
		ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("bash", fmt.Sprintf(`{"command":%q}`, cmd)))}, nil)
		assertEntries(t, cmd, ops.written, nil)
	}
}

// fixture 21 上限与淘汰（三档降级链，用户 2026-10-11 裁定）：常态 resolveOps 不封顶；
// capOps 是减灾档工具（A5 在 S+块+tail 超帽时调用）——砍最老（先进先出），两表各自独立砍。
func TestFileOpsCapEvictsOldest(t *testing.T) {
	registry := map[string]string{"read_file": "read", "write_file": "write"}
	var msgs []ModelMessage
	for i := 0; i < 55; i++ {
		msgs = append(msgs, fileCallMsg("r_1", fileCall("read_file", fmt.Sprintf(`{"path":"/f%02d"}`, i))))
	}
	res := resolveOps(extractFileOps(msgs, registry))
	if len(res.read) != 55 {
		t.Fatalf("常态不封顶：read %d 条，want 55", len(res.read))
	}
	capped := capOps(res.read)
	if len(capped) != fileOpsTableCap {
		t.Fatalf("减灾档 cap 后 %d 条，want %d", len(capped), fileOpsTableCap)
	}
	if capped[0].path != "/f05" || capped[len(capped)-1].path != "/f54" {
		t.Fatalf("淘汰应砍最老 5 条：头 %q 尾 %q，want /f05 与 /f54", capped[0].path, capped[len(capped)-1].path)
	}
	// 两表独立：51 条 written 由 capOps 砍到 50；同场景下 3 条 read 不被带动。
	var mixed []ModelMessage
	for i := 0; i < 51; i++ {
		mixed = append(mixed, fileCallMsg("r_1", fileCall("write_file", fmt.Sprintf(`{"path":"/g%02d"}`, i))))
	}
	for i := 0; i < 3; i++ {
		mixed = append(mixed, fileCallMsg("r_1", fileCall("read_file", fmt.Sprintf(`{"path":"/h%02d"}`, i))))
	}
	res = resolveOps(extractFileOps(mixed, registry))
	changed := capOps(res.changed)
	if len(changed) != fileOpsTableCap || changed[0].path != "/g01" || changed[len(changed)-1].path != "/g50" {
		t.Fatalf("changed 表减灾 cap 形状漂移：%d 条 头 %q 尾 %q", len(changed), changed[0].path, changed[len(changed)-1].path)
	}
	if len(res.read) != 3 {
		t.Fatalf("read 表 %d 条，want 3（两表各自独立砍，改过表的 cap 不动读过）", len(res.read))
	}
}

// fixture 22 坏 arguments JSON：跳过 + log，不炸也不猜（write_doc 若不跳过本会产 written）。
func TestFileOpsBadArgumentsJSON(t *testing.T) {
	ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("write_doc", `{not json`))}, nil)
	assertEntries(t, "read", ops.read, nil)
	assertEntries(t, "written", ops.written, nil)
	assertEntries(t, "edited", ops.edited, nil)
}

// fixture 23 命令名只在段首生效：参数位置出现同名词是 echo/printf 的 payload，不抓
// （旧实现逐词匹配，echo touch d1 会把 d1 误记成「改过」）；正例（touch 在段首）见 fixture 16。
func TestFileOpsShellCommandWordOnlyAtSegmentStart(t *testing.T) {
	for _, cmd := range []string{"echo touch d1", "printf %s cp", "echo cp a b"} {
		ops := extractFileOps([]ModelMessage{fileCallMsg("r_1", fileCall("bash", fmt.Sprintf(`{"command":%q}`, cmd)))}, nil)
		assertEntries(t, cmd, ops.written, nil)
	}
}
