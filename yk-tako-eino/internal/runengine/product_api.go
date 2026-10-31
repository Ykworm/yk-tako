package runengine

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func (s *Server) requirePrincipal(c *gin.Context) (principal, bool) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return principal{}, false
	}
	return p, true
}

// overQuota 是抢闸**之前**的两道预检：日 token 配额（真闸，读-判-写，本批不动）
// ＋ 租户并发满的**快速失败提示**。
//
// 并发那段的地位要说准（2026-10-23，doc/43）：**它不是闸**。闸在 Store.AcquireRun 里——
// 数活 run 与插 run 行在同一笔 PG 事务里，中间持一把所有副本共享的 DB 锁
// （lockTenantGate）。这里保留一次读，只为**早点拒**：满租户的请求不该走到 media 落盘、
// MCP 连接之后。它**只能拒、不能放**（数到 ≥ 上限就 429），所以它不构成超发路径。
func (s *Server) overQuota(c *gin.Context, p principal) bool {
	if p.TenantID == "" {
		return false
	}
	ctx := c.Request.Context()
	maxRuns, _, daily, _, err := s.store.TenantQuota(ctx, p.TenantID)
	if err != nil {
		internal(c, err)
		return true
	}
	// 提示用的读（不是闸的判定）：闸在 AcquireRun 的事务里，见上面的说明。
	live, err := s.store.CountLiveRuns(ctx, p.TenantID)
	if err != nil {
		internal(c, err)
		return true
	}
	if daily > 0 {
		used, uerr := s.store.DailyTokenUsage(ctx, p.TenantID, time.Now())
		if uerr != nil {
			internal(c, uerr)
			return true
		}
		if used >= daily {
			writeEnvelope(c, http.StatusTooManyRequests, codeQuota, "这个工作区的额度用完了", nil)
			return true
		}
	}
	// 提示：并发看起来满了 → 早退。**权威判定在 AcquireRun**（DB 锁下），这里只抢先拒。
	if live >= maxRuns {
		writeEnvelope(c, http.StatusTooManyRequests, codeQuota, "这个工作区的额度用完了", nil)
		return true
	}
	return false
}

func (s *Server) createTenant(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeEnvelope(c, http.StatusBadRequest, codeNameRequired, "请填写工作区名称", nil)
		return
	}
	tenant, member, err := s.store.CreateTeamTenant(c.Request.Context(), p.UserID, name)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", ginTenant(tenant, member.Role))
}

