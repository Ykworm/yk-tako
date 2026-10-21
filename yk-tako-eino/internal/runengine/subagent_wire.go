// 本文件是子代理实例档案（doc/39）：每次委派一份 JSONL，
// 落在 <会话目录>/.tako/subagents/<sa_id>.jsonl。
// 挂载了项目的会话也写会话目录，不写项目目录——删会话时 .tako/ 一起清掉。
//
// 写句柄和已追加条数放在 ctx 里的 *subagentWire 上。middleware 实例会被同一场 run
// 里并行的同名委派共用，实例字段不能记这一次调用写到哪了（doc/25 第 4 节，先例 callerStampMiddleware）。
package runengine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/oklog/ulid/v2"
)

const subagentWireProtocol = 1

// subagentWireKey 是 ctx 携带本次委派写句柄的私有 key。
type subagentWireKey struct{}

// subagentWire 是一次委派的追加写句柄。written 是已经写进文件的 state.Messages 条数，
// 按下标差分，不按内容比对。
type subagentWire struct {
	mu      sync.Mutex
	f       *os.File
	id      string
	written int
	done    bool
}

func withSubagentWire(ctx context.Context, w *subagentWire) context.Context {
	return context.WithValue(ctx, subagentWireKey{}, w)
}

func subagentWireFrom(ctx context.Context) *subagentWire {
	w, _ := ctx.Value(subagentWireKey{}).(*subagentWire)
	return w
}

func newSubagentID() (string, error) {
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return "", err
	}
	return "sa_" + id.String(), nil
}

// subagentWireDir 从作业根引用推出档案目录。Root 可能是项目目录，档案仍在会话目录下。
func subagentWireDir(ref WorkspaceRef) (string, error) {
	if ref.TenantRoot == "" {
		return "", os.ErrInvalid
	}
	sess, err := sessionDir(filepath.Dir(filepath.Dir(ref.TenantRoot)), ref.TenantID, ref.SessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(sess, ".tako", "subagents"), nil
}

type wireMetadata struct {
	Type        string `json:"type"`
	SaID        string `json:"sa_id"`
	AgentName   string `json:"agent_name"`
	ModelRef    string `json:"model_ref"`
	RunID       string `json:"run_id"`
	ResumedFrom string `json:"resumed_from"`
	Protocol    int    `json:"protocol"`
	CreatedAt   string `json:"created_at"`
}

type wireMessageLine struct {
	Type string          `json:"type"`
	Msg  *schema.Message `json:"msg"`
}

type wireTerminal struct {
	Type       string `json:"type"`
	Status     string `json:"status"`
	Result     string `json:"result"`
	FinishedAt string `json:"finished_at"`
}

func writeJSONLine(f *os.File, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// openSubagentWire 创建档案并写下 metadata 行。调用方负责 close。
func openSubagentWire(dir, saID, agentName, modelRef, runID, resumedFrom string) (*subagentWire, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, saID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	meta := wireMetadata{
		Type: "metadata", SaID: saID, AgentName: agentName, ModelRef: modelRef,
		RunID: runID, ResumedFrom: resumedFrom, Protocol: subagentWireProtocol,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeJSONLine(f, meta); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &subagentWire{f: f, id: saID}, nil
}

// beginDelegationWire 在内层开跑前铸实例。没有作业根、目录算不出、落盘失败都只记日志，
// 委派照常跑——档案是增强，不是这次调用能不能完成的条件。
// resumedFrom 写进新档案的 metadata：普通委派是空字符串，resume 填被续上的那份 sa_id。
func beginDelegationWire(ctx context.Context, agentName, resumedFrom string) (context.Context, *subagentWire) {
	ref, ok := workspaceFrom(ctx)
	if !ok {
		return ctx, nil
	}
	dir, err := subagentWireDir(ref)
	if err != nil {
		log.Printf("subagent wire %s: 会话目录: %v", agentName, err)
		return ctx, nil
	}
	saID, err := newSubagentID()
	if err != nil {
		log.Printf("subagent wire %s: 铸 id: %v", agentName, err)
		return ctx, nil
	}
	modelRef := ""
	if table := agentModelTableFrom(ctx); table != nil {
		modelRef = table.entryFor(agentName).ref
	}
	runID := ""
	if target, ok := journalTargetFrom(ctx); ok {
		runID = string(target.runID)
	}
	w, err := openSubagentWire(dir, saID, agentName, modelRef, runID, resumedFrom)
	if err != nil {
		log.Printf("subagent wire %s: 开档失败: %v", saID, err)
		return ctx, nil
	}
	log.Printf("subagent wire %s: %s", saID, dir)
	return withSubagentWire(ctx, w), w
}

func (w *subagentWire) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
}

// finish 追加 terminal 行。status 是 complete 或 error。中断路径不调用它，文件保持半成品。
func (w *subagentWire) finish(status, result string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil || w.done {
		return
	}
	line := wireTerminal{
		Type: "terminal", Status: status, Result: result,
		FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeJSONLine(w.f, line); err != nil {
		log.Printf("subagent wire %s: terminal 写入失败: %v", w.id, err)
		return
	}
	w.done = true
}

func (w *subagentWire) appendRange(msgs []*schema.Message, from, to int) {
	for i := from; i < to; i++ {
		if msgs[i] == nil {
			w.written++
			continue
		}
		if err := writeJSONLine(w.f, wireMessageLine{Type: "message", Msg: msgs[i]}); err != nil {
			log.Printf("subagent wire %s: 追加消息失败: %v", w.id, err)
			return
		}
		w.written++
	}
}

// appendBefore 写这一轮模型调用前新出现的消息（上一轮的工具结果在这里才进 state）。
// 条数变少说明前面的钩子已经换过历史，把游标收到当前长度，避免按旧下标越界。
func (w *subagentWire) appendBefore(msgs []*schema.Message) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return
	}
	if len(msgs) < w.written {
		w.written = len(msgs)
		return
	}
	w.appendRange(msgs, w.written, len(msgs))
}

// appendAfter 写这一轮模型回复。折叠若把 state 换短，文件里已经有折前原文，
// 这里只补末条（这一轮并入的回复），并把游标收到新长度。
func (w *subagentWire) appendAfter(msgs []*schema.Message) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil || len(msgs) == 0 {
		return
	}
	if len(msgs) > w.written {
		w.appendRange(msgs, w.written, len(msgs))
		return
	}
	if len(msgs) < w.written {
		last := len(msgs) - 1
		if msgs[last] != nil {
			if err := writeJSONLine(w.f, wireMessageLine{Type: "message", Msg: msgs[last]}); err != nil {
				log.Printf("subagent wire %s: 追加消息失败: %v", w.id, err)
				return
			}
		}
		w.written = len(msgs)
	}
}

