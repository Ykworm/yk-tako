// 本文件：会话——每个会话一套专属 agent/runner（看板正本与 checkpoint 不跨会话串账）
// 与单轮并发闸。模型下一轮读的是本进程里已装入的档案消息；还没装过，或补发改过档案，才再读库。
package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/compact"
	"yk-tako-eino/internal/guard"
)

// approvalSeq 给审批请求发全局递增 id：前端回执只用它定位，不解析内容。
var approvalSeq atomic.Uint64

// liveRun 是会话上正在跑的那一场。非 nil 即 409 的依据。
// cancel 取消的是进程 ctx 的子上下文，和任何一条 HTTP 连接都无关。
// stopped 只由 POST /api/stop 置位，收尾时据此写成 stop 而不是 error。
type liveRun struct {
	id            string
	cancel        context.CancelFunc
	stopped       bool
	userText      string // 这一轮用户原话。给人看的 start 事件用它，不换成摘要
	modelUserText string // 摘要接在原话前面。空表示这一轮没有压缩，收尾仍用 userText
	dropped       []string
}

// archivedTurn 是一轮已经结束、还留在内存里的档案。Msgs 与 modelMsgs 里的对象是同一批。
type archivedTurn struct {
	RunID string
	Msgs  []*schema.Message
}

// session 是一个聊天会话的全部状态。除 agent/runner/db/proc 不可变外，其余字段由 mu 保护：
// HTTP handler 与 run goroutine 会并发碰它们（-race 是合入门槛，《10》§6）。
type session struct {
	id     string
	agent  adk.Agent
	runner *adk.Runner
	db     *LogDB          // 事件库；写它用 proc，不用请求 ctx
	events *eventMem       // 与 Server 共用的事件列表；读者按序号读它
	proc   context.Context // 进程生命周期，库操作不被单条连接取消

	mu   sync.Mutex
	turn int      // 轮次计数：checkpointID 用它拼，且每会话独立
	live *liveRun // 非 nil 表示有 run 在跑（POST /api/chat 的 409 判定）

	// modelReady 为真时 modelMsgs 就是下一轮要接上的档案消息，不再查 run_archive。
	// 进程刚起来、或补发改过档案后为假，下一次开聊从库装入。失败和停止不往里加。
	modelReady bool
	modelMsgs  []*schema.Message
	modelTurns []archivedTurn // 与 modelMsgs 同一批消息，按轮记下 run_id，压缩时按它取舍

	// staged 为真时 snapMsgs / snapTurns 是这一轮第一次压缩之前的内存。失败时退回这一份。
	staged    bool
	snapMsgs  []*schema.Message
	snapTurns []archivedTurn

	pending map[string]chan string // 审批请求 id → 决策通道（ask 挂在这上面）
}

// 编译期校验 session 实现了 compact.Memory。压缩 Handler 通过这个接口改内存，不直接碰库。
var _ compact.Memory = (*session)(nil)

// newSession 建会话壳。agent 要等 WithMemory 把这份 session 放进 ctx 之后再造，
// 所以 runner 放到 bindAgent。checkpoint store 每会话一份，进程内存，重启即丢（《10》§4.4）。
func newSession(ctx context.Context, id string, db *LogDB) *session {
	return &session{
		id:      id,
		db:      db,
		proc:    ctx,
		pending: map[string]chan string{},
	}
}

// bindAgent 接上这一会话专属的 agent，并建它的 runner。
func (s *session) bindAgent(ctx context.Context, agent adk.Agent) {
	s.agent = agent
	s.runner = adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, EnableStreaming: true, CheckPointStore: adkrun.NewStore()})
}

// beginTurn 开一轮：占并发闸、记下 run id，并从档案装入已完成轮次的消息。
// 已有 run 在跑返回 ok=false（调用方回 409）。装档案失败返回 err，并放开闸。
// 返回的 runCtx 给本轮 Run，不要换成请求 ctx。
func (s *session) beginTurn(runID, message string) (turn int, messages []*schema.Message, runCtx context.Context, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live != nil {
		return 0, nil, nil, false, nil
	}
	runCtx, cancel := context.WithCancel(s.proc)
	s.live = &liveRun{id: runID, cancel: cancel, userText: message}
	prior, err := s.completedMessagesLocked()
	if err != nil {
		s.live = nil
		cancel()
		return 0, nil, nil, false, err
	}
	s.turn++
	turn = s.turn
	messages = append(prior, schema.UserMessage(message))
	return turn, messages, runCtx, true, nil
}