type workspaceBody struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"createdAt"`
	Visibility string    `json:"visibility"`
	Owner      bool      `json:"owner"`
	// IsDefault 标记这是「当前租户 × 这个用户」的默认项目。默认项目不可删
	//（doc/37 的 114005）——页面据此把「删除 / 重命名」藏起来，别让用户点一个必然失败的按钮。
	IsDefault bool `json:"isDefault"`
}

type workspaceData struct {
	Workspace workspaceBody `json:"workspace"`
}

// postWorkspaces 建一个项目。归属是 token 里的 active tenant 和当前用户，请求体只带名字。
func (s *Server) postWorkspaces(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	name, ok := workspaceNameFrom(c)
	if !ok {
		return
	}
	row, err := s.store.CreateWorkspace(c.Request.Context(), p.TenantID, p.UserID, name)
	if err != nil {
		log.Printf("POST /api/workspaces: %v", err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", workspaceData{Workspace: toWorkspaceBody(row, p.UserID)})
}

type workspaceListData struct {
	Workspaces []workspaceBody `json:"workspaces"`
}

// listWorkspaces 只列当前用户在 active tenant 里创建的项目。
// 2026-10-22（doc/42）：`?scope=tenant` 退役——跨成员共享项目取消，它现在与不传同形。
func (s *Server) listWorkspaces(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	rows, err := s.store.ListWorkspaces(c.Request.Context(), p.TenantID, p.UserID, c.Query("scope"))
	if err != nil {
		log.Printf("GET /api/workspaces: %v", err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
		return
	}
	items := make([]workspaceBody, 0, len(rows))
	for _, row := range rows {
		items = append(items, toWorkspaceBody(row, p.UserID))
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", workspaceListData{Workspaces: items})
}

// workspaceInternalDirs 是用户不看、也不列的内部目录。
//
//	.media —— 入口媒体的落盘处（doc/32）
//	.tako  —— 搜索截断落盘（doc/21 H3：<项目>/.tako/search/*.md）
//
// 子代理 wire 不在项目目录（它在会话目录 .tako/subagents/，doc/39），这里碰不到。
var workspaceInternalDirs = map[string]bool{".media": true, ".tako": true}

// fileEntry 是列目录的一行：目录只给名字与种类，文件额外给字节数与修改时间。
// size / modifiedAt 用指针 + omitempty：目录没有这两个字段（不是 0），空文件要有 size:0。
type fileEntry struct {
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Size       *int64     `json:"size,omitempty"`
	ModifiedAt *time.Time `json:"modifiedAt,omitempty"`
}

type workspaceFilesData struct {
	Path    string      `json:"path"`
	Entries []fileEntry `json:"entries"`
}

// listWorkspaceFiles 列项目目录下的直接子项（doc/48 B1）。
//
// 为什么要有这个端点：agent 写出的文件落在项目目录里，此前 UI 只能看见聊天里那行文字——
// 用户既看不到目录、也拿不到字节。字节出口 GET /api/media 今天就能读项目内文件
// （getMedia 的 root 就是项目目录），缺的只有「列目录」这一半。
//
// 两道锁，一道都不能少：
//   - 可见性先判（VisibleWorkspace）：看不见的项目回 404 114001，不泄露别人的项目存不存在。
//   - 路径再判（resolveInWorkspace）：与 agent 工具层同一把锁（含符号链接逃逸检查），
//     绝对路径与越界一律 400 111002「路径不对」。
func (s *Server) listWorkspaceFiles(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	wid := strings.TrimSpace(c.Param("id"))
	if _, err := s.store.VisibleWorkspace(c.Request.Context(), wid, p.TenantID, p.UserID); err != nil {
		if errors.Is(err, ErrWorkspaceNotFound) {
			writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
			return
		}
		internal(c, err)
		return
	}
	root, err := workspaceDir(s.mediaRoot, p.TenantID, wid)
	if err != nil {
		writeEnvelope(c, http.StatusNotFound, codeMediaMissing, "找不到这个文件", nil)
		return
	}
	rel := strings.TrimSpace(c.Query("path"))
	if rel == "" {
		rel = "."
	}
	full, err := resolveInWorkspace(root, rel)
	if err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "路径不对", nil)
		return
	}
	des, err := os.ReadDir(full)
	if err != nil {
		// 目录不存在 / 不是目录 / 读不动，一律 404「找不到这个文件」。
		writeEnvelope(c, http.StatusNotFound, codeMediaMissing, "找不到这个文件", nil)
		return
	}
	items := make([]fileEntry, 0, len(des))
	for _, de := range des {
		name := de.Name()
		if workspaceInternalDirs[name] {
			continue
		}
		// 符号链接不列：它可能指向项目外，列出来等于泄露「那边有什么」，
		// 而点进去 resolveInWorkspace 也会挡住——列了是坏体验，不列是安全。
		if de.Type()&os.ModeSymlink != 0 {
			continue
		}
		if de.IsDir() {
			items = append(items, fileEntry{Name: name, Kind: "dir"})
			continue
		}
		if !de.Type().IsRegular() {
			continue // 套接字、设备文件之类不是文件，不列
		}
		info, err := de.Info()
		if err != nil {
			continue // 拿不到元信息就跳过，不让一条坏条目毁掉整份清单
		}
		size := info.Size()
		mod := info.ModTime()
		items = append(items, fileEntry{Name: name, Kind: "file", Size: &size, ModifiedAt: &mod})
	}
	// 目录在前，同类按名升序（os.ReadDir 本来就按名升序，跨类型要再排一次）。
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind == "dir"
		}
		return items[i].Name < items[j].Name
	})
	writeEnvelope(c, http.StatusOK, codeOK, "", workspaceFilesData{
		Path:    filepath.ToSlash(filepath.Clean(rel)),
		Entries: items,
	})
}

// patchWorkspace 改名。不是创建者，或这行不存在，都回 404，不透露别人有没有这个项目。
func (s *Server) patchWorkspace(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	name, ok := workspaceNameFrom(c)
	if !ok {
		return
	}
	row, err := s.store.RenameWorkspace(c.Request.Context(), p.TenantID, p.UserID, c.Param("id"), name)
	if errors.Is(err, ErrWorkspaceNotFound) {
		writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
		return
	}
	if err != nil {
		log.Printf("PATCH /api/workspaces/%s: %v", c.Param("id"), err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
		return
	}
	s.publishEvent(c.Request.Context(), p.TenantID, eventWorkspaceRenamed, row.ID)
	writeEnvelope(c, http.StatusOK, codeOK, "", workspaceData{Workspace: toWorkspaceBody(row, p.UserID)})
}

// patchWorkspaceVisibility 已退役（2026-10-22，doc/42 形态收窄）：跨成员共享项目取消，
// 项目可见性恒为 private，没有「改成 tenant」这件事了。
//
// **端点留着但恒回 404**（与「目标不存在」同形，code 114001）——不直接摘掉路由：摘掉会让
// gin's 默认 404 接手，那是一段 HTML/裸 JSON，前端的信封拆包器接不住（doc/40 的拦截器
// 靠 code 分支）。留着恒 404 → 客户端拿到的仍是合规信封。
func (s *Server) patchWorkspaceVisibility(c *gin.Context) {
	writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
}

// deleteWorkspace 删创建者自己的项目，并软删挂在上面的会话。
// 有活 run 则 409，项目行、会话行和目录都留着。默认工作区仍不能删。
// 成功是先提交事务，再删项目目录和各会话目录。目录删失败只记日志，响应仍是 deleted。
func (s *Server) deleteWorkspace(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	id := c.Param("id")
	sessionIDs, err := s.store.DeleteWorkspaceCascade(c.Request.Context(), p.TenantID, p.UserID, id)
	switch {
	case errors.Is(err, ErrWorkspaceNotFound):
		writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
	case errors.Is(err, ErrWorkspaceDefault):
		writeEnvelope(c, http.StatusConflict, codeWorkspaceDefault, "默认工作区不能删除", nil)
	case errors.Is(err, ErrWorkspaceBusy):
		writeEnvelope(c, http.StatusConflict, codeWorkspaceBusyLive, "项目里有正在运行的会话，等它跑完再删", nil)
	case err != nil:
		log.Printf("DELETE /api/workspaces/%s: %v", id, err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
	default:
		if dir, dirErr := workspaceDir(s.mediaRoot, p.TenantID, id); dirErr != nil {
			log.Printf("DELETE /api/workspaces/%s: %v", id, dirErr)
		} else if rmErr := os.RemoveAll(dir); rmErr != nil {
			log.Printf("DELETE /api/workspaces/%s: %v", id, rmErr)
		}
		for _, sid := range sessionIDs {
			sdir, serr := sessionDir(s.mediaRoot, p.TenantID, sid)
			if serr != nil {
				log.Printf("DELETE /api/workspaces/%s session %s: %v", id, sid, serr)
				continue
			}
			if rmErr := os.RemoveAll(sdir); rmErr != nil {
				log.Printf("DELETE /api/workspaces/%s session %s: %v", id, sid, rmErr)
			}
		}
		s.publishEvent(c.Request.Context(), p.TenantID, eventWorkspaceDeleted, id)
		writeEnvelope(c, http.StatusOK, codeOK, "", struct {
			Status string `json:"status"`
		}{Status: "deleted"})
	}
}

// deleteSession 软删属主自己的一场会话。看不见走 404（112001），看得见但不是本人走 111204。
// 有活 run 则 409，行和目录都留着。成功先提交，再只删会话目录下的 .tako 与 .media。
// 产出文件和项目目录不动。目录删失败只记日志，响应仍是 deleted。
func (s *Server) deleteSession(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	id := SessionID(c.Param("id"))
	err := s.store.DeleteSession(c.Request.Context(), p.TenantID, p.UserID, id)
	switch {
	case errors.Is(err, ErrSessionNotFound):
		writeEnvelope(c, http.StatusNotFound, codeSessionMissing, "找不到这场会话", nil)
	case errors.Is(err, ErrSessionForbidden):
		writeEnvelope(c, http.StatusForbidden, codeNotOwner, "只有本人能修改", nil)
	case errors.Is(err, ErrSessionBusy):
		writeEnvelope(c, http.StatusConflict, codeSessionBusy, "这场会话正在运行，等它跑完再删", nil)
	case err != nil:
		log.Printf("DELETE /api/sessions/%s: %v", id, err)
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
	default:
		removeSessionSystemArchives(s.mediaRoot, p.TenantID, id)
		s.publishEvent(c.Request.Context(), p.TenantID, eventSessionDeleted, string(id))
		writeEnvelope(c, http.StatusOK, codeOK, "", struct {
			Status string `json:"status"`
		}{Status: "deleted"})
	}
}

// removeSessionSystemArchives 删这场会话目录里的系统档案。产出留在会话目录里。
// .tako 是搜索截断和子代理 wire，.media 是旧媒体兜底。目录不存在算成功。
func removeSessionSystemArchives(root, tenantID string, id SessionID) {
	dir, err := sessionDir(root, tenantID, id)
	if err != nil {
		log.Printf("DELETE session %s: %v", id, err)
		return
	}
	for _, name := range []string{".tako", ".media"} {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			log.Printf("DELETE session %s %s: %v", id, name, err)
		}
	}
}

func workspaceNameFrom(c *gin.Context) (string, bool) {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return "", false
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeEnvelope(c, http.StatusBadRequest, codeWorkspaceNameRequired, "请填写项目名称", nil)
		return "", false
	}
	// 名字有边：侧栏/卡片靠 CSS 截断只是把问题藏到布局里，换个地方（导出、通知、日志）就溢出来。
	// 按字符（rune）算，不是字节——中文项目名按 100 个汉字收。
	if utf8.RuneCountInString(name) > workspaceNameMax {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "项目名最多 100 个字符", nil)
		return "", false
	}
	return name, true
}

const workspaceNameMax = 100

func toWorkspaceBody(row Workspace, userID string) workspaceBody {
	return workspaceBody{
		ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt,
		Visibility: row.Visibility, Owner: row.UserID == userID,
		IsDefault: row.IsDefault,
	}
}

func ginTenant(t Tenant, role string) any {
	return struct {
		Tenant struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Plan   string `json:"plan"`
		} `json:"tenant"`
		Membership struct {
			TenantID string `json:"tenantId"`
			Role     string `json:"role"`
		} `json:"membership"`
	}{
		Tenant: struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Plan   string `json:"plan"`
		}{ID: t.ID, Kind: t.Kind, Name: t.Name, Status: t.Status, Plan: t.Plan},
		Membership: struct {
			TenantID string `json:"tenantId"`
			Role     string `json:"role"`
		}{TenantID: t.ID, Role: role},
	}
}

func (s *Server) listTenants(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	rows, err := s.store.ListUserTenants(c.Request.Context(), p.UserID)
	if err != nil {
		internal(c, err)
		return
	}
	type item struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	out := make([]item, 0, len(rows))
	for _, row := range rows {
		out = append(out, item{ID: row.ID, Name: row.Name, Kind: row.Kind, Role: row.Role, Status: row.Status})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Tenants []item `json:"tenants"`
	}{Tenants: out})
}

func (s *Server) getTenant(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	t, err := s.store.GetTenant(c.Request.Context(), p.TenantID)
	if err != nil {
		writeEnvelope(c, http.StatusNotFound, codeNotMember, "你不是这个工作区的成员", nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Tenant struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Plan   string `json:"plan"`
		} `json:"tenant"`
	}{Tenant: struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Plan   string `json:"plan"`
	}{ID: t.ID, Kind: t.Kind, Name: t.Name, Status: t.Status, Plan: t.Plan}})
}

func (s *Server) putTenant(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeEnvelope(c, http.StatusBadRequest, codeNameRequired, "请填写工作区名称", nil)
		return
	}
	if err := s.store.RenameTenant(c.Request.Context(), p.TenantID, name); err != nil {
		internal(c, err)
		return
	}
	s.getTenant(c)
}

func (s *Server) deleteTenant(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	err := s.store.ArchiveTenant(c.Request.Context(), p.TenantID)
	if errors.Is(err, ErrPersonalTenant) {
		writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	// 租户归档：事件流键无 TTL（MAXLEN 只限长度），不删就成残留。
	s.store.deleteEvents(c.Request.Context(), p.TenantID)
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Status string `json:"status"`
	}{Status: "archived"})
}

type noteBody struct {
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	Workspace string `json:"workspace"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	Quote     string `json:"quote"`
}

