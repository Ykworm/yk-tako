package runengine

import (
	"crypto/rand"
	"errors"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/oklog/ulid/v2"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// Auth 是身份面（doc/28 A0）。成功和失败都走 AGENTS.md 的 {code,message,data} 信封。
type Auth struct {
	store      *Store
	dev        bool
	issuer     *tokenIssuer
	refreshTTL time.Duration
}

type tokenIssuer struct {
	secret []byte
	ttl    time.Duration
}

type principal struct {
	UserID      string
	Email       string
	TenantID    string
	Role        string
	SystemAdmin bool
}

type envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type userBody struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type tenantBody struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type memberBody struct {
	TenantID string `json:"tenantId"`
	Role     string `json:"role"`
}

type membershipItem struct {
	TenantID   string `json:"tenantId"`
	TenantName string `json:"tenantName"`
	Role       string `json:"role"`
}

type signupData struct {
	User         userBody   `json:"user"`
	Tenant       tenantBody `json:"tenant"`
	Membership   memberBody `json:"membership"`
	AccessToken  string     `json:"accessToken"`
	TokenType    string     `json:"tokenType"`
	ExpiresIn    int        `json:"expiresIn"`
	RefreshToken string     `json:"refreshToken"`
}

type pairData struct {
	AccessToken  string `json:"accessToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int    `json:"expiresIn"`
	RefreshToken string `json:"refreshToken"`
}

type sessionData struct {
	AccessToken  string           `json:"accessToken"`
	TokenType    string           `json:"tokenType"`
	ExpiresIn    int              `json:"expiresIn"`
	RefreshToken string           `json:"refreshToken"`
	User         userBody         `json:"user"`
	ActiveTenant tenantBody       `json:"activeTenant"`
	Memberships  []membershipItem `json:"memberships"`
}

type meData struct {
	UserID      string           `json:"userId"`
	Email       string           `json:"email"`
	TenantID    string           `json:"tenantId"`
	Role        string           `json:"role"`
	SystemAdmin bool             `json:"systemAdmin"`
	Memberships []membershipItem `json:"memberships"`
}

type devTenantData struct {
	Tenant     tenantBody `json:"tenant"`
	Membership memberBody `json:"membership"`
}

const (
	codeOK = 0
	// 码段口径（doc/40 §4）：前两位是模块——11 身份与参数，12 会话与运行，13 配额，
	// 14 项目与工作区。1110xx 参数，1111xx 登录与 token，1112xx 权限，1114xx 冲突，
	// 1119xx 这一模块的内部失败；1120xx 资源缺失，1121xx 会话与运行的冲突；
	// 1130xx 配额与准入；1140xx 项目，1141xx 项目里的文件。
	codeBadJSON          = 111001
	codeBadInput         = 111002
	codeNameRequired     = 111003
	// 111004：入口图片不合法（mime 白名单外／base64 解不开／超 5MB／超 4 张）。
	// 原因文字进 data.reason，不进 message（doc/40 §3 规则 3）。
	codeBadMedia         = 111004
	// 111005：mcp.json 读不出来。旧的 400 直接把 Go 原始错误串当 body 吐出来，
	// 那是机器细节；现在固定 message，细节进 data.reason。
	codeBadMcpConf       = 111005
	codeBadCredentials   = 111101
	codeMissingToken     = 111102
	codeInvalidToken     = 111103
	codeTokenExpired     = 111104
	codeBadRefresh       = 111105
	codeNotMember        = 111201
	codeForbidden        = 111203
	codeNotOwner         = 111204
	codeEmailTaken       = 111401
	codeNoTenant         = 111402
	codeLastOwner        = 111403
	codeLastAdmin        = 111404
	codeInviteInvalid    = 111405
	codeInviteMember     = 111406
	codeInviteOwner      = 111407
	codeModelUnavailable = 111408
	codeUnknownTool      = 111409
	codeMcpIllegal       = 111410
	codeMcpDown          = 111411
	codeAgentTaken       = 111412
	codeSessionMissing   = 112001
	// 112002：run 不可见。doc/28 §5.4 早已定码，这里补上代码里的常量。
	codeRunMissing = 112002
	codeAgentMissing = 112003
	// 112101：这场会话已有活 run，发消息撞闸。
	// ⚠️ 与 codeSessionBusy（114007）不是一回事：114007 是「删除会话时这场会话有活 run」
	// （doc/37 D3），语义不同，码不能撞。
	codeSessionRunning = 112101
	// 112102：这场 run 已定稿，没有可续的。前端据此不显示续跑按钮。
	codeNotResumable = 112102
	// 112103：租约仍有效，原执行者可能只是慢了一拍——不是失败，转去重建帧面。
	codeRunAlive = 112103
	// 112104：续跑次数用尽，提示重发。
	codeResumeExhausted = 112104
	codeQuota = 113001
	// 113003：单条消息超准入线。limit 与 tokens 进 data，message 只放人话。
	codeMessageTooLarge = 113003
	codeWorkspaceMissing = 114001
	// 114002 退役。删除项目不再因为「还有会话」拒绝。码位留着，没有任何路径返回它。
	codeWorkspaceNotEmpty     = 114002
	codeWorkspaceNameRequired = 114003
	// codeWorkspaceBusy（114004「项目正在另一场会话里运行」）已退役：2026-10-22 项目闸
	// 随跨成员共享项目一起取消（doc/42）。同名项目并发不再被拒——改由文件级防护兜。
	codeWorkspaceDefault      = 114005
	codeWorkspaceBusyLive     = 114006
	codeSessionBusy           = 114007
	// 114101：media 的文件真的不存在（旧的一个 not_found 字符串拆出来的四种语义之一）。
	codeMediaMissing = 114101
	// 114102：media 的 path 不是 workspaces/<wid>/… 格式，或越出工作区。
	codeMediaPathBad = 114102
	codeInternal     = 111901
)

var emailPattern = regexp.MustCompile(`^[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}$`)

func writeEnvelope(c *gin.Context, status, code int, message string, data any) {
	c.JSON(status, envelope{Code: code, Message: message, Data: data})
}

func (a *Auth) fail(c *gin.Context, status, code int, message string, err error) {
	if err != nil {
		log.Printf("%s %s: %v", c.Request.Method, c.Request.URL.Path, err)
	}
	writeEnvelope(c, status, code, message, nil)
}

// NewAuth 读签发配置。密钥短于 32 字节、TTL 非法，都在这里失败，调用方启动即退。
func NewAuth(store *Store, dev bool) (*Auth, error) {
	secret := strings.TrimSpace(os.Getenv("TAKO_JWT_SECRET"))
	if len(secret) < 32 {
		return nil, errors.New("TAKO_JWT_SECRET 长度不足 32 字节（未设置算 0）")
	}
	ttl, err := envDuration("TAKO_ACCESS_TTL", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	refresh, err := envDuration("TAKO_REFRESH_TTL", 30*24*time.Hour)
	if err != nil {
		return nil, err
	}
	return &Auth{
		store:      store,
		dev:        dev,
		issuer:     &tokenIssuer{secret: []byte(secret), ttl: ttl},
		refreshTTL: refresh,
	}, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, errors.New(key + "=" + raw + " 必须是正时长（如 720h、30d）")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, errors.New(key + "=" + raw + " 必须是正时长（如 15m、2s、30d）")
	}
	return d, nil
}

func (a *Auth) Signup(c *gin.Context) {
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
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	user, tenant, member, err := a.store.Signup(c.Request.Context(), email, string(hash))
	if errors.Is(err, ErrEmailTaken) {
		writeEnvelope(c, http.StatusConflict, codeEmailTaken, "这个邮箱已经注册", nil)
		return
	}
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	access, exp, refresh, err := a.issuePair(c, user, tenant.ID)
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", signupData{
		User:         userBody{ID: user.ID, Email: user.Email},
		Tenant:       tenantBody{ID: tenant.ID, Kind: tenant.Kind, Name: tenant.Name},
		Membership:   memberBody{TenantID: member.TenantID, Role: member.Role},
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    exp,
		RefreshToken: refresh,
	})
}

func (a *Auth) Login(c *gin.Context) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	email, _ := normalizeEmail(req.Email)
	user, err := a.store.GetUserByEmail(c.Request.Context(), email)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeEnvelope(c, http.StatusUnauthorized, codeBadCredentials, "邮箱或密码不正确", nil)
		return
	}
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil || user.Disabled {
		writeEnvelope(c, http.StatusUnauthorized, codeBadCredentials, "邮箱或密码不正确", nil)
		return
	}
	data, access, exp, refresh, err := a.sessionFor(c, user)
	if errors.Is(err, ErrNotMember) {
		writeEnvelope(c, http.StatusConflict, codeNoTenant, "请先选择工作区", nil)
		return
	}
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	data.AccessToken = access
	data.ExpiresIn = exp
	data.RefreshToken = refresh
	writeEnvelope(c, http.StatusOK, codeOK, "", data)
}

func (a *Auth) Refresh(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.RefreshToken) == "" {
		if err != nil {
			writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
			return
		}
		writeEnvelope(c, http.StatusUnauthorized, codeBadRefresh, "登录已失效，请重新登录", nil)
		return
	}
	userID, raw, err := a.store.RotateRefreshToken(c.Request.Context(), hashRefresh(req.RefreshToken), a.refreshTTL)
	if errors.Is(err, ErrRefreshInvalid) {
		writeEnvelope(c, http.StatusUnauthorized, codeBadRefresh, "登录已失效，请重新登录", nil)
		return
	}
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	user, err := a.store.GetUserByID(c.Request.Context(), userID)
	if err != nil || user.Disabled {
		writeEnvelope(c, http.StatusUnauthorized, codeBadRefresh, "登录已失效，请重新登录", nil)
		return
	}
	tenant, _, _, err := a.store.ActiveTenantFor(c.Request.Context(), user)
	if err != nil {
		writeEnvelope(c, http.StatusUnauthorized, codeBadRefresh, "登录已失效，请重新登录", nil)
		return
	}
	access, exp, err := a.issuer.issueAccessToken(user.ID, tenant.ID, user.TokenVersion)
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", pairData{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: exp, RefreshToken: raw,
	})
}

func (a *Auth) Logout(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	if err := a.store.RevokeUserRefreshTokens(c.Request.Context(), p.UserID); err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *Auth) Me(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	rows, err := a.store.ListMemberships(c.Request.Context(), p.UserID)
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", meData{
		UserID: p.UserID, Email: p.Email, TenantID: p.TenantID, Role: p.Role,
		SystemAdmin: p.SystemAdmin, Memberships: toItems(rows),
	})
}

func (a *Auth) SwitchTenant(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	var req struct {
		TenantID     string `json:"tenantId"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	if strings.TrimSpace(req.TenantID) == "" || strings.TrimSpace(req.RefreshToken) == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	err := a.store.SwitchDefaultTenant(c.Request.Context(), p.UserID, req.TenantID, hashRefresh(req.RefreshToken))
	if errors.Is(err, ErrNotMember) {
		writeEnvelope(c, http.StatusForbidden, codeNotMember, "你不是这个工作区的成员", nil)
		return
	}
	if errors.Is(err, ErrRefreshInvalid) {
		writeEnvelope(c, http.StatusUnauthorized, codeBadRefresh, "登录已失效，请重新登录", nil)
		return
	}
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	user, err := a.store.GetUserByID(c.Request.Context(), p.UserID)
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	data, access, exp, refresh, err := a.sessionFor(c, user)
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	data.AccessToken = access
	data.ExpiresIn = exp
	data.RefreshToken = refresh
	writeEnvelope(c, http.StatusOK, codeOK, "", data)
}

func (a *Auth) CreateDevTenant(c *gin.Context) {
	if !a.dev {
		c.Status(http.StatusNotFound)
		return
	}
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
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
	tenant, member, err := a.store.CreateTeamTenant(c.Request.Context(), p.UserID, name)
	if err != nil {
		a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
		return
	}
	writeEnvelope(c, http.StatusCreated, codeOK, "", devTenantData{
		Tenant:     tenantBody{ID: tenant.ID, Kind: tenant.Kind, Name: tenant.Name},
		Membership: memberBody{TenantID: member.TenantID, Role: member.Role},
	})
}

func (a *Auth) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearer(c.GetHeader("Authorization"))
		if !ok {
			writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
			c.Abort()
			return
		}
		sub, tid, ver, err := a.issuer.parseAccessToken(raw)
		if err != nil {
			if errors.Is(err, jwt.ErrTokenExpired) {
				writeEnvelope(c, http.StatusUnauthorized, codeTokenExpired, "登录已过期，请重新登录", nil)
			} else {
				writeEnvelope(c, http.StatusUnauthorized, codeInvalidToken, "登录已失效，请重新登录", nil)
			}
			c.Abort()
			return
		}
		user, err := a.store.GetUserByID(c.Request.Context(), sub)
		if err != nil || user.Disabled || user.TokenVersion != ver || tid == "" {
			writeEnvelope(c, http.StatusUnauthorized, codeInvalidToken, "登录已失效，请重新登录", nil)
			c.Abort()
			return
		}
		okMember, err := a.store.HasActiveMembership(c.Request.Context(), user.ID, tid)
		if err != nil {
			a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
			c.Abort()
			return
		}
		if !okMember {
			writeEnvelope(c, http.StatusForbidden, codeNotMember, "你不是这个工作区的成员", nil)
			c.Abort()
			return
		}
		role := ""
		rows, err := a.store.ListMemberships(c.Request.Context(), user.ID)
		if err != nil {
			a.fail(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", err)
			c.Abort()
			return
		}
		for _, row := range rows {
			if row.TenantID == tid {
				role = row.Role
				break
			}
		}
		c.Set("tako.principal", principal{
			UserID: user.ID, Email: user.Email, TenantID: tid, Role: role, SystemAdmin: user.IsSystemAdmin,
		})
		c.Next()
	}
}

