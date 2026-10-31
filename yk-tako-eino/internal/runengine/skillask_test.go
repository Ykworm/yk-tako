// skillask_test.go 钉住对话内安装（doc/52 C3）的几条边界——它们都发生在「人点了批准之后」：
// 提议里的 staging 路径必须还在本租户 `.staging` 内、盘上没了要报过期、取消要真删掉暂存目录、
// 批准要把技能装进正式目录并从 staging 清走。钉这些是因为错了不会崩：
// 一个越界的 staging 路径会把任意目录 rename 进技能根，一个没删的 staging 会一直占着盘。
package runengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProposalSkill 在 staging 里放一个技能目录，返回提议 JSON。
func writeProposalSkill(t *testing.T, staging, name string) string {
	t.Helper()
	dir := filepath.Join(staging, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: 演示用技能。\n---\n\n正文。\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	prop := skillInstallProposal{
		Source:   skillPreviewSource{Kind: "github", Owner: "o", Repo: "r", Ref: "main", URL: "https://codeload.github.com/o/r/tar.gz/main"},
		Staging:  staging,
		Skills:   []string{name},
		Warnings: []string{},
	}
	b, err := json.Marshal(prop)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProposalStagingPath(t *testing.T) {
	mediaRoot := t.TempDir()
	skillsRoot := tenantSkillsRoot(mediaRoot, "t1")
	inside := filepath.Join(skillsRoot, ".staging", "abc")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := proposalStagingPath(skillsRoot, inside); !ok {
		t.Fatal("根内的 staging 应当通过")
	}
	outside := filepath.Join(mediaRoot, "tenants", "t1", "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := proposalStagingPath(skillsRoot, outside); ok {
		t.Error("`.staging` 之外的路径应当拒绝")
	}
	if _, ok := proposalStagingPath(skillsRoot, filepath.Join(inside, "..", "..", "x")); ok {
		t.Error("越过 `.staging` 的路径应当拒绝")
	}
	if _, ok := proposalStagingPath(skillsRoot, ""); ok {
		t.Error("空路径应当拒绝")
	}
}

func TestInstallApprovedProposal(t *testing.T) {
	store := newTestStore(t, testLease, testTTL)
	mediaRoot := t.TempDir()
	skillsRoot := tenantSkillsRoot(mediaRoot, "t1")
	staging := filepath.Join(skillsRoot, ".staging", "p1")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	args := writeProposalSkill(t, staging, "demo")
	srv := &Server{store: store, mediaRoot: mediaRoot}
	row := RunQuestion{TenantID: "t1", Kind: questionKindInstall, Args: args, Status: questionStatusAnswered}

	text := srv.installApprovedProposal(context.Background(), row)
	if !strings.Contains(text, "已安装技能：demo") {
		t.Fatalf("结果文本 = %q", text)
	}
	// 行进表、目录进技能根、staging 清走。
	got, found, err := store.GetSkill(context.Background(), "t1", "demo")
	if err != nil || !found {
		t.Fatalf("装完应当有行: found=%v err=%v", found, err)
	}
	if !strings.HasSuffix(got.Path, filepath.Join("demo", "SKILL.md")) {
		t.Errorf("path = %q", got.Path)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("装完 staging 应当删掉，stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsRoot, "demo", "SKILL.md")); err != nil {
		t.Errorf("正式目录里应当有 SKILL.md: %v", err)
	}
}

// 暂存目录被清掉（清扫器 / 人手删）→ 报过期，不装任何东西。
func TestInstallApprovedProposalExpired(t *testing.T) {
	store := newTestStore(t, testLease, testTTL)
	mediaRoot := t.TempDir()
	skillsRoot := tenantSkillsRoot(mediaRoot, "t1")
	staging := filepath.Join(skillsRoot, ".staging", "gone")
	args, err := json.Marshal(skillInstallProposal{Staging: staging, Skills: []string{"demo"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: store, mediaRoot: mediaRoot}
	text := srv.installApprovedProposal(context.Background(), RunQuestion{TenantID: "t1", Kind: questionKindInstall, Args: string(args)})
	if text != skillInstallExpiredText {
		t.Fatalf("文本 = %q，期望 %q", text, skillInstallExpiredText)
	}
	if _, found, _ := store.GetSkill(context.Background(), "t1", "demo"); found {
		t.Error("过期提议不应装出任何行")
	}
}

// 取消 = 什么都没发生：暂存目录删掉，正式目录与表都不动。
func TestDiscardInstallProposal(t *testing.T) {
	store := newTestStore(t, testLease, testTTL)
	mediaRoot := t.TempDir()
	skillsRoot := tenantSkillsRoot(mediaRoot, "t1")
	staging := filepath.Join(skillsRoot, ".staging", "p2")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	args := writeProposalSkill(t, staging, "demo")
	discardInstallProposal(store, mediaRoot, "t1", args)
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("取消后 staging 应当删掉，stat err = %v", err)
	}
	if _, found, _ := store.GetSkill(context.Background(), "t1", "demo"); found {
		t.Error("取消不应写行")
	}
}

// 行里的 skills 与盘上的候选不匹配（快照被改 / 盘被换）→ 报失败，不装半个。
func TestInstallApprovedProposalSelectionMismatch(t *testing.T) {
	store := newTestStore(t, testLease, testTTL)
	mediaRoot := t.TempDir()
	skillsRoot := tenantSkillsRoot(mediaRoot, "t1")
	staging := filepath.Join(skillsRoot, ".staging", "p3")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	args := writeProposalSkill(t, staging, "demo")
	var prop skillInstallProposal
	if err := json.Unmarshal([]byte(args), &prop); err != nil {
		t.Fatal(err)
	}
	prop.Skills = []string{"other"}
	b, _ := json.Marshal(prop)
	srv := &Server{store: store, mediaRoot: mediaRoot}
	text := srv.installApprovedProposal(context.Background(), RunQuestion{TenantID: "t1", Kind: questionKindInstall, Args: string(b)})
	if !strings.HasPrefix(text, "安装失败") {
		t.Fatalf("文本 = %q", text)
	}
	if _, found, _ := store.GetSkill(context.Background(), "t1", "demo"); found {
		t.Error("失败不应装出任何行")
	}
}
