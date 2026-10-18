package runengine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

// Server 把存储层接到 v6 的 API 面上（19v6-01 §0 端点表）。
// w 是 frames / log 两个拉取端点共用的单批上限（19v6-01 §0「拉取一律有界」，-batch-window 传入）。
type Server struct {
	store  *Store
	w      int
	worker *Worker
	// mediaRoot 是入口媒体落盘与 GET /api/media 的作业根（doc/32 A1/A4）——
	// 取自 worker 的 workspace（同一根、同一把牢笼）；单测直接构造 Server 时可单独给。
	mediaRoot string
	// mcpConfPath 是 mcp.json 的查找路径（引擎 cwd 相对）。字段只为单测可改指向临时文件；
	// 生产装配走 NewRouter 里的 defaultMcpConfPath。
	mcpConfPath string
	// mcp 连接缓存与互斥（mcp.go ensureMcpConn）；mcpConnect 可注入替身，单测不真拉子进程。
	mcpMu      sync.Mutex
	mcpConns   map[string]*mcpServerConn
	mcpConnect func(ctx context.Context, name string, sc McpServerConfig) (*mcpServerConn, error)
	// resumeMax 是单场 run 的续跑上限（-resume-max，doc/30 A1）：GET /api/runs/{id} 的
	// resumable = interrupted && resumeCount < resumeMax。A1 只读侧用它，A4a 的 claim 同值裁决。
	resumeMax int
}

// NewRouter 挂 v6 第一步的全部四个 API（19v6-01 §0）。整张面的心智模型就一条循环：
// **投一条消息，轮询看着回复一点点生成、定稿**。四个端点各管一段，两条游标别混：
//
//	eventOffset    —— run 内序号（1 起，start/delta/terminal 各占一个）。只挂在 frames 上。
//	                  定稿后 runlive 有 TTL，到期即失效 ⇒ 它不是历史地址。
//	runlogOffset   —— session 内全局序号（跨 run 单调，每条定稿记录占一个）。只挂在 log 上。
//	                  永久有效 ⇒ 双向合法：往前拨补新、往回拨翻旧。
//
// 客户端 reconcile（refresh / reconnect 同一条路径，19v6-02）：
//  1. GET /api/sessions/{id}      → {liveRunId, runlogOffset=高水位 H}
//  2. GET /api/sessions/{id}/log  → 游标取 max(0, H-窗口)（**不是 0**），分批拉定稿记录渲染
//  3. liveRunId 非空 → GET /api/runs/{runId}/frames 轮询，delta 按 eventOffset 追加成「正在生成」气泡
//  4. 收口：frames 见 terminal / status:gone / liveRunId 变 "" 或变了 → 回第 2 步拉 log，
//     按 runId 把 assistant 全文**替换**半截气泡（三个信号互为冗余，不押注单一信号）
//
// 不变量：两个**对齐游标**各自只在自己的端点前进；往上滚翻旧历史用的是**临时翻页游标**，
// 拉完即弃，不回退对齐游标。实时性 = 客户端轮询节奏（dev 建议 300–500ms），服务端无长连接、
// 无推送、无 heartbeat。两个拉取端点一律有界：单批上限 W（-batch-window，dev 默认 200）。
func NewRouter(store *Store, batchWindow int, dev bool, worker *Worker, resumeMax int) *gin.Engine {
	s := &Server{store: store, w: batchWindow, worker: worker, mediaRoot: worker.Workspace(), mcpConfPath: defaultMcpConfPath, resumeMax: resumeMax}
	r := gin.Default()
	r.POST("/api/runs", s.postRuns)
	r.POST("/api/runs/:runId/stop", s.stopRun)
	r.POST("/api/runs/:runId/resume", s.resumeRun)
	r.GET("/api/runs/:runId", s.getRunStatus)
	r.GET("/api/sessions/:id", s.getSession)
	r.GET("/api/runs/:runId/frames", s.getFrames)
	r.GET("/api/sessions/:id/log", s.getSessionLog)
	r.GET("/api/mcp/servers", s.getMcpServers)
	r.GET("/api/media", s.getMedia)
	if dev {
		r.DELETE("/api/dev/reset", s.devReset)
		r.GET("/api/dev/runs/:runId/journal", s.devJournal)
	}
	return r
}

