// 本文件是工作区配置与存量迁移的确定性测试（doc/74 施工包 §12.1 第 1 组）：
// 配置校验词表、plan_hash 与旧计划逐字节兼容、单件转换带配置、旧库补列回填默认值。
package runengine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestNormalizeWorkspaceConfig 钉住组级工作区配置的校验面（doc/74 §4.1）：
// mode 只认 shared/isolated（trim 后），shared 不许带 verifyCommand，
// verifyCommand 按字节限 4000、禁 NUL；拒绝一律 *workspaceConfigError（HTTP 117111）。
func TestNormalizeWorkspaceConfig(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		verify     string
		wantMode   string
		wantVerify string
		wantErr    string // 非空 = 期望拒绝，且 reason 含此片段
	}{
		{"省略即 shared", "", "", "shared", "", ""},
		{"显式 shared", "shared", "", "shared", "", ""},
		{"两侧空白 trim", " shared ", "  ", "shared", "", ""},
		{"isolated 无验证命令", "isolated", "", "isolated", "", ""},
		{"isolated 验证命令 trim", "isolated", " npm test ", "isolated", "npm test", ""},
		{"4000 字节边界收", "isolated", strings.Repeat("a", verifyCommandMaxBytes), "isolated", strings.Repeat("a", verifyCommandMaxBytes), ""},
		{"4001 字节拒", "isolated", strings.Repeat("a", verifyCommandMaxBytes+1), "", "", "4000"},
		{"按字节不按字符", "isolated", strings.Repeat("标", 1334), "", "", "4000"}, // 1334×3=4002 字节
		{"NUL 拒", "isolated", "npm\x00test", "", "", "NUL"},
		{"shared 带验证命令拒", "shared", "npm test", "", "", "verifyCommand"},
		{"省略 mode 带验证命令拒", "", "npm test", "", "", "verifyCommand"},
		{"大写不认", "ISOLATED", "", "", "", "shared 或 isolated"},
		{"Provider 名不是 mode", "git", "", "", "", "shared 或 isolated"},
	}
	for _, tc := range cases {
		mode, verify, err := normalizeWorkspaceConfig(tc.mode, tc.verify)
		if tc.wantErr != "" {
			if err == nil {
				t.Errorf("%s：被放行了，必须拒", tc.name)
				continue
			}
			wce, ok := err.(*workspaceConfigError)
			if !ok {
				t.Errorf("%s：错误类型 = %T，要 *workspaceConfigError", tc.name, err)
				continue
			}
			if !strings.Contains(wce.reason, tc.wantErr) {
				t.Errorf("%s：reason = %q，要含 %q", tc.name, wce.reason, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s：被拒：%v", tc.name, err)
			continue
		}
		if mode != tc.wantMode || verify != tc.wantVerify {
			t.Errorf("%s：得到 (%q,%q)，要 (%q,%q)", tc.name, mode, verify, tc.wantMode, tc.wantVerify)
		}
	}
}

// legacyOrchPlan 复刻 doc/74 之前的计划字节形状（字段序与 json 标签逐字相同）。
// 它只活在测试里：plan_hash 兼容的判据是「新代码算出的哈希 == 旧形状算出的哈希」，
// 而不是「新代码两种写法互等」——后者字段写错了也照样互等，钉不住兼容。
type legacyOrchPlan struct {
	SourceSessionID string     `json:"sourceSessionId"`
	WorkspaceID     string     `json:"workspaceId"`
	Goal            string     `json:"goal"`
	Tasks           []planTask `json:"tasks"`
	Links           []planLink `json:"links"`
}

// TestPlanWorkspaceHashCompat 钉住幂等比对的兼容根基（doc/74 §4.2）：
// 省略配置、显式 shared 的计划，与 doc/74 之前同一份计划的 plan_hash 逐字节一致；
// isolated 与 verifyCommand 是真实内容，变了必须变哈希（否则真换配置被判成同一计划）。
func TestPlanWorkspaceHashCompat(t *testing.T) {
	omitted, err := normalizeOrchestrationPlan(goodCmd())
	if err != nil {
		t.Fatalf("省略配置被拒：%v", err)
	}
	explicit := goodCmd()
	explicit.WorkspaceMode = "shared"
	shared, err := normalizeOrchestrationPlan(explicit)
	if err != nil {
		t.Fatalf("显式 shared 被拒：%v", err)
	}
	hOmitted, hShared := mustHash(t, omitted), mustHash(t, shared)
	if hOmitted != hShared {
		t.Fatalf("显式 shared 与省略不同哈希：%s != %s", hShared, hOmitted)
	}
	// 旧形状独立算一遍：新代码 shared 计划必须与它逐字节同哈希。
	legacy := legacyOrchPlan{
		SourceSessionID: omitted.SourceSessionID,
		WorkspaceID:     omitted.WorkspaceID,
		Goal:            omitted.Goal,
		Tasks:           omitted.Tasks,
		Links:           omitted.Links,
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if hLegacy := fmt.Sprintf("%x", sha256.Sum256(b)); hOmitted != hLegacy {
		t.Fatalf("shared 计划哈希偏离旧字节：%s != %s", hOmitted, hLegacy)
	}
	// shared 计划不落新字段（omitempty 生效的直接证据）。
	pb, err := json.Marshal(omitted)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if strings.Contains(string(pb), "workspaceMode") || strings.Contains(string(pb), "verifyCommand") {
		t.Fatalf("shared 计划带了工作区字段：%s", pb)
	}
	// isolated 是内容变化：哈希必变；verifyCommand 再变再变。
	iso := goodCmd()
	iso.WorkspaceMode = "isolated"
	pIso, err := normalizeOrchestrationPlan(iso)
	if err != nil {
		t.Fatalf("isolated 被拒：%v", err)
	}
	hIso := mustHash(t, pIso)
	if hIso == hOmitted {
		t.Fatalf("isolated 与 shared 同哈希")
	}
	iso.VerifyCommand = "npm test"
	pVerify, err := normalizeOrchestrationPlan(iso)
	if err != nil {
		t.Fatalf("isolated+verify 被拒：%v", err)
	}
	if mustHash(t, pVerify) == hIso {
		t.Fatalf("verifyCommand 变了哈希没变")
	}
	// 存储词表：计划里 shared 不落字段，存储列恒有值。
	if got := omitted.storageMode(); got != workspaceModeShared {
		t.Fatalf("storageMode = %q，要 shared", got)
	}
	if got := pIso.storageMode(); got != workspaceModeIsolated {
		t.Fatalf("storageMode = %q，要 isolated", got)
	}
}

// TestSingleTaskCommandWorkspaceConfig 钉住单件 Task 的配置带过（doc/74 §4.1）：
// isolated+verifyCommand 原样进计划；shared 转换后计划字段仍为空（哈希兼容同上一条）。
func TestSingleTaskCommandWorkspaceConfig(t *testing.T) {
	base := CreateTaskCommand{
		IdempotencyKey: "idem", WorkspaceID: "ws", SourceSessionID: "s1",
		Content: "单件", Agent: "coder",
	}
	iso := base
	iso.WorkspaceMode = "isolated"
	iso.VerifyCommand = "npm test"
	plan, err := normalizeOrchestrationPlan(singleTaskCommand(iso))
	if err != nil {
		t.Fatalf("isolated 单件被拒：%v", err)
	}
	if plan.WorkspaceMode != workspaceModeIsolated || plan.VerifyCommand != "npm test" {
		t.Fatalf("配置没带过：%+v", plan)
	}
	sharedPlan, err := normalizeOrchestrationPlan(singleTaskCommand(base))
	if err != nil {
		t.Fatalf("shared 单件被拒：%v", err)
	}
	if sharedPlan.WorkspaceMode != "" || sharedPlan.VerifyCommand != "" {
		t.Fatalf("shared 单件落了工作区字段：%+v", sharedPlan)
	}
}

// TestWorkspaceColumnsMigrationBackfill 钉住存量迁移（doc/74 §5.1）：
// 旧库（没有新列）里已有的组与工作区行，补列后必须回填默认值——
// 旧组 = shared、无验证命令、无发布状态；旧项目 = 无发布标记。
// 造法：先全量 migrate，再把新列与新索引删掉模拟旧库，插旧行，重跑 migrate。
func TestWorkspaceColumnsMigrationBackfill(t *testing.T) {
	store := newTestStore(t, 30*time.Second, 300*time.Second)
	db := store.db
	for _, ddl := range []string{
		`DROP INDEX IF EXISTS agent_orchestrations_publication`,
		`ALTER TABLE agent_orchestrations
			DROP COLUMN IF EXISTS workspace_mode,
			DROP COLUMN IF EXISTS verify_command,
			DROP COLUMN IF EXISTS publication_status,
			DROP COLUMN IF EXISTS publication_error_code,
			DROP COLUMN IF EXISTS publication_epoch,
			DROP COLUMN IF EXISTS published_at`,
		`ALTER TABLE workspaces
			DROP COLUMN IF EXISTS publication_orchestration_id,
			DROP COLUMN IF EXISTS publication_epoch`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("模拟旧库失败：%v\n%s", err, ddl)
		}
	}
	// 旧行不带任何新列（列已删，想带也带不了）。
	if err := db.Exec(`
INSERT INTO agent_orchestrations (id, tenant_id, workspace_id, source_session_id, goal, final_task_id, idempotency_key, created_at, updated_at)
VALUES ('or_legacy', 't1', 'ws1', 's1', '旧目标', 'at_legacy', 'k1', now(), now())`).Error; err != nil {
		t.Fatalf("插旧组行：%v", err)
	}
	if err := db.Exec(`
INSERT INTO workspaces (id, tenant_id, user_id, name, created_at)
VALUES ('ws_legacy', 't1', 'u1', '旧项目', now())`).Error; err != nil {
		t.Fatalf("插旧项目行：%v", err)
	}
	if err := autoMigrate(db); err != nil {
		t.Fatalf("重跑 migrate：%v", err)
	}
	var orch AgentOrchestration
	if err := db.Raw(`SELECT * FROM agent_orchestrations WHERE id = 'or_legacy'`).Scan(&orch).Error; err != nil {
		t.Fatalf("读回旧组：%v", err)
	}
	if orch.WorkspaceMode != "shared" || orch.VerifyCommand != "" ||
		orch.PublicationStatus != "" || orch.PublicationErrorCode != "" ||
		orch.PublicationEpoch != 0 || orch.PublishedAt != nil {
		t.Fatalf("旧组回填不对：%+v", orch)
	}
	var ws Workspace
	if err := db.Raw(`SELECT * FROM workspaces WHERE id = 'ws_legacy'`).Scan(&ws).Error; err != nil {
		t.Fatalf("读回旧项目：%v", err)
	}
	if ws.PublicationOrchestrationID != "" || ws.PublicationEpoch != 0 {
		t.Fatalf("旧项目回填不对：%+v", ws)
	}
	// 新索引也要在重跑后存在（慢对账按它扫）。
	var n int64
	if err := db.Raw(`SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'agent_orchestrations_publication'`).Scan(&n).Error; err != nil {
		t.Fatalf("查索引：%v", err)
	}
	if n != 1 {
		t.Fatalf("发布状态索引没建回来")
	}
}