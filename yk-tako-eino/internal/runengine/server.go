package runengine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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
	mcpConnect func(ctx context.Context, name string, sc McpServerConfig, root string) (*mcpServerConn, error)
	// resumeMax 是单场 run 的续跑上限（-resume-max，doc/30 A1）：GET /api/runs/{id} 的
	// resumable = interrupted && resumeCount < resumeMax。A1 只读侧用它，A4a 的 claim 同值裁决。
	resumeMax int
	// auth 是身份面。业务路由挂 RequireAuth；mcp/media/memory 在有归属模型前只放 system admin。
	auth *Auth
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
func NewRouter(store *Store, batchWindow int, dev bool, worker *Worker, resumeMax int, auth *Auth) *gin.Engine {
	s := &Server{store: store, w: batchWindow, worker: worker, mediaRoot: worker.Workspace(), mcpConfPath: defaultMcpConfPath, resumeMax: resumeMax, auth: auth}
	r := gin.Default()
	r.POST("/api/auth/signup", s.auth.Signup)
	r.POST("/api/auth/login", s.auth.Login)
	r.POST("/api/auth/refresh", s.auth.Refresh)
	r.POST("/api/auth/logout", s.auth.RequireAuth(), s.auth.Logout)
	r.GET("/api/auth/me", s.auth.RequireAuth(), s.auth.Me)
	r.POST("/api/auth/switch-tenant", s.auth.RequireAuth(), s.auth.SwitchTenant)
	authn := s.auth.RequireAuth()
	sys := s.auth.RequireSystemAdmin()
	r.POST("/api/runs", authn, s.postRuns)
	r.POST("/api/runs/:runId/stop", authn, s.stopRun)
	r.POST("/api/runs/:runId/resume", authn, s.resumeRun)
	r.GET("/api/runs/:runId", authn, s.getRunStatus)
	r.GET("/api/sessions", authn, s.listSessions)
	r.PATCH("/api/sessions/:id/visibility", authn, s.patchSessionVisibility)
	r.PATCH("/api/sessions/:id", authn, s.patchSessionWorkspace)
	r.GET("/api/sessions/:id", authn, s.getSession)
	r.DELETE("/api/sessions/:id", authn, s.deleteSession)
	r.GET("/api/sessions/:id/subagents", authn, s.getSessionSubagents)
	r.GET("/api/runs/:runId/frames", authn, s.getFrames)
	r.GET("/api/runs/:runId/stream", authn, s.getRunStream)
	r.GET("/api/sessions/:id/log", authn, s.getSessionLog)
	r.GET("/api/mcp/servers", authn, sys, s.getMcpServers)
	r.GET("/api/media", authn, s.getMedia)
	r.GET("/api/memory/items", authn, s.getMemoryItems)
	r.GET("/api/memory/jobs", authn, s.getMemoryJobs)
	r.GET("/api/memory/dialogue", authn, s.getMemoryDialogue)
	r.GET("/api/memory/recall", authn, s.getMemoryRecall)
	r.POST("/api/tenants", authn, s.createTenant)
	r.POST("/api/workspaces", authn, s.postWorkspaces)
	r.GET("/api/workspaces", authn, s.listWorkspaces)
	r.PATCH("/api/workspaces/:id", authn, s.patchWorkspace)
	r.PATCH("/api/workspaces/:id/visibility", authn, s.patchWorkspaceVisibility)
	r.DELETE("/api/workspaces/:id", authn, s.deleteWorkspace)
	r.GET("/api/tenants", authn, s.listTenants)
	r.GET("/api/me/invitations", authn, s.myInvitations)
	r.POST("/api/me/invitations/:id/accept", authn, s.acceptInvitation)
	r.POST("/api/me/invitations/:id/decline", authn, s.declineInvitation)
	r.GET("/api/invitations/:token", s.lookupInvitation)
	r.POST("/api/auth/register-by-invite", s.registerByInvite)
	ten := r.Group("/api/tenants/:id", authn, s.auth.PathTenantMatch())
	ten.GET("/members", s.auth.RequireTenantRole("viewer"), s.listMembers)
	ten.POST("/members", s.auth.RequireTenantRole("owner"), s.addMember)
	ten.PUT("/members/:user_id", s.auth.RequireTenantRole("owner"), s.updateMember)
	ten.DELETE("/members/:user_id", s.auth.RequireTenantRole("owner"), s.removeMember)
	ten.POST("/leave", s.auth.RequireTenantRole("viewer"), s.leaveTenant)
	ten.POST("/transfer-owner", s.auth.RequireTenantRole("owner"), s.transferOwner)
	ten.GET("/audit-log", s.auth.RequireTenantRole("admin"), s.tenantAudit)
	r.GET("/api/tenants/:id", authn, s.auth.PathTenantMatch(), s.auth.RequireTenantRole("viewer"), s.getTenant)
	r.PUT("/api/tenants/:id", authn, s.auth.PathTenantMatch(), s.auth.RequireTenantRole("owner"), s.putTenant)
	r.DELETE("/api/tenants/:id", authn, s.auth.PathTenantMatch(), s.auth.RequireTenantRole("owner"), s.deleteTenant)
	ten.GET("/notes", s.auth.RequireTenantRole("viewer"), s.listTenantNotes)
	ten.POST("/notes", s.auth.RequireTenantRole("admin"), s.postTenantNote)
	ten.GET("/agents", s.auth.RequireTenantRole("viewer"), s.listAgents)
	ten.POST("/agents", s.auth.RequireTenantRole("member"), s.createAgent)
	ten.POST("/agents/import", s.auth.RequireTenantRole("member"), s.importAgents)
	ten.PUT("/agents/:agent_id", s.auth.RequireTenantRole("member"), s.updateAgent)
	ten.DELETE("/agents/:agent_id", s.auth.RequireTenantRole("member"), s.deleteAgent)
	ten.GET("/usage", s.auth.RequireTenantRole("admin"), s.tenantUsage)
	ten.GET("/runtime-config", s.auth.RequireTenantRole("viewer"), s.getRuntimeConfig)
	ten.PUT("/runtime-config", s.auth.RequireTenantRole("admin"), s.putRuntimeConfig)
	ten.GET("/model-resolve", s.auth.RequireTenantRole("viewer"), s.getModelResolve)
	ten.GET("/mcp-status", s.auth.RequireTenantRole("admin"), s.getMcpStatus)
	ten.POST("/invitations", s.auth.RequireTenantRole("owner"), s.createInvitation)
	ten.GET("/invitations", s.auth.RequireTenantRole("viewer"), s.listInvitations)
	ten.DELETE("/invitations/:inv_id", s.auth.RequireTenantRole("owner"), s.revokeInvitation)
	sysg := r.Group("/api/system/admin", authn, sys)
	sysg.GET("/tenants", s.listAdminTenants)
	sysg.GET("/users", s.listAdminUsers)
	sysg.POST("/users", s.createAdminUser)
	sysg.POST("/promote", s.promoteAdmin)
	sysg.POST("/revoke", s.revokeAdmin)
	sysg.GET("/audit-log", s.systemAudit)
	if dev {
		r.DELETE("/api/dev/reset", s.devReset)
		r.GET("/api/dev/runs/:runId/journal", s.devJournal)
		r.GET("/api/dev/notes", authn, s.listDevNotes)
		r.POST("/api/dev/notes", authn, s.postDevNote)
		r.POST("/api/dev/memory", authn, s.postDevMemory)
		r.POST("/api/dev/workspace/file", authn, s.postDevFile)
		r.GET("/api/dev/workspace/file", authn, s.getDevFile)
		r.POST("/api/dev/workspace/bash", authn, s.postDevBash)
		r.POST("/api/dev/file-fingerprint", authn, s.devFileFingerprint)
		r.POST("/api/dev/file-lock-check", authn, s.devFileLockCheck)
		r.POST("/api/dev/pii-check", authn, s.devPiiCheck)
		r.POST("/api/dev/clean-sweep", s.devCleanSweep)
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
	SessionID   SessionID   `json:"sessionId"`
	MessageID   string      `json:"messageId"`
	Text        string      `json:"text"`
	Images      []postImage `json:"images,omitempty"`
	AgentID     string      `json:"agentId,omitempty"`
	WorkspaceID string      `json:"workspaceId,omitempty"`
}

// 字段序 = 19v6-01 §1 期望字节序：sessionId → runId → messageId。
type postRunsResponse struct {
	SessionID SessionID `json:"sessionId"`
	RunID     RunID     `json:"runId"`
	MessageID string    `json:"messageId"`
}

type sessionResponse struct {
	LiveRunID     RunID   `json:"liveRunId"`
	RunlogOffset  int     `json:"runlogOffset"`
	LastRunStatus string  `json:"lastRunStatus"` // doc/17 §4：最新一场 run 的终态，"" = 无 run；stopped 收口靠它
	LastRunID     RunID   `json:"lastRunId"`     // doc/30 A1：最新一场 run 的 id，"" = 无 run；崩溃后点开 /api/runs/{id}
	WorkspaceID   *string `json:"workspaceId"`   // doc/35 A4：挂着的项目；游离会话是 null
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
//	              合法图落工作区目录 .media/（内容寻址，占位 path 带 workspaces/<wid>/），user 行 text = 正文 + 每图一行
//	              [media …] 占位（含 seq 全会话序号与 name）——对话里只有 path，没有字节。
//
// 响应（字段序即契约字节序：sessionId → runId → messageId）：
//
//	202 {"code":0,"message":"","data":{"sessionId":"s_01","runId":"r_01","messageId":"m_1"}}
//	400 {"code":111001,"message":"请求格式不对","data":null}   ← 请求体不是合法 JSON
//	400 {"code":111004,"message":"图片不符合要求","data":{"reason":…}}
//	                                       ← images 不合法（mime 白名单外 / base64 解不开 /
//	                                         超 5MB / 超 4 张），doc/32 A1；零痕迹可改后重投
//	404 {"code":112001,"message":"找不到这场会话","data":null}   ← 只可能出现在「带未知 sessionId」时
//	409 {"code":112101,"message":"这场会话正在跑，等它跑完再发","data":null}
//	                                       ← 该会话已有活 run；**什么都没写**，客户端本地排队、本轮收口后补投
//	413 {"code":113003,"message":"这条消息太长，裁剪后再发","data":{"limit":M,"tokens":N}}
//	                                     ← 单条消息超准入线（仅 compact 开启时；limit = compactLimitFor(window)，
//	                                       线 = limit/3）；抢闸前拒收，零痕迹，客户端裁剪后可原样重投
//	500 {"code":111901,"message":"服务暂时不可用，请再试一次","data":null}
//
// 服务端在**一笔原子写**里做完（D1 落库后 = 一笔 PG 事务持有跨 Redis 写，planning §8.4）：
// 抢闸（INSERT runs，部分唯一索引冲突即 409）+ runlog 追加 role:user 记录（占一个
// runlogOffset）+ 建 runlive:{runId} 并压 start 帧（eventOffset=1）+ 建 runepoch。
// 要么整笔成功（消息开跑），要么整笔回滚（409/500 同形「什么都没写」）；**没有「入队待调度」这种中间态**。
// POST 本身不返回任何生成内容——客户端拿 runId 去 §2 轮询观测。拿到 runId 就能直接拉
// frames，不必先查 session；§4 那个状态查询是给 reconnect / 新 tab 用的入口。
func (s *Server) postRuns(c *gin.Context) {
	ctx := c.Request.Context()
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	var req postRunsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	sessionID := req.SessionID
	// 项目预检在会话准入之前：请求里的 workspaceId 看不见则 114001，不铸会话、不建 run。
	// 已有会话即使带了合法的 workspaceId，AcquireRun 也不改 workspace_id。
	// 已经挂上的项目被收回时，AcquireRun 按会话行上的列再看一次，这一发也不建 run。
	workspaceID := strings.TrimSpace(req.WorkspaceID)
	if workspaceID == "" {
		// 形态收窄（2026-10-22，doc/42）：**不再有「游离会话」**——每场会话都挂一个项目，
		// 没指定就用该成员的默认项目（没有就当场建，doc/35 §0 第 6 条）。
		// 为什么必须挂：产出文件落项目目录、会话目录只放会话自己的档案（子代理 wire / .media）
		// → 删会话不再留孤儿文件（原来的孤儿是「产出留在会话目录里」造成的）。
		ws, werr := s.store.EnsureDefaultWorkspace(ctx, p.TenantID, p.UserID)
		if werr != nil {
			internal(c, werr)
			return
		}
		workspaceID = ws.ID
	}
	if _, err := s.store.VisibleWorkspace(ctx, workspaceID, p.TenantID, p.UserID); err != nil {
		if errors.Is(err, ErrWorkspaceNotFound) {
			writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
			return
		}
		internal(c, err)
		return
	}
	// 归属预检在 AdmitText / 落盘之前：别人用超长 text 或坏图不能靠 413/400 探到会话是否存在。
	var bound Session
	if sessionID != "" {
		var kind accessKind
		var err error
		bound, kind, err = s.store.SessionAccess(ctx, sessionID, p)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
				return
			}
			internal(c, err)
			return
		}
		if kind != accessOwner {
			writeEnvelope(c, http.StatusForbidden, codeNotOwner, "只有本人能修改", nil)
			return
		}
		// 存量游离会话归位（2026-10-22，doc/42）：改造前建的会话 workspace_id 为空 →
		// 它的产出仍落会话目录（删会话会留孤儿文件）。本场 run 把它挂到默认项目，之后
		// 产出就落项目目录了（会话目录只剩会话自己的档案）。
		if bound.WorkspaceID == nil || *bound.WorkspaceID == "" {
			if err := s.store.SetSessionWorkspace(ctx, sessionID, p, workspaceID); err != nil {
				log.Printf("attach default workspace %s: %v", sessionID, err)
			}
		}
	}
	// 本场要用的助手：请求里的 agentId 优先；没带就沿用会话上已经记下的。
	// 看不见的助手在抢闸前 404，不建 run。角色门槛只卡「这次新指定」，
	// 沿用旧绑定的后续消息不再重判 member。
	agentID := req.AgentID
	if agentID == "" {
		agentID = bound.AgentID
	}
	if req.AgentID != "" && roleRank(p.Role) < roleRank("member") {
		writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
		return
	}
	prof, aerr := s.assembleRun(ctx, p, agentID)
	if aerr != nil {
		writeEnvelope(c, aerr.Status, aerr.Code, aerr.Message, nil)
		return
	}
	handed := false
	defer func() {
		if !handed {
			prof.Close()
		}
	}()
	if s.overQuota(c, p) {
		return
	}
	// 会话 id 在落盘前定下来。占位 path 带 workspaces/<wid>/，GET /api/media 读同一条。
	// 已有会话不看这次请求里的 workspaceId：列为空就进默认工作区。新会话才用请求里的项目。
	// 这时还没抢闸。后面 400/413/409 可能在工作区目录里留下内容寻址文件，落库面仍是空的。
	sessionExists := sessionID != ""
	if sessionID == "" {
		var err error
		if sessionID, err = newSessionID(); err != nil {
			internal(c, err)
			return
		}
	}
	// 入口媒体（doc/32 A1）：先校验+落盘（400 同形 bad_media 带原因），再准入（A2 把 base64
	// 体折进 413 预算）——落盘在闸前，因为占位要进 user 行 text，而 user 行在 AcquireRun
	// 的原子写里定稿。409/500 时工作区目录里的 .media/ 可能多出文件：内容寻址幂等、无对话痕迹，
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
		dir, prefix, err := s.imageRoot(ctx, p, sessionID, workspaceID, sessionExists)
		if errors.Is(err, ErrWorkspaceNotFound) {
			writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
			return
		}
		if err != nil {
			internal(c, err)
			return
		}
		_, _, _, maxBytes, qerr := s.store.TenantQuota(ctx, p.TenantID)
		if qerr != nil {
			internal(c, qerr)
			return
		}
		var quota func(int) (func(), error)
		if maxBytes > 0 {
			quota = func(n int) (func(), error) {
				return s.store.ReserveStorage(ctx, p.TenantID, int64(n), maxBytes, func() int64 {
					return directoryBytes(tenantStorageRoot(s.mediaRoot, p.TenantID))
				})
			}
		}
		ph, err := ingestImagesAt(dir, req.Images, seqStart, prefix, quota)
		if errors.Is(err, errStorageQuota) {
			writeEnvelope(c, http.StatusTooManyRequests, codeQuota, "这个工作区的额度用完了", nil)
			return
		}
		if err != nil {
			writeEnvelope(c, http.StatusBadRequest, codeBadMedia, "图片不符合要求", gin.H{"reason": err.Error()})
			return
		}
		placeholders = ph
	}
	// 准入（C5-A1，tail 支）：单条消息超 limit/3 → 抢闸前拒收——不烧闸、不落 mh 行、零痕迹，
	// 客户端可裁剪重发（messageId 复用无妨：幂等只在闸层面，这里根本没到闸）。
	// 只数正文（doc/32 A2 翻案 2026-10-18）：图的门槛是 ingestImages 的硬帽（上面已过），
	// base64 不进 cl100k——文本管道下模型窗口里只有占位行，数字节既拒错人又跑不动。
	if limit, tokens, ok := s.worker.AdmitText(req.Text); !ok {
		// limit 与 tokens 进 data（客户端据此裁剪重发），message 只放人话（doc/40 §3 规则 3）。
		writeEnvelope(c, http.StatusRequestEntityTooLarge, codeMessageTooLarge, "这条消息太长，裁剪后再发",
			gin.H{"limit": limit, "tokens": tokens})
		return
	}
	runID, err := newRunID()
	if err != nil {
		internal(c, err)
		return
	}
	// user 行 text = 正文 + 每图一行占位（doc/32 纪律：对话里只有 path，没有字节）。
	userText := req.Text
	if placeholders != "" {
		userText = strings.TrimSpace(userText + "\n" + placeholders)
	}
	user := Row{RunID: runID, Role: "user", MessageID: req.MessageID, Text: userText}
	acquired, err := s.store.AcquireRun(ctx, sessionID, runID, user, p, workspaceID)
	if errors.Is(err, ErrSessionNotFound) {
		writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
		return
	}
	if errors.Is(err, ErrSessionForbidden) {
		writeEnvelope(c, http.StatusForbidden, codeNotOwner, "只有本人能修改", nil)
		return
	}
	if errors.Is(err, ErrWorkspaceNotFound) {
		writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
		return
	}
	// errProjectBusy（项目忙 409）已随项目闸退役（doc/42，2026-10-22）——同项目并发不再被拒。
	if err != nil {
		internal(c, err)
		return
	}
	if !acquired {
		writeEnvelope(c, http.StatusConflict, codeSessionRunning, "这场会话正在跑，等它跑完再发", nil)
		return
	}
	// 抢闸成功才开 worker：POST 返回 202 时 run 已在生成，frames/log 立刻可拉（19v6-01 §1）。
	// Background 而非请求 ctx：run 的生命周期不属于这个请求，响应写完它还在跑。
	// POST 路径 = 出生执行者，令牌固定 1（doc/30 §7）；续跑路径（A4a）传 claim 返回值。
	if req.AgentID != "" {
		_ = s.store.db.WithContext(ctx).Exec(`UPDATE sessions SET agent_id = ? WHERE id = ?`, req.AgentID, sessionID).Error
	}
	go s.worker.Run(withRunProfile(withPrincipal(context.Background(), p), prof), sessionID, runID, 1)
	handed = true
	writeEnvelope(c, http.StatusAccepted, codeOK, "", postRunsResponse{SessionID: sessionID, RunID: runID, MessageID: req.MessageID})
}

