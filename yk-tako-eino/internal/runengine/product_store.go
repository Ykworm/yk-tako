package runengine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrPersonalTenant = errors.New("personal tenant")
	ErrInviteInvalid  = errors.New("invite invalid")
	ErrAgentNotFound  = errors.New("agent not found")
	ErrAgentExists    = errors.New("agent exists")
	ErrBadPolicy      = errors.New("bad policy")
)

type TenantInvitation struct {
	ID        string    `gorm:"primaryKey"`
	TenantID  string    `gorm:"not null;index"`
	Email     string    `gorm:"not null;index"`
	Role      string    `gorm:"not null"`
	TokenHash string    `gorm:"not null;uniqueIndex"`
	Status    string    `gorm:"not null"`
	ExpiresAt time.Time `gorm:"not null"`
	CreatedBy string    `gorm:"not null"`
	CreatedAt time.Time `gorm:"not null"`
}

func (TenantInvitation) TableName() string { return "tenant_invitations" }

type TenantAgent struct {
	ID          string    `gorm:"primaryKey"`
	TenantID    string    `gorm:"not null;index;uniqueIndex:tenant_agent_name"`
	DefaultID   string    `gorm:"not null;default:''"`
	Name        string    `gorm:"not null;uniqueIndex:tenant_agent_name"`
	Kind        string    `gorm:"not null;default:''"`
	Description string    `gorm:"not null;default:''"`
	Spec        string    `gorm:"not null"`
	ModelRef    string    `gorm:"not null;default:''"`
	Tools       string    `gorm:"not null;default:''"`
	ToolPolicy  string    `gorm:"not null;default:''"`
	SkillPolicy string    `gorm:"not null;default:''"` // 技能作用域 JSON（E1）；空串 = inherit
	Visibility  string    `gorm:"not null"`
	Source      string    `gorm:"not null;default:''"`
	IsBuiltin   bool      `gorm:"not null"`
	CreatedBy   string    `gorm:"not null"`
	CreatedAt   time.Time `gorm:"not null"`
}

// AgentDefault 是平台默认 agent。id 以 ad_ 开头，租户没覆盖时列表直接用这一行。
type AgentDefault struct {
	ID          string    `gorm:"primaryKey"`
	Name        string    `gorm:"not null;uniqueIndex"`
	Kind        string    `gorm:"not null"`
	Description string    `gorm:"not null;default:''"`
	Spec        string    `gorm:"not null;default:''"`
	ModelRef    string    `gorm:"not null;default:''"`
	ToolPolicy  string    `gorm:"not null;default:''"`
	SkillPolicy string    `gorm:"not null;default:''"`
	Version     int       `gorm:"not null;default:1"`
	CreatedAt   time.Time `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null"`
}

func (AgentDefault) TableName() string { return "agent_defaults" }

// ToolPolicy 是工具策略的库存和响应形状。不写策略等于 inherit。
type ToolPolicy struct {
	Mode  string   `json:"mode"`
	Names []string `json:"names"`
}

// SkillPolicy 是技能作用域（doc/52 E1）：形状与 ToolPolicy 逐字段相同，写法也相同——
// inherit（不写）/ allow（skills:，只给这些）/ exclude（exclude_skills:，从全集减去）。
// 为什么单独一个类型：两者语义无关，共用 ToolPolicy 会让「改工具策略」的手误改技能面。
// 转换到 ToolPolicy 只为复用同一套 JSON 校验（两个字段集合同形、同一组 mode 值）。
type SkillPolicy struct {
	Mode  string   `json:"mode"`
	Names []string `json:"names"`
}

// EffectiveAgent 是某个租户在一个 kind 下实际看得到的一行。同名租户行盖住默认行。
type EffectiveAgent struct {
	ID          string
	DefaultID   string
	Source      string
	Overridden  bool
	Name        string
	Description string
	Spec        string
	ModelRef    string
	ToolPolicy  string
	SkillPolicy string
	Visibility  string
	Kind        string
}

func (TenantAgent) TableName() string { return "tenant_agents" }

type TenantSetting struct {
	TenantID  string    `gorm:"primaryKey"`
	Config    string    `gorm:"not null"`
	UpdatedBy string    `gorm:"not null;default:''"`
	UpdatedAt time.Time `gorm:"not null"`
}

func (TenantSetting) TableName() string { return "tenant_settings" }

// policyFromSpec 把 md 的 tools / exclude_tools 收成一份策略。两行都没写就是 inherit。
func policyFromSpec(sp AgentSpec) ToolPolicy {
	if sp.Tools != nil {
		names := append([]string{}, (*sp.Tools)...)
		if names == nil {
			names = []string{}
		}
		return ToolPolicy{Mode: "allow", Names: names}
	}
	if sp.ExcludeTools != nil {
		names := append([]string{}, (*sp.ExcludeTools)...)
		if names == nil {
			names = []string{}
		}
		return ToolPolicy{Mode: "exclude", Names: names}
	}
	return ToolPolicy{Mode: "inherit", Names: []string{}}
}

