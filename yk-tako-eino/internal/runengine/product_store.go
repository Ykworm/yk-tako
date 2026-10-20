package runengine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

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
				Spec: sp.Prompt, ModelRef: sp.Model, ToolPolicy: policy,
				Version: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
				return err
			}
			continue
		}
		cur := found[0]
		if cur.Kind == kind && cur.Description == sp.Description && cur.Spec == sp.Prompt && cur.ModelRef == sp.Model && cur.ToolPolicy == policy {
			continue
		}
		if err := s.db.WithContext(ctx).Model(&AgentDefault{}).Where("id = ?", cur.ID).Updates(map[string]any{
			"kind": kind, "description": sp.Description, "spec": sp.Prompt, "model_ref": sp.Model,
			"tool_policy": policy, "version": cur.Version + 1, "updated_at": now,
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
			Spec: d.Spec, ModelRef: d.ModelRef, ToolPolicy: d.ToolPolicy, Visibility: "tenant",
			Kind: d.Kind,
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
		ToolPolicy: row.ToolPolicy, Visibility: row.Visibility, Kind: row.Kind,
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
			ToolPolicy: def.ToolPolicy, Visibility: "tenant", Kind: def.Kind,
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
			"visibility": row.Visibility, "source": row.Source, "default_id": row.DefaultID,
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

func (s *Store) ListScopedNotes(ctx context.Context, workspace string) ([]Note, error) {
	q := applyNoteScope(s.db.WithContext(ctx), scopeFrom(ctx)).Where("superseded_by IS NULL").Order("created_at, id")
	if workspace != "" {
		q = q.Where("workspace = ?", workspace)
	}
	var rows []Note
	err := q.Find(&rows).Error
	if rows == nil {
		rows = []Note{}
	}
	return rows, err
}

func (s *Store) SeedMemoryFact(ctx context.Context, kind, topic, content string) (string, error) {
	sc := scopeFrom(ctx)
	id, err := newRowID("mem_")
	if err != nil {
		return "", err
	}
	if kind == "" {
		kind = "fact"
	}
	ws := s.memoryCfg.Workspace
	if ws == "" {
		ws = "default"
	}
	err = s.db.WithContext(ctx).Exec(`
INSERT INTO memory_items (id, tenant_id, user_id, workspace, kind, content, topic,
        source_session_id, source_run_id, source_seq, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, '', '', 0, now())`,
		id, sc.TenantID, sc.UserID, ws, kind, content, topic).Error
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
}

func normalizeRuntime(cfg runtimeConfig) runtimeConfig {
	if cfg.McpServers == nil {
		cfg.McpServers = map[string]McpServerConfig{}
	}
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
