// 本文件：压缩 Handler 读写会话内存的接口。Web 会话和 REPL 各有一份实现。
// 包不引用 webui：webui 反过来引用本包。
package compact

import "context"

// Commit 是一次压缩已经决定要留下的轮次。Stage 只改内存，不改 run_archive。
type Commit struct {
	// KeptRunIDs 是这一次拼装里还保留的、已经结束的轮。顺序与原档案一致。
	KeptRunIDs []string
	// DroppedRunIDs 是这一次拼装里不再保留的轮。收尾时清空它们的 produced_json。
	DroppedRunIDs []string
	// CombinedUser 是摘要接在这一轮用户原话前面的整句。空摘要时不会出现在这里。
	CombinedUser string
}

// Memory 是一场会话交给压缩 Handler 的那一份状态。
// 一个 Handler 只绑一份 Memory。几场会话共用同一个 Handler 会把轮次写串。
type Memory interface {
	// SessionID 是事件库里的会话 id。REPL 没有事件库，返回固定的 "repl"。
	SessionID() string
	// OriginalUser 是这一轮用户刚说的那句原话，不是已经接过摘要的那句。
	OriginalUser() string
	// CurrentRunID 是这一轮的 run_id。还没开轮时为空。
	CurrentRunID() string
	// CompletedRunIDs 按时间顺序列出已经结束、并且还留在内存里的轮。不含正在进行的这一轮。
	CompletedRunIDs() []string
	// Stage 把内存里的已结束轮换成 KeptRunIDs 对应的那些，并记下摘要。同一轮里第一次调用留下快照。
	Stage(Commit) error
	// Notify 告诉界面正在压缩。正文是给界面的短句，不是摘要。
	Notify(text string)
	// WriteRAG 把离开保留集合的一轮原文写进检索。失败由调用方记日志。
	WriteRAG(runID, body string) error
	// SearchRAG 用这一轮用户原话查一次旧原文。失败由调用方记日志。
	SearchRAG(query string) (string, error)
}

type memoryKey struct{}

// WithMemory 把这场会话的 Memory 放进造 agent 的 ctx。生产的 newAgent 只在取得到它时才装 Handler。
func WithMemory(ctx context.Context, mem Memory) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, memoryKey{}, mem)
}

// MemoryFrom 取出 WithMemory 放进去的那份。测试里的 NewAgent 不读它，也就不会装压缩。
func MemoryFrom(ctx context.Context) (Memory, bool) {
	if ctx == nil {
		return nil, false
	}
	mem, ok := ctx.Value(memoryKey{}).(Memory)
	return mem, ok && mem != nil
}