type postImage struct {
	MIME       string `json:"mime"`
	DataBase64 string `json:"dataBase64"`
	// Name 是用户上传时的原始文件名（A6，doc/32 §3 通信字段）——可省；服务端 sanitize 后
	// 进占位行 name= 列，盘名仍是内容寻址（sha16），名字只进对话不做盘名。
	Name string `json:"name,omitempty"`
}

type postRunsRequest struct {
	SessionID SessionID   `json:"sessionId"`
	MessageID string      `json:"messageId"`
	Text      string      `json:"text"`
	Images    []postImage `json:"images,omitempty"`
}

// 字段序 = 19v6-01 §1 期望字节序：sessionId → runId → messageId。
type postRunsResponse struct {
	SessionID SessionID `json:"sessionId"`
	RunID     RunID     `json:"runId"`
	MessageID string    `json:"messageId"`
}

type sessionResponse struct {
	LiveRunID     RunID  `json:"liveRunId"`
	RunlogOffset  int    `json:"runlogOffset"`
	LastRunStatus string `json:"lastRunStatus"` // doc/17 §4：最新一场 run 的终态，"" = 无 run；stopped 收口靠它
	LastRunID     RunID  `json:"lastRunId"`     // doc/30 A1：最新一场 run 的 id，"" = 无 run；崩溃后点开 /api/runs/{id}
}

// 字段序 = doc/30 §8.4 契约字节序：runId → status → resumeCount。
type resumeResponse struct {
	RunID       RunID  `json:"runId"`
	Status      string `json:"status"`
	ResumeCount int    `json:"resumeCount"`
}

// 字段序 = doc/30 §5.1 契约字节序：runId → sessionId → status → resumeCount → resumable。
type runResponse struct {
	RunID       RunID     `json:"runId"`
	SessionID   SessionID `json:"sessionId"`
	Status      string    `json:"status"`
	ResumeCount int       `json:"resumeCount"`
	Resumable   bool      `json:"resumable"`
}

// 字段序 = 19v6-01 §2/§3 期望字节序。
type framesResponse struct {
	Frames     []Frame `json:"frames"`
	NextOffset int     `json:"nextOffset"`
	Status     string  `json:"status"`
}

type logResponse struct {
	Rows       []Row `json:"rows"`
	NextOffset int   `json:"nextOffset"`
}