func notesData(rows []Note) any {
	out := make([]noteBody, 0, len(rows))
	for _, row := range rows {
		out = append(out, noteBody{ID: row.ID, Scope: row.Scope, Workspace: row.Workspace, Kind: row.Kind, Content: row.Content, Quote: row.Quote})
	}
	return struct {
		Notes []noteBody `json:"notes"`
	}{Notes: out}
}

func (s *Server) listDevNotes(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "user"})
	rows, err := s.store.ListScopedNotes(ctx, c.Query("workspace"))
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", notesData(rows))
}

func (s *Server) postDevNote(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Workspace string `json:"workspace"`
		Kind      string `json:"kind"`
		Content   string `json:"content"`
		Quote     string `json:"quote"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	if req.Kind == "" {
		req.Kind = noteKindAgent
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "user"})
	res, err := s.store.writeNote(ctx, req.Workspace, "", 0, noteWrite{Action: "add", Kind: req.Kind, Content: req.Content, Quote: req.Quote})
	if err != nil {
		internal(c, err)
		return
	}
	if res.fail != "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "笔记内容不符合要求", nil)
		return
	}
	rows, err := s.store.ListScopedNotes(ctx, req.Workspace)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", notesData(rows))
}

func (s *Server) listTenantNotes(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, Scope: "tenant"})
	rows, err := s.store.ListScopedNotes(ctx, c.Query("workspace"))
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", notesData(rows))
}

func (s *Server) postTenantNote(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Workspace string `json:"workspace"`
		Kind      string `json:"kind"`
		Content   string `json:"content"`
		Quote     string `json:"quote"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	if req.Workspace == "" {
		req.Workspace = "team"
	}
	if req.Kind == "" {
		req.Kind = noteKindAgent
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	res, err := s.store.writeNote(ctx, req.Workspace, "", 0, noteWrite{Action: "add", Kind: req.Kind, Content: req.Content, Quote: req.Quote})
	if err != nil {
		internal(c, err)
		return
	}
	if res.fail != "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "笔记内容不符合要求", nil)
		return
	}
	rows, err := s.store.ListScopedNotes(ctx, req.Workspace)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", notesData(rows))
}