// skillPolicyFromSpec 把 md 的 skills / exclude_skills 收成一份技能作用域。两行都没写就是 inherit。
func skillPolicyFromSpec(sp AgentSpec) SkillPolicy {
	if sp.Skills != nil {
		names := append([]string{}, (*sp.Skills)...)
		if names == nil {
			names = []string{}
		}
		return SkillPolicy{Mode: "allow", Names: names}
	}
	if sp.ExcludeSkills != nil {
		names := append([]string{}, (*sp.ExcludeSkills)...)
		if names == nil {
			names = []string{}
		}
		return SkillPolicy{Mode: "exclude", Names: names}
	}
	return SkillPolicy{Mode: "inherit", Names: []string{}}
}

// skillPolicyJSON 与 policyJSON 同一套校验（两个类型的字段集与 mode 词表相同，直接转换复用）。
func skillPolicyJSON(p SkillPolicy) (string, error) { return policyJSON(ToolPolicy(p)) }

// parseSkillPolicy 与 parseToolPolicy 同一条降级：坏值/空串 = inherit（坏配置不该让整场 run 起不来）。
// 额外过一遍 normalizeSkillPolicy：tool 的 parse 只挡空 mode，不认得的 mode 原样带出；
// 技能作用域在装配与解析两处都要判，归一放在读入口比在每个消费点判一遍可靠。
func parseSkillPolicy(raw string) SkillPolicy {
	return normalizeSkillPolicy(SkillPolicy(parseToolPolicy(raw)))
}

func policyJSON(p ToolPolicy) (string, error) {
	switch p.Mode {
	case "allow", "exclude":
	case "inherit", "none":
		if len(p.Names) > 0 {
			return "", ErrBadPolicy
		}
	default:
		return "", ErrBadPolicy
	}
	if p.Names == nil {
		p.Names = []string{}
	}
	b, err := json.Marshal(p)
	return string(b), err
}

func parseToolPolicy(raw string) ToolPolicy {
	p := ToolPolicy{Mode: "inherit", Names: []string{}}
	if strings.TrimSpace(raw) == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Mode == "" {
		return ToolPolicy{Mode: "inherit", Names: []string{}}
	}
	if p.Names == nil {
		p.Names = []string{}
	}
	return p
}