// postRuns（19v6-01 §1）= 创建一场 run，语义上就是「投递一条用户消息并立刻开跑」。
//
// 请求体（四个字段，按情形发）：
//
//	{"sessionId":"s_01","messageId":"m_2","text":"...","images":[{"mime":"image/png","dataBase64":"..."}]}
//
//	sessionId  —— 可省。**不带 = 懒建会话**（服务端在投递事务内铸 s_<ULID>，客户端必须把
//	              返回的 sessionId 存下来，之后所有请求都带着它）；
//	            **带上 = 准入检查**：只认登记过的，未知一律 404，绝不隐式创建。
//	messageId  —— 客户端生成、投递前就有（绑本地气泡），不参与排序。超时重发就原样重投
//	              （同 messageId 同 text）；服务端**不按 messageId 去重**，幂等只在「闸」层面。
//	text       —— 消息正文。
//	images     —— 可省（doc/32 A1）。每项 {mime, dataBase64, name?}：name = 用户上传文件名
//	              （可省，A6 通信字段）；白名单与单文件 5MB / 单消息 4 张帽见 media.go。
//	              合法图落 workspace/.media/（内容寻址），user 行 text = 正文 + 每图一行
//	              [media …] 占位（含 seq 全会话序号与 name）——对话里只有 path，没有字节。
//
// 响应（字段序即契约字节序：sessionId → runId → messageId）：
//
//	202 {"sessionId":"s_01","runId":"r_01","messageId":"m_1"}
//	400 {"error":"bad_request"}          ← 请求体不是合法 JSON
//	400 {"error":"bad_media","reason":…} ← images 不合法（mime 白名单外 / base64 解不开 /
//	                                       超 5MB / 超 4 张），doc/32 A1；零痕迹可改后重投
//	404 {"error":"session_not_found"}    ← 只可能出现在「带未知 sessionId」时
//	409 {"error":"session_busy"}         ← 该会话已有活 run；**什么都没写**，客户端本地排队、本轮收口后补投
//	413 {"error":"message_too_large","limit":M,"tokens":N}
//	                                     ← 单条消息超准入线（仅 compact 开启时；limit = compactLimitFor(window)，
//	                                       线 = limit/3）；抢闸前拒收，零痕迹，客户端裁剪后可原样重投
//	500 {"error":"internal"}
//
// 服务端在**一笔原子写**里做完（D1 落库后 = 一笔 PG 事务持有跨 Redis 写，planning §8.4）：
// 抢闸（INSERT runs，部分唯一索引冲突即 409）+ runlog 追加 role:user 记录（占一个
// runlogOffset）+ 建 runlive:{runId} 并压 start 帧（eventOffset=1）+ 建空 draft。
// 要么整笔成功（消息开跑），要么整笔回滚（409/500 同形「什么都没写」）；**没有「入队待调度」这种中间态**。
// POST 本身不返回任何生成内容——客户端拿 runId 去 §2 轮询观测。拿到 runId 就能直接拉
// frames，不必先查 session；§4 那个状态查询是给 reconnect / 新 tab 用的入口。
func (s *Server) postRuns(c *gin.Context) {
	ctx := c.Request.Context()
	var req postRunsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	sessionID := req.SessionID
	if sessionID != "" {
		exists, err := s.store.Exists(ctx, sessionID)
		if err != nil {
			internal(c, err)
			return
		}
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "session_not_found"})
			return
		}
	}
	// 入口媒体（doc/32 A1）：先校验+落盘（400 同形 bad_media 带原因），再准入（A2 把 base64
	// 体折进 413 预算）——落盘在闸前，因为占位要进 user 行 text，而 user 行在 AcquireRun
	// 的原子写里定稿。409/500 时 .media/ 可能多出文件：内容寻址幂等、无对话痕迹，
	// 「什么都没写」语义只对落库面承诺。
	// seq 起点（doc/32 A6）：数会话既有占位数 +1——user 行是唯一携带 seq 的行，同会话同时
	// 只有一个 run 在写（409 忙闸），计数无并发竞争；闸在 PG 不在进程，多副本下照成立。
	// 懒建会话（不带 sessionId）= 新会话，从 1 起。
	placeholders := ""
	if len(req.Images) > 0 {
		seqStart := 1
		if sessionID != "" {
			n, err := s.store.CountMediaPlaceholders(ctx, sessionID)
			if err != nil {
				internal(c, err)
				return
			}
			seqStart = n + 1
		}
		ph, err := ingestImages(s.mediaRoot, req.Images, seqStart)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_media", "reason": err.Error()})
			return
		}
		placeholders = ph
	}
	// 准入（C5-A1，tail 支）：单条消息超 limit/3 → 抢闸前拒收——不烧闸、不落 mh 行、零痕迹，
	// 客户端可裁剪重发（messageId 复用无妨：幂等只在闸层面，这里根本没到闸）。
	// 只数正文（doc/32 A2 翻案 2026-10-18）：图的门槛是 ingestImages 的硬帽（上面已过），
	// base64 不进 cl100k——文本管道下模型窗口里只有占位行，数字节既拒错人又跑不动。
	if limit, tokens, ok := s.worker.AdmitText(req.Text); !ok {
		// 字段序 = error → limit → tokens（gin.H 经 encoding/json 按 key 字典序输出，三键恰好此序；
		// 若增删字段需重钉字节序）。
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "message_too_large", "limit": limit, "tokens": tokens})
		return
	}
	runID, err := newRunID()
	if err != nil {
		internal(c, err)
		return
	}
	if sessionID == "" {
		if sessionID, err = newSessionID(); err != nil {
			internal(c, err)
			return
		}
	}
	// user 行 text = 正文 + 每图一行占位（doc/32 纪律：对话里只有 path，没有字节）。
	userText := req.Text
	if placeholders != "" {
		userText = strings.TrimSpace(userText + "\n" + placeholders)
	}
	user := Row{RunID: runID, Role: "user", MessageID: req.MessageID, Text: userText}
	acquired, err := s.store.AcquireRun(ctx, sessionID, runID, user)
	if err != nil {
		internal(c, err)
		return
	}
	if !acquired {
		c.JSON(http.StatusConflict, gin.H{"error": "session_busy"})
		return
	}
	// 抢闸成功才开 worker：POST 返回 202 时 run 已在生成，frames/log 立刻可拉（19v6-01 §1）。
	// Background 而非请求 ctx：run 的生命周期不属于这个请求，响应写完它还在跑。
	// POST 路径 = 出生执行者，令牌固定 1（doc/30 §7）；续跑路径（A4a）传 claim 返回值。
	go s.worker.Run(context.Background(), sessionID, runID, 1)
	c.JSON(http.StatusAccepted, postRunsResponse{SessionID: sessionID, RunID: runID, MessageID: req.MessageID})
}

