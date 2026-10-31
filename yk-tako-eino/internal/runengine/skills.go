// skills.go 是「技能」（doc/52）的存储骨架与列表面（A 段）。
//
// 这个方案的第一件东西不是模型也不是工具，是**两张表**：
//
//   - skill_rows：目录。名字、描述、路径、依赖、可用性。正文与资源不在表里，在卷上的文件里。
//   - skill_disables：租户级禁用。(tenant_id, name) 做主键——闸就是主键。
//
// 「文件是正文，表是目录」（doc/52 §4.7）：表只回答「有哪些技能、叫什么、描述是什么」，
// 目录是这个表的派生视图，run 装配时读一次拼进 manager 的 system prompt。
// 盘上的偏离靠写入口与 rescan 收，不靠启动扫盘（那样每个副本启动都写一次表）。
//
// 本文件当前只做 A1：空表 → GET /api/skills 回空数组。装、扫、读正文在后面的拍里落。
package runengine

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SkillRow 是 skill_rows 的一行：一个技能的**目录条目**（doc/52 §4.7）。
//
// 几件事写死在这里，后面的拍直接依赖：
//
//   - Scope/TenantID/ProjectID 是「归属」三件套，rescan 的**扫描范围与比对范围必须一致**——
//     扫哪几个根，就只动那几个归属的行。这三列存在，是为了让「扫的范围」和「比的范围」
//     天生是同一个集合。
//   - Path 是**入口文件**的绝对路径（目录形态是 <name>/SKILL.md，平铺形态是 <name>.md）。
//     它只用来打开文件，不参与鉴权。
//   - Requires 存 JSON 数组；**missing 不入库**（那是每台主机现算的事实，见 §4.7 末段）。
//   - Valid=false + Reason 的行也进表：盘上有东西但没通过校验的，管理页要看得见，
//     否则用户只会看到「放进去没反应」。
type SkillRow struct {
	ID          string `gorm:"primaryKey"`
	Scope       string `gorm:"not null;default:tenant"`                    // system / tenant（项目层开了再加 project）
	TenantID    string `gorm:"not null;index:skill_rows_scope,priority:1"` // 团队行为租户 id；系统行写空串
	ProjectID   string `gorm:"not null;default:''"`                        // 项目行为项目 id；其余空串
	Name        string `gorm:"not null"`
	Path        string `gorm:"not null"`
	Description string `gorm:"not null;default:''"`
	Requires    string `gorm:"not null;default:'[]'"` // JSON 数组，如 ["python3"]
	Bytes       int64  `gorm:"not null;default:0"`    // 技能目录里所有普通文件之和
	// Valid **不能带 default:true**（2026-10-28 A2 踩到）：GORM 插入时跳过零值字段，
	// 带默认值就意味着「Valid=false 这一列根本不进 INSERT」，数据库落回 true——
	// 坏技能于是被当成生效技能进了目录，且 reason 还挂在行上，看起来像「校验失败但可用」。
	// 不带 default 时 GORM 显式写 false，零值就是零值。
	Valid     bool      `gorm:"not null"`
	Reason    string    `gorm:"not null;default:''"` // valid=false 时的原因
	UpdatedBy string    `gorm:"not null;default:''"`
	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

func (SkillRow) TableName() string { return "skill_rows" }

// SkillDisable 是 skill_disables 的一行：这个租户不用这个技能。
//
// 为什么不挂在 skill_rows 上：出厂技能的行是**全集群一行**（scope=system，tenant_id 空串），
// 租户 A 把 pdf 标禁用会让所有租户一起禁用。禁用是租户级动作，挂不到共享行上。
// 为什么不塞进租户 config 的 JSON：那段文本只有四个已登记的格子，读是 json.Unmarshal 进
// 四字段结构体（没登记的键读不出来），写是整列覆盖（下次谁改模型配置就把它抹掉）——两头都不报错。
//
// 所以单独一张两列的表，(tenant_id, name) 做主键：重复禁用撞主键即无事。
type SkillDisable struct {
	TenantID string `gorm:"primaryKey"`
	Name     string `gorm:"primaryKey"`
}

func (SkillDisable) TableName() string { return "skill_disables" }

// skillBody 是 GET /api/skills 里一行技能的响应体。
//
// 字段序是契约（doc/52 §6.1）：name → description → source → path → enabled → bytes
// → requires → missing → updatedAt → valid。前端按这个顺序渲染，后端改动它就是改契约。
//
// enabled 是**派生**的：skill_disables 里没有这行才是 true。skill_rows 上不存这一列——
// 派生的东西不重复存两份，两份会不同步。
type skillBody struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Source      string   `json:"source"`
	Path        string   `json:"path"`
	Enabled     bool     `json:"enabled"`
	Bytes       int64    `json:"bytes"`
	Requires    []string `json:"requires"`
	Missing     []string `json:"missing"`
	UpdatedAt   string   `json:"updatedAt"`
	Valid       bool     `json:"valid"`
}