// imageRoot 决定这批入口图写到哪，并给出占位 path 的前缀 workspaces/<wid>/。
// 已有会话只看会话行上的项目；新会话看这次请求的 workspaceId。两边都空就用默认工作区，没有这行就建。
// 解析出来的项目当前用户看不见时，返回 ErrWorkspaceNotFound，调用方回 114001，字节不落盘。
func (s *Server) imageRoot(ctx context.Context, p principal, sessionID SessionID, requestWorkspaceID string, sessionExists bool) (string, string, error) {
	wid := ""
	if sessionExists {
		var err error
		wid, err = s.store.SessionWorkspaceID(ctx, sessionID)
		if err != nil {
			return "", "", err
		}
	} else {
		wid = requestWorkspaceID
	}
	if wid == "" {
		ws, err := s.store.EnsureDefaultWorkspace(ctx, p.TenantID, p.UserID)
		if err != nil {
			return "", "", err
		}
		wid = ws.ID
	} else if _, err := s.store.VisibleWorkspace(ctx, wid, p.TenantID, p.UserID); err != nil {
		return "", "", err
	}
	dir, err := workspaceDir(s.mediaRoot, p.TenantID, wid)
	if err != nil {
		return "", "", err
	}
	return dir, "workspaces/" + wid + "/", nil
}