func (s *Server) postDevMemory(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
		Topic   string `json:"topic"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Content) == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "user"})
	id, err := s.store.SeedMemoryFact(ctx, req.Kind, req.Topic, req.Content)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", struct {
		ID string `json:"id"`
	}{ID: id})
}

func (s *Server) devWorkspaceDir(c *gin.Context, p principal) (string, SessionID, bool) {
	sid, err := s.store.EnsureDevSession(c.Request.Context(), p)
	if err != nil {
		internal(c, err)
		return "", "", false
	}
	dir, err := sessionWorkspace(s.mediaRoot, p.TenantID, sid)
	if err != nil {
		internal(c, err)
		return "", "", false
	}
	return dir, sid, true
}

func (s *Server) postDevFile(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	dir, sid, ok := s.devWorkspaceDir(c, p)
	if !ok {
		return
	}
	full, err := resolveInWorkspace(dir, req.Path)
	if err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		internal(c, err)
		return
	}
	if err := os.WriteFile(full, []byte(req.Content), 0o644); err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", struct {
		Path      string    `json:"path"`
		SessionID SessionID `json:"sessionId"`
	}{Path: req.Path, SessionID: sid})
}

func (s *Server) getDevFile(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	path := c.Query("path")
	if path == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	dir, sid, ok := s.devWorkspaceDir(c, p)
	if !ok {
		return
	}
	full, err := resolveInWorkspace(dir, path)
	if err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
		return
	}
	b, err := os.ReadFile(full)
	if err != nil {
		writeEnvelope(c, http.StatusNotFound, codeAgentMissing, "找不到这个文件", nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Path      string    `json:"path"`
		Content   string    `json:"content"`
		SessionID SessionID `json:"sessionId"`
	}{Path: path, Content: string(b), SessionID: sid})
}

// devFileFingerprint 是同文件并发写底座的观测口（脚手架：验收通过后转只读诊断口）。
// 打出目标文件此刻的指纹（mtime + size + sha256；>64MB 只出 mtime+size 并标 weak）。
//
//	POST /api/dev/file-fingerprint {"path":"a.md"}
//	200 {"code":0,"message":"","data":{"path":"a.md","mtime":…,"size":…,"sha256":"…","hasHash":true,"weak":false}}
func (s *Server) devFileFingerprint(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	dir, _, ok := s.devWorkspaceDir(c, p)
	if !ok {
		return
	}
	full, err := resolveInWorkspace(dir, req.Path)
	if err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "路径不对", nil)
		return
	}
	fp, err := fingerprintOf(full)
	if err != nil {
		writeEnvelope(c, http.StatusNotFound, codeAgentMissing, "找不到这个文件", nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Path    string    `json:"path"`
		Mtime   time.Time `json:"mtime"`
		Size    int64     `json:"size"`
		SHA256  string    `json:"sha256"`
		HasHash bool      `json:"hasHash"`
		Weak    bool      `json:"weak"`
	}{Path: req.Path, Mtime: fp.Mtime, Size: fp.Size, SHA256: fp.SHA256, HasHash: fp.SHA256 != "", Weak: fp.weak()})
}

// devFileLockCheck 是写锁的观测口（脚手架）：拿一把路径锁、持锁 holdMs、报实际等了多久。
// 两个并发打它 → 第二个的 waitedMs ≥ 前一个的 holdMs = **串行**（不是并行）。
//
//	POST /api/dev/file-lock-check {"path":"a.md","holdMs":1500}
//	200 {"code":0,"message":"","data":{"acquired":true,"waitedMs":1502}}
func (s *Server) devFileLockCheck(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Path   string `json:"path"`
		HoldMs int    `json:"holdMs"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	dir, _, ok := s.devWorkspaceDir(c, p)
	if !ok {
		return
	}
	full, err := resolveInWorkspace(dir, req.Path)
	if err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "路径不对", nil)
		return
	}
	// 锁目录与工具层同一处：租户根下的 .tako/locks（工具层走 ctx 的 workspace ref）。
	lockDir := filepath.Join(s.mediaRoot, "tenants", p.TenantID, ".tako", "locks")
	start := time.Now()
	unlock, err := lockPaths(lockDir, []string{full})
	if err != nil {
		if errors.Is(err, errLockTimeout) {
			writeEnvelope(c, http.StatusConflict, codeWorkspaceBusyLive, "这个文件正被另一个 agent 写，稍后重试", nil)
			return
		}
		internal(c, err)
		return
	}
	waited := time.Since(start)
	if req.HoldMs > 0 {
		time.Sleep(time.Duration(req.HoldMs) * time.Millisecond)
	}
	unlock()
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Acquired bool   `json:"acquired"`
		WaitedMs int64  `json:"waitedMs"`
		HoldMs   int    `json:"holdMs"`
		LockPath string `json:"lockPath"`
	}{Acquired: true, WaitedMs: waited.Milliseconds(), HoldMs: req.HoldMs, LockPath: lockPathFor(lockDir, full)})
}