// invalidSkillBody 是 invalid[] 里的一项：盘上有东西，但没通过校验。
// 只给路径与原因，不给内容——坏文件不进任何上下文。
type invalidSkillBody struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// skillsData 是 GET /api/skills 的 data。
type skillsData struct {
	Skills  []skillBody        `json:"skills"`
	Invalid []invalidSkillBody `json:"invalid"`
}

// ListSkills 列出这个租户看得见的技能目录。
//
// 可见范围 = **系统层 + 本租户层**两桶（doc/52 §4.7）：别的租户的行一概看不见。
// 系统行的 tenant_id 是空串，是它自己的一桶。
//
// valid=true 的行进 skills[]；valid=false 的行进 invalid[]，且**只放本租户桶的**——
// 出厂层坏掉的文件是运维的事，不该出现在每个租户的管理页上刷存在感。
func (s *Store) ListSkills(ctx context.Context, tenantID string) ([]SkillRow, []SkillRow, error) {
	var rows []SkillRow
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? OR tenant_id = ''", tenantID).
		Order("name").
		Find(&rows).Error
	if err != nil {
		return nil, nil, err
	}
	valid := make([]SkillRow, 0, len(rows))
	invalid := make([]SkillRow, 0)
	for _, row := range rows {
		if row.Valid {
			valid = append(valid, row)
		} else if row.TenantID == tenantID {
			invalid = append(invalid, row)
		}
	}
	return valid, invalid, nil
}

// ListSkillDisables 列出这个租户禁用了哪些技能（按名字）。
func (s *Store) ListSkillDisables(ctx context.Context, tenantID string) (map[string]bool, error) {
	var rows []SkillDisable
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[row.Name] = true
	}
	return out, nil
}

// SetSkillDisabled 写/删一条租户级禁用（doc/52 §4.5）。enabled=false 才落行。
//
// 主键即闸：重复禁用是 FirstOrCreate（无事发生），恢复是删行（删不存在的行也成功）——
// 两个方向都幂等，前端连点不会攒出错误。不碰 skill_rows：禁用是「这个租户不用」，
// 不是「这个技能坏了」；rescan 也不该因为禁用而改 valid。
func (s *Store) SetSkillDisabled(ctx context.Context, tenantID, name string, enabled bool) error {
	if enabled {
		return s.db.WithContext(ctx).
			Where("tenant_id = ? AND name = ?", tenantID, name).
			Delete(&SkillDisable{}).Error
	}
	var row SkillDisable
	return s.db.WithContext(ctx).
		Where(SkillDisable{TenantID: tenantID, Name: name}).
		FirstOrCreate(&row).Error
}

// skillsResponse 把表行拼成响应体。requires 从 JSON 解析，missing 按本副本过滤后的
// PATH 现算（D1）：它是「这台机器此刻缺什么」，不入库、不缓存。
func skillsResponse(rows []SkillRow, invalid []SkillRow, disabled map[string]bool, tenantRoot string) skillsData {
	data := skillsData{Skills: make([]skillBody, 0, len(rows)), Invalid: make([]invalidSkillBody, 0, len(invalid))}
	for _, row := range rows {
		data.Skills = append(data.Skills, skillBodyOf(row, disabled, tenantRoot))
	}
	for _, row := range invalid {
		data.Invalid = append(data.Invalid, invalidSkillBody{Path: row.Path, Reason: row.Reason})
	}
	return data
}