func (a *Auth) sessionFor(c *gin.Context, user User) (sessionData, string, int, string, error) {
	tenant, _, rows, err := a.store.ActiveTenantFor(c.Request.Context(), user)
	if err != nil {
		return sessionData{}, "", 0, "", err
	}
	access, exp, refresh, err := a.issuePair(c, user, tenant.ID)
	if err != nil {
		return sessionData{}, "", 0, "", err
	}
	return sessionData{
		TokenType:    "Bearer",
		User:         userBody{ID: user.ID, Email: user.Email},
		ActiveTenant: tenantBody{ID: tenant.ID, Kind: tenant.Kind, Name: tenant.Name},
		Memberships:  toItems(rows),
	}, access, exp, refresh, nil
}

func (a *Auth) issuePair(c *gin.Context, user User, tenantID string) (string, int, string, error) {
	access, exp, err := a.issuer.issueAccessToken(user.ID, tenantID, user.TokenVersion)
	if err != nil {
		return "", 0, "", err
	}
	refresh, err := a.store.IssueRefreshToken(c.Request.Context(), user.ID, tenantID, a.refreshTTL)
	if err != nil {
		return "", 0, "", err
	}
	return access, exp, refresh, nil
}

// RequireSystemAdmin 必须挂在 RequireAuth 后面：principal 已按当次请求从库里取出。
// 普通成员打到没有归属模型的全局端点时，这里回 403，不把媒体和记忆放给任意登录用户。
func (a *Auth) RequireSystemAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := principalFrom(c)
		if !ok || !p.SystemAdmin {
			writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireTenantRole 比较的是 token 里当前工作区的角色，不是平台管理员身份。
func (a *Auth) RequireTenantRole(min string) gin.HandlerFunc {
	need := roleRank(min)
	return func(c *gin.Context) {
		p, ok := principalFrom(c)
		if !ok || roleRank(p.Role) < need {
			writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

// PathTenantMatch 要求 URL 上的 tenant 就是 token 里的 active tenant。要管别的工作区，先切过去。
func (a *Auth) PathTenantMatch() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := principalFrom(c)
		if !ok || c.Param("id") == "" || c.Param("id") != p.TenantID {
			writeEnvelope(c, http.StatusForbidden, codeNotMember, "你不是这个工作区的成员", nil)
			c.Abort()
			return
		}
		if _, err := a.store.GetTenant(c.Request.Context(), p.TenantID); err != nil {
			writeEnvelope(c, http.StatusForbidden, codeNotMember, "你不是这个工作区的成员", nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

func principalFrom(c *gin.Context) (principal, bool) {
	v, ok := c.Get("tako.principal")
	if !ok {
		return principal{}, false
	}
	p, ok := v.(principal)
	return p, ok
}

func bearer(header string) (string, bool) {
	raw, ok := strings.CutPrefix(header, "Bearer ")
	raw = strings.TrimSpace(raw)
	return raw, ok && raw != ""
}

func normalizeEmail(raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if !emailPattern.MatchString(email) {
		return "", false
	}
	return email, true
}

func toItems(rows []MemberListing) []membershipItem {
	out := make([]membershipItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, membershipItem{TenantID: row.TenantID, TenantName: row.TenantName, Role: row.Role})
	}
	return out
}

func (t *tokenIssuer) issueAccessToken(userID, tenantID string, ver int) (string, int, error) {
	now := time.Now().UTC()
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return "", 0, err
	}
	claims := jwt.MapClaims{
		"sub": userID,
		"tid": tenantID,
		"jti": id.String(),
		"ver": ver,
		"iat": now.Unix(),
		"exp": now.Add(t.ttl).Unix(),
		"iss": "tako",
		"aud": "tako-api",
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", 0, err
	}
	return signed, int(t.ttl.Seconds()), nil
}

func (t *tokenIssuer) parseAccessToken(raw string) (sub, tid string, ver int, err error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer("tako"),
		jwt.WithAudience("tako-api"),
		jwt.WithExpirationRequired(),
	)
	tok, err := parser.Parse(raw, func(tok *jwt.Token) (any, error) {
		return t.secret, nil
	})
	if err != nil {
		return "", "", 0, err
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return "", "", 0, jwt.ErrTokenInvalidClaims
	}
	sub, _ = claims["sub"].(string)
	tid, _ = claims["tid"].(string)
	switch n := claims["ver"].(type) {
	case float64:
		ver = int(n)
	case int:
		ver = n
	}
	if sub == "" || claims["aud"] != "tako-api" {
		return "", "", 0, jwt.ErrTokenInvalidClaims
	}
	return sub, tid, ver, nil
}
