// 本文件：进程内的事件列表。页面按序号读副本，读走不删。
// 已落库的帧才放进来。进程重启后列表是空的，第一次读取再从事件表装回。
package webui

import "sync"

// runEvents 是一场 run 在本进程里的事件列表。
// ready 为真表示可以按序号读：要么本进程写下了 seq=1，要么已经从库装过一次。
// gen 在列表有变化时加一。notify 被关闭表示有新变化，读者据此醒来。
type runEvents struct {
	ready  bool
	events []sseEvent
	gen    uint64
	notify chan struct{}
}

// eventMem 按 run 保留列表。完成时不删除，进程退出才一起丢掉。
type eventMem struct {
	mu   sync.Mutex
	runs map[string]*runEvents
}

func newEventMem() *eventMem {
	return &eventMem{runs: map[string]*runEvents{}}
}

// publish 追加一帧已落库的事件。seq 相同的帧只留一份。
// 第一帧 seq 为 1 时，这场列表立刻可读，不必再从库装。
func (m *eventMem) publish(ev sseEvent) {
	if m == nil || ev.Run == "" || ev.Seq <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buf(ev.Run, ev.Seq == 1)
	b.events = insertSeq(b.events, ev)
	b.bump()
}

// buf 取出或新建一场 run 的列表。调用方须持有 m.mu。bornReady 只在新建时生效。
func (m *eventMem) buf(runID string, bornReady bool) *runEvents {
	b := m.runs[runID]
	if b == nil {
		b = &runEvents{ready: bornReady, notify: make(chan struct{})}
		m.runs[runID] = b
	}
	if b.notify == nil {
		b.notify = make(chan struct{})
	}
	return b
}

// bump 通知正在等的读者。调用方须持有 m.mu。
func (b *runEvents) bump() {
	b.gen++
	close(b.notify)
	b.notify = make(chan struct{})
}

// snapshot 返回 seq 大于 after 的副本、当前代数，以及下一次变化的通知。
// ready 为 false 时调用方应先从库装入。
func (m *eventMem) snapshot(runID string, after int64, limit int) (batch []sseEvent, gen uint64, notify <-chan struct{}, ready bool) {
	if m == nil {
		return nil, 0, nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.runs[runID]
	if b == nil || !b.ready {
		return nil, 0, nil, false
	}
	return copyAfter(b.events, after, limit), b.gen, b.notify, true
}

// upsertPartial 把还在流出的正文放进内存。同一条未完成正文沿用原序号，只更新文本。
// 不写库。返回带序号的那一帧。
func (m *eventMem) upsertPartial(runID string, ev sseEvent) sseEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buf(runID, false)
	if n := len(b.events); n > 0 {
		last := b.events[n-1]
		if last.Partial && last.Kind == ev.Kind {
			last.Text = ev.Text
			last.Role = ev.Role
			last.Agent = ev.Agent
			b.events[n-1] = last
			b.bump()
			return last
		}
	}
	ev.Run = runID
	ev.Partial = true
	ev.Seq = int64(1)
	if n := len(b.events); n > 0 {
		ev.Seq = b.events[n-1].Seq + 1
	}
	b.events = append(b.events, ev)
	b.bump()
	return ev
}

// sealPartial 把未完成正文收成可落库的一帧：序号不变，Partial 清掉，文本换成最终全文。
func (m *eventMem) sealPartial(runID string, ev sseEvent) (sseEvent, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.runs[runID]
	if b == nil || len(b.events) == 0 {
		return sseEvent{}, false
	}
	last := b.events[len(b.events)-1]
	if !last.Partial {
		return sseEvent{}, false
	}
	last.Partial = false
	last.Failed = false
	last.Text = ev.Text
	last.Kind = ev.Kind
	last.Role = ev.Role
	last.Agent = ev.Agent
	last.ToolName = ev.ToolName
	last.ToolCalls = ev.ToolCalls
	b.events[len(b.events)-1] = last
	b.bump()
	return last, true
}

// failPartial 把未完成正文标成失败。不落库。没有半截时返回 false。
func (m *eventMem) failPartial(runID string) (sseEvent, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.runs[runID]
	if b == nil || len(b.events) == 0 {
		return sseEvent{}, false
	}
	last := b.events[len(b.events)-1]
	if !last.Partial {
		return sseEvent{}, false
	}
	last.Partial = false
	last.Failed = true
	last.Text = "模型没写完，请再发一次"
	b.events[len(b.events)-1] = last
	b.bump()
	return last, true
}

// discardPartial 静默去掉末尾半截，不通知成一条失败。最终结果不是正文时用。
func (m *eventMem) discardPartial(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.runs[runID]
	if b == nil || len(b.events) == 0 || !b.events[len(b.events)-1].Partial {
		return
	}
	b.events = b.events[:len(b.events)-1]
	b.bump()
}

// discardSeq 读者已经拿到失败帧之后，把它从内存删掉。
func (m *eventMem) discardSeq(runID string, seq int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.runs[runID]
	if b == nil {
		return
	}
	kept := b.events[:0]
	for _, ev := range b.events {
		if ev.Seq == seq && ev.Failed {
			continue
		}
		kept = append(kept, ev)
	}
	b.events = kept
}

// has 报告内存里是否已有这个序号。
func (m *eventMem) has(runID string, seq int64) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.runs[runID]
	if b == nil {
		return false
	}
	for _, ev := range b.events {
		if ev.Seq == seq {
			return true
		}
	}
	return false
}

// after 返回 seq 严格大于 from 的副本，最多 limit 条。
// 第二返回值为 false 表示这场 run 还不能从内存读，调用方应先装库。
func (m *eventMem) after(runID string, from int64, limit int) ([]sseEvent, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.runs[runID]
	if b == nil || !b.ready {
		return nil, false
	}
	return copyAfter(b.events, from, limit), true
}

// install 在内存还没对齐库时，把库里的帧并进来，之后这场 run 即可按序号读。
// 装载期间若又 publish 了更新的帧，按 seq 合并，不覆盖。
func (m *eventMem) install(runID string, fromDB []sseEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buf(runID, false)
	if b.ready {
		return
	}
	merged := make([]sseEvent, 0, len(fromDB)+len(b.events))
	merged = append(merged, fromDB...)
	for _, ev := range b.events {
		merged = insertSeq(merged, ev)
	}
	b.events = merged
	b.ready = true
	b.bump()
}

// insertSeq 按 seq 升序插入。已有相同 seq 时原样返回。
func insertSeq(list []sseEvent, ev sseEvent) []sseEvent {
	for _, old := range list {
		if old.Seq == ev.Seq {
			return list
		}
	}
	i := len(list)
	for i > 0 && list[i-1].Seq > ev.Seq {
		i--
	}
	list = append(list, sseEvent{})
	copy(list[i+1:], list[i:])
	list[i] = ev
	return list
}

// copyAfter 拷贝 seq > from 的前 limit 条。调用方须已持有锁，或传入不再被写的切片。
func copyAfter(list []sseEvent, from int64, limit int) []sseEvent {
	if limit <= 0 {
		limit = pollBatch
	}
	out := make([]sseEvent, 0, limit)
	for _, ev := range list {
		if ev.Seq <= from {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	return out
}