// runView 是 run 端点的归属结果。written=true 表示 401/500 已经写出，调用方直接 return。
type runView struct {
	P       principal
	SID     SessionID
	Kind    accessKind
	Found   bool
	written bool
}

func (s *Server) viewRun(c *gin.Context, runID RunID) runView {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return runView{written: true}
	}
	sid, found, err := s.store.RunSession(c.Request.Context(), runID)
	if err != nil {
		internal(c, err)
		return runView{written: true}
	}
	if !found {
		return runView{P: p}
	}
	_, kind, err := s.store.SessionAccess(c.Request.Context(), sid, p)
	if errors.Is(err, ErrSessionNotFound) {
		return runView{P: p, SID: sid}
	}
	if err != nil {
		internal(c, err)
		return runView{written: true}
	}
	return runView{P: p, SID: sid, Kind: kind, Found: true}
}

func denyRunWrite(c *gin.Context, v runView) bool {
	if v.written {
		return true
	}
	if !v.Found || v.Kind == accessNone {
		writeEnvelope(c, http.StatusNotFound, codeRunMissing, "找不到这次运行", nil)
		return true
	}
	if v.Kind != accessOwner {
		writeEnvelope(c, http.StatusForbidden, codeNotOwner, "只有本人能修改", nil)
		return true
	}
	return false
}

