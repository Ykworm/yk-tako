package runengine

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// 这个文件是**契约字节的快闸**：钉住信封、session/frames/log 三个响应体、帧 kind 词表、
// 关键业务码、工具分档现状。
//
// 为什么要有它：19v6-04 的期望字节原先只有一份人读散文，改契约字节后「跑歪了不会红」——
// go test 里没有这份断言，唯一的防线是人当时在不在场。现在分两层：
//
//	快闸（本文件）：不打模型、不用 API Key、不依赖 PG/Redis —— 每拍收尾的 go test 就跑它；
//	活体（scripts/acceptance/）：真起引擎、真跑一轮 —— 见 scripts/acceptance/README.md。
//
// 下面的期望字节**不是手写的**，是 2026-10-22 从活引擎实跑抄下来的：
//
//	GET /api/sessions/{sid}
//	{"code":0,"message":"","data":{"liveRunId":"","runlogOffset":2,"lastRunStatus":"complete","lastRunId":"r_…","workspaceId":null}}
//	GET /api/runs/{rid}/frames?eventOffset=0
//	{"code":0,"message":"","data":{"frames":[{"eventOffset":1,"kind":"start","agent":"","model":"","messageId":"m_v6"},…],"nextOffset":4,"status":"done"}}
//	GET /api/sessions/{sid}/log?runlogOffset=0
//	{"code":0,"message":"","data":{"rows":[{"runlogOffset":1,"runId":"r_…","role":"user","agent":"","model":"","messageId":"m_v6","text":"只回复 ok"},…],"nextOffset":2}}

// mustJSON 在 worker_test.go 里已有（同包共用），这里不重复定义。

// TestContractEnvelopeBytes 钉住信封三字段的**顺序**（code → message → data）与失败体形状。
//
// 顺序是契约的一部分：页面拦截器按 code 分支，前端「拆包跟着改」只拆一次（doc/40 §1）。
func TestContractEnvelopeBytes(t *testing.T) {
	got := mustJSON(t, envelope{Code: 0, Message: "", Data: nil})
	const want = `{"code":0,"message":"","data":null}`
	if got != want {
		t.Fatalf("信封字节 = %s\nwant %s", got, want)
	}
	// 失败体：data 为 null 时**键仍然在**（页面按 code 分支，不靠字段在不在）。
	got = mustJSON(t, envelope{Code: 112001, Message: "找不到这场会话", Data: nil})
	const wantFail = `{"code":112001,"message":"找不到这场会话","data":null}`
	if got != wantFail {
		t.Fatalf("失败信封字节 = %s\nwant %s", got, wantFail)
	}
}

// TestContractSessionResponseBytes 钉住 GET /api/sessions/{id} 的 data 字段序。
//
// 五字段顺序 = doc/30 §8.4 契约字节序（liveRunId → runlogOffset → lastRunStatus → lastRunId → workspaceId）。
// 少一个、多一个、换个位置，都是契约漂移。
func TestContractSessionResponseBytes(t *testing.T) {
	got := mustJSON(t, sessionResponse{
		LiveRunID:     "",
		RunlogOffset:  2,
		LastRunStatus: "complete",
		LastRunID:     "r_01M5FWGHGYFKM836A692ZB7F31",
		WorkspaceID:   nil,
	})
	const want = `{"liveRunId":"","runlogOffset":2,"lastRunStatus":"complete","lastRunId":"r_01M5FWGHGYFKM836A692ZB7F31","workspaceId":null}`
	if got != want {
		t.Fatalf("session 字节 = %s\nwant %s", got, want)
	}
}

// TestContractFramesResponseBytes 钉住 frames 响应体与单帧的字节（19v6-01 §2）。
func TestContractFramesResponseBytes(t *testing.T) {
	got := mustJSON(t, framesResponse{
		Frames: []Frame{
			{EventOffset: 1, Kind: "start", MessageID: "m_v6"},
			{EventOffset: 2, Kind: "delta", Model: "deepseek/deepseek-flash", Text: "ok"},
			{EventOffset: 4, Kind: "terminal", Status: "complete"},
		},
		NextOffset: 4,
		Status:     "done",
	})
	const want = `{"frames":[{"eventOffset":1,"kind":"start","agent":"","model":"","messageId":"m_v6"},` +
		`{"eventOffset":2,"kind":"delta","agent":"","model":"deepseek/deepseek-flash","text":"ok"},` +
		`{"eventOffset":4,"kind":"terminal","agent":"","model":"","status":"complete"}],"nextOffset":4,"status":"done"}`
	if got != want {
		t.Fatalf("frames 字节 = %s\nwant %s", got, want)
	}
}