// stopRun（doc/17 §4 手动停止）：POST /api/runs/{runId}/stop。
// 竞态裁决在条件 UPDATE（Store.StopRun）：1 行 = 停止赢过正常收尾，翻 stopped；0 行 = 已是
// 终态，读回现值原样返回；行不存在 = 404。赢了之后查 worker 的取消注册表：本进程在跑这场
// run 就立刻 cancel（秒级断流）；打到别的副本时由跑着那侧的 watchdog 在下一个续约 tick
// （dev 10s）看见续约 CAS 0 再 cancel——本端点不为此另做通知。terminal 帧由 worker 收尾侧压
// （EndRun stopped 分支），这里不压帧、不动 Redis。
func (s *Server) stopRun(c *gin.Context) {
	runID := RunID(c.Param("runId"))
	status, found, err := s.store.StopRun(c.Request.Context(), runID)
	if err != nil {
		internal(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "run_not_found"})
		return
	}
	if status == "stopped" {
		// 用户停止永远最大：不看 epoch（doc/30 §7），注册表里是谁就 cancel 谁。
		if c, hit := s.worker.runCancels.Load(runID); hit {
			c.(*runCancel).cancel()
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": status})
}

// resumeRun（doc/30 §8）= 崩溃后接管一场 run：POST /api/runs/{runId}/resume，无请求体。
//
//	202 {"runId":"r_01","status":"running","resumeCount":1}
//	404 {"error":"run_not_found"}
//	409 {"error":"not_resumable"|"run_alive"|"resume_exhausted"}
//	500 {"error":"internal"}
//
// 顺序即正确性：claim（PG CAS，epoch+1，提交后才算接管）→ 重放账本 →
// 种 Redis 现场（令牌换代、runlive 重置为一条 resume 帧、draft 覆盖为账本正文）→ 开 worker。
// claim 之后任何一步失败都 AbandonClaim 把租约改成过期：epoch 不回退（次数照样消耗），
// 但下一发立刻可接——不把会话卡在一个半接管状态里。
func (s *Server) resumeRun(c *gin.Context) {
	ctx := c.Request.Context()
	runID := RunID(c.Param("runId"))
	claim, err := s.store.ClaimResume(ctx, runID, s.resumeMax)
	switch {
	case errors.Is(err, errRunNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "run_not_found"})
		return
	case errors.Is(err, errNotResumable):
		c.JSON(http.StatusConflict, gin.H{"error": "not_resumable"})
		return
	case errors.Is(err, errRunAlive):
		// 租约仍有效 = 原执行者可能只是慢了一拍，抢会造出双执行者（doc/30 §8.2）。
		c.JSON(http.StatusConflict, gin.H{"error": "run_alive"})
		return
	case errors.Is(err, errResumeExhausted):
		c.JSON(http.StatusConflict, gin.H{"error": "resume_exhausted"})
		return
	case err != nil:
		internal(c, err)
		return
	}
	replay, err := replayJournal(runID, claim.Rows)
	if err != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		internal(c, err)
		return
	}
	// 种子帧（doc/30 §8.3）：客户端原本轮询的 eventOffset 游标全部作废——runlive 被重置，
	// 第一条就是这条 resume 帧，正文 = 账本里已产出的 root assistant 拼接；draft 同值，
	// 续跑再追加 delta 后定稿文本天然连续。
	frameJSON, err := json.Marshal(Frame{Kind: "resume", Text: replay.Seed})
	if err != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		internal(c, err)
		return
	}
	if err := s.store.seedResume(ctx, runID, claim.Epoch, string(frameJSON), replay.Seed); err != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		internal(c, err)
		return
	}
	// 与 POST 同一条纪律：run 的生命周期不属于这个请求。
	go s.worker.RunResume(context.Background(), claim.SessionID, runID, claim.Epoch, replay)
	c.JSON(http.StatusAccepted, resumeResponse{RunID: runID, Status: "running", ResumeCount: int(claim.Epoch - 1)})
}

