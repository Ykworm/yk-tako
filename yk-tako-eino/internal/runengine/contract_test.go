package runengine

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

// TestContractLogQuestionSegmentsBytes 钉住含提问 run 的 assistant 行分段形状（planning §3.1）：
// segments 紧跟账单字段；无分段的行字节见上一条（不写该键）。
func TestContractLogQuestionSegmentsBytes(t *testing.T) {
	got := mustJSON(t, Row{
		RunlogOffset: 3, RunID: "r_1", Role: "assistant", Model: "deepseek/deepseek-flash",
		Text:     "先问一下。好的。",
		Usage:    &UsageBill{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		Segments: []RunlogSegment{{End: 5, ToolCallID: "call_ask_1"}},
	})
	const want = `{"runlogOffset":3,"runId":"r_1","role":"assistant","agent":"","model":"deepseek/deepseek-flash",` +
		`"text":"先问一下。好的。","usage":{"promptTokens":1,"completionTokens":2,"totalTokens":3},` +
		`"segments":[{"end":5,"toolCallId":"call_ask_1"}]}`
	if got != want {
		t.Fatalf("分段行字节 = %s\nwant %s", got, want)
	}
}

// TestContractQuestionViewBytes 钉住 questions 响应行的字段序与新增字段（planning §3.3）：
// toolCallId 在 runId 之后；answer 对所有 kind 回放。
func TestContractQuestionViewBytes(t *testing.T) {
	got := mustJSON(t, questionView{
		QuestionID: "qs_1", RunID: "r_1", ToolCallID: "call_ask_1", Kind: "question",
		Question: "选 A 还是 B？", Status: "answered", Answer: "A",
		CreatedAt: time.Unix(0, 0).UTC(),
	})
	const want = `{"questionId":"qs_1","runId":"r_1","toolCallId":"call_ask_1","kind":"question",` +
		`"question":"选 A 还是 B？","status":"answered","answer":"A","createdAt":"1970-01-01T00:00:00Z"}`
	if got != want {
		t.Fatalf("question 行字节 = %s\nwant %s", got, want)
	}
	// Task 问题设了截止时间才多出 expiresAt（doc/72 §4.5）；普通会话行字节不变。
	exp := time.Unix(86400, 0).UTC()
	got = mustJSON(t, questionView{
		QuestionID: "qs_2", RunID: "r_1", ToolCallID: "call_ask_2", Kind: "question",
		Question: "选 A 还是 B？", Status: "pending", CreatedAt: time.Unix(0, 0).UTC(), ExpiresAt: &exp,
	})
	const wantExpires = `{"questionId":"qs_2","runId":"r_1","toolCallId":"call_ask_2","kind":"question",` +
		`"question":"选 A 还是 B？","status":"pending","createdAt":"1970-01-01T00:00:00Z",` +
		`"expiresAt":"1970-01-02T00:00:00Z"}`
	if got != wantExpires {
		t.Fatalf("Task 问题行字节 = %s\nwant %s", got, wantExpires)
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
		{"请求格式不对", codeBadJSON, 111001},
		{"请求参数不对", codeBadInput, 111002},
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
		{"记忆模式不提供观测口", codeMemoryMode, 118001},
		{"找不到任务", codeTaskMissing, 117001},
		{"找不到编排组", codeOrchMissing, 117002},
		{"计划不合法", codePlanInvalid, 117101},
		{"幂等键冲突", codePlanConflict, 117102},
		{"任务不可恢复", codeTaskNotResumable, 117103},
		{"任务已结束", codeTaskFinished, 117104},
		{"编排组已结束", codeOrchFinished, 117105},
		{"计划版本过期", codeRevisionStale, 117106},
		{"编排组不可修订", codeNotRevisable, 117107},
		{"修订键命令冲突", codeRevisionConflict, 117108},
		{"提问已过期", codeQuestionExpired, 115102},
		{"工作区配置不合法", codeWorkspaceConfigInvalid, 117111},
		{"任务不可重新发布", codeTaskNotRepublishable, 117112},
		{"项目正在发布结果", codeWorkspacePublishing, 117113},
		{"找不到命令", codeJobMissing, 117117},
		{"命令不可停止", codeJobNotStoppable, 117118},
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
// 那条「进度误会」记录第 1 条记的误会：以为工具说明已经隔着 tool_search 检索，
// 其实只有 MCP 档隔着，一手工具（get_time、文件工具族、bash、memory、search_history、web_search…）
// 仍是 direct 档，每轮全量声明（19v6-06 §3）。
//
// 要改成「一手工具也进 deferred 档」时，先改 19v6-06 与那份「进度误会」记录，再改这里。
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
		"memory", "search_history", "web_search", "web_fetch",
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
// （web/src/mutation.ts 的 dispatch）。下面的字节不是手写的，是 2026-10-23 从活引擎实跑抄的：
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

// TestContractRunViewsBytes 钉住对话页右侧工作区两个读口（doc/78 §4）的 data 字段序：
//
//	changed-files：runId → files →（每项）path → op
//	run tasks：runId → rows →（每行）no → subject → status → description → result → skillName → updatedAt → total → done
//
// 下面的字节不是手写的，是 2026-11-13 从隔离引擎（8943 / tako_acc78）实跑抄下来的；
// 时间在实跑里带 +08:00 偏移，这里固定成 UTC 只为让字节可复现（时间自带时区，不是契约）。
func TestContractRunViewsBytes(t *testing.T) {
	got := mustJSON(t, envelope{Code: 0, Message: "", Data: runChangedFilesData{
		RunID: "r_acc65",
		Files: []runChangedFile{
			{Path: "quote.py", Op: "write"},
			{Path: "test_quote.py", Op: "edit"},
		},
	}})
	const wantChanged = `{"code":0,"message":"","data":{"runId":"r_acc65","files":[{"path":"quote.py","op":"write"},{"path":"test_quote.py","op":"edit"}]}}`
	if got != wantChanged {
		t.Fatalf("改动文件字节序漂移：\n得到 %s\n期望 %s", got, wantChanged)
	}

	at := time.Date(2026, 11, 13, 6, 32, 26, 503178000, time.UTC)
	got = mustJSON(t, envelope{Code: 0, Message: "", Data: runTasksData{
		RunID: "r_acc65",
		Rows: []runTaskRow{
			{No: 1, Subject: "实现报价计算", Status: "completed", Description: "", Result: "读取两份文件并生成 quote.py", SkillName: "", UpdatedAt: at},
			{No: 2, Subject: "跑单测", Status: "in_progress", Description: "", Result: "", SkillName: "", UpdatedAt: at},
		},
		Total: 2, Done: 1,
	}})
	const wantTasks = `{"code":0,"message":"","data":{"runId":"r_acc65","rows":[{"no":1,"subject":"实现报价计算","status":"completed","description":"","result":"读取两份文件并生成 quote.py","skillName":"","updatedAt":"2026-11-13T06:32:26.503178Z"},{"no":2,"subject":"跑单测","status":"in_progress","description":"","result":"","skillName":"","updatedAt":"2026-11-13T06:32:26.503178Z"}],"total":2,"done":1}}`
	if got != wantTasks {
		t.Fatalf("看板字节序漂移：\n得到 %s\n期望 %s", got, wantTasks)
	}

	// 空清单/空看板：数组恒在（不是 null），前端 .map 才不炸。
	got = mustJSON(t, envelope{Code: 0, Message: "", Data: runChangedFilesData{RunID: "r_e", Files: []runChangedFile{}}})
	const wantEmptyFiles = `{"code":0,"message":"","data":{"runId":"r_e","files":[]}}`
	if got != wantEmptyFiles {
		t.Fatalf("空改动清单字节 = %s\n期望 %s", got, wantEmptyFiles)
	}
	got = mustJSON(t, envelope{Code: 0, Message: "", Data: runTasksData{RunID: "r_e", Rows: []runTaskRow{}, Total: 0, Done: 0}})
	const wantEmptyRows = `{"code":0,"message":"","data":{"runId":"r_e","rows":[],"total":0,"done":0}}`
	if got != wantEmptyRows {
		t.Fatalf("空看板字节 = %s\n期望 %s", got, wantEmptyRows)
	}

	// 两个失败体与 /skills 同字节：看不见与不存在不区分。
	got = mustJSON(t, envelope{Code: codeRunMissing, Message: "找不到这次运行", Data: nil})
	const wantMissing = `{"code":112002,"message":"找不到这次运行","data":null}`
	if got != wantMissing {
		t.Fatalf("404 字节 = %s\n期望 %s", got, wantMissing)
	}
	got = mustJSON(t, envelope{Code: codeMissingToken, Message: "请先登录", Data: nil})
	const want401 = `{"code":111102,"message":"请先登录","data":null}`
	if got != want401 {
		t.Fatalf("401 字节 = %s\n期望 %s", got, want401)
	}
}

// TestContractWorkspaceFilesBytes 钉住 GET /api/workspaces/{id}/files 的 data 字段序
// （doc/48 B1）：path → entries；entry = name → kind →（**只有文件**才有）size → modifiedAt。
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

// TestContractMemoryObserveBytes 钉住记忆观测口（doc/65 §6.5）的契约字节。
//
// 下面的字节不是手写的，是 2026-11-10 从活引擎实跑抄下来的（两个模式各起一台）：
//
//	markdown 引擎：GET /api/memory/items、/jobs、/recall?q=x 三连 409，同一份守卫体；
//	pipeline 引擎空库：items/jobs 的 data 是空数组（不是 null，页面 .map 才不炸）；
//	recall 无命中回四键全零。
//
// recall 的 data 是 gin.H（map）——字节序由 encoding/json 按键名字典序定，服务端写键的
// 顺序不作数，所以这里用同一个 gin.H 复现：block → dialogueHits → injectedRunes → memoryHits。
// 模式门控的活体断言在 expected.json 用例 35–38（needs=markdown-mode / pipeline-mode）。
func TestContractMemoryObserveBytes(t *testing.T) {
	got := mustJSON(t, envelope{Code: codeMemoryMode, Message: ErrMemoryModeUnsupported.Error(), Data: nil})
	const wantGuard = `{"code":118001,"message":"当前记忆模式不提供这个观测口","data":null}`
	if got != wantGuard {
		t.Fatalf("记忆模式守卫字节 = %s\nwant %s", got, wantGuard)
	}
	got = mustJSON(t, gin.H{"items": []memoryItemResponse{}})
	if got != `{"items":[]}` {
		t.Fatalf("items 空库字节 = %s, want {\"items\":[]}", got)
	}
	got = mustJSON(t, gin.H{"jobs": []memoryJobResponse{}})
	if got != `{"jobs":[]}` {
		t.Fatalf("jobs 空库字节 = %s, want {\"jobs\":[]}", got)
	}
	got = mustJSON(t, gin.H{"block": "", "memoryHits": 0, "dialogueHits": 0, "injectedRunes": 0})
	const wantRecall = `{"block":"","dialogueHits":0,"injectedRunes":0,"memoryHits":0}`
	if got != wantRecall {
		t.Fatalf("recall 空命中字节 = %s\nwant %s", got, wantRecall)
	}
	got = mustJSON(t, envelope{Code: codeBadInput, Message: "state 只能是 active、superseded、removed 或 archived", Data: nil})
	const wantState = `{"code":111002,"message":"state 只能是 active、superseded、removed 或 archived","data":null}`
	if got != wantState {
		t.Fatalf("state 校验字节 = %s\nwant %s", got, wantState)
	}
	got = mustJSON(t, envelope{Code: codeBadJSON, Message: "请求格式不对", Data: nil})
	const wantEmptyQ = `{"code":111001,"message":"请求格式不对","data":null}`
	if got != wantEmptyQ {
		t.Fatalf("recall 空 q 字节 = %s\nwant %s", got, wantEmptyQ)
	}
}

// TestContractAgentTaskViewBytes 钉住 Task 视图的字段序（doc/70 §4.3；施工包 §4.3）：
// taskId → orchestrationId → key → content → agent → modelRef → workspaceId → status
// → currentRunId → result → error → resultRevision → createdAt → startedAt → finishedAt → updatedAt。
// 未提交时 result 为 null；失败时 error = {code, message}（机器原因与展示文案分开）。
func TestContractAgentTaskViewBytes(t *testing.T) {
	rev := mustJSON(t, agentTaskView{
		TaskID: "at_1", OrchestrationID: "or_1", Key: "C", Content: "汇总成报告",
		Agent: "coder", ModelRef: "", WorkspaceID: "ws_1", Status: "succeeded",
		CurrentRunID: "r_1",
		Result: &TaskResult{
			Summary:   "报告写好了",
			Artifacts: []TaskArtifact{{Path: "reports/a.md", Kind: "report"}},
		},
		ResultRevision: 1,
		CreatedAt:      time.Unix(0, 0).UTC(),
		StartedAt:      ptrTime(time.Unix(1, 0).UTC()),
		FinishedAt:     ptrTime(time.Unix(2, 0).UTC()),
		UpdatedAt:      time.Unix(0, 0).UTC(),
	})
	const wantRev = `{"taskId":"at_1","orchestrationId":"or_1","key":"C","content":"汇总成报告","agent":"coder","modelRef":"","planCheckpoint":false,"createdRevision":0,"workspaceId":"ws_1","status":"succeeded","currentRunId":"r_1",` +
		`"result":{"summary":"报告写好了","artifacts":[{"path":"reports/a.md","kind":"report"}],"metadata":null},"error":null,"resultRevision":1,` +
		`"createdAt":"1970-01-01T00:00:00Z","startedAt":"1970-01-01T00:00:01Z","finishedAt":"1970-01-01T00:00:02Z","updatedAt":"1970-01-01T00:00:00Z"}`
	if rev != wantRev {
		t.Fatalf("Task 视图字节序漂移：\n得到 %s\n期望 %s", rev, wantRev)
	}
	failed := mustJSON(t, agentTaskView{
		TaskID: "at_2", OrchestrationID: "or_1", Key: "A", Content: "调研", Agent: "researcher",
		WorkspaceID: "ws_1", Status: "failed",
		Error:      &taskError{Code: taskErrRunError, Message: "执行 run 失败"},
		CreatedAt:  time.Unix(0, 0).UTC(),
		UpdatedAt:  time.Unix(0, 0).UTC(),
		FinishedAt: ptrTime(time.Unix(2, 0).UTC()),
	})
	const wantFail = `{"taskId":"at_2","orchestrationId":"or_1","key":"A","content":"调研","agent":"researcher","modelRef":"","planCheckpoint":false,"createdRevision":0,"workspaceId":"ws_1","status":"failed","currentRunId":"",` +
		`"result":null,"error":{"code":"run_error","message":"执行 run 失败"},"resultRevision":0,` +
		`"createdAt":"1970-01-01T00:00:00Z","startedAt":null,"finishedAt":"1970-01-01T00:00:02Z","updatedAt":"1970-01-01T00:00:00Z"}`
	if failed != wantFail {
		t.Fatalf("失败 Task 字节漂移：\n得到 %s\n期望 %s", failed, wantFail)
	}
}

// TestContractOrchestrationViewBytes 钉住编排组视图的字段序（doc/70 §4.3；doc/73 §4.4）：
// 组体 orchestrationId → goal → workspaceId → sourceSessionId → status → finalTaskId
// → planRevision → planHold → deliveryStatus → deliveryRunId → deliveryErrorCode
// → createdAt → finishedAt → deliveredAt → updatedAt；
// 外层 orchestration → tasks → links；边 = parentTaskId → childTaskId。
func TestContractOrchestrationViewBytes(t *testing.T) {
	got := mustJSON(t, orchestrationView{
		Orchestration: orchestrationBody{
			OrchestrationID: "or_1", Goal: "调研 A、B 后汇总", WorkspaceID: "ws_1",
			SourceSessionID: "s_1", Status: "active", FinalTaskID: "at_1",
			DeliveryStatus: "", DeliveryRunID: "", DeliveryErrorCode: "",
			CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
		},
		Tasks: []agentTaskView{{
			TaskID: "at_1", OrchestrationID: "or_1", Key: "C", Content: "汇总成报告",
			Agent: "coder", WorkspaceID: "ws_1", Status: "ready",
			CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
		}},
		Links: []taskLinkView{{ParentTaskID: "at_2", ChildTaskID: "at_1"}},
	})
	const want = `{"orchestration":{"orchestrationId":"or_1","goal":"调研 A、B 后汇总","workspaceId":"ws_1","sourceSessionId":"s_1","status":"active","finalTaskId":"at_1","planRevision":0,"planHold":false,"deliveryStatus":"","deliveryRunId":"","deliveryErrorCode":"",` +
		`"createdAt":"1970-01-01T00:00:00Z","finishedAt":null,"deliveredAt":null,"updatedAt":"1970-01-01T00:00:00Z"},` +
		`"tasks":[{"taskId":"at_1","orchestrationId":"or_1","key":"C","content":"汇总成报告","agent":"coder","modelRef":"","planCheckpoint":false,"createdRevision":0,"workspaceId":"ws_1","status":"ready","currentRunId":"",` +
		`"result":null,"error":null,"resultRevision":0,"createdAt":"1970-01-01T00:00:00Z","startedAt":null,"finishedAt":null,"updatedAt":"1970-01-01T00:00:00Z"}],` +
		`"links":[{"parentTaskId":"at_2","childTaskId":"at_1"}]}`
	if got != want {
		t.Fatalf("编排组视图字节序漂移：\n得到 %s\n期望 %s", got, want)
	}
}

// TestContractWorkspaceViewBytes 钉住隔离组的工作区读面字节（doc/74 §4.3）。
// 真实字节来自 2026-11-12 隔离引擎 curl（tmp/74 探针 P9/P14）：workspace 恒在视图**末尾**，
// 键序 mode → provider → verification(configured → status，收尾证据追加 exitCode → outputPath)
// → publication(status → errorCode → epoch)。
// shared 组不带 workspace 键——那半由 TestContractAgentTaskViewBytes / TestContractOrchestrationViewBytes
// 的既有字节钉着（它们就是 doc/74 之前的形状，一个字节都不能动）。
func TestContractWorkspaceViewBytes(t *testing.T) {
	ws := &taskWorkspaceView{
		Mode: "isolated", Provider: "pending",
		Verification: workspaceVerificationView{Configured: true, Status: ""},
		Publication:  workspacePublicationView{Status: "", ErrorCode: "", Epoch: 0},
	}
	got := mustJSON(t, agentTaskView{
		TaskID: "at_1", OrchestrationID: "or_1", Key: "main", Content: "探针单件",
		Agent: "coder", WorkspaceID: "ws_1", Status: "ready",
		CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
		Workspace: ws,
	})
	const wantTask = `{"taskId":"at_1","orchestrationId":"or_1","key":"main","content":"探针单件","agent":"coder","modelRef":"","planCheckpoint":false,"createdRevision":0,"workspaceId":"ws_1","status":"ready","currentRunId":"","result":null,"error":null,"resultRevision":0,` +
		`"createdAt":"1970-01-01T00:00:00Z","startedAt":null,"finishedAt":null,"updatedAt":"1970-01-01T00:00:00Z",` +
		`"workspace":{"mode":"isolated","provider":"pending","verification":{"configured":true,"status":""},"publication":{"status":"","errorCode":"","epoch":0}}}`
	if got != wantTask {
		t.Fatalf("隔离 Task 视图字节漂移：\n得到 %s\n期望 %s", got, wantTask)
	}
	got = mustJSON(t, orchestrationBody{
		OrchestrationID: "or_1", Goal: "探针组", WorkspaceID: "ws_1",
		SourceSessionID: "s_1", Status: "active", FinalTaskID: "at_1",
		CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
		Workspace: ws,
	})
	const wantOrch = `{"orchestrationId":"or_1","goal":"探针组","workspaceId":"ws_1","sourceSessionId":"s_1","status":"active","finalTaskId":"at_1","planRevision":0,"planHold":false,"deliveryStatus":"","deliveryRunId":"","deliveryErrorCode":"",` +
		`"createdAt":"1970-01-01T00:00:00Z","finishedAt":null,"deliveredAt":null,"updatedAt":"1970-01-01T00:00:00Z",` +
		`"workspace":{"mode":"isolated","provider":"pending","verification":{"configured":true,"status":""},"publication":{"status":"","errorCode":"","epoch":0}}}`
	if got != wantOrch {
		t.Fatalf("隔离组体字节漂移：\n得到 %s\n期望 %s", got, wantOrch)
	}
	// 发布 blocked 的形状（errorCode/epoch 有值时键序不变）。
	blocked := mustJSON(t, taskWorkspaceView{
		Mode: "isolated", Provider: "git",
		Verification: workspaceVerificationView{Configured: true, Status: "passed"},
		Publication:  workspacePublicationView{Status: "blocked", ErrorCode: "workspace_changed", Epoch: 3},
	})
	const wantBlocked = `{"mode":"isolated","provider":"git","verification":{"configured":true,"status":"passed"},"publication":{"status":"blocked","errorCode":"workspace_changed","epoch":3}}`
	if blocked != wantBlocked {
		t.Fatalf("blocked 工作区字节漂移：\n得到 %s\n期望 %s", blocked, wantBlocked)
	}
	// 验证证据收尾后追加 exitCode → outputPath（真实字节来自 tmp/74 探针 probe7.out V1/V3）；
	// 无证据/running 时这两键不出现——上面几段字节就是无证据形状，保持不变。
	exit3 := 3
	failed := mustJSON(t, workspaceVerificationView{
		Configured: true, Status: "failed", ExitCode: &exit3,
		OutputPath: "verification/at_01M7784TJ59P82HYAR3V44YV19.out",
	})
	const wantFailed = `{"configured":true,"status":"failed","exitCode":3,"outputPath":"verification/at_01M7784TJ59P82HYAR3V44YV19.out"}`
	if failed != wantFailed {
		t.Fatalf("验证失败证据字节漂移：\n得到 %s\n期望 %s", failed, wantFailed)
	}
	exit0 := 0
	passed := mustJSON(t, workspaceVerificationView{
		Configured: true, Status: "passed", ExitCode: &exit0,
		OutputPath: "verification/at_01M7784YT324ZSZ6AG3PJXY4XN.out",
	})
	const wantPassed = `{"configured":true,"status":"passed","exitCode":0,"outputPath":"verification/at_01M7784YT324ZSZ6AG3PJXY4XN.out"}`
	if passed != wantPassed {
		t.Fatalf("验证通过证据字节漂移：\n得到 %s\n期望 %s", passed, wantPassed)
	}
}

// TestContractTaskCreateRespBytes 钉住创建端点的 data（doc/70 §4.1；doc/82 §6）：
// 单件 taskId → orchestrationId → status → currentRunId；
// 整组 orchestrationId → status → finalTaskId（finalTaskId 只追加在末尾，旧字段顺序不变）。
func TestContractTaskCreateRespBytes(t *testing.T) {
	got := mustJSON(t, createTaskResp{TaskID: "at_1", OrchestrationID: "or_1", Status: "ready", CurrentRunID: ""})
	const wantTask = `{"taskId":"at_1","orchestrationId":"or_1","status":"ready","currentRunId":""}`
	if got != wantTask {
		t.Fatalf("创建 Task 字节 = %s\nwant %s", got, wantTask)
	}
	got = mustJSON(t, createOrchestrationResp{OrchestrationID: "or_1", Status: "active", FinalTaskID: "at_1"})
	const wantOrch = `{"orchestrationId":"or_1","status":"active","finalTaskId":"at_1"}`
	if got != wantOrch {
		t.Fatalf("创建编排组字节 = %s\nwant %s", got, wantOrch)
	}
}

// TestContractJobBytes 钉住后台命令（doc/82 §6.3）四个端点的 data 字节与失败信封。
// 真实字节来自 2026-11-15 隔离引擎（8943）实打：一条真实 bash 工具调用（echo hello-82）
// 登记成一行 job，再分别读详情/输出/停止。
//
//   - jobView（列表行与详情同形）：键序 jobId → workspaceId → sourceSessionId → sourceRunId
//     → sourceTaskId → sourceOrchestrationId → status → exitCode → stopRequested → backgrounded
//     → command → stdoutPath → stderrPath → createdAt → startedAt → endedAt；
//     非终态 exitCode/endedAt 是 null（不省略），chat 来源 sourceTaskId/sourceOrchestrationId 是空串。
//   - jobOutputView（/output 与 bash_output 同形）：jobId → status → exitCode → stdout → stderr
//     → stdoutPath → stderrPath。
//   - jobStopView：jobId → status → stopRequested（running → stopping+true；
//     已终态 → 当前态+false；仍 stopping → 409 117118，不是 200）。
func TestContractJobBytes(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	started := time.Date(2026, 11, 15, 12, 20, 44, 801376000, cst)
	ended := time.Date(2026, 11, 15, 12, 20, 44, 822077000, cst)
	zero := 0
	base := jobView{
		JobID:           "bj_01M7EPZA41EEWV0G90BGEZFBN8",
		WorkspaceID:     "ws_01M7EPZ8F3ANYRAJMW6EVDCCF1",
		SourceSessionID: "s_01M7EPZ8F3ANYRAJMW6EVDCCF1",
		SourceRunID:     "r_01M7EPZ8HFRNRJ5ZE8VYNVM9XX",
		Command:         "echo hello-82",
		StdoutPath:      "runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stdout.out",
		StderrPath:      "runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stderr.out",
		CreatedAt:       started,
		StartedAt:       &started,
	}

	// 运行中（后台身份）：exitCode/endedAt 为 null，键序不变。
	running := base
	running.Status = jobStatusRunning
	running.Backgrounded = true
	const wantRunning = `{"jobId":"bj_01M7EPZA41EEWV0G90BGEZFBN8","workspaceId":"ws_01M7EPZ8F3ANYRAJMW6EVDCCF1","sourceSessionId":"s_01M7EPZ8F3ANYRAJMW6EVDCCF1","sourceRunId":"r_01M7EPZ8HFRNRJ5ZE8VYNVM9XX","sourceTaskId":"","sourceOrchestrationId":"","status":"running","exitCode":null,"stopRequested":false,"backgrounded":true,"command":"echo hello-82","stdoutPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stdout.out","stderrPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stderr.out","createdAt":"2026-11-15T12:20:44.801376+08:00","startedAt":"2026-11-15T12:20:44.801376+08:00","endedAt":null}`
	if got := mustJSON(t, running); got != wantRunning {
		t.Fatalf("命令详情（运行中）字节 = %s\nwant %s", got, wantRunning)
	}

	// 停止中（中间态，非终态）：stopRequested=true，exitCode/endedAt 仍是 null。
	stopping := running
	stopping.Status = jobStatusStopping
	stopping.StopRequested = true
	const wantStopping = `{"jobId":"bj_01M7EPZA41EEWV0G90BGEZFBN8","workspaceId":"ws_01M7EPZ8F3ANYRAJMW6EVDCCF1","sourceSessionId":"s_01M7EPZ8F3ANYRAJMW6EVDCCF1","sourceRunId":"r_01M7EPZ8HFRNRJ5ZE8VYNVM9XX","sourceTaskId":"","sourceOrchestrationId":"","status":"stopping","exitCode":null,"stopRequested":true,"backgrounded":true,"command":"echo hello-82","stdoutPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stdout.out","stderrPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stderr.out","createdAt":"2026-11-15T12:20:44.801376+08:00","startedAt":"2026-11-15T12:20:44.801376+08:00","endedAt":null}`
	if got := mustJSON(t, stopping); got != wantStopping {
		t.Fatalf("命令详情（停止中）字节 = %s\nwant %s", got, wantStopping)
	}

	// 前台短命令（Backgrounded=false）终态：succeeded=0、endedAt 有值。
	done := base
	done.Status = jobStatusSucceeded
	done.ExitCode = &zero
	done.EndedAt = &ended
	const wantDone = `{"jobId":"bj_01M7EPZA41EEWV0G90BGEZFBN8","workspaceId":"ws_01M7EPZ8F3ANYRAJMW6EVDCCF1","sourceSessionId":"s_01M7EPZ8F3ANYRAJMW6EVDCCF1","sourceRunId":"r_01M7EPZ8HFRNRJ5ZE8VYNVM9XX","sourceTaskId":"","sourceOrchestrationId":"","status":"succeeded","exitCode":0,"stopRequested":false,"backgrounded":false,"command":"echo hello-82","stdoutPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stdout.out","stderrPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stderr.out","createdAt":"2026-11-15T12:20:44.801376+08:00","startedAt":"2026-11-15T12:20:44.801376+08:00","endedAt":"2026-11-15T12:20:44.822077+08:00"}`
	if got := mustJSON(t, done); got != wantDone {
		t.Fatalf("命令详情（终态）字节 = %s\nwant %s", got, wantDone)
	}

	// /output（运行中）：exitCode null，stdout 是当前尾部，两个路径恒在。
	outRunning := mustJSON(t, jobOutputView{
		JobID: running.JobID, Status: jobStatusRunning,
		Stdout: "hello-82\n", Stderr: "",
		StdoutPath: running.StdoutPath, StderrPath: running.StderrPath,
	})
	const wantOutRunning = `{"jobId":"bj_01M7EPZA41EEWV0G90BGEZFBN8","status":"running","exitCode":null,"stdout":"hello-82\n","stderr":"","stdoutPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stdout.out","stderrPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stderr.out"}`
	if outRunning != wantOutRunning {
		t.Fatalf("命令输出（运行中）字节 = %s\nwant %s", outRunning, wantOutRunning)
	}
	outDone := mustJSON(t, jobOutputView{
		JobID: done.JobID, Status: jobStatusSucceeded, ExitCode: &zero,
		Stdout: "hello-82\n", Stderr: "",
		StdoutPath: done.StdoutPath, StderrPath: done.StderrPath,
	})
	const wantOutDone = `{"jobId":"bj_01M7EPZA41EEWV0G90BGEZFBN8","status":"succeeded","exitCode":0,"stdout":"hello-82\n","stderr":"","stdoutPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stdout.out","stderrPath":"runtime/tmp/bash/jobs/bj_01M7EPZA41EEWV0G90BGEZFBN8/stderr.out"}`
	if outDone != wantOutDone {
		t.Fatalf("命令输出（终态）字节 = %s\nwant %s", outDone, wantOutDone)
	}

	// 停止端点三态：running → stopping+true；已终态 → 当前态+false。
	stopRunning := mustJSON(t, jobStopView{JobID: done.JobID, Status: jobStatusStopping, StopRequested: true})
	const wantStopRunning = `{"jobId":"bj_01M7EPZA41EEWV0G90BGEZFBN8","status":"stopping","stopRequested":true}`
	if stopRunning != wantStopRunning {
		t.Fatalf("停止（运行中）字节 = %s\nwant %s", stopRunning, wantStopRunning)
	}
	stopDone := mustJSON(t, jobStopView{JobID: done.JobID, Status: jobStatusSucceeded, StopRequested: false})
	const wantStopDone = `{"jobId":"bj_01M7EPZA41EEWV0G90BGEZFBN8","status":"succeeded","stopRequested":false}`
	if stopDone != wantStopDone {
		t.Fatalf("停止（终态）字节 = %s\nwant %s", stopDone, wantStopDone)
	}

	// 失败信封（实打）：未知/不可见 404 117117、不可停止 409 117118、项目不见 404 114001、缺 token 401 111102。
	envCases := []struct {
		code    int
		message string
		want    string
	}{
		{codeJobMissing, "找不到这条命令", `{"code":117117,"message":"找不到这条命令","data":null}`},
		{codeJobNotStoppable, "这条命令正在停止中，不能重复请求", `{"code":117118,"message":"这条命令正在停止中，不能重复请求","data":null}`},
		{codeWorkspaceMissing, "找不到这个项目", `{"code":114001,"message":"找不到这个项目","data":null}`},
		{codeMissingToken, "请先登录", `{"code":111102,"message":"请先登录","data":null}`},
	}
	for _, c := range envCases {
		got := mustJSON(t, envelope{Code: c.code, Message: c.message, Data: nil})
		if got != c.want {
			t.Fatalf("命令失败信封字节 = %s\nwant %s", got, c.want)
		}
	}
}

// TestContractTaskResumeStopBytes 钉住 resume 与 stop 的 data 字节（doc/70 §4.3/§4.4）。
// resume 是结构体（taskId → runId → status → resumeCount）；stop 是 gin.H——
// encoding/json 对 map 按键名字典序输出（status < taskId），前端拆包时键序照这个来。
func TestContractTaskResumeStopBytes(t *testing.T) {
	got := mustJSON(t, resumeTaskResponse{TaskID: "at_1", RunID: "r_9", Status: "running", ResumeCount: 1})
	const wantResume = `{"taskId":"at_1","runId":"r_9","status":"running","resumeCount":1}`
	if got != wantResume {
		t.Fatalf("resume 字节 = %s\nwant %s", got, wantResume)
	}
	got = mustJSON(t, gin.H{"taskId": "at_1", "status": "cancelled"})
	const wantStop = `{"status":"cancelled","taskId":"at_1"}`
	if got != wantStop {
		t.Fatalf("stop 字节 = %s\nwant %s", got, wantStop)
	}
}

// TestContractPlanRevisionBytes 钉住计划修订三个端点的 data 字节（doc/72 §4.2–§4.4）。
// 真实字节来自隔离引擎 curl（revision 201 / continue 200 / 修订历史 200）：
// 成功体字段序 revisionId → orchestrationId → revision → finalTaskId → planHold → addedTaskIds；
// 继续体 orchestrationId → revision → planHold；审计行的三个数组恒为数组（空时输出 []，不是 null）。
func TestContractPlanRevisionBytes(t *testing.T) {
	got := mustJSON(t, revisionResp{
		RevisionID: "pr_1", OrchestrationID: "or_1", Revision: 1,
		FinalTaskID: "at_1", PlanHold: false, AddedTaskIDs: []string{"at_9"},
	})
	const wantResp = `{"revisionId":"pr_1","orchestrationId":"or_1","revision":1,"finalTaskId":"at_1","planHold":false,"addedTaskIds":["at_9"]}`
	if got != wantResp {
		t.Fatalf("修订成功体字节 = %s\nwant %s", got, wantResp)
	}
	got = mustJSON(t, revisionResp{
		RevisionID: "pr_1", OrchestrationID: "or_1", Revision: 1,
		FinalTaskID: "at_1", PlanHold: false, AddedTaskIDs: []string{},
	})
	const wantEmpty = `{"revisionId":"pr_1","orchestrationId":"or_1","revision":1,"finalTaskId":"at_1","planHold":false,"addedTaskIds":[]}`
	if got != wantEmpty {
		t.Fatalf("修订空 addedTaskIds 字节 = %s\nwant %s", got, wantEmpty)
	}

	got = mustJSON(t, revisionView{
		RevisionID: "pr_1", Revision: 1, BaseRevision: 0,
		Reason: "A 的结果表明必须补充 D",
		AddTasks: []revisionTaskView{{
			Key: "D", Content: "补充调研 D，写入 d.md", Agent: "researcher",
			ModelRef: "", PlanCheckpoint: false,
		}},
		AddLinks: []planLink{{Parent: "D", Child: "B"}}, CancelTasks: []string{},
		FinalTaskKey: "", SourceRunID: "", CreatedAt: time.Unix(0, 0).UTC(),
	})
	const wantView = `{"revisionId":"pr_1","revision":1,"baseRevision":0,"reason":"A 的结果表明必须补充 D",` +
		`"addTasks":[{"key":"D","content":"补充调研 D，写入 d.md","agent":"researcher","modelRef":"","planCheckpoint":false}],` +
		`"addLinks":[{"parent":"D","child":"B"}],"cancelTasks":[],"finalTaskKey":"","sourceRunId":"",` +
		`"createdAt":"1970-01-01T00:00:00Z"}`
	if got != wantView {
		t.Fatalf("修订历史行字节 = %s\nwant %s", got, wantView)
	}

	got = mustJSON(t, continueResp{OrchestrationID: "or_1", Revision: 1, PlanHold: false})
	const wantContinue = `{"orchestrationId":"or_1","revision":1,"planHold":false}`
	if got != wantContinue {
		t.Fatalf("继续字节 = %s\nwant %s", got, wantContinue)
	}
}

// TestContractAttentionItemBytes 钉住「需要我处理」每行的字段序（doc/72 §4.6；doc/73 §4.4）：
// 十个字段恒出现（runId 后是 notificationId）；不适用处是 "" / 0，前端不按缺字段分支。
func TestContractAttentionItemBytes(t *testing.T) {
	got := mustJSON(t, attentionItem{
		Kind: "question", TaskID: "at_1", OrchestrationID: "or_1", Title: "A",
		Count: 3, QuestionID: "qs_1", RunID: "r_1", Status: "running",
		UpdatedAt: time.Unix(0, 0).UTC(),
	})
	const want = `{"kind":"question","taskId":"at_1","orchestrationId":"or_1","title":"A","count":3,` +
		`"questionId":"qs_1","runId":"r_1","notificationId":"","status":"running","updatedAt":"1970-01-01T00:00:00Z"}`
	if got != want {
		t.Fatalf("处理项字节 = %s\nwant %s", got, want)
	}
	got = mustJSON(t, attentionItem{
		Kind: "delivery_failed", OrchestrationID: "or_1", Title: "调研",
		Count: 1, RunID: "r_2", NotificationID: "an_1", Status: "failed",
		UpdatedAt: time.Unix(0, 0).UTC(),
	})
	const wantNotif = `{"kind":"delivery_failed","taskId":"","orchestrationId":"or_1","title":"调研","count":1,` +
		`"questionId":"","runId":"r_2","notificationId":"an_1","status":"failed","updatedAt":"1970-01-01T00:00:00Z"}`
	if got != wantNotif {
		t.Fatalf("交付失败处理项字节 = %s\nwant %s", got, wantNotif)
	}
}

// TestContractNotificationDetailBytes 钉住通知详情的字段序（doc/73 §4.1；施工包 §4.1；doc/82 §6.5）。
// 真实字节来自隔离引擎 curl（2026-11-11，-task-interval=-1s 起法）：
// notificationId → orchestrationId → taskId → kind → status → runId → attempt → errorCode
// → result → createdAt → claimedAt → failedAt → consumedAt → updatedAt → attempts → backgroundJobId；
// 无认领时 runId=""、attempt=0、attempts=[]、三个时间 null；非整组交付通知的 result 为 null。
// backgroundJobId 是 doc/82 §6.5 追加的末位字段：编排类通知恒空串，bash_job_finished 才带命令号。
func TestContractNotificationDetailBytes(t *testing.T) {
	first := time.Unix(9, 0).UTC()
	got := mustJSON(t, agentNotificationView{
		NotificationID: "an_1", OrchestrationID: "or_1", TaskID: "at_1",
		Kind: notifKindOrchFinished, Status: notifStatusFailed, RunID: "r_1", Attempt: 1,
		ErrorCode: deliveryErrRunError,
		Result: &notificationResultView{
			FinalTaskID: "at_1", ResultRevision: 1, Summary: "交付摘要正文", Artifacts: []TaskArtifact{},
		},
		CreatedAt: time.Unix(0, 0).UTC(), ClaimedAt: ptrTime(time.Unix(1, 0).UTC()),
		FailedAt: ptrTime(time.Unix(2, 0).UTC()), UpdatedAt: time.Unix(2, 0).UTC(),
		Attempts: []notificationAttemptView{{RunID: "r_1", Attempt: 1, Status: "error", CreatedAt: first}},
	})
	const want = `{"notificationId":"an_1","orchestrationId":"or_1","taskId":"at_1","kind":"orchestration_finished","status":"failed","runId":"r_1","attempt":1,"errorCode":"delivery_run_error",` +
		`"result":{"finalTaskId":"at_1","resultRevision":1,"summary":"交付摘要正文","artifacts":[]},` +
		`"createdAt":"1970-01-01T00:00:00Z","claimedAt":"1970-01-01T00:00:01Z","failedAt":"1970-01-01T00:00:02Z","consumedAt":null,"updatedAt":"1970-01-01T00:00:02Z",` +
		`"attempts":[{"runId":"r_1","attempt":1,"status":"error","createdAt":"1970-01-01T00:00:09Z"}],"backgroundJobId":""}`
	if got != want {
		t.Fatalf("通知详情字节 = %s\nwant %s", got, want)
	}
	got = mustJSON(t, agentNotificationView{
		NotificationID: "an_2", OrchestrationID: "or_1", TaskID: "at_1",
		Kind: "task_failed", Status: notifStatusPending,
		CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
		Attempts: []notificationAttemptView{},
	})
	const wantPending = `{"notificationId":"an_2","orchestrationId":"or_1","taskId":"at_1","kind":"task_failed","status":"pending","runId":"","attempt":0,"errorCode":"",` +
		`"result":null,"createdAt":"1970-01-01T00:00:00Z","claimedAt":null,"failedAt":null,"consumedAt":null,"updatedAt":"1970-01-01T00:00:00Z","attempts":[],"backgroundJobId":""}`
	if got != wantPending {
		t.Fatalf("无认领通知详情字节 = %s\nwant %s", got, wantPending)
	}
	// bash 来源（doc/82 §6.5）：orchestrationId 与 taskId 为空串，末位 backgroundJobId 带命令号，
	// 前端据此打开命令详情；result 恒 null（结果走 job 的输出端点，不走 Task 结果视图）。
	got = mustJSON(t, agentNotificationView{
		NotificationID: "an_3", Kind: "bash_job_finished", Status: notifStatusPending,
		BackgroundJobID: "bj_1",
		CreatedAt:       time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
		Attempts:        []notificationAttemptView{},
	})
	const wantBash = `{"notificationId":"an_3","orchestrationId":"","taskId":"","kind":"bash_job_finished","status":"pending","runId":"","attempt":0,"errorCode":"",` +
		`"result":null,"createdAt":"1970-01-01T00:00:00Z","claimedAt":null,"failedAt":null,"consumedAt":null,"updatedAt":"1970-01-01T00:00:00Z","attempts":[],"backgroundJobId":"bj_1"}`
	if got != wantBash {
		t.Fatalf("bash 通知详情字节 = %s\nwant %s", got, wantBash)
	}
}

// TestContractNotificationRetryBytes 钉住人工重试的 data 与失败信封（doc/73 §4.2；施工包 §4.2）。
// 202 新尝试 / 200 已有处理中的 run：notificationId → runId → status → attempt；
// 404 117003、409 117109／117110／112101、403 111203 的文案逐字。
func TestContractNotificationRetryBytes(t *testing.T) {
	got := mustJSON(t, notificationRetryView{
		NotificationID: "an_1", RunID: "r_2", Status: notifStatusClaimed, Attempt: 2,
	})
	const wantNew = `{"notificationId":"an_1","runId":"r_2","status":"claimed","attempt":2}`
	if got != wantNew {
		t.Fatalf("重试新尝试字节 = %s\nwant %s", got, wantNew)
	}
	got = mustJSON(t, notificationRetryView{
		NotificationID: "an_1", RunID: "r_1", Status: notifStatusClaimed, Attempt: 1,
	})
	const wantExisting = `{"notificationId":"an_1","runId":"r_1","status":"claimed","attempt":1}`
	if got != wantExisting {
		t.Fatalf("重试已有尝试字节 = %s\nwant %s", got, wantExisting)
	}

	cases := []struct {
		code    int
		message string
		want    string
	}{
		{codeNotifMissing, "找不到这条通知", `{"code":117003,"message":"找不到这条通知","data":null}`},
		{codeNotifNotRetryable, "这条通知现在不能重新处理", `{"code":117109,"message":"这条通知现在不能重新处理","data":null}`},
		{codeArtifactInvalid, "结果产物已不可读取", `{"code":117110,"message":"结果产物已不可读取","data":null}`},
		{codeSessionRunning, "这场会话正在生成，请稍后再试", `{"code":112101,"message":"这场会话正在生成，请稍后再试","data":null}`},
		{codeForbidden, "没有权限做这件事", `{"code":111203,"message":"没有权限做这件事","data":null}`},
	}
	for _, c := range cases {
		got = mustJSON(t, envelope{Code: c.code, Message: c.message, Data: nil})
		if got != c.want {
			t.Fatalf("重试失败信封字节 = %s\nwant %s", got, c.want)
		}
	}
}

// TestContractPublicationRetryBytes 钉住 retry-publication 的 data 与两个端点的失败信封
// （doc/74 §4.4/§4.5；施工包 §4.4/§4.5）。真实字节来自 2026-11-12 隔离引擎 curl
// （tmp/74 探针 probe6.out B3/B4/A2–A9/B5/B6/C3）：
// data 字段序 taskId → runId → status → publicationEpoch；202 新处理 run 与 200 幂等命中同形。
func TestContractPublicationRetryBytes(t *testing.T) {
	got := mustJSON(t, publicationRetryView{
		TaskID: "at_01M777EHF1QR09R69TKCWKY7Y5", RunID: "r_01M777EQG8HF8W38GDJPZZGRG5",
		Status: "running", PublicationEpoch: 2,
	})
	const want = `{"taskId":"at_01M777EHF1QR09R69TKCWKY7Y5","runId":"r_01M777EQG8HF8W38GDJPZZGRG5","status":"running","publicationEpoch":2}`
	if got != want {
		t.Fatalf("重新发布字节 = %s\nwant %s", got, want)
	}

	cases := []struct {
		code    int
		message string
		want    string
	}{
		{codeArtifactInvalid, "结果产物已不可读取", `{"code":117110,"message":"结果产物已不可读取","data":null}`},
		{codeTaskMissing, "找不到这件任务", `{"code":117001,"message":"找不到这件任务","data":null}`},
		{codeMissingToken, "请先登录", `{"code":111102,"message":"请先登录","data":null}`},
		{codeTaskNotRepublishable, "这件任务现在不能重新发布", `{"code":117112,"message":"这件任务现在不能重新发布","data":null}`},
		{codeForbidden, "没有权限做这件事", `{"code":111203,"message":"没有权限做这件事","data":null}`},
	}
	for _, c := range cases {
		got = mustJSON(t, envelope{Code: c.code, Message: c.message, Data: nil})
		if got != c.want {
			t.Fatalf("产物/重新发布失败信封字节 = %s\nwant %s", got, c.want)
		}
	}
}

// TestContractWorkspaceConfigErrBytes 钉住工作区配置/发布准入的失败信封（doc/74 §4.1/§4.3）。
// 真实字节来自隔离引擎 curl（8948，2026-11-12）：117111 的 data 是 gin.H{"reason":…}（map 按键序），
// 117113 在 POST /api/runs 准入处挡下（workspaces.publication_orchestration_id 非空），data 恒 null。
func TestContractWorkspaceConfigErrBytes(t *testing.T) {
	reasons := []string{
		"workspaceMode 只允许 shared 或 isolated",
		"共享工作区不接受 verifyCommand",
		"verifyCommand 超过 4000 字节",
	}
	for _, r := range reasons {
		got := mustJSON(t, envelope{Code: codeWorkspaceConfigInvalid, Message: "工作区配置不合法", Data: gin.H{"reason": r}})
		want := `{"code":117111,"message":"工作区配置不合法","data":{"reason":"` + r + `"}}`
		if got != want {
			t.Fatalf("工作区配置失败信封字节 = %s\nwant %s", got, want)
		}
	}
	got := mustJSON(t, envelope{Code: codeWorkspacePublishing, Message: "项目正在发布结果，请稍后再试", Data: nil})
	const wantPublishing = `{"code":117113,"message":"项目正在发布结果，请稍后再试","data":null}`
	if got != wantPublishing {
		t.Fatalf("发布准入失败信封字节 = %s\nwant %s", got, wantPublishing)
	}
}

// TestContractSuccessorBytes 钉住继任组创建／交接／组详情尾字段与项目删除成功体
// （doc/75 §4.1/§4.3/§4.4；doc/76 §4/§6.4）。真实字节来自 2026-11-12 隔离引擎 curl
// （expected.json 的 61-goal-replan / 62-project-layout / 63-project-delete-cleanup）：
// 创建 202 四键序；handoff 200 五键序；组详情只在继任组末尾追加 replanOf/replanDepth；
// 删除成功体在 status 后加 cleanup；旧布局入口统一 409 117116。
func TestContractSuccessorBytes(t *testing.T) {
	got := mustJSON(t, successorResp{
		OrchestrationID: "or_s", Status: "active", ReplanOf: "or_o", ReplanDepth: 1,
	})
	const wantCreate = `{"orchestrationId":"or_s","status":"active","replanOf":"or_o","replanDepth":1}`
	if got != wantCreate {
		t.Fatalf("继任创建字节 = %s\nwant %s", got, wantCreate)
	}

	got = mustJSON(t, handoffView{
		OrchestrationID: "or_s", ReplanOf: "or_o", ReplanDepth: 1,
		Reuse: []handoffReuseEntry{{TaskID: "at_a", ResultRevision: 1}},
		Body:  "\n# 为什么重规划\n\n局部换路\n",
	})
	const wantHandoff = `{"orchestrationId":"or_s","replanOf":"or_o","replanDepth":1,"reuse":[{"taskId":"at_a","resultRevision":1}],"body":"\n# 为什么重规划\n\n局部换路\n"}`
	if got != wantHandoff {
		t.Fatalf("交接读取字节 = %s\nwant %s", got, wantHandoff)
	}

	t0 := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	base := orchestrationBody{
		OrchestrationID: "or_o", Goal: "g", Status: "active", CreatedAt: t0, UpdatedAt: t0,
	}
	const wantBase = `{"orchestrationId":"or_o","goal":"g","workspaceId":"","sourceSessionId":"","status":"active","finalTaskId":"","planRevision":0,"planHold":false,"deliveryStatus":"","deliveryRunId":"","deliveryErrorCode":"","createdAt":"2026-11-12T00:00:00Z","finishedAt":null,"deliveredAt":null,"updatedAt":"2026-11-12T00:00:00Z"}`
	got = mustJSON(t, base)
	if got != wantBase {
		t.Fatalf("非继任组详情字节 = %s\nwant %s", got, wantBase)
	}
	succ := base
	succ.ReplanOf, succ.ReplanDepth = "or_g", 1
	const wantSuccTail = `,"updatedAt":"2026-11-12T00:00:00Z","replanOf":"or_g","replanDepth":1}`
	got = mustJSON(t, succ)
	if !strings.HasSuffix(got, wantSuccTail) {
		t.Fatalf("继任组详情尾字节 = %s\nwant 尾 %s", got, wantSuccTail)
	}

	// 删除成功体是 handler 内的匿名结构（字段序 status → cleanup）。
	type deleteResult struct {
		Status  string `json:"status"`
		Cleanup string `json:"cleanup"`
	}
	got = mustJSON(t, deleteResult{Status: "deleted", Cleanup: "done"})
	const wantDone = `{"status":"deleted","cleanup":"done"}`
	if got != wantDone {
		t.Fatalf("删除成功体字节 = %s\nwant %s", got, wantDone)
	}
	got = mustJSON(t, deleteResult{Status: "deleted", Cleanup: "pending"})
	const wantPending = `{"status":"deleted","cleanup":"pending"}`
	if got != wantPending {
		t.Fatalf("删除待补偿体字节 = %s\nwant %s", got, wantPending)
	}

	cases := []struct {
		code    int
		message string
		want    string
	}{
		{codeLayoutUnmigrated, "这个项目还需要迁移目录后才能使用", `{"code":117116,"message":"这个项目还需要迁移目录后才能使用","data":null}`},
		{codeHandoffMissing, "找不到这次重规划交接", `{"code":117004,"message":"找不到这次重规划交接","data":null}`},
		{codeSuccessorNotAllowed, "这个编排组现在不能创建继任组", `{"code":117114,"message":"这个编排组现在不能创建继任组","data":null}`},
	}
	for _, c := range cases {
		got = mustJSON(t, envelope{Code: c.code, Message: c.message, Data: nil})
		if got != c.want {
			t.Fatalf("继任/布局失败信封字节 = %s\nwant %s", got, c.want)
		}
	}
	// 117115 的 data 是 gin.H{"reason":…}（map 按键序）；真实原因来自 store 的纯校验。
	reason := `目标可行性判定必须是 feasible 才能创建继任组（当前 "不确定"）`
	got = mustJSON(t, envelope{Code: codeSuccessorInvalid, Message: "继任方案不合法", Data: gin.H{"reason": reason}})
	want := `{"code":117115,"message":"继任方案不合法","data":{"reason":"` + `目标可行性判定必须是 feasible 才能创建继任组（当前 \"不确定\"）` + `"}}`
	if got != want {
		t.Fatalf("117115 失败信封字节 = %s\nwant %s", got, want)
	}
}