func denyRunRead(c *gin.Context, v runView) bool {
	if v.written {
		return true
	}
	if !v.Found || v.Kind == accessNone {
		writeEnvelope(c, http.StatusNotFound, codeRunMissing, "找不到这次运行", nil)
		return true
	}
	return false
}

// stopRun（doc/17 §4 手动停止）：POST /api/runs/{runId}/stop。
//
//	200 {"code":0,"message":"","data":{"status":"stopped"}}
//	404 {"code":112002,"message":"找不到这次运行","data":null}
//	500 {"code":111901,"message":"服务暂时不可用，请再试一次","data":null}
//
// 竞态裁决在条件 UPDATE（Store.StopRun）：1 行 = 停止赢过正常收尾，翻 stopped；0 行 = 已是
// 终态，读回现值原样返回；行不存在 = 404。赢了之后查 worker 的取消注册表：本进程在跑这场
// run 就立刻 cancel（秒级断流）；打到别的副本时由跑着那侧的 watchdog 在下一个续约 tick
// （dev 10s）看见续约 CAS 0 再 cancel——本端点不为此另做通知。terminal 帧由 worker 收尾侧压
// （EndRun stopped 分支），这里不压帧、不动 Redis。
func (s *Server) stopRun(c *gin.Context) {
	runID := RunID(c.Param("runId"))
	if denyRunWrite(c, s.viewRun(c, runID)) {
		return
	}
	status, found, err := s.store.StopRun(c.Request.Context(), runID)
	if err != nil {
		internal(c, err)
		return
	}
	if !found {
		writeEnvelope(c, http.StatusNotFound, codeRunMissing, "找不到这次运行", nil)
		return
	}
	if status == "stopped" {
		// 用户停止永远最大：不看 epoch（doc/30 §7），注册表里是谁就 cancel 谁。
		if c, hit := s.worker.runCancels.Load(runID); hit {
			c.(*runCancel).cancel()
		}
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"status": status})
}