// getSession（19v6-01 §4）= 会话状态查询，reconcile 的**第一步**（refresh / 新 tab 都从它进）。
//
//	GET /api/sessions/s_01
//	200 {"liveRunId":"r_1","runlogOffset":1}   ← 有 run 正在跑；runlog 已有 1 条（刚投递的 user 消息）
//	200 {"liveRunId":"","runlogOffset":2}      ← run 已结束；助手全文已进 runlog
//	404 {"error":"session_not_found"}
//
// 两个字段各回答一个问题，客户端据此决定后面拉哪个端点：
//   - `liveRunId`：该会话当前的活 run（读闸），`""` = 会话闲着。**非空才需要去拉 §2 的 frames**；
//     它从非空变 `""`、或换成了另一个 runId，本身就是「该收口了」的触发信号之一。
//   - `runlogOffset`：`runlog:{sessionId}` 的**高水位**（LLEN）。两个用处——判断有没有新定稿
//     要补（本地对齐游标 < 高水位 就还得拉），以及给 §3 算初始窗口游标 `max(0, H - 窗口)`。
//
// 它**不返回任何生成内容**，也不参与投递：发消息后可以直接拿 runId 去轮询 frames，不必先过这里。
// 三个要素（登记 / 闸 / 高水位）由单条 SQL 子查询取一致快照（原 MULTI 的等价物）——
// 「读到活 run 却配了旧高水位」在这条语句里不可能出现。闸的判活即租约谓词
// `status='running' AND lease_until > now()`：租约已过的死行**读侧即回收**，不等清扫。
func (s *Server) getSession(c *gin.Context) {
	liveRunID, runlogOffset, lastStatus, lastRunID, exists, err := s.store.SessionStatus(c.Request.Context(), SessionID(c.Param("id")))
	if err != nil {
		internal(c, err)
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "session_not_found"})
		return
	}
	c.JSON(http.StatusOK, sessionResponse{LiveRunID: liveRunID, RunlogOffset: runlogOffset, LastRunStatus: lastStatus, LastRunID: lastRunID})
}

// getRunStatus（doc/30 A1）= 单场 run 的对外状态，崩溃后「这场是死了还是还能续」的唯一读口。
//
//	GET /api/runs/r_01
//	200 {"runId":"r_01","sessionId":"s_01","status":"running","resumeCount":0,"resumable":false}
//	404 {"error":"run_not_found"}
//
// status 五态里只有 interrupted 是派生值：runs.status 仍是 running、租约已过（PG now()），
// 读侧当场判「执行者已经不在了」，不等清扫（清扫只在下一发 POST 时发生）。
// resumeCount = lease_epoch - 1（每次 claim 续跑 +1）；resumable = interrupted && 次数未耗尽。
// 未知 run 与 session 端点同形回 404（这里与 frames 的 200 gone 不同：那个是 runlive TTL 的
// 生命周期，这里是 PG 行存在性——行在不在是确定的）。
func (s *Server) getRunStatus(c *gin.Context) {
	st, found, err := s.store.RunStatus(c.Request.Context(), RunID(c.Param("runId")), s.resumeMax)
	if err != nil {
		internal(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "run_not_found"})
		return
	}
	c.JSON(http.StatusOK, runResponse{
		RunID:       st.RunID,
		SessionID:   st.SessionID,
		Status:      st.Status,
		ResumeCount: st.ResumeCount,
		Resumable:   st.Resumable,
	})
}