func (s *Server) postDevBash(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Cmd     string `json:"cmd"`
		Command string `json:"command"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	command := req.Cmd
	if command == "" {
		command = req.Command
	}
	if strings.TrimSpace(command) == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
		return
	}
	dir, sid, ok := s.devWorkspaceDir(c, p)
	if !ok {
		return
	}
	out, err := runBash(c.Request.Context(), dir, command, bashDevTimeout, true)
	if err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeForbidden, err.Error(), nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Output    string    `json:"output"`
		SessionID SessionID `json:"sessionId"`
		Sandboxed bool      `json:"sandboxed"`
	}{Output: out, SessionID: sid, Sandboxed: sandboxAvailable()})
}

func (s *Server) memoryContext(c *gin.Context) (bool, bool) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return false, true
	}
	sc := dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "user"}
	if c.Query("scope") == "tenant" {
		if p.Role != "owner" && p.Role != "admin" && !p.SystemAdmin {
			writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
			return false, true
		}
		sc.Scope = "tenant"
	}
	if tid := strings.TrimSpace(c.Query("tenantId")); tid != "" {
		if !p.SystemAdmin {
			writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
			return false, true
		}
		sc.TenantID = tid
		sc.Scope = "tenant"
	}
	c.Request = c.Request.WithContext(withScope(c.Request.Context(), sc))
	return true, false
}

func (s *Server) listAgents(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	kind := c.Query("kind")
	if kind != "root" && kind != "subagent" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "kind 只能是 root 或 subagent", nil)
		return
	}
	rows, err := s.store.ListEffectiveAgents(c.Request.Context(), p.TenantID, p.UserID, kind)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Agents []effectiveAgentBody `json:"agents"`
	}{Agents: effectiveBodies(rows)})
}

type effectiveAgentBody struct {
	ID          string      `json:"id"`
	DefaultID   string      `json:"defaultId"`
	Source      string      `json:"source"`
	Overridden  bool        `json:"overridden"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Spec        string      `json:"spec"`
	ModelRef    string      `json:"modelRef"`
	ToolPolicy  ToolPolicy  `json:"toolPolicy"`
	SkillPolicy SkillPolicy `json:"skillPolicy"`
	Visibility  string      `json:"visibility"`
}

func effectiveBodies(rows []EffectiveAgent) []effectiveAgentBody {
	out := make([]effectiveAgentBody, 0, len(rows))
	for _, row := range rows {
		out = append(out, effectiveAgentBody{
			ID: row.ID, DefaultID: row.DefaultID, Source: row.Source, Overridden: row.Overridden,
			Name: row.Name, Description: row.Description, Spec: row.Spec, ModelRef: row.ModelRef,
			ToolPolicy: parseToolPolicy(row.ToolPolicy), SkillPolicy: parseSkillPolicy(row.SkillPolicy),
			Visibility: row.Visibility,
		})
	}
	return out
}

type agentBody struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Spec       string `json:"spec"`
	ModelRef   string `json:"modelRef"`
	Visibility string `json:"visibility"`
	Builtin    bool   `json:"builtin"`
	CreatedBy  string `json:"createdBy"`
}

func agentBodies(rows []TenantAgent) []agentBody {
	out := make([]agentBody, 0, len(rows))
	for _, row := range rows {
		out = append(out, agentBody{ID: row.ID, Name: row.Name, Spec: row.Spec, ModelRef: row.ModelRef, Visibility: row.Visibility, Builtin: row.IsBuiltin, CreatedBy: row.CreatedBy})
	}
	return out
}

type agentWrite struct {
	Name        string       `json:"name"`
	Kind        string       `json:"kind"`
	Description *string      `json:"description"`
	Spec        *string      `json:"spec"`
	ModelRef    *string      `json:"modelRef"`
	ToolPolicy  *ToolPolicy  `json:"toolPolicy"`
	SkillPolicy *SkillPolicy `json:"skillPolicy"`
	Visibility  *string      `json:"visibility"`
}

func validAgentKind(kind string) bool {
	return kind == "root" || kind == "subagent" || kind == "both"
}

func writeAgentFields(c *gin.Context, row *TenantAgent, req agentWrite) bool {
	if err := applyAgentWrite(row, req); err != nil {
		msg := "工具策略不合法"
		switch {
		case errors.Is(err, ErrBadVisibility):
			msg = "助手的名字或 kind 不对"
		case errors.Is(err, errBadAgentName):
			msg = agentNameRuleMessage
		}
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, msg, nil)
		return false
	}
	return true
}

func applyAgentWrite(row *TenantAgent, req agentWrite) error {
	if name := strings.TrimSpace(req.Name); name != "" {
		if !validAgentName(name) {
			return errBadAgentName
		}
		row.Name = name
	}
	if req.Kind != "" {
		if !validAgentKind(req.Kind) {
			return ErrBadVisibility
		}
		row.Kind = req.Kind
	}
	if req.Description != nil {
		row.Description = *req.Description
	}
	if req.Spec != nil {
		row.Spec = *req.Spec
	}
	if req.ModelRef != nil {
		row.ModelRef = strings.TrimSpace(*req.ModelRef)
	}
	if req.Visibility != nil {
		if *req.Visibility != "private" && *req.Visibility != "tenant" {
			return ErrBadVisibility
		}
		row.Visibility = *req.Visibility
	}
	if req.ToolPolicy != nil {
		raw, err := policyJSON(*req.ToolPolicy)
		if err != nil {
			return err
		}
		row.ToolPolicy = raw
	}
	if req.SkillPolicy != nil {
		raw, err := skillPolicyJSON(*req.SkillPolicy)
		if err != nil {
			return err
		}
		row.SkillPolicy = raw
	}
	return nil
}