// TestContractUsageFrameBytes 钉住 usage 帧的五个数值列与顺序（doc/27 §4a）。
//
// 账单缺失不发帧、不发 0 —— 所以这里是「有值时」的形状。
func TestContractUsageFrameBytes(t *testing.T) {
	got := mustJSON(t, Frame{
		EventOffset: 3, Kind: "usage", Model: "deepseek/deepseek-flash",
		PromptTokens: 3932, CompletionTokens: 2, TotalTokens: 3934, Window: 256000,
	})
	const want = `{"eventOffset":3,"kind":"usage","agent":"","model":"deepseek/deepseek-flash",` +
		`"promptTokens":3932,"completionTokens":2,"totalTokens":3934,"window":256000}`
	if got != want {
		t.Fatalf("usage 帧字节 = %s\nwant %s", got, want)
	}
}

// TestContractLogResponseBytes 钉住 runlog 响应体与行字节（19v6-01 §3）。
func TestContractLogResponseBytes(t *testing.T) {
	rows := []Row{
		{RunlogOffset: 1, RunID: "r_01M5FWGHGYFKM836A692ZB7F31", Role: "user", MessageID: "m_v6", Text: "只回复 ok"},
		{RunlogOffset: 2, RunID: "r_01M5FWGHGYFKM836A692ZB7F31", Role: "assistant", Model: "deepseek/deepseek-flash", Text: "ok",
			Usage: &UsageBill{PromptTokens: 3932, CompletionTokens: 2, TotalTokens: 3934, Window: 256000}},
	}
	got := mustJSON(t, logResponse{Rows: rows, NextOffset: 2})
	const want = `{"rows":[{"runlogOffset":1,"runId":"r_01M5FWGHGYFKM836A692ZB7F31","role":"user","agent":"","model":"","messageId":"m_v6","text":"只回复 ok"},` +
		`{"runlogOffset":2,"runId":"r_01M5FWGHGYFKM836A692ZB7F31","role":"assistant","agent":"","model":"deepseek/deepseek-flash","text":"ok",` +
		`"usage":{"promptTokens":3932,"completionTokens":2,"totalTokens":3934,"window":256000}}],"nextOffset":2}`
	if got != want {
		t.Fatalf("log 字节 = %s\nwant %s", got, want)
	}
}

// TestContractBusinessCodes 钉住 doc/40 §4 业务码表里的关键码值。
//
// 码值是**契约**：前端按 code 分支（登录过期 111104、会话忙 112101、找不到会话 112001……）。
// 码值变了而页面没跟着改，页面会静默走进错分支——所以这里逐个钉死。
func TestContractBusinessCodes(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"成功", codeOK, 0},
		{"缺 token", codeMissingToken, 111102},
		{"token 过期", codeTokenExpired, 111104},
		{"模型不可用", codeModelUnavailable, 111408},
		{"找不到会话", codeSessionMissing, 112001},
		{"找不到 run", codeRunMissing, 112002},
		{"会话正在跑", codeSessionRunning, 112101},
		{"不可续跑", codeNotResumable, 112102},
		{"续跑次数用尽", codeResumeExhausted, 112104},
		{"找不到项目", codeWorkspaceMissing, 114001},
		{"找不到提问", codeQuestionMissing, 115002},
		{"提问已答过", codeQuestionAnswered, 115101},
		// 114004「项目正忙」已退役（2026-10-22，项目闸随跨成员共享项目一起取消，doc/42）——
		// 同项目并发不再被拒，改由文件级防护兜（flock + 写前指纹比对）。
		{"默认项目不能删", codeWorkspaceDefault, 114005},
		{"活 run 不能删", codeSessionBusy, 114007},
		{"内部错误", codeInternal, 111901},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s 码 = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestContractFrameKindVocabulary 钉住帧 kind 的**词表**：八种，不多不少。
//
// kind 是事件流的基本词汇表（19v6-01 §2）：客户端按 kind 分派渲染。
// 加一种 kind 而不同步契约文档，客户端就会撞上没见过的分支——所以这里扫生产代码里
// 所有 `Kind: "…"` 字面量，跟词表对账。少了（改没了）或多了一种都红。
// 只认 Frame 字面量上的 kind —— 别的结构体也有 Kind 字段（租户的 kind = personal/team，
// 那不是帧 kind）。
var frameKindRE = regexp.MustCompile(`Frame\{Kind:\s*"([a-z_]+)"`)

func TestContractFrameKindVocabulary(t *testing.T) {
	want := []string{"delta", "question", "resume", "start", "task", "terminal", "tool_call", "tool_result", "usage"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录失败：%v", err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", name, err)
		}
		for _, m := range frameKindRE.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = true
		}
	}
	got := make([]string, 0, len(found))
	for k := range found {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("帧 kind 词表 = %v\nwant %v（加了一种 kind 或改没了，都得同步 19v6-01 §2 与本测试）", got, want)
	}
}

