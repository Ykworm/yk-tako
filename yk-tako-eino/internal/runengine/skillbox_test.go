// skillbox_test.go 钉住 B3（doc/52 D25）盒子两头的口径：use_skill 写进去的是什么、
// 子代理拿到的是什么。错了不会崩：写错一端，围栏对错的目录放行（泄漏）；
// 子代理那端漏换空盒，爹用过的技能根会跟着 bash 一起进子代理。
package runengine

import (
	"context"
	"testing"
)

// 同根重复登记只留一份；list 拿到的是快照，改它不回写盒子。
func TestSkillBoxAddDedupeAndSnapshot(t *testing.T) {
	box := newSkillBox()
	box.add("demo", "/ws/skills/demo")
	box.add("demo", "/ws/skills/demo")
	box.add("other", "/ws/skills/other")
	box.add("empty", "")

	got := box.list()
	if len(got) != 2 {
		t.Fatalf("roots = %v，期望两个", got)
	}
	if got[1].Name != "other" || got[1].Root != "/ws/skills/other" {
		t.Errorf("第二条 = %+v", got[1])
	}
	got[0].Root = "/tampered"
	if box.list()[0].Root != "/ws/skills/demo" {
		t.Error("改快照回写了盒子")
	}
}

// use_skill 成功记的是 SKILL.md 的**目录**，不是文件路径本身（围栏要的是 subpath 根）。
// 没有盒子时不 panic（单测/异常路径），工具结果照常返回。
func TestRecordUsedSkill(t *testing.T) {
	box := newSkillBox()
	ctx := withSkillBox(context.Background(), box)
	recordUsedSkill(ctx, SkillRow{Name: "demo", Path: "/ws/tenants/t1/skills/demo/SKILL.md"})
	got := box.list()
	if len(got) != 1 || got[0].Root != "/ws/tenants/t1/skills/demo" {
		t.Fatalf("roots = %+v", got)
	}

	recordUsedSkill(context.Background(), SkillRow{Name: "demo", Path: "/x/SKILL.md"})
	if skillBoxFrom(context.Background()) != nil {
		t.Error("无盒 ctx 不该凭空出现盒子")
	}
}

// 重复 use 会把技能挪到末尾：末尾 = 「最近一次」，TaskCreate 盖戳与围栏都读这一条（D32）。
func TestSkillBoxLastUsedOrder(t *testing.T) {
	box := newSkillBox()
	box.add("demo", "/ws/skills/demo")
	box.add("other", "/ws/skills/other")
	box.add("demo", "/ws/skills/demo")
	got := box.list()
	if len(got) != 2 || got[1].Name != "demo" {
		t.Fatalf("roots = %+v，期望 demo 排到末尾且只一份", got)
	}
	ctx := withSkillBox(context.Background(), box)
	if name := usedSkillName(ctx); name != "demo" {
		t.Fatalf("usedSkillName = %q, want demo", name)
	}
	// 没有盒子 / 空盒 → 空串（不是技能任务）。
	if name := usedSkillName(context.Background()); name != "" {
		t.Fatalf("无盒 usedSkillName = %q, want 空", name)
	}
	if name := usedSkillName(withSkillBox(context.Background(), newSkillBox())); name != "" {
		t.Fatalf("空盒 usedSkillName = %q, want 空", name)
	}
}

// 子代理在共用闸后换空盒：爹的根不进子代理，爹的盒子也不被子代理写。
func TestRunSubagentGuardedEmptySkillBox(t *testing.T) {
	parent := newSkillBox()
	parent.add("demo", "/ws/skills/demo")
	ctx := withSkillBox(context.Background(), parent)

	var seen []skillRoot
	_, err := runSubagentGuarded(ctx, nil, make(chan struct{}, 1), 0, "coder", "",
		func(ctx context.Context) (string, error) {
			seen = skillBoxFrom(ctx).list()
			skillBoxFrom(ctx).add("inner", "/ws/skills/inner")
			return "ok", nil
		})
	if err != nil {
		t.Fatalf("runSubagentGuarded: %v", err)
	}
	if len(seen) != 0 {
		t.Errorf("子代理看到 %v，期望空盒", seen)
	}
	if got := parent.list(); len(got) != 1 || got[0].Root != "/ws/skills/demo" {
		t.Errorf("子代理写回了爹的盒子: %v", got)
	}
}