// skillScopeDeclared 报告一行的技能作用域是否真的收窄了（allow/exclude）。空串与坏值都是 inherit。
func skillScopeDeclared(raw string) bool {
	mode := parseSkillPolicy(raw).Mode
	return mode == "allow" || mode == "exclude"
}

// rejectSubagentSkillScope 是「普通子代理写了技能作用域」的统一出口（doc/52 E1）：
// 子代理没有技能目录，这两个字段对它们无意义——静默接受会让人以为配了就有目录。
// kind 在写入口已知（请求或原行），所以这道判落在落库之前，不让坏配置进表。
func rejectSubagentSkillScope(c *gin.Context, kind, raw string) bool {
	if kind != "subagent" || !skillScopeDeclared(raw) {
		return false
	}
	writeEnvelope(c, http.StatusBadRequest, codeBadInput, "skills:／exclude_skills: 只对主助手有效（普通子代理没有技能目录）", nil)
	return true
}

func (s *Server) writeAgent(c *gin.Context, status int, row TenantAgent) {
	body := effectiveBodies([]EffectiveAgent{effectiveFromTenant(row)})[0]
	writeEnvelope(c, status, codeOK, "", struct {
		Agent effectiveAgentBody `json:"agent"`
	}{Agent: body})
}

func (s *Server) rejectDefaultAgent(c *gin.Context, id string) bool {
	_, err := s.store.AgentDefaultByID(c.Request.Context(), id)
	if err == nil {
		writeEnvelope(c, http.StatusForbidden, codeForbidden, "默认助手不能直接改", nil)
		return true
	}
	if !errors.Is(err, ErrAgentNotFound) {
		internal(c, err)
		return true
	}
	return false
}