// TestContractToolTiering 钉住工具分档的**现状**：deferred 档只收 mcp__ 前缀，一手工具每轮全量声明。
//
// 这不是「已完成」的断言，是「现状如此」的断言——它防的是
// 「进度误会.md」第 1 条记的那个误会：以为工具说明已经隔着 tool_search 检索，
// 其实只有 MCP 档隔着，一手工具（get_time、文件工具族、bash、memory、search_history、web_search…）
// 仍是 direct 档，每轮全量声明（19v6-06 §3、进度误会.md 第 1 条）。
//
// 要改成「一手工具也进 deferred 档」时，先改 19v6-06 与进度误会.md，再改这里。
func TestContractToolTiering(t *testing.T) {
	if deferredToolPrefix != "mcp__" {
		t.Fatalf("deferred 前缀 = %q, want \"mcp__\"", deferredToolPrefix)
	}
	deferred := []string{"mcp__fs__read_file", "mcp__anything__tool"}
	for _, n := range deferred {
		if !isDeferredTool(n) {
			t.Errorf("%s 应属 deferred 档", n)
		}
	}
	direct := []string{
		"get_time", "bash",
		"write_file", "read_text_file", "edit_file", "list_directory", "search_files",
		"memory", "search_history", "web_search",
	}
	for _, n := range direct {
		if isDeferredTool(n) {
			t.Errorf("%s 应属 direct 档（每轮全量声明）", n)
		}
	}
}

// TestContractStreamEventBytes 钉住 SSE 事件帧的 data 字节（doc/46 §3.3 用例 9）。
//
// 键序 seq → type → id → at 是契约的一部分：前端 EventSource 收到后按 type 分派失效
//（web/src/mutation.ts 的 dispatch）。下面的字节不是手写的，是 2026-10-23 从活引擎实跑抄的：
//
//	GET /api/events（curl -N 挂读）后 DELETE /api/sessions/{sid}，收到的帧：
//	id: 1792737898948-0
//	data: {"seq":"1792737898948-0","type":"session_deleted","id":"s_01M5J5VGJ6VWG1DBCW0R8WA4JG","at":"2026-10-23T06:44:58Z"}
//
// 挂读型端点的活体断言在 scripts/acceptance/events_live.sh；这里只钉可纯函数判定的字节。
func TestContractStreamEventBytes(t *testing.T) {
	got := mustJSON(t, streamEvent{
		Seq:  "1792737898948-0",
		Type: "session_deleted",
		ID:   "s_01M5J5VGJ6VWG1DBCW0R8WA4JG",
		At:   "2026-10-23T06:44:58Z",
	})
	const want = `{"seq":"1792737898948-0","type":"session_deleted","id":"s_01M5J5VGJ6VWG1DBCW0R8WA4JG","at":"2026-10-23T06:44:58Z"}`
	if got != want {
		t.Fatalf("事件帧字节 = %s\nwant %s", got, want)
	}
}

// TestContractWorkspaceFilesBytes 钉住 GET /api/workspaces/{id}/files 的 data 字段序
//（doc/48 B1）：path → entries；entry = name → kind →（**只有文件**才有）size → modifiedAt。
//
// 目录**不出现** size / modifiedAt 这两个键（不是 0）——加字段忘了同步、或把目录写成 0，
// 这里就红。下面的字节不是手写的，是 2026-10-23 从活引擎实跑抄下来的。
func TestContractWorkspaceFilesBytes(t *testing.T) {
	size := int64(14)
	mod := time.Date(2026, 10, 23, 0, 7, 12, 0, time.UTC)
	got := mustJSON(t, workspaceFilesData{
		Path: ".",
		Entries: []fileEntry{
			{Name: "sub", Kind: "dir"},
			{Name: "hello.txt", Kind: "file", Size: &size, ModifiedAt: &mod},
		},
	})
	const want = `{"path":".","entries":[{"name":"sub","kind":"dir"},{"name":"hello.txt","kind":"file","size":14,"modifiedAt":"2026-10-23T00:07:12Z"}]}`
	if got != want {
		t.Fatalf("文件清单字节序漂移：\n得到 %s\n期望 %s", got, want)
	}
}

// TestContractWorkspaceBodyBytes 钉住项目对象的字段序（doc/48 C1）。
//
// isDefault 加在**最后**（既有字段序不动）。默认项目不可删（服务端 114005）——
// 页面靠这个字段把删除按钮藏起来，别让用户点一个必然失败的按钮。
// 下面的字节不是手写的，是 2026-10-23 从活引擎实跑抄下来的。
func TestContractWorkspaceBodyBytes(t *testing.T) {
	got := mustJSON(t, workspaceBody{
		ID: "ws_01M5J00E5X3AH3DVKX5QA0647Q", Name: "验收项目",
		CreatedAt:  time.Date(2026, 10, 22, 14, 24, 0, 0, time.UTC),
		Visibility: "private", Owner: true, IsDefault: false,
	})
	const want = `{"id":"ws_01M5J00E5X3AH3DVKX5QA0647Q","name":"验收项目","createdAt":"2026-10-22T14:24:00Z","visibility":"private","owner":true,"isDefault":false}`
	if got != want {
		t.Fatalf("项目对象字节序漂移：\n得到 %s\n期望 %s", got, want)
	}
}