func withSubagentInstance(text, saID string) string {
	if saID == "" {
		return text
	}
	return text + "\n（本次委派实例：" + saID + "）"
}

func subagentWireID(w *subagentWire) string {
	if w == nil {
		return ""
	}
	return w.id
}

// wireAppendHandler 挂在子代理 middleware 链上，紧挨折叠钩子之前。
// 字段只有基类：一次调用的写句柄从 ctx 取。
type wireAppendHandler struct {
	*adk.BaseChatModelAgentMiddleware
}

func newWireAppendHandler() *wireAppendHandler {
	return &wireAppendHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
}

var _ adk.ChatModelAgentMiddleware = (*wireAppendHandler)(nil)

func (h *wireAppendHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if state != nil {
		if w := subagentWireFrom(ctx); w != nil {
			w.appendBefore(state.Messages)
		}
	}
	return ctx, state, nil
}

func (h *wireAppendHandler) AfterModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if state != nil {
		if w := subagentWireFrom(ctx); w != nil {
			w.appendAfter(state.Messages)
		}
	}
	return ctx, state, nil
}

// subagentListItem 是 GET /api/sessions/:id/subagents 的一行。
// 有 terminal 行标 complete（wire 里的 error 终态在这里也是 complete）；没有标 interrupted。
type subagentListItem struct {
	SaID       string `json:"saId"`
	AgentName  string `json:"agentName"`
	Status     string `json:"status"`
	CreatedAt  string `json:"createdAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// listSubagentWires 扫档案目录，按 metadata 与末行组装。目录不存在返回空切片。
func listSubagentWires(dir string) ([]subagentListItem, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	items := make([]subagentListItem, 0, len(matches))
	for _, path := range matches {
		item, ok := readSubagentWireSummary(path)
		if ok {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt == items[j].CreatedAt {
			return items[i].SaID < items[j].SaID
		}
		return items[i].CreatedAt < items[j].CreatedAt
	})
	return items, nil
}

func readSubagentWireSummary(path string) (subagentListItem, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("subagent wire 读取 %s: %v", path, err)
		return subagentListItem{}, false
	}
	var lines [][]byte
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, []byte(line))
		}
	}
	if len(lines) == 0 {
		return subagentListItem{}, false
	}
	var meta wireMetadata
	if err := json.Unmarshal(lines[0], &meta); err != nil || meta.Type != "metadata" || meta.SaID == "" {
		log.Printf("subagent wire %s: 首行不是 metadata", path)
		return subagentListItem{}, false
	}
	item := subagentListItem{
		SaID: meta.SaID, AgentName: meta.AgentName, Status: "interrupted", CreatedAt: meta.CreatedAt,
	}
	var last wireTerminal
	if err := json.Unmarshal(lines[len(lines)-1], &last); err == nil && last.Type == "terminal" {
		item.Status = "complete"
		item.FinishedAt = last.FinishedAt
	}
	return item, true
}