// getFrames（19v6-01 §2）= 拉某场 run **正在生成**的帧序列（Redis LIST `runlive:{runId}`）。
//
//	GET /api/runs/r_1/frames?eventOffset=0
//	200 {"frames":[
//	  {"eventOffset":1,"kind":"start","messageId":"m_1"},
//	  {"eventOffset":2,"kind":"delta","text":"好的"},
//	  {"eventOffset":3,"kind":"delta","text":"，我来读"},
//	  {"eventOffset":4,"kind":"terminal","status":"complete"}
//	],"nextOffset":4,"status":"done"}
//
// 游标语义：`eventOffset=N` 返回该 run 内 offset **> N** 的帧，升序，单批最多 W 条；缺省、
// 非法、负数都当 0（= 从 start 起，新客户端能看到整段生成，超长回复分批拉完）。实现是
// `LRANGE runlive:{runId} N N+W-1`，**index+1 才是 eventOffset**。
// `nextOffset` = 本批最大 offset，**空批 = N**；被 W 截断时它就是本批末帧的 offset，
// 客户端直接拿它当下次游标续拉即补齐。
//
// 帧只有三种 kind：`start`（带 messageId）、`delta`（text 是**增量片段**不是累计全文，
// 客户端按 eventOffset 顺序**追加**进气泡；去重 = 「见过的 eventOffset 跳过」）、
// `terminal`（这场 run 的最后一个事件，带终态 status，本步只有 complete/error，
// stopped 留给后续 loop）。
//
// status 三态**只靠「runlive 键存在性 + 本批是否含 terminal」推出**，无额外状态字段：
//   - generating → 还没 terminal，按轮询间隔继续拉 nextOffset 之后；
//   - done       → 见 terminal，**收口**：回 log 端点按 runId 拿 assistant 全文替换半截气泡；
//   - gone       → runlive 键已过期删除，该 run 早已定稿，**转拉 log**。
//
// 未知 run **故意不返 404**：「从未存在」与「TTL 到期删除」不区分，都是 200 gone——
// 客户端两种情况的动作本来相同（转拉 log），区分它没有价值。
func (s *Server) getFrames(c *gin.Context) {
	from := queryOffset(c, "eventOffset")
	frames, next, exists, err := s.store.ReadFrames(c.Request.Context(), RunID(c.Param("runId")), from, s.w)
	if err != nil {
		internal(c, err)
		return
	}
	status := "generating"
	if !exists {
		status = "gone"
	} else {
		for _, f := range frames {
			if f.Kind == "terminal" {
				status = "done"
				break
			}
		}
	}
	c.JSON(http.StatusOK, framesResponse{Frames: frames, NextOffset: next, Status: status})
}

// getSessionLog（19v6-01 §3）= 拉 session 的**定稿历史**（PG `runlog_entries` 表）。
//
//	GET /api/sessions/s_01/log?runlogOffset=0
//	200 {"rows":[
//	  {"runlogOffset":1,"runId":"r_1","role":"user","agent":"","model":"","messageId":"m_1","text":"读一下 doc/19"},
//	  {"runlogOffset":2,"runId":"r_1","role":"assistant","agent":"","model":"deepseek/deepseek-flash","text":"好的，我来读……"}
//	],"nextOffset":2}
//	404 {"error":"session_not_found"}
//
// 游标语义：`runlogOffset` 是 **session 内全局序号**（跨 run 单调，每条定稿记录占一个，
// index+1 即 offset），跟 §2 那个 run 内的 eventOffset 是两个东西，别混。N 返回 offset **> N**
// 的记录，升序，单批最多 W 条；`nextOffset` = 本批最大 offset，空批 = N，被 W 截断时续拉即补齐。
//
// **初始游标不是 0，是 max(0, 高水位 - 窗口)**：高水位从 §4 拿。
// **为什么初始值不能是 0**：runlog 永久增长且无上限（compact 压的是 run_model_messages，不动
// runlog；它是历史真源，D1 已落 PG），「从 0 全量」在长会话下就是无界请求。
// 窗口多大由客户端定，服务端只用 W 兜底截断。offset 永久有效 ⇒ **往回拨翻更早的历史同样合法**
// （那是临时翻页游标，拉完渲染即可，别拿它回退本地的对齐游标）。
//
// 内容约定：**这里永远只有已定稿的 message**，没有半截——生成中的半截在 §2 的 frames 里。
// 所以 error 结局的残缺文本不进 runlog、也不另留痕（它只在 runlive 的 TTL 窗口内可见）。
// 每条记录带 `runId`：客户端就是按它把定稿的 assistant 全文对上「正在生成」气泡做**替换**（收口）。
// T3-2 起子代理的行也在列（工具动作行 + 各 agent 的终稿行）：`agent`/`model` 两列是溯源坐标
// （agent 空 = root；model 空 = 非模型产物——user 行两列皆空）。
func (s *Server) getSessionLog(c *gin.Context) {
	from := queryOffset(c, "runlogOffset")
	rows, next, exists, err := s.store.ReadLog(c.Request.Context(), SessionID(c.Param("id")), from, s.w)
	if err != nil {
		internal(c, err)
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "session_not_found"})
		return
	}
	c.JSON(http.StatusOK, logResponse{Rows: rows, NextOffset: next})
}