// completedMessagesLocked 给出本会话已完成且写了产出的消息。
// 本进程已经装过就直接返回副本。调用方须持有 s.mu。失败和停止没有产出，不在结果里。
func (s *session) completedMessagesLocked() ([]*schema.Message, error) {
	if s.modelReady {
		return cloneMsgs(s.modelMsgs), nil
	}
	turns, err := s.db.CompletedTurns(s.proc, s.id)
	if err != nil {
		return nil, err
	}
	var out []*schema.Message
	var loaded []archivedTurn
	for _, row := range turns {
		user := schema.UserMessage(row.UserMessage)
		var produced []*schema.Message
		if err := json.Unmarshal([]byte(row.ProducedJSON), &produced); err != nil {
			return nil, fmt.Errorf("解析轮次产出: %w", err)
		}
		msgs := append([]*schema.Message{user}, produced...)
		loaded = append(loaded, archivedTurn{RunID: row.RunID, Msgs: msgs})
		out = append(out, msgs...)
	}
	s.modelTurns = loaded
	s.modelMsgs = out
	s.modelReady = true
	return cloneMsgs(out), nil
}

// noteCompleted 在档案已经写下这轮产出之后，把这一轮接进内存。
// 压缩过时用 modelUserText，也就是摘要接在原话前面的那句；否则用用户原话。
// 调用时 live 还在。还没装过库（例如中途被补发作废）则不动，下次开聊再读库。
func (s *session) noteCompleted(produced []*schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil || !s.modelReady {
		return
	}
	text := s.live.userText
	if s.live.modelUserText != "" {
		text = s.live.modelUserText
	}
	user := schema.UserMessage(text)
	copied := compact.CloneMessages(produced)
	msgs := append([]*schema.Message{user}, copied...)
	s.modelMsgs = append(s.modelMsgs, msgs...)
	s.modelTurns = append(s.modelTurns, archivedTurn{RunID: s.live.id, Msgs: msgs})
	s.staged = false
	s.snapMsgs = nil
	s.snapTurns = nil
}

// dropModelCache 丢掉已装入的档案消息。补发改过 run_archive 时用，下次开聊重读。
// 压缩快照一并丢掉，避免退回一份已经和库不一致的旧列表。
func (s *session) dropModelCache() {
	s.mu.Lock()
	s.modelReady = false
	s.modelMsgs = nil
	s.modelTurns = nil
	s.staged = false
	s.snapMsgs = nil
	s.snapTurns = nil
	s.mu.Unlock()
}

// cloneMsgs 复制切片，避免调用方 append 时改到缓存的底层数组。
func cloneMsgs(in []*schema.Message) []*schema.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]*schema.Message, len(in))
	copy(out, in)
	return out
}

// abortTurn 放开并发闸并取消还没跑起来的上下文，不改历史。BeginRun 失败时用。
func (s *session) abortTurn() {
	s.mu.Lock()
	live := s.live
	s.live = nil
	s.mu.Unlock()
	if live != nil && live.cancel != nil {
		live.cancel()
	}
}

// stop 在 run 匹配时取消它。不写事件，终局由 run 自己的退出路径落盘。
// 重复调用是幂等的：cancel 可以调多次。run 已经清掉则返回 false（调用方回 404）。
func (s *session) stop(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil || s.live.id != runID {
		return false
	}
	s.live.stopped = true
	s.live.cancel()
	return true
}

// wasStopped 报告这场还在闸上的 run 有没有被停止请求取消过。必须在 finishTurn 之前读。
func (s *session) wasStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live != nil && s.live.stopped
}

// liveID 返回当前 run。没有则是空字符串。给 GET /api/session 用。
func (s *session) liveID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil {
		return ""
	}
	return s.live.id
}