// skillBodyOf 把一行表行拼成响应体。列表与详情共用这一份——
// 字段序是契约，两处各拼一份迟早拼出两个顺序。
func skillBodyOf(row SkillRow, disabled map[string]bool, tenantRoot string) skillBody {
	requires := parseSkillRequires(row.Requires)
	return skillBody{
		Name:        row.Name,
		Description: row.Description,
		Source:      row.Scope,
		Path:        row.Path,
		Enabled:     !disabled[row.Name],
		Bytes:       row.Bytes,
		Requires:    requires,
		Missing:     missingSkillDependencies(requires, tenantRoot),
		UpdatedAt:   row.UpdatedAt.UTC().Format(time.RFC3339),
		Valid:       row.Valid,
	}
}

// GetSkill 按 name 取**这个租户视界内**的那一行（doc/52 §4.3：详情端点按视界解析）。
// 视界 = 系统桶 + 本租户桶；别的租户的同名行不在查询里——它不是「403 拒绝」，
// 是「查无此行」，调用方拿到的字节与查一个不存在的名字完全一样（不泄漏存在性）。
// 只取 valid=true：invalid 行在列表里只有 path 没有 name，详情页按名字打不开它；
// 名字级解析只对生效行有意义。跨桶同名按 A3 的显式比对不该共存，
// 万一真共存了（先占判定有洞），租户行优先——它比出厂行更具体。
func (s *Store) GetSkill(ctx context.Context, tenantID, name string) (SkillRow, bool, error) {
	var rows []SkillRow
	err := s.db.WithContext(ctx).
		Where("name = ? AND valid AND (tenant_id = ? OR tenant_id = '')", name, tenantID).
		Order("name").
		Find(&rows).Error
	if err != nil {
		return SkillRow{}, false, err
	}
	for _, row := range rows {
		if row.TenantID == tenantID {
			return row, true, nil
		}
	}
	if len(rows) > 0 {
		return rows[0], true, nil
	}
	return SkillRow{}, false, nil
}