func (s *Server) createAgent(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req agentWrite
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || !validAgentKind(req.Kind) {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "助手的名字或 kind 不对", nil)
		return
	}
	if !validAgentName(req.Name) {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, agentNameRuleMessage, nil)
		return
	}
	if _, err := s.store.TenantAgentByName(c.Request.Context(), p.TenantID, req.Name); err == nil {
		writeEnvelope(c, http.StatusConflict, codeAgentTaken, "这个名字已经有一份配置", nil)
		return
	} else if !errors.Is(err, ErrAgentNotFound) {
		internal(c, err)
		return
	}
	def, err := s.store.AgentDefaultByName(c.Request.Context(), req.Name)
	var row TenantAgent
	if err == nil {
		row = TenantAgent{
			DefaultID: def.ID, Name: def.Name, Kind: def.Kind, Description: def.Description,
			Spec: def.Spec, ModelRef: def.ModelRef, ToolPolicy: def.ToolPolicy,
			Visibility: "tenant", Source: "override",
		}
	} else if errors.Is(err, ErrAgentNotFound) {
		row = TenantAgent{Name: req.Name, Kind: req.Kind, Source: "custom", Visibility: "private", ToolPolicy: `{"mode":"inherit","names":[]}`}
	} else {
		internal(c, err)
		return
	}
	if !writeAgentFields(c, &row, req) {
		return
	}
	if rejectSubagentSkillScope(c, row.Kind, row.SkillPolicy) {
		return
	}
	row.TenantID = p.TenantID
	row.CreatedBy = p.UserID
	saved, err := s.store.CreateAgent(c.Request.Context(), row)
	if errors.Is(err, ErrAgentExists) {
		writeEnvelope(c, http.StatusConflict, codeAgentTaken, "这个名字已经有一份配置", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	s.writeAgent(c, http.StatusCreated, saved)
}

func (s *Server) updateAgent(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	if s.rejectDefaultAgent(c, c.Param("agent_id")) {
		return
	}
	row, err := s.store.VisibleAgent(c.Request.Context(), p.TenantID, p.UserID, c.Param("agent_id"))
	if errors.Is(err, ErrAgentNotFound) {
		writeEnvelope(c, http.StatusNotFound, codeAgentMissing, "找不到这个助手", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	if p.Role != "owner" && p.Role != "admin" && row.CreatedBy != p.UserID {
		writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
		return
	}
	var req agentWrite
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	if !writeAgentFields(c, &row, req) {
		return
	}
	if rejectSubagentSkillScope(c, row.Kind, row.SkillPolicy) {
		return
	}
	if err := s.store.UpdateAgent(c.Request.Context(), row); errors.Is(err, ErrAgentExists) {
		writeEnvelope(c, http.StatusConflict, codeAgentTaken, "这个名字已经有一份配置", nil)
		return
	} else if err != nil {
		internal(c, err)
		return
	}
	s.writeAgent(c, http.StatusOK, row)
}

func (s *Server) deleteAgent(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	if s.rejectDefaultAgent(c, c.Param("agent_id")) {
		return
	}
	row, err := s.store.VisibleAgent(c.Request.Context(), p.TenantID, p.UserID, c.Param("agent_id"))
	if errors.Is(err, ErrAgentNotFound) {
		writeEnvelope(c, http.StatusNotFound, codeAgentMissing, "找不到这个助手", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	if row.IsBuiltin {
		writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
		return
	}
	if p.Role != "owner" && p.Role != "admin" && row.CreatedBy != p.UserID {
		writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
		return
	}
	if err := s.store.DeleteAgent(c.Request.Context(), p.TenantID, row.ID); err != nil {
		internal(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) importAgents(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Files []struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		} `json:"files"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	if len(req.Files) == 0 {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "请上传至少一份规格", nil)
		return
	}
	rows := make([]TenantAgent, 0, len(req.Files))
	seen := map[string]struct{}{}
	for _, file := range req.Files {
		spec, err := parseAgentSpec([]byte(file.Content))
		if err != nil {
			msg := "有一份规格格式不对"
			if errors.Is(err, errBadAgentName) {
				msg = agentNameRuleMessage
			}
			writeEnvelope(c, http.StatusBadRequest, codeBadInput, msg, nil)
			return
		}
		if _, ok := seen[spec.Name]; ok {
			writeEnvelope(c, http.StatusBadRequest, codeBadInput, "同一次上传里有重名的助手", nil)
			return
		}
		seen[spec.Name] = struct{}{}
		if _, err := s.store.TenantAgentByName(c.Request.Context(), p.TenantID, spec.Name); err == nil {
			writeEnvelope(c, http.StatusConflict, codeAgentTaken, "这个名字已经有一份配置", nil)
			return
		} else if !errors.Is(err, ErrAgentNotFound) {
			internal(c, err)
			return
		}
		pol, err := policyJSON(policyFromSpec(spec))
		if err != nil {
			writeEnvelope(c, http.StatusBadRequest, codeBadInput, "有一份规格格式不对", nil)
			return
		}
		def, err := s.store.AgentDefaultByName(c.Request.Context(), spec.Name)
		var row TenantAgent
		if err == nil {
			row = TenantAgent{
				DefaultID: def.ID, Name: def.Name, Kind: def.Kind, Visibility: "tenant", Source: "override",
			}
		} else if errors.Is(err, ErrAgentNotFound) {
			// 与手工新建同默认（仅自己）：两条入口在同一个页面上，导入的人不该因为「文件里没写可见性」
			// 就把助手发给全团队。要共享，导入后显式改成团队可见。
			row = TenantAgent{Name: spec.Name, Kind: "subagent", Visibility: "private", Source: "custom"}
		} else {
			internal(c, err)
			return
		}
		pSkill, perr := skillPolicyJSON(skillPolicyFromSpec(spec))
		if perr != nil {
			writeEnvelope(c, http.StatusBadRequest, codeBadInput, "有一份规格格式不对", nil)
			return
		}
		if rejectSubagentSkillScope(c, row.Kind, pSkill) {
			return
		}
		row.Description = spec.Description
		row.Spec = spec.Prompt
		row.ModelRef = spec.Model
		row.ToolPolicy = pol
		row.SkillPolicy = pSkill
		row.TenantID = p.TenantID
		row.CreatedBy = p.UserID
		rows = append(rows, row)
	}
	saved, err := s.store.InsertAgents(c.Request.Context(), rows)
	if errors.Is(err, ErrAgentExists) {
		writeEnvelope(c, http.StatusConflict, codeAgentTaken, "这个名字已经有一份配置", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	out := make([]effectiveAgentBody, 0, len(saved))
	for _, row := range saved {
		out = append(out, effectiveBodies([]EffectiveAgent{effectiveFromTenant(row)})[0])
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", struct {
		Agents []effectiveAgentBody `json:"agents"`
	}{Agents: out})
}

func (s *Server) tenantUsage(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	tokens, live, maxRuns, maxSub, daily, storage, err := s.store.TenantUsage(c.Request.Context(), p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Tokens            int64 `json:"tokens"`
		LiveRuns          int   `json:"liveRuns"`
		MaxConcurrentRuns int   `json:"maxConcurrentRuns"`
		MaxSubagents      int   `json:"maxSubagents"`
		DailyTokens       int64 `json:"dailyTokens"`
		MaxStorageBytes   int64 `json:"maxStorageBytes"`
	}{Tokens: tokens, LiveRuns: live, MaxConcurrentRuns: maxRuns, MaxSubagents: maxSub, DailyTokens: daily, MaxStorageBytes: storage})
}

func (s *Server) getRuntimeConfig(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	cfg, err := s.store.GetRuntimeConfig(c.Request.Context(), p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Config runtimeConfig `json:"config"`
	}{Config: maskSecrets(cfg)})
}

func (s *Server) putRuntimeConfig(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var cfg runtimeConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	if err := validateRuntime(cfg); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "MCP 配置不合法", nil)
		return
	}
	if err := s.store.PutRuntimeConfig(c.Request.Context(), p.TenantID, p.UserID, cfg); err != nil {
		internal(c, err)
		return
	}
	s.getRuntimeConfig(c)
}

func (s *Server) createInvitation(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	email, okEmail := normalizeEmail(req.Email)
	if !okEmail {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
		return
	}
	if req.Role == "owner" {
		writeEnvelope(c, http.StatusBadRequest, codeInviteOwner, "不能邀请所有者", nil)
		return
	}
	if !assignableRole(req.Role) {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
		return
	}
	inv, token, err := s.store.CreateInvitation(c.Request.Context(), p.TenantID, p.UserID, email, req.Role)
	if errors.Is(err, ErrAlreadyMember) {
		writeEnvelope(c, http.StatusConflict, codeInviteMember, "已经是这个工作区的成员", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", struct {
		Invitation  inviteBody `json:"invitation"`
		InviteToken string     `json:"inviteToken"`
	}{Invitation: inviteBodyOf(inv), InviteToken: token})
}

type inviteBody struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expiresAt"`
}

func inviteBodyOf(inv TenantInvitation) inviteBody {
	return inviteBody{ID: inv.ID, Email: inv.Email, Role: inv.Role, Status: inv.Status, ExpiresAt: inv.ExpiresAt.UTC().Format(time.RFC3339)}
}

func (s *Server) listInvitations(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	rows, err := s.store.ListTenantInvitations(c.Request.Context(), p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	out := make([]inviteBody, 0, len(rows))
	for _, row := range rows {
		out = append(out, inviteBodyOf(row))
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Invitations []inviteBody `json:"invitations"`
	}{Invitations: out})
}

func (s *Server) revokeInvitation(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	err := s.store.RevokeInvitation(c.Request.Context(), p.TenantID, c.Param("inv_id"))
	if errors.Is(err, ErrInviteInvalid) {
		writeEnvelope(c, http.StatusNotFound, codeInviteInvalid, "邀请无效或已过期", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) myInvitations(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	rows, err := s.store.ListMyInvitations(c.Request.Context(), p.Email)
	if err != nil {
		internal(c, err)
		return
	}
	// 预览接口带团队名，这份「我的邀请」原来没有。没有名字和 tenantId，
	// 切换器那一行写不出团队，接受之后也不知道 switch-tenant 该切哪一个。
	out := make([]myInviteBody, 0, len(rows))
	for _, row := range rows {
		tenant, err := s.store.GetTenant(c.Request.Context(), row.TenantID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			internal(c, err)
			return
		}
		item := inviteBodyOf(row)
		out = append(out, myInviteBody{
			ID: item.ID, Email: item.Email, Role: item.Role, Status: item.Status, ExpiresAt: item.ExpiresAt,
			TenantID: tenant.ID, TenantName: tenant.Name,
		})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Invitations []myInviteBody `json:"invitations"`
	}{Invitations: out})
}

type myInviteBody struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Role       string `json:"role"`
	Status     string `json:"status"`
	ExpiresAt  string `json:"expiresAt"`
	TenantID   string `json:"tenantId"`
	TenantName string `json:"tenantName"`
}

func (s *Server) acceptInvitation(c *gin.Context) {
	s.answerInvitation(c, true)
}

func (s *Server) declineInvitation(c *gin.Context) {
	s.answerInvitation(c, false)
}

func (s *Server) answerInvitation(c *gin.Context, accept bool) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var err error
	if accept {
		err = s.store.AcceptInvitation(c.Request.Context(), p.UserID, p.Email, c.Param("id"))
	} else {
		err = s.store.DeclineInvitation(c.Request.Context(), p.Email, c.Param("id"))
	}
	if errors.Is(err, ErrInviteInvalid) {
		writeEnvelope(c, http.StatusNotFound, codeInviteInvalid, "邀请无效或已过期", nil)
		return
	}
	if errors.Is(err, ErrAlreadyMember) {
		writeEnvelope(c, http.StatusConflict, codeInviteMember, "已经是这个工作区的成员", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Status string `json:"status"`
	}{Status: map[bool]string{true: "accepted", false: "declined"}[accept]})
}

func (s *Server) getModelResolve(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	if s.worker == nil || s.worker.models == nil {
		writeEnvelope(c, http.StatusConflict, codeModelUnavailable, "这个工作区的模型还不可用", nil)
		return
	}
	cfg, raw, err := s.store.RuntimeConfigRecord(c.Request.Context(), p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	secrets, err := unsealSecretMap(cfg.Secrets)
	if err != nil {
		log.Printf("model-resolve unseal tenant=%s: %v", p.TenantID, err)
		internal(c, err)
		return
	}
	ref := strings.TrimSpace(c.Query("ref"))
	if ref == "" {
		ref = effectiveModelRef("", cfg.ModelRef, s.worker.models.RootRef())
	}
	built, cacheKey, err := s.worker.models.Resolve(c.Request.Context(), p.TenantID, ref, raw, secrets)
	if err != nil {
		log.Printf("model-resolve tenant=%s ref=%s: %v", p.TenantID, ref, err)
		writeEnvelope(c, http.StatusConflict, codeModelUnavailable, "这个工作区的模型还不可用", nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Ref            string `json:"ref"`
		Provider       string `json:"provider"`
		Model          string `json:"model"`
		KeyFingerprint string `json:"keyFingerprint"`
		CacheKey       string `json:"cacheKey"`
	}{Ref: built.Ref, Provider: built.Provider, Model: built.ModelID, KeyFingerprint: built.KeyFP, CacheKey: cacheKey})
}

func (s *Server) lookupInvitation(c *gin.Context) {
	inv, tenant, err := s.store.LookupInvitation(c.Request.Context(), c.Param("token"))
	if errors.Is(err, ErrInviteInvalid) {
		writeEnvelope(c, http.StatusNotFound, codeInviteInvalid, "邀请无效或已过期", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		TenantName   string `json:"tenantName"`
		Role         string `json:"role"`
		Status       string `json:"status"`
		InvitationID string `json:"invitationId"`
		Email        string `json:"email"`
		ExpiresAt    string `json:"expiresAt"`
	}{
		TenantName:   tenant.Name,
		Role:         inv.Role,
		Status:       inv.Status,
		InvitationID: inv.ID,
		Email:        inv.Email,
		ExpiresAt:    inv.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) registerByInvite(c *gin.Context) {
	var req struct {
		Token    string `json:"token"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	email, ok := normalizeEmail(req.Email)
	if !ok || len([]rune(req.Password)) < 8 || strings.TrimSpace(req.Token) == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
		return
	}
	inv, _, err := s.store.LookupInvitation(c.Request.Context(), req.Token)
	if err != nil || inv.Email != email || inv.Status != "pending" || time.Now().After(inv.ExpiresAt) {
		writeEnvelope(c, http.StatusNotFound, codeInviteInvalid, "邀请无效或已过期", nil)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		internal(c, err)
		return
	}
	user, err := s.store.RegisterByInvite(c.Request.Context(), email, string(hash), inv.ID)
	if errors.Is(err, ErrEmailTaken) {
		writeEnvelope(c, http.StatusConflict, codeEmailTaken, "这个邮箱已经注册", nil)
		return
	}
	if errors.Is(err, ErrInviteInvalid) {
		writeEnvelope(c, http.StatusNotFound, codeInviteInvalid, "邀请无效或已过期", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", struct {
		UserID string `json:"userId"`
		Status string `json:"status"`
	}{UserID: user.ID, Status: "accepted"})
}