// resumeRun（doc/30 §8）= 崩溃后接管一场 run：POST /api/runs/{runId}/resume，无请求体。
//
//	202 {"code":0,"message":"","data":{"runId":"r_01","status":"running","resumeCount":1}}
//	404 {"code":112002,"message":"找不到这次运行","data":null}
//	409 {"code":112102,"message":"这场 run 已经定稿，没有可续的","data":null}   ← not_resumable
//	409 {"code":112103,"message":"这场 run 还活着，别抢","data":null}           ← run_alive
//	409 {"code":112104,"message":"续跑次数用完了，重新发一轮","data":null}       ← resume_exhausted
//	500 {"code":111901,"message":"服务暂时不可用，请再试一次","data":null}
//
// 顺序即正确性：claim（PG CAS，epoch+1，提交后才算接管）→ 重放账本 →
// 种 Redis 现场（令牌换代、runlive 重置为一条 resume 帧）→ 开 worker。
// claim 之后任何一步失败都 AbandonClaim 把租约改成过期：epoch 不回退（次数照样消耗），
// 但下一发立刻可接——不把会话卡在一个半接管状态里。
// 已删会话先经过 viewRun → SessionAccess。deleted_at 非空时 SessionAccess 回未找到，
// denyRunWrite 写成 404 run_not_found，ClaimResume 不会执行。
func (s *Server) resumeRun(c *gin.Context) {
	ctx := c.Request.Context()
	runID := RunID(c.Param("runId"))
	if denyRunWrite(c, s.viewRun(c, runID)) {
		return
	}
	claim, err := s.store.ClaimResume(ctx, runID, s.resumeMax)
	switch {
	case errors.Is(err, errRunNotFound):
		writeEnvelope(c, http.StatusNotFound, codeRunMissing, "找不到这次运行", nil)
		return
	case errors.Is(err, errNotResumable):
		writeEnvelope(c, http.StatusConflict, codeNotResumable, "这场 run 已经定稿，没有可续的", nil)
		return
	case errors.Is(err, errRunAlive):
		// 租约仍有效 = 原执行者可能只是慢了一拍，抢会造出双执行者（doc/30 §8.2）。
		writeEnvelope(c, http.StatusConflict, codeRunAlive, "这场 run 还活着，别抢", nil)
		return
	case errors.Is(err, errResumeExhausted):
		writeEnvelope(c, http.StatusConflict, codeResumeExhausted, "续跑次数用完了，重新发一轮", nil)
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
	// 第一条就是这条 resume 帧，正文 = 账本里已产出的 root assistant 拼接。
	// 续跑再追加 delta 后，定稿读 turn（账本重放 + 新枪），文本天然连续。
	frameJSON, err := json.Marshal(Frame{Kind: "resume", Text: replay.Seed})
	if err != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		internal(c, err)
		return
	}
	if err := s.store.seedResume(ctx, runID, claim.Epoch, string(frameJSON)); err != nil {
		_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
		internal(c, err)
		return
	}
	// 与 POST 同一条纪律：run 的生命周期不属于这个请求。
	// 续跑也要重新装配租户模型。装配失败就放弃这次认领，不拿进程里的平台密钥接着跑。
	bg := context.Background()
	if p, ok := principalFrom(c); ok {
		bg = withPrincipal(bg, p)
		if s.worker != nil && s.worker.models != nil {
			agentID := ""
			if sess, _, serr := s.store.SessionAccess(ctx, claim.SessionID, p); serr == nil {
				agentID = sess.AgentID
			}
			prof, aerr := s.assembleRun(ctx, p, agentID)
			if aerr != nil {
				_ = s.store.AbandonClaim(context.WithoutCancel(ctx), runID, claim.Epoch)
				writeEnvelope(c, aerr.Status, aerr.Code, aerr.Message, nil)
				return
			}
			bg = withRunProfile(bg, prof)
		}
	}
	go s.worker.RunResume(bg, claim.SessionID, runID, claim.Epoch, replay)
	writeEnvelope(c, http.StatusAccepted, codeOK, "", resumeResponse{RunID: runID, Status: "running", ResumeCount: int(claim.Epoch - 1)})
}

// getSession（19v6-01 §4）= 会话状态查询，reconcile 的**第一步**（refresh / 新 tab 都从它进）。
//
//	GET /api/sessions/s_01
//	200 {"code":0,"message":"","data":{"liveRunId":"r_1","runlogOffset":1,…}}
//	                              ← 有 run 正在跑；runlog 已有 1 条（刚投递的 user 消息）
//	200 {"code":0,"message":"","data":{"liveRunId":"","runlogOffset":2,…}}
//	                              ← run 已结束；助手全文已进 runlog
//	404 {"code":112001,"message":"找不到这场会话","data":null}
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
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	sess, _, err := s.store.SessionAccess(c.Request.Context(), SessionID(c.Param("id")), p)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
			return
		}
		internal(c, err)
		return
	}
	liveRunID, runlogOffset, lastStatus, lastRunID, exists, err := s.store.SessionStatus(c.Request.Context(), SessionID(c.Param("id")), p)
	if err != nil {
		internal(c, err)
		return
	}
	if !exists {
		writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", sessionResponse{
		LiveRunID: liveRunID, RunlogOffset: runlogOffset, LastRunStatus: lastStatus, LastRunID: lastRunID,
		WorkspaceID: sess.WorkspaceID,
	})
}

// getSessionSubagents 列出这场会话目录下的子代理档案。
// 权限与 getSession 同一道 SessionAccess。目录不存在时 subagents 是空数组。
//
//	GET /api/sessions/s_01/subagents
//	200 {"code":0,"message":"","data":{"subagents":[{"saId":"sa_…","agentName":"researcher","status":"complete","createdAt":"…","finishedAt":"…"}]}}
//	404 {"code":112001,"message":"找不到这场会话","data":null}
func (s *Server) getSessionSubagents(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	sid := SessionID(c.Param("id"))
	if _, _, err := s.store.SessionAccess(c.Request.Context(), sid, p); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
			return
		}
		log.Printf("GET /api/sessions/%s/subagents: %v", sid, err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
		return
	}
	dir, err := sessionDir(s.mediaRoot, p.TenantID, sid)
	if err != nil {
		log.Printf("GET /api/sessions/%s/subagents: %v", sid, err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
		return
	}
	items, err := listSubagentWires(filepath.Join(dir, ".tako", "subagents"))
	if err != nil {
		log.Printf("GET /api/sessions/%s/subagents: %v", sid, err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Subagents []subagentListItem `json:"subagents"`
	}{Subagents: items})
}

type sessionListItem struct {
	ID          SessionID `json:"id"`
	Title       string    `json:"title"`
	Visibility  string    `json:"visibility"`
	Owner       bool      `json:"owner"`
	CreatedAt   time.Time `json:"createdAt"`
	WorkspaceID *string   `json:"workspaceId"`
}

type sessionListData struct {
	Sessions []sessionListItem `json:"sessions"`
}

type visibilityRequest struct {
	Visibility string `json:"visibility"`
}