// SeedAgentDefaults 把 agents/*.md 写进 agent_defaults。coder / researcher 是 kind=both，
// 其余是 subagent。同名再启动只改内容，id 保持不变。目录不存在就什么都不写。
func (s *Store) SeedAgentDefaults(ctx context.Context, dir string) error {
	specs, err := LoadAgentSpecs(dir)
	if err != nil || len(specs) == 0 {
		return err
	}
	now := time.Now()
	for _, sp := range specs {
		raw, err := json.Marshal(policyFromSpec(sp))
		if err != nil {
			return err
		}
		policy := string(raw)
		kind := "subagent"
		if sp.Name == "coder" || sp.Name == "researcher" {
			kind = "both"
		}
		// 普通子代理没有技能目录：写了作用域字段只说明作者以为它有，静默忽略会让这份规格
		// 看起来「配了但没生效」。拒载这一条、其余照载（与坏文件的处理同一条线）。
		if kind == "subagent" && sp.hasSkillScope() {
			log.Printf("agents: %s: 拒载: skills:/exclude_skills: 只对主助手有效（普通子代理没有技能目录）", sp.Name)
			continue
		}
		skillPolicy, err := skillPolicyJSON(skillPolicyFromSpec(sp))
		if err != nil {
			return err
		}
		var found []AgentDefault
		err = s.db.WithContext(ctx).Where("name = ?", sp.Name).Limit(1).Find(&found).Error
		if err != nil {
			return err
		}
		if len(found) == 0 {
			id, err := newRowID("ad_")
			if err != nil {
				return err
			}
			row := AgentDefault{
				ID: id, Name: sp.Name, Kind: kind, Description: sp.Description,
				Spec: sp.Prompt, ModelRef: sp.Model, ToolPolicy: policy, SkillPolicy: skillPolicy,
				Version: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
				return err
			}
			continue
		}
		cur := found[0]
		if cur.Kind == kind && cur.Description == sp.Description && cur.Spec == sp.Prompt && cur.ModelRef == sp.Model && cur.ToolPolicy == policy && cur.SkillPolicy == skillPolicy {
			continue
		}
		if err := s.db.WithContext(ctx).Model(&AgentDefault{}).Where("id = ?", cur.ID).Updates(map[string]any{
			"kind": kind, "description": sp.Description, "spec": sp.Prompt, "model_ref": sp.Model,
			"tool_policy": policy, "skill_policy": skillPolicy, "version": cur.Version + 1, "updated_at": now,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListEffectiveAgents(ctx context.Context, tenantID, userID, kind string) ([]EffectiveAgent, error) {
	var defaults []AgentDefault
	if err := s.db.WithContext(ctx).Where("kind = ? OR kind = ?", kind, "both").Order("name").Find(&defaults).Error; err != nil {
		return nil, err
	}
	var rows []TenantAgent
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND (kind = ? OR kind = ?) AND (visibility = ? OR created_by = ?)", tenantID, kind, "both", "tenant", userID).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	byName := make(map[string]EffectiveAgent, len(defaults)+len(rows))
	names := make([]string, 0, len(defaults)+len(rows))
	for _, d := range defaults {
		names = append(names, d.Name)
		byName[d.Name] = EffectiveAgent{
			ID: d.ID, DefaultID: d.ID, Source: "default", Name: d.Name, Description: d.Description,
			Spec: d.Spec, ModelRef: d.ModelRef, ToolPolicy: d.ToolPolicy, SkillPolicy: d.SkillPolicy,
			Visibility: "tenant", Kind: d.Kind,
		}
	}
	for _, row := range rows {
		if _, ok := byName[row.Name]; !ok {
			names = append(names, row.Name)
		}
		byName[row.Name] = effectiveFromTenant(row)
	}
	sort.Strings(names)
	out := make([]EffectiveAgent, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out, nil
}

func effectiveFromTenant(row TenantAgent) EffectiveAgent {
	source := row.Source
	if source == "" {
		if row.DefaultID != "" {
			source = "override"
		} else {
			source = "custom"
		}
	}
	return EffectiveAgent{
		ID: row.ID, DefaultID: row.DefaultID, Source: source, Overridden: source == "override",
		Name: row.Name, Description: row.Description, Spec: row.Spec, ModelRef: row.ModelRef,
		ToolPolicy: row.ToolPolicy, SkillPolicy: row.SkillPolicy, Visibility: row.Visibility, Kind: row.Kind,
	}
}

func (s *Store) AgentDefaultByName(ctx context.Context, name string) (AgentDefault, error) {
	return s.oneDefault(ctx, "name = ?", name)
}

func (s *Store) AgentDefaultByID(ctx context.Context, id string) (AgentDefault, error) {
	return s.oneDefault(ctx, "id = ?", id)
}

func (s *Store) oneDefault(ctx context.Context, query string, arg any) (AgentDefault, error) {
	var rows []AgentDefault
	if err := s.db.WithContext(ctx).Where(query, arg).Limit(1).Find(&rows).Error; err != nil {
		return AgentDefault{}, err
	}
	if len(rows) == 0 {
		return AgentDefault{}, ErrAgentNotFound
	}
	return rows[0], nil
}

func (s *Store) TenantAgentByName(ctx context.Context, tenantID, name string) (TenantAgent, error) {
	var rows []TenantAgent
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND name = ?", tenantID, name).Limit(1).Find(&rows).Error; err != nil {
		return TenantAgent{}, err
	}
	if len(rows) == 0 {
		return TenantAgent{}, ErrAgentNotFound
	}
	return rows[0], nil
}

type tenantListRow struct {
	ID     string
	Name   string
	Kind   string
	Role   string
	Status string
}

func (s *Store) ListUserTenants(ctx context.Context, userID string) ([]tenantListRow, error) {
	var rows []tenantListRow
	err := s.db.WithContext(ctx).Raw(`
SELECT t.id, t.name, t.kind, m.role, t.status
  FROM tenant_members m
  JOIN tenants t ON t.id = m.tenant_id
 WHERE m.user_id = ? AND m.status = 'active' AND t.status = 'active'
 ORDER BY m.joined_at, t.id`, userID).Scan(&rows).Error
	if rows == nil {
		rows = []tenantListRow{}
	}
	return rows, err
}

func (s *Store) RenameTenant(ctx context.Context, tenantID, name string) error {
	res := s.db.WithContext(ctx).Model(&Tenant{}).Where("id = ? AND status = ?", tenantID, "active").Update("name", name)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (s *Store) ArchiveTenant(ctx context.Context, tenantID string) error {
	var t Tenant
	if err := s.db.WithContext(ctx).Where("id = ?", tenantID).Take(&t).Error; err != nil {
		return err
	}
	if t.Kind == "personal" {
		return ErrPersonalTenant
	}
	return s.db.WithContext(ctx).Model(&Tenant{}).Where("id = ?", tenantID).Update("status", "archived").Error
}

// ResolveRunAgent 把会话上的 agent id 收成这场 run 要用的那一行。
// ad_ 先找默认同名的租户覆盖；ag_ 用租户行。看不见就当没有。
func (s *Store) ResolveRunAgent(ctx context.Context, tenantID, userID, id string) (EffectiveAgent, error) {
	if strings.TrimSpace(id) == "" {
		return EffectiveAgent{}, ErrAgentNotFound
	}
	def, err := s.AgentDefaultByID(ctx, id)
	if err == nil {
		if row, nerr := s.TenantAgentByName(ctx, tenantID, def.Name); nerr == nil {
			if row.Visibility == "tenant" || row.CreatedBy == userID {
				return effectiveFromTenant(row), nil
			}
		} else if !errors.Is(nerr, ErrAgentNotFound) {
			return EffectiveAgent{}, nerr
		}
		return EffectiveAgent{
			ID: def.ID, DefaultID: def.ID, Source: "default", Name: def.Name,
			Description: def.Description, Spec: def.Spec, ModelRef: def.ModelRef,
			ToolPolicy: def.ToolPolicy, SkillPolicy: def.SkillPolicy, Visibility: "tenant", Kind: def.Kind,
		}, nil
	}
	if !errors.Is(err, ErrAgentNotFound) {
		return EffectiveAgent{}, err
	}
	row, err := s.VisibleAgent(ctx, tenantID, userID, id)
	if err != nil {
		return EffectiveAgent{}, err
	}
	return effectiveFromTenant(row), nil
}

// EffectiveSubagentSpecs 把有效子代理收成装载器认得的四态规格。没配策略就是继承。
func (s *Store) EffectiveSubagentSpecs(ctx context.Context, tenantID, userID string) ([]AgentSpec, error) {
	rows, err := s.ListEffectiveAgents(ctx, tenantID, userID, "subagent")
	if err != nil {
		return nil, err
	}
	out := make([]AgentSpec, 0, len(rows))
	for _, row := range rows {
		out = append(out, specFromEffective(row))
	}
	return out, nil
}

func specFromEffective(row EffectiveAgent) AgentSpec {
	spec := AgentSpec{
		Name: row.Name, Description: row.Description, Model: row.ModelRef, Prompt: row.Spec,
	}
	policy := parseToolPolicy(row.ToolPolicy)
	switch policy.Mode {
	case "allow":
		names := append([]string{}, policy.Names...)
		spec.Tools = &names
	case "none":
		empty := []string{}
		spec.Tools = &empty
	case "exclude":
		names := append([]string{}, policy.Names...)
		spec.ExcludeTools = &names
	}
	return spec
}

func (s *Store) VisibleAgent(ctx context.Context, tenantID, userID, id string) (TenantAgent, error) {
	var row TenantAgent
	err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", id, tenantID).Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return TenantAgent{}, ErrAgentNotFound
		}
		return TenantAgent{}, err
	}
	if row.Visibility == "private" && row.CreatedBy != userID {
		return TenantAgent{}, ErrAgentNotFound
	}
	return row, nil
}

func (s *Store) ListAgents(ctx context.Context, tenantID, userID string) ([]TenantAgent, error) {
	var rows []TenantAgent
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND (visibility = ? OR created_by = ?)", tenantID, "tenant", userID).
		Order("is_builtin DESC, created_at, id").Find(&rows).Error
	if rows == nil {
		rows = []TenantAgent{}
	}
	return rows, err
}

func (s *Store) CreateAgent(ctx context.Context, row TenantAgent) (TenantAgent, error) {
	id, err := newRowID("ag_")
	if err != nil {
		return TenantAgent{}, err
	}
	row.ID = id
	row.CreatedAt = time.Now()
	if row.Visibility == "" {
		row.Visibility = "private"
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isUniqueViolation(err) {
			return TenantAgent{}, ErrAgentExists
		}
		return TenantAgent{}, err
	}
	return row, nil
}

func (s *Store) InsertAgents(ctx context.Context, rows []TenantAgent) ([]TenantAgent, error) {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range rows {
			id, err := newRowID("ag_")
			if err != nil {
				return err
			}
			rows[i].ID = id
			rows[i].CreatedAt = time.Now()
			if rows[i].Visibility == "" {
				rows[i].Visibility = "private"
			}
			if err := tx.Create(&rows[i]).Error; err != nil {
				if isUniqueViolation(err) {
					return ErrAgentExists
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) UpdateAgent(ctx context.Context, row TenantAgent) error {
	res := s.db.WithContext(ctx).Model(&TenantAgent{}).Where("id = ? AND tenant_id = ?", row.ID, row.TenantID).
		Updates(map[string]any{
			"name": row.Name, "kind": row.Kind, "description": row.Description, "spec": row.Spec,
			"model_ref": row.ModelRef, "tools": row.Tools, "tool_policy": row.ToolPolicy,
			"skill_policy": row.SkillPolicy,
			"visibility":   row.Visibility, "source": row.Source, "default_id": row.DefaultID,
		})
	if isUniqueViolation(res.Error) {
		return ErrAgentExists
	}
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrAgentNotFound
	}
	return nil
}

func (s *Store) DeleteAgent(ctx context.Context, tenantID, id string) error {
	var row TenantAgent
	if err := s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", id, tenantID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrAgentNotFound
		}
		return err
	}
	if row.IsBuiltin {
		return ErrPersonalTenant
	}
	return s.db.WithContext(ctx).Delete(&TenantAgent{}, "id = ?", id).Error
}

// TenantTeamInstructions 读该租户的团队规范（租户系统提示材料，不是记忆）。
//
// 读失败**不阻断 run**：团队规范是配置面的一部分，读不到就不带它（系统提示照常拼），
// 但要记日志——静默丢掉一段团队规矩会让模型以为没有这条规矩。
func (s *Store) TenantTeamInstructions(ctx context.Context, tenantID string) (string, error) {
	if tenantID == "" {
		return "", nil
	}
	cfg, err := s.GetRuntimeConfig(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return cfg.TeamInstructions, nil
}

// teamInstructionsFor 取当前上下文的租户团队规范（拼进系统提示用）。
//
// 空串 = 这段上下文没有租户身份（旁路调用）或该租户没配团队规范。
func (s *Store) teamInstructionsFor(ctx context.Context) string {
	tenantID := ""
	if ref, ok := workspaceFrom(ctx); ok && ref.TenantID != "" {
		tenantID = ref.TenantID
	} else if p, ok := principalFromCtx(ctx); ok && p.TenantID != "" {
		tenantID = p.TenantID
	}
	if tenantID == "" {
		return ""
	}
	text, err := s.TenantTeamInstructions(ctx, tenantID)
	if err != nil {
		log.Printf("team instructions: 读租户 %s 的团队规范失败：%v", tenantID, err)
		return ""
	}
	return strings.TrimSpace(text)
}

// teamInstructionsSection 把团队规范渲染成系统提示里的一段。
//
// 形态：标题 + 正文 + 空行。**明确标成「租户规范」而不是「记忆」**——它是管理员配的
// 租户系统提示材料，与 user/workspace 两值记忆轴无关（doc/65 §6.1）。
func teamInstructionsSection(text string) string {
	if text == "" {
		return ""
	}
	return "租户规范（本租户统一的规矩，优先于一般偏好）：\n" + text
}

// DevMemorySeed 是 dev 数据准备口（POST /api/dev/memory）的输入：新行结构的五个字段
// 加 scope=workspace 时的项目 id。词表校验在 handler 做，这里只落库。
type DevMemorySeed struct {
	Layer       string
	Scope       string
	State       string
	Content     string
	Quote       string
	WorkspaceID string
}

// SeedMemoryFact 插一条手动种下的记忆（dev 工具用）。
//
// 新结构（doc/65 §4.2）没有 kind/topic 两列：分类改成 layer（读在哪一层）+ scope（谁能看），
// 来源用 source='model'（doc/65 §4.2：直接工具写入的行）。
// 种历史行（state 非 active）时补 left_active_at/removed_at，让时间审计列与状态一致。
func (s *Store) SeedMemoryFact(ctx context.Context, seed DevMemorySeed) (string, error) {
	sc := scopeFrom(ctx)
	id, err := newRowID("mem_")
	if err != nil {
		return "", err
	}
	userID := sc.UserID
	if seed.Scope == MemoryScopeWorkspace {
		userID = ""
	}
	var leftActive, removedAt *time.Time
	now := time.Now()
	if seed.State != MemoryStateActive {
		leftActive = &now
	}
	if seed.State == MemoryStateRemoved {
		removedAt = &now
	}
	err = s.db.WithContext(ctx).Exec(`
INSERT INTO memory_items (id, tenant_id, user_id, workspace, layer, scope, content, quote,
        source, source_session_id, source_run_id, source_seq, state, left_active_at, removed_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'model', '', '', 0, ?, ?, ?, now(), now())`,
		id, sc.TenantID, userID, seed.WorkspaceID, seed.Layer, seed.Scope, seed.Content, seed.Quote,
		seed.State, leftActive, removedAt).Error
	return id, err
}

func (s *Store) EnsureDevSession(ctx context.Context, p principal) (SessionID, error) {
	var ids []SessionID
	err := s.db.WithContext(ctx).Raw(`
SELECT id FROM sessions WHERE tenant_id = ? AND user_id = ? AND title = 'dev-workspace'
 ORDER BY created_at LIMIT 1`, p.TenantID, p.UserID).Scan(&ids).Error
	if err != nil {
		return "", err
	}
	if len(ids) > 0 && ids[0] != "" {
		return ids[0], nil
	}
	sid, err := newSessionID()
	if err != nil {
		return "", err
	}
	err = s.db.WithContext(ctx).Exec(`
INSERT INTO sessions (id, title, tenant_id, user_id, visibility, agent_id, created_at)
VALUES (?, 'dev-workspace', ?, ?, 'private', '', now())`, sid, p.TenantID, p.UserID).Error
	return sid, err
}

func newInviteToken() (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashRefresh(raw), nil
}

func (s *Store) CreateInvitation(ctx context.Context, tenantID, actor, email, role string) (TenantInvitation, string, error) {
	raw, hash, err := newInviteToken()
	if err != nil {
		return TenantInvitation{}, "", err
	}
	now := time.Now()
	var inv TenantInvitation
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Raw(`
SELECT COUNT(*) FROM users u
  JOIN tenant_members m ON m.user_id = u.id
 WHERE u.email = ? AND m.tenant_id = ? AND m.status = 'active'`, email, tenantID).Scan(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadyMember
		}
		var pending []TenantInvitation
		if err := tx.Where("tenant_id = ? AND email = ? AND status = 'pending'", tenantID, email).Find(&pending).Error; err != nil {
			return err
		}
		if len(pending) > 0 {
			inv = pending[0]
			inv.Role = role
			inv.TokenHash = hash
			inv.ExpiresAt = now.Add(7 * 24 * time.Hour)
			return tx.Model(&TenantInvitation{}).Where("id = ?", inv.ID).
				Updates(map[string]any{"role": role, "token_hash": hash, "expires_at": inv.ExpiresAt}).Error
		}
		id, err := newRowID("in_")
		if err != nil {
			return err
		}
		inv = TenantInvitation{
			ID: id, TenantID: tenantID, Email: email, Role: role, TokenHash: hash,
			Status: "pending", ExpiresAt: now.Add(7 * 24 * time.Hour), CreatedBy: actor, CreatedAt: now,
		}
		return tx.Create(&inv).Error
	})
	return inv, raw, err
}

func (s *Store) ListTenantInvitations(ctx context.Context, tenantID string) ([]TenantInvitation, error) {
	var rows []TenantInvitation
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at DESC").Find(&rows).Error
	if rows == nil {
		rows = []TenantInvitation{}
	}
	return rows, err
}

func (s *Store) RevokeInvitation(ctx context.Context, tenantID, id string) error {
	res := s.db.WithContext(ctx).Model(&TenantInvitation{}).
		Where("id = ? AND tenant_id = ? AND status = 'pending'", id, tenantID).
		Update("status", "revoked")
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrInviteInvalid
	}
	return nil
}

func (s *Store) LookupInvitation(ctx context.Context, rawToken string) (TenantInvitation, Tenant, error) {
	var inv TenantInvitation
	err := s.db.WithContext(ctx).Where("token_hash = ?", hashRefresh(rawToken)).Take(&inv).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return TenantInvitation{}, Tenant{}, ErrInviteInvalid
		}
		return TenantInvitation{}, Tenant{}, err
	}
	var t Tenant
	if err := s.db.WithContext(ctx).Where("id = ?", inv.TenantID).Take(&t).Error; err != nil {
		return TenantInvitation{}, Tenant{}, err
	}
	return inv, t, nil
}

