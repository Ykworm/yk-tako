package runengine

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type memberItem struct {
	UserID string `json:"userId"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type membersData struct {
	Members []memberItem `json:"members"`
}

type memberData struct {
	Member memberItem `json:"member"`
}

type tenantAdminItem struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Plan      string    `json:"plan"`
	CreatedAt time.Time `json:"createdAt"`
}

type userAdminItem struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	SystemAdmin bool      `json:"systemAdmin"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"createdAt"`
}

type auditItem struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenantId"`
	ActorUserID string    `json:"actorUserId"`
	ActorRole   string    `json:"actorRole"`
	Action      string    `json:"action"`
	TargetType  string    `json:"targetType"`
	TargetID    string    `json:"targetId"`
	Outcome     string    `json:"outcome"`
	Details     string    `json:"details"`
	CreatedAt   time.Time `json:"createdAt"`
}

type auditData struct {
	Rows []auditItem `json:"rows"`
}

func toMemberItem(row MemberRow) memberItem {
	return memberItem{UserID: row.UserID, Email: row.Email, Role: row.Role, Status: row.Status}
}

func (s *Server) listMembers(c *gin.Context) {
	rows, err := s.store.ListMembers(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	items := make([]memberItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, toMemberItem(row))
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", membersData{Members: items})
}

func (s *Server) addMember(c *gin.Context) {
	p, _ := principalFrom(c)
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	email, ok := normalizeEmail(req.Email)
	if !ok {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
		return
	}
	row, err := s.store.AddMemberByEmail(c.Request.Context(), c.Param("id"), email, req.Role, p)
	if err != nil {
		s.writeMemberErr(c, err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", memberData{Member: toMemberItem(row)})
}

func (s *Server) updateMember(c *gin.Context) {
	p, _ := principalFrom(c)
	var req struct {
		Role string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	row, err := s.store.UpdateMemberRole(c.Request.Context(), c.Param("id"), c.Param("user_id"), req.Role, p)
	if err != nil {
		s.writeMemberErr(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", memberData{Member: toMemberItem(row)})
}

func (s *Server) removeMember(c *gin.Context) {
	p, _ := principalFrom(c)
	err := s.store.RemoveMember(c.Request.Context(), c.Param("id"), c.Param("user_id"), p)
	if err != nil {
		s.writeMemberErr(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", map[string]bool{"removed": true})
}

func (s *Server) leaveTenant(c *gin.Context) {
	p, _ := principalFrom(c)
	err := s.store.LeaveTenant(c.Request.Context(), c.Param("id"), p.UserID, p)
	if err != nil {
		s.writeMemberErr(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", map[string]bool{"left": true})
}

func (s *Server) transferOwner(c *gin.Context) {
	p, _ := principalFrom(c)
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	err := s.store.TransferOwner(c.Request.Context(), c.Param("id"), req.UserID, p)
	if err != nil {
		s.writeMemberErr(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", map[string]string{"owner": req.UserID})
}

func (s *Server) writeMemberErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrBadRole):
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
	case errors.Is(err, ErrAlreadyMember), errors.Is(err, ErrEmailTaken):
		writeEnvelope(c, http.StatusConflict, codeEmailTaken, "这个邮箱已经注册", nil)
	case errors.Is(err, ErrLastOwner):
		writeEnvelope(c, http.StatusConflict, codeLastOwner, "至少要留一个所有者", nil)
	case errors.Is(err, ErrNotMember):
		writeEnvelope(c, http.StatusNotFound, codeNotMember, "你不是这个工作区的成员", nil)
	default:
		s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
	}
}

func (s *Server) listAdminTenants(c *gin.Context) {
	rows, err := s.store.ListAllTenants(c.Request.Context())
	if err != nil {
		s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	items := make([]tenantAdminItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, tenantAdminItem{ID: row.ID, Kind: row.Kind, Name: row.Name, Status: row.Status, Plan: row.Plan, CreatedAt: row.CreatedAt})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Tenants []tenantAdminItem `json:"tenants"`
	}{Tenants: items})
}

func (s *Server) listAdminUsers(c *gin.Context) {
	rows, err := s.store.ListUsers(c.Request.Context())
	if err != nil {
		s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	items := make([]userAdminItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, userAdminItem{ID: row.ID, Email: row.Email, SystemAdmin: row.IsSystemAdmin, Disabled: row.Disabled, CreatedAt: row.CreatedAt})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Users []userAdminItem `json:"users"`
	}{Users: items})
}

func (s *Server) createAdminUser(c *gin.Context) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	email, ok := normalizeEmail(req.Email)
	if !ok || len([]rune(req.Password)) < 8 {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "邮箱或密码不符合要求", nil)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	user, tenant, member, err := s.store.Signup(c.Request.Context(), email, string(hash))
	if errors.Is(err, ErrEmailTaken) {
		writeEnvelope(c, http.StatusConflict, codeEmailTaken, "这个邮箱已经注册", nil)
		return
	}
	if err != nil {
		s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", struct {
		User   userBody   `json:"user"`
		Tenant tenantBody `json:"tenant"`
		Member memberBody `json:"membership"`
	}{
		User:   userBody{ID: user.ID, Email: user.Email},
		Tenant: tenantBody{ID: tenant.ID, Kind: tenant.Kind, Name: tenant.Name},
		Member: memberBody{TenantID: member.TenantID, Role: member.Role},
	})
}

func (s *Server) promoteAdmin(c *gin.Context) {
	s.setAdminFlag(c, true)
}

func (s *Server) revokeAdmin(c *gin.Context) {
	s.setAdminFlag(c, false)
}

func (s *Server) setAdminFlag(c *gin.Context, on bool) {
	p, _ := principalFrom(c)
	var req struct {
		UserID string `json:"userId"`
		Email  string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	userID := req.UserID
	if userID == "" {
		email, ok := normalizeEmail(req.Email)
		if !ok {
			writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
			return
		}
		user, err := s.store.GetUserByEmail(c.Request.Context(), email)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeEnvelope(c, http.StatusNotFound, codeNotMember, "你不是这个工作区的成员", nil)
			return
		}
		if err != nil {
			s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
			return
		}
		userID = user.ID
	}
	err := s.store.SetSystemAdmin(c.Request.Context(), userID, on, p)
	switch {
	case errors.Is(err, ErrLastAdmin):
		writeEnvelope(c, http.StatusConflict, codeLastAdmin, "至少要留一个平台管理员", nil)
	case err != nil:
		s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
	default:
		writeEnvelope(c, http.StatusOK, codeOK, "", struct {
			UserID string `json:"userId"`
			Admin  bool   `json:"systemAdmin"`
		}{UserID: userID, Admin: on})
	}
}

func (s *Server) tenantAudit(c *gin.Context) {
	rows, err := s.store.ListAudit(c.Request.Context(), c.Param("id"), false, 50)
	if err != nil {
		s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", auditData{Rows: toAuditItems(rows)})
}

func (s *Server) systemAudit(c *gin.Context) {
	rows, err := s.store.ListAudit(c.Request.Context(), "", true, 50)
	if err != nil {
		s.auth.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", auditData{Rows: toAuditItems(rows)})
}

func toAuditItems(rows []AuditLog) []auditItem {
	items := make([]auditItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, auditItem{
			ID: row.ID, TenantID: row.TenantID, ActorUserID: row.ActorUserID, ActorRole: row.ActorRole,
			Action: row.Action, TargetType: row.TargetType, TargetID: row.TargetID,
			Outcome: row.Outcome, Details: row.Details, CreatedAt: row.CreatedAt,
		})
	}
	return items
}