// finishTurn 放开并发闸。这一轮进不进 modelMsgs，看调用前 noteCompleted 有没有加上；
// 只有档案成功收成 complete 才会加。失败和停止不加。
func (s *session) finishTurn() {
	s.mu.Lock()
	live := s.live
	s.live = nil
	s.mu.Unlock()
	if live != nil && live.cancel != nil {
		live.cancel()
	}
}

// waitApproval 登记一个审批请求，返回等决策的通道。
func (s *session) waitApproval(id string) chan string {
	ch := make(chan string, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	return ch
}

// resolveApproval 把决策送进对应通道；id 不存在或已处理返回 false（HTTP 404）。
func (s *session) resolveApproval(id, decision string) bool {
	s.mu.Lock()
	ch, ok := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if !ok {
		return false
	}
	ch <- decision
	return true
}

// dropApproval 取消挂起的审批（run 的 ctx 取消时调用），避免 pending 泄漏。
func (s *session) dropApproval(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// runTurn 跑一轮。每条要给人看的事件只 Append 进事件库，不往连接上推。
// 读者自己按 seq 轮询。写库用进程 ctx：一条连接断了不能把这帧写丢。
// 写失败会取消本次 runCtx，让 Run 停下来，返回值优先用这笔写错误。
func (s *session) runTurn(ctx context.Context, turn int, runID string, messages []*schema.Message) ([]*schema.Message, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var persistErr error
	emit := func(ev sseEvent) {
		if persistErr != nil {
			return
		}
		ev.Run = runID
		stored, err := s.db.Append(s.proc, ev)
		if err != nil {
			persistErr = err
			cancel()
			return
		}
		s.events.publish(stored)
	}
	// 逐字正文只进内存。写完才按原序号落库。失败的半截标失败，不落库。
	sink := func(ev adkrun.Event) {
		if persistErr != nil {
			return
		}
		switch {
		case ev.Partial:
			s.events.upsertPartial(runID, sseEventFromAdkrun(ev))
		case ev.Drop:
			s.events.failPartial(runID)
		case ev.Discard:
			s.events.discardPartial(runID)
		default:
			frame := sseEventFromAdkrun(ev)
			if sealed, ok := s.events.sealPartial(runID, frame); ok {
				if _, err := s.db.AppendSealed(s.proc, sealed); err != nil {
					persistErr = err
					cancel()
				}
				return
			}
			emit(frame)
		}
	}
	// ask 是交给 adkrun.Run 的审批回调（《10》§4.3）：不阻塞任何输入，
	// ① 落盘并推一条 approval，② 挂起等 POST /api/approval，③ 继续之前先落盘回执。
	// resume 值不变：仍返回 guard.ResumeApprove / guard.ResumeReject 字符串。
	ask := func(info guard.ApprovalInfo) (string, error) {
		id := fmt.Sprintf("%s-a%d", s.id, approvalSeq.Add(1))
		ch := s.waitApproval(id)
		emit(sseEvent{Kind: "approval", Approval: &sseApproval{ID: id, Tool: info.Tool, Args: info.Args, Message: info.Message}})
		if persistErr != nil {
			s.dropApproval(id)
			return "", persistErr
		}
		select {
		case decision := <-ch:
			emit(sseEvent{Kind: "approval_result", Approval: &sseApproval{ID: id, Decision: decision}})
			if persistErr != nil {
				return "", persistErr
			}
			return decision, nil
		case <-runCtx.Done():
			s.dropApproval(id)
			if persistErr != nil {
				return "", persistErr
			}
			return "", runCtx.Err()
		}
	}
	produced, err := adkrun.Run(runCtx, s.runner, s.agent, fmt.Sprintf("turn-%d", turn), messages, sink, ask)
	if persistErr != nil {
		return produced, persistErr
	}
	return produced, err
}

// SessionID 实现 compact.Memory。就是会话 id。
func (s *session) SessionID() string { return s.id }

// OriginalUser 实现 compact.Memory。返回这一轮的用户原话，不返回已经接过摘要的那句。
func (s *session) OriginalUser() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil {
		return ""
	}
	return s.live.userText
}

// CurrentRunID 实现 compact.Memory。
func (s *session) CurrentRunID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil {
		return ""
	}
	return s.live.id
}

