// skillfence_test.go 钉住 B3 的技能围栏（doc/52 D25/D37）：技能根只对「本场 use_skill 过的」
// 开只读、不开写；没登记的目录照旧看不见；出站网络开、入站不开。
//
// 技能根故意造在 /tmp 下（真实路径 /private/tmp **不在**系统读白名单里）：若造在
// t.TempDir()（/var/folders → /private/var）下，那条「没登记就读不到」的用例会被系统白名单
// 顺带放行、偶然变绿，钉子就抓不住「盒子没接上 profile」这个 bug。
package runengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBashFenceSkillRoots(t *testing.T) {
	if !sandboxAvailable() {
		t.Skip("这台机器没有可用的沙箱（seatbelt/bwrap 都探测失败），围栏用例无法证明沙箱路径")
	}
	root := t.TempDir()
	skillDir, err := os.MkdirTemp("/tmp", "tako-skill-")
	if err != nil {
		t.Skipf("造不出 /tmp 下的技能目录：%v", err)
	}
	defer os.RemoveAll(skillDir)
	main := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(main, []byte("DEMO-BODY-MARKER-42"), 0o644); err != nil {
		t.Fatal(err)
	}

	// ① 没登记：读被拒（新 run 没调 use_skill 的现场）
	out, _ := runBash(context.Background(), root, "cat "+main, 10*time.Second, true)
	if !sandboxDenied(out) {
		t.Fatalf("没登记的技能根应读不到，得到 %q", out)
	}

	// ② use_skill 登记后：读成功
	ctx := withSkillBox(context.Background(), newSkillBox())
	skillBoxFrom(ctx).add("demo", skillDir)
	out2, _ := runBash(ctx, root, "cat "+main, 10*time.Second, true)
	if !strings.Contains(out2, "DEMO-BODY-MARKER-42") {
		t.Fatalf("登记过的技能根应能读，得到 %q", out2)
	}

	// ③ 只读：写被拒，盘上内容不变
	out3, _ := runBash(ctx, root, "echo x > "+main, 10*time.Second, true)
	if !sandboxDenied(out3) {
		t.Fatalf("技能根写应被拒绝，得到 %q", out3)
	}
	if raw, _ := os.ReadFile(main); string(raw) != "DEMO-BODY-MARKER-42" {
		t.Fatalf("技能根被写坏了: %q", raw)
	}

	// ④ 隔壁技能：没登记就看不见
	otherDir, err := os.MkdirTemp("/tmp", "tako-skill-other-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(otherDir)
	other := filepath.Join(otherDir, "SKILL.md")
	if err := os.WriteFile(other, []byte("OTHER"), 0o644); err != nil {
		t.Fatal(err)
	}
	out4, _ := runBash(ctx, root, "cat "+other, 10*time.Second, true)
	if !sandboxDenied(out4) {
		t.Fatalf("隔壁技能应读不到，得到 %q", out4)
	}
}

// 出站开、入站不开（D37）；extra 根按真实路径写进 profile，且只挂 file-read*。
func TestSandboxProfileNetworkAndExtraRoots(t *testing.T) {
	skillDir, err := os.MkdirTemp("/tmp", "tako-prof-")
	if err != nil {
		t.Skipf("造不出 /tmp 下的技能目录：%v", err)
	}
	defer os.RemoveAll(skillDir)
	realSkill, err := filepath.EvalSymlinks(skillDir)
	if err != nil {
		t.Fatal(err)
	}

	profile := sandboxProfile("/tmp/tako-root", []string{skillDir})
	if !strings.Contains(profile, "(allow network-outbound)\n") {
		t.Error("profile 缺 network-outbound")
	}
	if strings.Contains(profile, "network-inbound") {
		t.Error("profile 不该开 network-inbound")
	}
	if !strings.Contains(profile, `(allow file-read* (subpath "/private/etc/ssl"))`) {
		t.Error("profile 缺 /private/etc/ssl（curl 起手要读 openssl.cnf 与 CA 包）")
	}
	if !strings.Contains(profile, `(allow file-read* (subpath "`+realSkill+`"))`) {
		t.Errorf("技能根没按真实路径写进 profile:\n%s", profile)
	}
	if strings.Contains(profile, `file-write* (subpath "`+realSkill+`")`) {
		t.Error("技能根不该带写权限")
	}
}

// skillReadRoots：没盒子 = 空；有盒子 = 逐条根。
func TestSkillReadRoots(t *testing.T) {
	if got := skillReadRoots(context.Background()); got != nil {
		t.Errorf("无盒 ctx 得到 %v", got)
	}
	ctx := withSkillBox(context.Background(), newSkillBox())
	skillBoxFrom(ctx).add("demo", "/ws/skills/demo")
	got := skillReadRoots(ctx)
	if len(got) != 1 || got[0] != "/ws/skills/demo" {
		t.Errorf("got %v", got)
	}
}