func (s *Store) ListMyInvitations(ctx context.Context, email string) ([]TenantInvitation, error) {
	var rows []TenantInvitation
	err := s.db.WithContext(ctx).
		Where("email = ? AND status = 'pending' AND expires_at > now()", email).
		Order("created_at DESC").Find(&rows).Error
	if rows == nil {
		rows = []TenantInvitation{}
	}
	return rows, err
}

func (s *Store) AcceptInvitation(ctx context.Context, userID, email, invID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var inv TenantInvitation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", invID).Take(&inv).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInviteInvalid
			}
			return err
		}
		if inv.Status != "pending" || inv.Email != email || time.Now().After(inv.ExpiresAt) {
			return ErrInviteInvalid
		}
		var n int64
		if err := tx.Model(&TenantMember{}).Where("tenant_id = ? AND user_id = ? AND status = 'active'", inv.TenantID, userID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadyMember
		}
		now := time.Now()
		var existing []TenantMember
		if err := tx.Where("tenant_id = ? AND user_id = ?", inv.TenantID, userID).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) == 0 {
			if err := tx.Create(&TenantMember{TenantID: inv.TenantID, UserID: userID, Role: inv.Role, Status: "active", InvitedBy: inv.CreatedBy, JoinedAt: now}).Error; err != nil {
				return err
			}
		} else if err := tx.Model(&TenantMember{}).Where("tenant_id = ? AND user_id = ?", inv.TenantID, userID).
			Updates(map[string]any{"role": inv.Role, "status": "active", "joined_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&TenantInvitation{}).Where("id = ?", inv.ID).Update("status", "accepted").Error
	})
}