// parseSkillRequires 解析 requires 列（JSON 数组）。坏值当空数组——这列是**声明**，
// 解析不出来不该让整个列表挂掉。
func parseSkillRequires(raw string) []string {
	out := []string{}
	if raw == "" {
		return out
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return out
	}
	for _, item := range list {
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// skillRowsUniqueIdx 是 skill_rows 的**部分唯一索引**：(tenant_id, name) WHERE valid。
//
// 为什么是部分唯一、不是全表唯一：同名冲突时要把**后来者**记成 valid=false 一行，
// 而 invalid 必须进表（否则管理页看不见盘上有这么个东西）。同一个桶里迟早会撞上——
// 租户已经装过 pdf，有人又往项目根放一份 pdf，rescan 要把项目那行记成 invalid，
// 两行的桶和名字都一样。不带 WHERE valid 的唯一键这时插不进去：两条要求直接互斥。
//
// 加了 WHERE valid 之后：同一个桶内生效行仍只有一个名字（闸还在，且仍然是「插入失败即拒装」），
// 同名的 invalid 行可以有多条。rescan 把一条 invalid 转成生效时，在同一事务里先删旧 invalid 行再插。
func skillRowsUniqueIdx(db *gorm.DB) error {
	// 已建过的表上，valid 列还带着 default:true（A1 建的）。GORM 插入时跳过零值字段，
	// 带默认值就意味着「Valid=false 不进 INSERT」——坏技能会被当成生效技能。
	// 这里显式把默认值摘掉，让零值就是零值。
	_ = db.Exec(`ALTER TABLE skill_rows ALTER COLUMN valid DROP DEFAULT`).Error
	return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS skill_rows_name_valid
ON skill_rows (tenant_id, name)
WHERE valid`).Error
}

// SkillLoads 派生「这场 run 加载过哪些技能」（doc/52 §8.2-3）：只查本 run 的工具扩展行，
// 按 seq 排序后交给 loadsFromEntries 配对 call/result。
//
// 不做 SQL 级过滤（不在 WHERE 里匹配 payload 里的工具名）：过滤条件要跟工具名表同步，
// 而一个 run 的工具行本来就有界；配对与筛选放在纯函数里，单测能逐字节钉住。
// runlog 在 EndRun 才写，run 未结束时返回空数组是契约（§8.2-3）。
func (s *Store) SkillLoads(ctx context.Context, runID RunID) ([]runSkillLoad, error) {
	var entries []RunlogEntry
	err := s.db.WithContext(ctx).
		Where("run_id = ? AND role IN ?", runID, []string{"tool_call", "tool_result"}).
		Order("seq").
		Find(&entries).Error
	if err != nil {
		return nil, err
	}
	return loadsFromEntries(entries), nil
}

// getSkills 是 GET /api/skills：列出生效技能与坏行（doc/52 §6.1）。
//
// 权限：租户内所有成员可读（doc/52 §6.3）。路由上挂 authn，租户从 principal 取——
// 端点没有 :tenantId 路径参数，租户**只能**来自 token，否则就是个越权面。
//
// 响应：200 {"code":0,"message":"","data":{"skills":[…],"invalid":[…]}}
func (s *Server) getSkills(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	rows, invalid, err := s.store.ListSkills(ctx, p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	disabled, err := s.store.ListSkillDisables(ctx, p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", skillsResponse(rows, invalid, disabled, tenantStorageRoot(s.mediaRoot, p.TenantID)))
}

// skillDetailData 是 GET /api/skills/:name 的 data：目录条目 + 盘上正文。
// 键序 skill → body 也是契约。
type skillDetailData struct {
	Skill skillBody `json:"skill"`
	Body  string    `json:"body"`
}

// skillCatalogBlock 读本租户的生效技能与禁用集，渲染成 <available_skills> 段（空串 = 无技能）。
// 只给 manager 的装配用（profile.go assembleRun）；失败只记日志返回空串——
// 目录是横切支撑，不该让整场 run 起不来。
// policy 是这只助手的技能作用域（E1）：只过滤「看得见哪些」，不改行、不碰禁用。
func (s *Server) skillCatalogBlock(ctx context.Context, tenantID string, policy SkillPolicy) string {
	rows, _, err := s.store.ListSkills(ctx, tenantID)
	if err != nil {
		log.Printf("skills: 目录读库失败 tenant=%s: %v", tenantID, err)
		return ""
	}
	disabled, err := s.store.ListSkillDisables(ctx, tenantID)
	if err != nil {
		log.Printf("skills: 禁用集读库失败 tenant=%s: %v", tenantID, err)
		return ""
	}
	return availableSkillsPrompt(scopeSkillRows(rows, policy), disabled)
}

// getSkill 是 GET /api/skills/:name：单条详情 + 盘上 SKILL.md 逐字原文（doc/52 §6.1）。
//
// 权限与列表同一条：租户从 token 取，所有成员可读。名字查不到与「别人的技能」
// 回同一个字节（404 116001）——按租户视界解析，存在性不跨租户泄漏。
// body 是 os.ReadFile 的原样字节，不做转义不截断；转义是 read_skill 工具的事（B2）。
func (s *Server) getSkill(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	row, found, err := s.store.GetSkill(ctx, p.TenantID, c.Param("name"))
	if err != nil {
		internal(c, err)
		return
	}
	if !found {
		writeEnvelope(c, http.StatusNotFound, codeSkillMissing, "找不到这个技能", nil)
		return
	}
	body, err := os.ReadFile(row.Path)
	if err != nil {
		// 行在、盘上没了（手工删目录还没重扫）：这是表与盘的失步，不是「查无此技能」，
		// 回 404 会教用户以为名字错了。照实 500，日志里带路径。
		log.Printf("GET /api/skills/%s: 读正文失败 path=%s: %v", row.Name, row.Path, err)
		internal(c, err)
		return
	}
	disabled, err := s.store.ListSkillDisables(ctx, p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", skillDetailData{
		Skill: skillBodyOf(row, disabled, tenantStorageRoot(s.mediaRoot, p.TenantID)),
		Body:  string(body),
	})
}

// patchSkillBody 是 PATCH /api/skills/:name 的入参。指针用来分开「没给」与「给了 false」——
// 缺省当 true 会把「少写一个字段」变成「悄悄启用」。
type patchSkillBody struct {
	Enabled *bool `json:"enabled"`
}

// patchSkill 是 PATCH /api/skills/:name：开/关一个技能（doc/52 §6.1，E2 的写口）。
//
// 禁用是租户级动作（`skill_disables`），系统技能也能禁——出厂行共享，但「哪家不用」
// 必须按租户记，所以本端点只写禁用表，不碰 `skill_rows`，也不碰盘。
//
// 名字按租户视界解析（与详情同一条）：看不见的技能回 404 116001，与「不存在」同字节。
// 响应回整行 `data.skill`（enabled 已是新值）——前端拿它就地替换，不用再拉列表。
func (s *Server) patchSkill(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var in patchSkillBody
	if err := c.ShouldBindJSON(&in); err != nil || in.Enabled == nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, `请求体要是 {"enabled":true|false}`, nil)
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	row, found, err := s.store.GetSkill(ctx, p.TenantID, c.Param("name"))
	if err != nil {
		internal(c, err)
		return
	}
	if !found {
		writeEnvelope(c, http.StatusNotFound, codeSkillMissing, "找不到这个技能", nil)
		return
	}
	if err := s.store.SetSkillDisabled(ctx, p.TenantID, row.Name, *in.Enabled); err != nil {
		internal(c, err)
		return
	}
	// 目录是下一场 run 的派生输入：别的端不重拉就会继续把禁用项拼进去。
	s.publishEvent(c.Request.Context(), p.TenantID, eventSkillsChanged, row.Name)
	disabled := map[string]bool{}
	if !*in.Enabled {
		disabled[row.Name] = true
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"skill": skillBodyOf(row, disabled, tenantStorageRoot(s.mediaRoot, p.TenantID))})
}

// skillCheckData 是 GET /api/skills/:name/check 的 data（doc/52 §6.1 / D1）。
// 键序 dependencies → checkedOn 是契约。
//
// checkedOn 现在恒为 "host"：这一拍只查宿主过滤后的 PATH。F2 加了租户工具根之后
// 才需要区分「这条是从工具根解析到的，还是宿主」——那时这个字段与 dependencies[].path
// 才会一起变。
type skillCheckData struct {
	Dependencies []skillDependency `json:"dependencies"`
	CheckedOn    string            `json:"checkedOn"`
}

// getSkillCheck 是 GET /api/skills/:name/check：在当前副本重查这个技能的依赖声明。
//
// 为什么单开一个端点而不塞进详情：依赖是**每台主机、每一刻**的事实（§4.7 末段），
// 打开详情看到的 missing 可能已经过时；这个端点让人「现在再查一次」。
// 与列表/详情同一条权限：所有成员可读，租户从 token 取，名字按视界解析
// （看不见的技能与不存在同字节）。
// 禁用状态不影响检查——检查的是「这台机器上有没有」，与「这个租户用不用」无关。
func (s *Server) getSkillCheck(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	row, found, err := s.store.GetSkill(ctx, p.TenantID, c.Param("name"))
	if err != nil {
		internal(c, err)
		return
	}
	if !found {
		writeEnvelope(c, http.StatusNotFound, codeSkillMissing, "找不到这个技能", nil)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", skillCheckData{
		Dependencies: skillDependencies(parseSkillRequires(row.Requires), tenantStorageRoot(s.mediaRoot, p.TenantID)),
		CheckedOn:    "host",
	})
}