// getMedia（doc/32 A4）= 前端把占位行里的 path 换回字节流的唯一出口。
// 牢笼与工具侧同一把闸（resolveInWorkspace）：越界 / 缺 path → 400，不存在 / 目录 → 404。
// mime 按扩展名表（mediaMimeByExt，与 read_media_file 同表）；回流走 ServeContent（支持 Range）。
func (s *Server) getMedia(c *gin.Context) {
	p := c.Query("path")
	if p == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "reason": "缺 path"})
		return
	}
	full, err := resolveInWorkspace(s.mediaRoot, p)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "reason": "路径越出 workspace"})
		return
	}
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	f, err := os.Open(full)
	if err != nil {
		internal(c, err)
		return
	}
	defer f.Close()
	mime := mediaMimeByExt[strings.ToLower(filepath.Ext(full))]
	if mime == "" {
		mime = "application/octet-stream"
	}
	c.Header("Content-Type", mime)
	http.ServeContent(c.Writer, c.Request, fi.Name(), fi.ModTime(), f)
}

// internal 是存储 / ID 生成故障的统一出口：客户端拿 19v6-01 §1 的 500 字节，原因只进服务端日志。
func internal(c *gin.Context, err error) {
	log.Printf("%s %s: %v", c.Request.Method, c.Request.URL.Path, err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
}

// queryOffset 解析拉取游标：缺省 / 非法 / 负数一律视为 0（契约只规定缺省=0，不发明新错误码）。
func queryOffset(c *gin.Context, name string) int {
	n, err := strconv.Atoi(c.Query(name))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// 字段序 = doc/30 §10.4 契约字节序（rows 按 seq 升序，空串字段照出，方便 curl 逐字段断言）。
type journalRowResponse struct {
	Seq        int64  `json:"seq"`
	Epoch      int64  `json:"epoch"`
	Role       string `json:"role"`
	Model      string `json:"model"`
	Text       string `json:"text"`
	ToolCalls  string `json:"toolCalls"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Args       string `json:"args"`
	Usage      string `json:"usage"`
}

type journalResponse struct {
	RunID RunID                `json:"runId"`
	Epoch int64                `json:"epoch"`
	Rows  []journalRowResponse `json:"rows"`
}

// devJournal 是账本的 dev 观测端点（doc/30 §10.4，长期 dev 工具、只在 -dev 注册）：
// 崩溃后 curl 一眼看到「模型说到哪、哪个工具可能跑过」。run 存在但账本已删（终态）= 200 rows:[]；
// 未知 run = 404。顶层 epoch 是 runs.lease_epoch 当前值，每行 epoch 是写它时的令牌。
func (s *Server) devJournal(c *gin.Context) {
	epoch, rows, found, err := s.store.JournalView(c.Request.Context(), RunID(c.Param("runId")))
	if err != nil {
		internal(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "run_not_found"})
		return
	}
	resp := journalResponse{RunID: RunID(c.Param("runId")), Epoch: epoch, Rows: make([]journalRowResponse, 0, len(rows))}
	for _, r := range rows {
		resp.Rows = append(resp.Rows, journalRowResponse{
			Seq: r.Seq, Epoch: r.Epoch, Role: r.Role, Model: r.Model, Text: r.Text,
			ToolCalls: r.ToolCalls, ToolCallID: r.ToolCallID, ToolName: r.ToolName,
			Args: r.Args, Usage: r.Usage,
		})
	}
	c.JSON(http.StatusOK, resp)
}

// devReset 是 dev-only 全局归零（planning §4 脚手架表：长期 dev 工具，只在 dev 装配注册）。
// D1 后两半都清：PG 五表 TRUNCATE（M1 起含 notes）+ `tk:*` 键 UNLINK（runs 必须同清——它是闸的落点）。
func (s *Server) devReset(c *gin.Context) {
	deleted, err := s.store.Reset(c.Request.Context())
	if err != nil {
		internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

// newSessionID / newRunID 生成 s_/r_ 前缀 + ULID。rand 失败返回 error（调用方回 500），不 panic。
func newSessionID() (SessionID, error) {
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return "", err
	}
	return "s_" + SessionID(id.String()), nil
}

func newRunID() (RunID, error) {
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return "", err
	}
	return "r_" + RunID(id.String()), nil
}