func (s *Store) DeclineInvitation(ctx context.Context, email, invID string) error {
	res := s.db.WithContext(ctx).Model(&TenantInvitation{}).
		Where("id = ? AND email = ? AND status = 'pending'", invID, email).
		Update("status", "declined")
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrInviteInvalid
	}
	return nil
}

// RegisterByInvite 在同一笔事务里核对邀请、建用户、写个人工作区成员和团队成员。
// 邀请过期或状态不对时，事务回滚，用户行不会留下。
func (s *Store) RegisterByInvite(ctx context.Context, email, hash, invID string) (User, error) {
	var user User
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var inv TenantInvitation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", invID).Take(&inv).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInviteInvalid
			}
			return err
		}
		if inv.Status != "pending" || inv.Email != email || time.Now().After(inv.ExpiresAt) {
			return ErrInviteInvalid
		}
		uid, err := newRowID("u_")
		if err != nil {
			return err
		}
		tid, err := newRowID("t_")
		if err != nil {
			return err
		}
		now := time.Now()
		local := email
		if i := strings.IndexByte(email, '@'); i > 0 {
			local = email[:i]
		}
		user = User{ID: uid, Email: email, PasswordHash: hash, CreatedAt: now}
		if err := tx.Create(&user).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrEmailTaken
			}
			return err
		}
		tenant := Tenant{ID: tid, Kind: "personal", Name: local, Status: "active", Plan: "free", CreatedBy: uid, CreatedAt: now}
		if err := tx.Create(&tenant).Error; err != nil {
			return err
		}
		if err := tx.Create(&TenantMember{TenantID: tid, UserID: uid, Role: "owner", Status: "active", JoinedAt: now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&User{}).Where("id = ?", uid).Update("default_tenant_id", tid).Error; err != nil {
			return err
		}
		user.DefaultTenantID = &tid
		if err := tx.Create(&TenantMember{
			TenantID: inv.TenantID, UserID: uid, Role: inv.Role, Status: "active", InvitedBy: inv.CreatedBy, JoinedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Model(&TenantInvitation{}).Where("id = ?", inv.ID).Update("status", "accepted").Error
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Store) AcceptInvitationToken(ctx context.Context, userID, email, rawToken string) error {
	inv, _, err := s.LookupInvitation(ctx, rawToken)
	if err != nil {
		return err
	}
	return s.AcceptInvitation(ctx, userID, email, inv.ID)
}

type runtimeSandbox struct {
	Network bool `json:"network"`
}

type runtimeConfig struct {
	ModelRef   string                     `json:"modelRef"`
	McpServers map[string]McpServerConfig `json:"mcpServers"`
	Secrets    map[string]string          `json:"secrets,omitempty"`
	Sandbox    runtimeSandbox             `json:"sandbox"`
	// TeamInstructions 是租户级团队规范：进系统提示的**租户系统提示材料**，不是记忆。
	//
	// 它为什么在这而不是在记忆里（doc/65 §6.1）：记忆只有 user / workspace 两个可见域，
	// 「团队规范」不属于任何一个——它是租户配置的产物（管理员写、迁移时从旧 notes 的
	// scope='tenant' 行搬过来），不从对话自动提取，也**不提供记忆管理面**。
	// 它不进 user/workspace 两值词表、不进常驻快照的容量口径、也不出现在 GET /api/memory/* 的行里。
	TeamInstructions string `json:"teamInstructions,omitempty"`
	// Memory 是记忆相关的租户配置（doc/65 §5.1 的 memory.residentRunes）。
	Memory memoryRuntimeConfig `json:"memory,omitempty"`
}

// memoryRuntimeConfig 是租户的记忆相关配置。
//
// ResidentRunes 是常驻记忆的 rune 预算：缺省 2200，可配 500–10000。
// 0 = 用缺省（不写这个字段就是缺省）。
type memoryRuntimeConfig struct {
	ResidentRunes int `json:"residentRunes,omitempty"`
}

// teamInstructionsCap 是团队规范的长度上限（rune）。超了拒绝保存（不静默截断——
// 截断会把管理员写的半句话当成完整规范发给模型）。
const teamInstructionsCap = 4000

func normalizeRuntime(cfg runtimeConfig) runtimeConfig {
	if cfg.McpServers == nil {
		cfg.McpServers = map[string]McpServerConfig{}
	}
	// TeamInstructions 只做无害归一：去首尾空白。**不截断**——超长是校验的事（拒绝），
	// 截断会把管理员写的半句话当成完整规范。
	cfg.TeamInstructions = strings.TrimSpace(cfg.TeamInstructions)
	for name, sc := range cfg.McpServers {
		if sc.Args == nil {
			sc.Args = []string{}
		}
		if sc.Env == nil {
			sc.Env = map[string]string{}
		}
		cfg.McpServers[name] = sc
	}
	return cfg
}

func validateRuntime(cfg runtimeConfig) error {
	// 常驻预算：0 = 缺省；范围外**拒绝**（不静默夹取——夹取会让管理员以为自己设成了 10000，
	// 实际生效的是别的数）。
	if r := cfg.Memory.ResidentRunes; r != 0 && (r < 500 || r > 10000) {
		return fmt.Errorf("常驻记忆预算 %d 不在允许范围（500–10000，或留空用缺省 2200）", r)
	}
	if n := utf8.RuneCountInString(cfg.TeamInstructions); n > teamInstructionsCap {
		return fmt.Errorf("团队规范太长了（%d 字，上限 %d 字）——删掉一些再保存", n, teamInstructionsCap)
	}
	for name, sc := range cfg.McpServers {
		if !mcpServerNameRe.MatchString(name) || strings.TrimSpace(sc.Command) == "" {
			return errors.New("mcp config")
		}
	}
	return nil
}

func (s *Store) GetRuntimeConfig(ctx context.Context, tenantID string) (runtimeConfig, error) {
	var row TenantSetting
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return normalizeRuntime(runtimeConfig{}), nil
	}
	if err != nil {
		return runtimeConfig{}, err
	}
	var cfg runtimeConfig
	if err := json.Unmarshal([]byte(row.Config), &cfg); err != nil {
		return runtimeConfig{}, err
	}
	cfg = normalizeRuntime(cfg)
	// 存量明文在读到时封存回库。运行时另走 RuntimeConfigRecord，读的是封存后的那一串。
	if secretsNeedSeal(cfg.Secrets) {
		sealed, serr := sealSecrets(cfg.Secrets, nil)
		if serr == nil {
			cfg.Secrets = sealed
			if b, merr := json.Marshal(cfg); merr == nil {
				_ = s.db.WithContext(ctx).Model(&TenantSetting{}).Where("tenant_id = ?", tenantID).Update("config", string(b)).Error
			}
		}
	}
	return cfg, nil
}

func (s *Store) PutRuntimeConfig(ctx context.Context, tenantID, userID string, cfg runtimeConfig) error {
	old, err := s.GetRuntimeConfig(ctx, tenantID)
	if err != nil {
		return err
	}
	sealed, err := sealSecrets(cfg.Secrets, old.Secrets)
	if err != nil {
		return err
	}
	cfg.Secrets = sealed
	cfg = normalizeRuntime(cfg)
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	row := TenantSetting{TenantID: tenantID, Config: string(b), UpdatedBy: userID, UpdatedAt: time.Now()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"config", "updated_by", "updated_at"}),
	}).Create(&row).Error
}