// CompletedRunIDs 实现 compact.Memory。顺序与 modelTurns 一致，不含正在进行的这一轮。
func (s *session) CompletedRunIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.modelTurns))
	for i, turn := range s.modelTurns {
		out[i] = turn.RunID
	}
	return out
}

// Stage 实现 compact.Memory。只改内存。第一次调用留下快照；同一轮里再压，仍相对那份快照取舍，
// 避免第二次只看见已经缩短的列表，把第一次换掉的 run_id 弄丢。
func (s *session) Stage(c compact.Commit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil {
		return fmt.Errorf("compact: 没有正在进行的轮次")
	}
	if !s.staged {
		s.snapMsgs = cloneMsgs(s.modelMsgs)
		s.snapTurns = append([]archivedTurn(nil), s.modelTurns...)
		s.staged = true
	}
	kept, dropped := splitKept(s.snapTurns, c.KeptRunIDs)
	s.modelTurns = kept
	s.modelMsgs = flattenTurns(kept)
	s.live.modelUserText = c.CombinedUser
	s.live.dropped = dropped
	s.modelReady = true
	return nil
}

// revertStage 把 modelMsgs 退回这一轮第一次压缩前的快照。库不在这里改。
func (s *session) revertStage() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.staged {
		return
	}
	s.modelMsgs = s.snapMsgs
	s.modelTurns = s.snapTurns
	s.staged = false
	s.snapMsgs = nil
	s.snapTurns = nil
	if s.live != nil {
		s.live.modelUserText = ""
		s.live.dropped = nil
	}
}

// archivePatch 取出成功收尾时要和 produced_json 一起写入的修改。还没压缩过则 ok 为假。
func (s *session) archivePatch() (ArchivePatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.staged || s.live == nil {
		return ArchivePatch{}, false
	}
	return ArchivePatch{
		UserMessage: s.live.modelUserText,
		DropRunIDs:  append([]string(nil), s.live.dropped...),
	}, true
}

// Notify 实现 compact.Memory。追加一条 kind 为 compact 的事件，不改已经写下的帧。
// 不持有 s.mu 去写库。run 已经不在 running 时 Append 会失败，这里只记日志。
func (s *session) Notify(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	id := ""
	if s.live != nil {
		id = s.live.id
	}
	db := s.db
	events := s.events
	proc := s.proc
	s.mu.Unlock()
	if id == "" || db == nil {
		return
	}
	stored, err := db.Append(proc, sseEvent{Kind: "compact", Text: text, Run: id})
	if err != nil {
		log.Printf("compact: 通知失败: %v", err)
		return
	}
	if events != nil {
		events.publish(stored)
	}
}

// WriteRAG 实现 compact.Memory。用进程 ctx，避免用户断开连接时把这次写入取消掉。
func (s *session) WriteRAG(runID, body string) error {
	if s.db == nil {
		return fmt.Errorf("compact: 没有事件库")
	}
	return s.db.WriteRAG(s.proc, s.id, runID, body)
}

// SearchRAG 实现 compact.Memory。先取出这一会话的原文，再按词重叠截断。
func (s *session) SearchRAG(query string) (string, error) {
	if s.db == nil {
		return "", fmt.Errorf("compact: 没有事件库")
	}
	bodies, err := s.db.RAGBodies(s.proc, s.id)
	if err != nil {
		return "", err
	}
	return compact.SearchBodies(query, bodies), nil
}

// splitKept 按快照的原顺序留下 id 还在集合里的轮，其余 id 就是这次要清空的行。
func splitKept(turns []archivedTurn, ids []string) (kept []archivedTurn, dropped []string) {
	keep := map[string]struct{}{}
	for _, id := range ids {
		if id != "" {
			keep[id] = struct{}{}
		}
	}
	for _, turn := range turns {
		if _, ok := keep[turn.RunID]; ok {
			kept = append(kept, turn)
			continue
		}
		if turn.RunID != "" {
			dropped = append(dropped, turn.RunID)
		}
	}
	return kept, dropped
}

// flattenTurns 把留下的轮展开成 modelMsgs。
func flattenTurns(turns []archivedTurn) []*schema.Message {
	var out []*schema.Message
	for _, turn := range turns {
		out = append(out, turn.Msgs...)
	}
	return out
}