// listSessions 是新接口：默认只列本人的会话；?scope=tenant 列本租户里显式共享的会话。
// 创建者用 workspaceId 筛自己的项目时，结果含别人挂在上面的会话。详情和 log 仍按会话 visibility。
func (s *Server) listSessions(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	workspaceID := strings.TrimSpace(c.Query("workspaceId"))
	if workspaceID != "" {
		if _, err := s.store.VisibleWorkspace(c.Request.Context(), workspaceID, p.TenantID, p.UserID); err != nil {
			if errors.Is(err, ErrWorkspaceNotFound) {
				writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
				return
			}
			log.Printf("%s %s: %v", c.Request.Method, c.Request.URL.Path, err)
			writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
			return
		}
	}
	rows, err := s.store.ListSessions(c.Request.Context(), p, c.Query("scope"), workspaceID)
	if err != nil {
		log.Printf("%s %s: %v", c.Request.Method, c.Request.URL.Path, err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
		return
	}
	items := make([]sessionListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, sessionListItem{
			ID: row.ID, Title: row.Title, Visibility: row.Visibility,
			Owner: row.UserID == p.UserID, CreatedAt: row.CreatedAt, WorkspaceID: row.WorkspaceID,
		})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", sessionListData{Sessions: items})
}

type workspaceBindData struct {
	WorkspaceID *string `json:"workspaceId"`
}

// patchSessionWorkspace 把会话挂进项目，或用空串解绑。磁盘上已有的文件留在原地。
func (s *Server) patchSessionWorkspace(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	var req workspaceBindData
	if err := c.ShouldBindJSON(&req); err != nil || req.WorkspaceID == nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	wid := strings.TrimSpace(*req.WorkspaceID)
	if wid != "" {
		if _, err := s.store.VisibleWorkspace(c.Request.Context(), wid, p.TenantID, p.UserID); err != nil {
			if errors.Is(err, ErrWorkspaceNotFound) {
				writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
				return
			}
			log.Printf("PATCH /api/sessions/%s: %v", c.Param("id"), err)
			writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
			return
		}
	}
	err := s.store.SetSessionWorkspace(c.Request.Context(), SessionID(c.Param("id")), p, wid)
	switch {
	case errors.Is(err, ErrSessionNotFound):
		writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
	case errors.Is(err, ErrSessionForbidden):
		writeEnvelope(c, http.StatusForbidden, codeNotOwner, "只有本人能修改", nil)
	case errors.Is(err, ErrWorkspaceNotFound):
		writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
	case err != nil:
		log.Printf("PATCH /api/sessions/%s: %v", c.Param("id"), err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
	default:
		var out *string
		if wid != "" {
			out = &wid
		}
		writeEnvelope(c, http.StatusOK, codeOK, "", workspaceBindData{WorkspaceID: out})
	}
}

// patchSessionVisibility 只有 owner 能把会话改成 private 或 tenant。
func (s *Server) patchSessionVisibility(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	var req visibilityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	err := s.store.SetSessionVisibility(c.Request.Context(), SessionID(c.Param("id")), p, req.Visibility)
	switch {
	case errors.Is(err, ErrBadVisibility):
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
	case errors.Is(err, ErrSessionNotFound):
		writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
	case errors.Is(err, ErrSessionForbidden):
		writeEnvelope(c, http.StatusForbidden, codeNotOwner, "只有本人能修改", nil)
	case err != nil:
		log.Printf("%s %s: %v", c.Request.Method, c.Request.URL.Path, err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
	default:
		writeEnvelope(c, http.StatusOK, codeOK, "", visibilityRequest{Visibility: req.Visibility})
	}
}

// getRunStatus（doc/30 A1）= 单场 run 的对外状态，崩溃后「这场是死了还是还能续」的唯一读口。
//
//	GET /api/runs/r_01
//	200 {"code":0,"message":"","data":{"runId":"r_01","sessionId":"s_01","status":"running","resumeCount":0,"resumable":false}}
//	404 {"code":112002,"message":"找不到这次运行","data":null}
//
// status 五态里只有 interrupted 是派生值：runs.status 仍是 running、租约已过（PG now()），
// 读侧当场判「执行者已经不在了」，不等清扫（清扫只在下一发 POST 时发生）。
// resumeCount = lease_epoch - 1（每次 claim 续跑 +1）；resumable = interrupted && 次数未耗尽。
// 未知 run 与 session 端点同形回 404（这里与 frames 的 200 gone 不同：那个是 runlive TTL 的
// 生命周期，这里是 PG 行存在性——行在不在是确定的）。
func (s *Server) getRunStatus(c *gin.Context) {
	if denyRunRead(c, s.viewRun(c, RunID(c.Param("runId")))) {
		return
	}
	st, found, err := s.store.RunStatus(c.Request.Context(), RunID(c.Param("runId")), s.resumeMax)
	if err != nil {
		internal(c, err)
		return
	}
	if !found {
		writeEnvelope(c, http.StatusNotFound, codeRunMissing, "找不到这次运行", nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", runResponse{
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
//	200 {"code":0,"message":"","data":{
//	  "frames":[
//	    {"eventOffset":1,"kind":"start","messageId":"m_1"},
//	    {"eventOffset":2,"kind":"delta","text":"好的"},
//	    {"eventOffset":3,"kind":"delta","text":"，我来读"},
//	    {"eventOffset":4,"kind":"terminal","status":"complete"}
//	  ],"nextOffset":4,"status":"done"
//	}}
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
	v := s.viewRun(c, RunID(c.Param("runId")))
	if v.written {
		return
	}
	if !v.Found || v.Kind == accessNone {
		writeEnvelope(c, http.StatusOK, codeOK, "", framesResponse{Frames: []Frame{}, NextOffset: from, Status: "gone"})
		return
	}
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
	writeEnvelope(c, http.StatusOK, codeOK, "", framesResponse{Frames: frames, NextOffset: next, Status: status})
}

// sse 节奏两个常数（doc/31 §4/§5）：tail 是流 handler 读 runlive 的间隔（定时读共享 Redis
// ⇒ 任何副本可服务任何流，无进程内等待者）；heartbeat 是静默期注释帧的写出间隔（防 NAT/代理
// 掐闲连接——纯拉模式没有连接可掐，流模式没有它最长挂不过中间层的空闲超时）。
// 用 var 不用 const：单测调小验心跳，生产值不动。
var (
	sseTailInterval      = 150 * time.Millisecond
	sseHeartbeatInterval = 15 * time.Second
)

// getRunStream（doc/31 §4）= 第五个 API：同一帧面的第二种运输方式（text/event-stream）。
//
//	GET /api/runs/r_1/stream
//	id: 1
//	event: frame
//	data: {"eventOffset":1,"kind":"start","agent":"","model":"","messageId":"m_1"}
//
//	id: 2
//	event: frame
//	data: {"eventOffset":2,"kind":"delta","agent":"","model":"deepseek/deepseek-flash","text":"我查一下"}
//	…
//
// 与 frames 端点同真源（runlive LIST）、同存储原语（ReadFrames）、同帧形状——data 行就是帧的
// JSON 原文，kind 全量原样透传（随 frames 契约走，含 doc/30 的 resume），流上没有新形状。拉是正确性、流是优化：
// 四 API 零改动，收口对账仍以拉路为准；流不承诺可靠送达（连接断 = 事件丢），续传把手是
// Last-Event-ID——每帧 `id:` 就是 eventOffset，重连带 `Last-Event-ID: N` 从 offset > N 继续，
// 不是流另认游标，是同一游标的第二种读法。
//
// 循环：读帧 → 逐帧写+flush → 见 terminal 关连接（run 结束 = 流的自然终点）；没 terminal →
// sleep tail 再读；整个静默期每 15s 写一行 `: ping` 注释帧（EventSource 自动忽略）。
// runlive 键不存在（含「从未存在」与 TTL 过期，与 frames 的 200 gone 同语义、故意不区分）→
// 一条 `event: gone`（无 data 行）后关闭。本端点不回 404；建流后服务端内部错 = 直接断流
// （SSE 协议没有错误语义，消费方凭「断了且没收到 terminal」知道该重连）。
// 单批上限 W 不适用：W 是拉路「防超长回复撑爆单次响应」的概念，流逐条推、无批；但一批
// 读满 W 说明积压中，立即再读不睡 tail（追平存量）。
func (s *Server) getRunStream(c *gin.Context) {
	runID := RunID(c.Param("runId"))
	v := s.viewRun(c, runID)
	if v.written {
		return
	}
	from := lastEventID(c.GetHeader("Last-Event-ID"))
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	write := func(block string) bool {
		if _, err := w.WriteString(block); err != nil {
			return false // 客户端已断开，SSE 无错误语义，直接退出
		}
		w.Flush()
		return true
	}
	ctx := c.Request.Context()
	if !v.Found || v.Kind == accessNone {
		write("event: gone\n\n")
		return
	}
	lastWrite := time.Now()
	for {
		if time.Since(lastWrite) >= sseHeartbeatInterval {
			if !write(": ping\n\n") {
				return
			}
			lastWrite = time.Now()
		}
		frames, next, exists, err := s.store.ReadFrames(ctx, runID, from, s.w)
		if err != nil {
			return
		}
		if !exists {
			write("event: gone\n\n")
			return
		}
		for _, f := range frames {
			b, _ := json.Marshal(f)
			if !write(fmt.Sprintf("id: %d\nevent: frame\ndata: %s\n\n", f.EventOffset, b)) {
				return
			}
			lastWrite = time.Now()
			if f.Kind == "terminal" {
				return
			}
		}
		if len(frames) == s.w {
			continue
		}
		from = next
		select {
		case <-ctx.Done():
			return
		case <-time.After(sseTailInterval):
		}
	}
}

// lastEventID 解析 SSE 续传游标：与 queryOffset 同一宽容度——缺省/非法/负数 = 0（全量重放）。
func lastEventID(h string) int {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// getSessionLog（19v6-01 §3）= 拉 session 的**定稿历史**（PG `runlog_entries` 表）。
//
//	GET /api/sessions/s_01/log?runlogOffset=0
//	200 {"code":0,"message":"","data":{
//	  "rows":[
//	    {"runlogOffset":1,"runId":"r_1","role":"user","agent":"","model":"","messageId":"m_1","text":"读一下 doc/19"},
//	    {"runlogOffset":2,"runId":"r_1","role":"assistant","agent":"","model":"deepseek/deepseek-flash","text":"好的，我来读……"}
//	  ],"nextOffset":2
//	}}
//	404 {"code":112001,"message":"找不到这场会话","data":null}
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
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	from := queryOffset(c, "runlogOffset")
	if _, _, err := s.store.SessionAccess(c.Request.Context(), SessionID(c.Param("id")), p); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
			return
		}
		internal(c, err)
		return
	}
	rows, next, exists, err := s.store.ReadLog(c.Request.Context(), SessionID(c.Param("id")), from, s.w)
	if err != nil {
		internal(c, err)
		return
	}
	if !exists {
		writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", logResponse{Rows: rows, NextOffset: next})
}

// 字段序 = doc/29 §7.2 契约字节序（items 按 created_at DESC, id DESC）。
type memoryItemResponse struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	Topic           string    `json:"topic"`
	Content         string    `json:"content"`
	SourceSessionID SessionID `json:"sourceSessionId"`
	SourceRunID     RunID     `json:"sourceRunId"`
	SourceSeq       int64     `json:"sourceSeq"`
	CreatedAt       time.Time `json:"createdAt"`
}

// getMemoryItems（doc/29 §7.2①）= 记忆列表（管理面 + 抽取验收口）。
// kind 空 = 全部（含 episode）；limit 默认 50、上限 200（钳制在 ListMemoryItems）。
// 信封（doc/40 A7-5）：200 {"code":0,"message":"","data":{"items":[…]}}；内层字段序仍按 doc/29 §7.2。
func (s *Server) getMemoryItems(c *gin.Context) {
	if ok, _ := s.memoryContext(c); !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, err := s.store.ListMemoryItems(c.Request.Context(), c.Query("kind"), limit)
	if err != nil {
		internal(c, err)
		return
	}
	out := make([]memoryItemResponse, 0, len(items))
	for _, it := range items {
		out = append(out, memoryItemResponse{
			ID: it.ID, Kind: it.Kind, Topic: it.Topic, Content: it.Content,
			SourceSessionID: it.SourceSessionID, SourceRunID: it.SourceRunID,
			SourceSeq: it.SourceSeq, CreatedAt: it.CreatedAt,
		})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"items": out})
}

// 字段序 = doc/29 §7.2② 契约字节序。
type memoryJobResponse struct {
	ID        string    `json:"id"`
	RunID     RunID     `json:"runId"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"lastError"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// getMemoryJobs（doc/29 §7.2②）= 抽取任务状态（线一观测口）；runId 空 = 最近 20 条。
// 信封（doc/40 A7-5）：200 {"code":0,"message":"","data":{"jobs":[…]}}。
func (s *Server) getMemoryJobs(c *gin.Context) {
	if ok, _ := s.memoryContext(c); !ok {
		return
	}
	jobs, err := s.store.ListMemoryJobs(c.Request.Context(), RunID(c.Query("runId")))
	if err != nil {
		internal(c, err)
		return
	}
	out := make([]memoryJobResponse, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, memoryJobResponse{
			ID: j.ID, RunID: j.RunID, Status: j.Status, Attempts: j.Attempts,
			LastError: j.LastError, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
		})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"jobs": out})
}

// getMemoryDialogue（doc/29 §7.2③）＝ 索引状态（线二观测口）；runId 空 = 最近 20 条。
// passage 随行返回：B 组要验块内容，dev 观测口，块帽 4000 rune 可接受。
// 信封（doc/40 A7-5）：200 {"code":0,"message":"","data":{"chunks":[…]}}。
type dialogueChunkResponse struct {
	RunID     RunID     `json:"runId"`
	SessionID SessionID `json:"sessionId"`
	Runes     int       `json:"runes"`
	Truncated bool      `json:"truncated"`
	IndexedAt time.Time `json:"indexedAt"`
	Passage   string    `json:"passage"`
}

func (s *Server) getMemoryDialogue(c *gin.Context) {
	if ok, _ := s.memoryContext(c); !ok {
		return
	}
	chunks, err := s.store.ListDialogueChunks(c.Request.Context(), RunID(c.Query("runId")))
	if err != nil {
		internal(c, err)
		return
	}
	out := make([]dialogueChunkResponse, 0, len(chunks))
	for _, ch := range chunks {
		out = append(out, dialogueChunkResponse{
			RunID: ch.RunID, SessionID: ch.SessionID, Runes: ch.Runes,
			Truncated: ch.Truncated, IndexedAt: ch.IndexedAt, Passage: ch.Passage,
		})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"chunks": out})
}

// getMemoryRecall（doc/29 §7.2④）= 召回预览：与 Worker 的注入调用同一个 Store.Recall，
// 开关同一份——预览显示的就是模型实际会看到的东西。q 空 = 400。
// 信封（doc/40 A7-5）：200 {"code":0,"message":"","data":{block,memoryHits,dialogueHits,injectedRunes}}；
// q 空是 400 {"code":111001,"message":"请求格式不对","data":null}。
func (s *Server) getMemoryRecall(c *gin.Context) {
	if ok, _ := s.memoryContext(c); !ok {
		return
	}
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	res, err := s.store.Recall(c.Request.Context(), q)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{
		"block":         res.Block,
		"memoryHits":    res.MemoryHits,
		"dialogueHits":  res.DialogueHits,
		"injectedRunes": res.InjectedRunes,
	})
}

// getMedia（doc/32 A4）= 前端把占位行里的 path 换回字节流的唯一出口。
// 有登录身份时，path 必须是 workspaces/<wid>/...；先 Clean 再按清完的 wid 做 VisibleWorkspace。
// 没有身份时仍相对 mediaRoot（单测探针）：越界 400，缺文件 404。
// mime 按扩展名表（mediaMimeByExt，与 read_media_file 同表）；回流走 ServeContent（支持 Range）。
//
// **成功体是图片字节，不是 JSON —— 不套信封**（doc/40 §6）。套了就把 doc/32
// 「对话里只存 path、显示时才换回字节」推翻。只有错误分支换信封，且把旧的一个
// not_found 字符串拆成三种语义（doc/40 §4.3）：
//
//	200 <图片字节>（Content-Type 按扩展名）
//	400 {"code":111001,"message":"请求格式不对","data":{"reason":"缺 path"}}
//	400 {"code":111001,"message":"请求格式不对","data":{"reason":"路径越出 workspace"}}   ← 仅无身份的探针
//	404 {"code":114001,"message":"找不到这个项目","data":null}   ← 工作区看不见
//	404 {"code":114101,"message":"找不到这个文件","data":null}   ← 文件不存在／是目录
//	404 {"code":114102,"message":"媒体路径不对","data":null}   ← path 不是 workspaces/<wid>/… 格式
func (s *Server) getMedia(c *gin.Context) {
	p := c.Query("path")
	if p == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", gin.H{"reason": "缺 path"})
		return
	}
	root := s.mediaRoot
	rel := p
	if who, ok := principalFrom(c); ok && who.TenantID != "" {
		wid, inside, okPath := parseSessionMediaPath(p)
		if !okPath {
			writeEnvelope(c, http.StatusNotFound, codeMediaPathBad, "媒体路径不对", nil)
			return
		}
		if _, err := s.store.VisibleWorkspace(c.Request.Context(), wid, who.TenantID, who.UserID); err != nil {
			if errors.Is(err, ErrWorkspaceNotFound) {
				writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
				return
			}
			internal(c, err)
			return
		}
		dir, err := workspaceDir(s.mediaRoot, who.TenantID, wid)
		if err != nil {
			writeEnvelope(c, http.StatusNotFound, codeMediaMissing, "找不到这个文件", nil)
			return
		}
		root = dir
		rel = inside
	}
	full, err := resolveInWorkspace(root, rel)
	if err != nil {
		if _, ok := principalFrom(c); ok {
			writeEnvelope(c, http.StatusNotFound, codeMediaMissing, "找不到这个文件", nil)
			return
		}
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", gin.H{"reason": "路径越出 workspace"})
		return
	}
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		writeEnvelope(c, http.StatusNotFound, codeMediaMissing, "找不到这个文件", nil)
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

// internal 是存储 / ID 生成故障的统一出口：客户端拿 500 信封，原因只进服务端日志。
// doc/40 A7-1：全局 500 出口换成信封——全仓 76 处调用都走这里，一改全场生效，
// 免得出现「某些端点已换信封、某些还是旧体」的半迁移状态。
func internal(c *gin.Context, err error) {
	log.Printf("%s %s: %v", c.Request.Method, c.Request.URL.Path, err)
	writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
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
// 信封（doc/40 A7-5）：200 {"code":0,"message":"","data":{runId,epoch,rows}}；
// 未知 run 是 404 {"code":112002,"message":"找不到这次运行","data":null}。
func (s *Server) devJournal(c *gin.Context) {
	epoch, rows, found, err := s.store.JournalView(c.Request.Context(), RunID(c.Param("runId")))
	if err != nil {
		internal(c, err)
		return
	}
	if !found {
		writeEnvelope(c, http.StatusNotFound, codeRunMissing, "找不到这次运行", nil)
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
	writeEnvelope(c, http.StatusOK, codeOK, "", resp)
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