// RuntimeConfigRecord 返回解开前的配置和库里的原文。配置版本用原文做哈希，改密文就会换缓存键。
func (s *Store) RuntimeConfigRecord(ctx context.Context, tenantID string) (runtimeConfig, string, error) {
	cfg, err := s.GetRuntimeConfig(ctx, tenantID)
	if err != nil {
		return runtimeConfig{}, "", err
	}
	var rows []TenantSetting
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Limit(1).Find(&rows).Error; err != nil {
		return runtimeConfig{}, "", err
	}
	if len(rows) == 0 {
		return cfg, "", nil
	}
	return cfg, rows[0].Config, nil
}

func maskSecrets(cfg runtimeConfig) runtimeConfig {
	if len(cfg.Secrets) == 0 {
		return cfg
	}
	out := make(map[string]string, len(cfg.Secrets))
	for k, v := range cfg.Secrets {
		if strings.TrimSpace(v) == "" {
			out[k] = ""
			continue
		}
		out[k] = "••••"
	}
	cfg.Secrets = out
	return cfg
}

func (s *Store) TenantUsage(ctx context.Context, tenantID string) (tokens int64, live, maxRuns, maxSub int, daily, storage int64, err error) {
	err = s.db.WithContext(ctx).Raw(`SELECT COALESCE(SUM(tokens), 0) FROM tenant_usage_daily WHERE tenant_id = ?`, tenantID).Scan(&tokens).Error
	if err != nil {
		return
	}
	live, err = s.CountLiveRuns(ctx, tenantID)
	if err != nil {
		return
	}
	maxRuns, maxSub, daily, storage, err = s.TenantQuota(ctx, tenantID)
	return
}
