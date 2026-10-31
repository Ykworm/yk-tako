// skillscope_test.go 钉住 E1（doc/52 §2.7）的两层：策略本身的判据，以及「解析时按作用域拒绝」
// 的那一段。作用域错了不会崩——多放一个技能进目录是省 token 的闸漏了，模型会多读一篇正文；
// 少放一个则是用户点名要用的技能静默消失。两个方向都要有机器可读的期望。
package runengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 三种模式的名字判据：inherit 全放、allow 只看名单、exclude 看名单外。
func TestSkillPolicyAllows(t *testing.T) {
	cases := []struct {
		name   string
		policy SkillPolicy
		want   map[string]bool
	}{
		{"inherit 全放", SkillPolicy{Mode: "inherit"}, map[string]bool{"demo": true, "x": true}},
		{"allow 只看名单", SkillPolicy{Mode: "allow", Names: []string{"demo"}}, map[string]bool{"demo": true, "x": false}},
		{"allow 空名单一个都不要", SkillPolicy{Mode: "allow", Names: []string{}}, map[string]bool{"demo": false}},
		{"exclude 看名单外", SkillPolicy{Mode: "exclude", Names: []string{"demo"}}, map[string]bool{"demo": false, "x": true}},
		{"坏模式按 inherit", SkillPolicy{Mode: "bogus", Names: []string{"demo"}}, map[string]bool{"demo": true}},
		{"零值按 inherit", SkillPolicy{}, map[string]bool{"demo": true}},
	}
	for _, tc := range cases {
		for name, want := range tc.want {
			if got := tc.policy.allows(name); got != want {
				t.Errorf("%s: allows(%q) = %v, want %v", tc.name, name, got, want)
			}
		}
	}
}

// 目录过滤：inherit 原样（同一份切片），allow/exclude 回新切片且只留名单内的行。
func TestScopeSkillRows(t *testing.T) {
	rows := []SkillRow{{Name: "demo"}, {Name: "other"}}
	if got := scopeSkillRows(rows, SkillPolicy{Mode: "inherit"}); len(got) != 2 {
		t.Fatalf("inherit 应原样返回，实得 %d 行", len(got))
	}
	got := scopeSkillRows(rows, SkillPolicy{Mode: "allow", Names: []string{"demo"}})
	if len(got) != 1 || got[0].Name != "demo" {
		t.Fatalf("allow[demo] 只该留 demo，实得 %+v", got)
	}
	got = scopeSkillRows(rows, SkillPolicy{Mode: "exclude", Names: []string{"demo"}})
	if len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("exclude[demo] 只该留 other，实得 %+v", got)
	}
	// 过滤不改原切片：目录是派生视图，别把行集合改坏。
	if rows[0].Name != "demo" || rows[1].Name != "other" {
		t.Fatalf("过滤不应改原切片: %+v", rows)
	}
}

// ctx 里的作用域：没写过 = inherit；写了坏值 = inherit（不把 run 卡在配置错误上）。
func TestSkillScopeCtx(t *testing.T) {
	ctx := context.Background()
	if got := skillScopeFrom(ctx).Mode; got != "inherit" {
		t.Fatalf("没写过应为 inherit，实得 %q", got)
	}
	ctx = withSkillScope(ctx, SkillPolicy{Mode: "allow", Names: []string{"demo"}})
	p := skillScopeFrom(ctx)
	if p.Mode != "allow" || !p.allows("demo") || p.allows("other") {
		t.Fatalf("作用域没落到 ctx: %+v", p)
	}
	ctx = withSkillScope(context.Background(), SkillPolicy{Mode: "nonsense"})
	if got := skillScopeFrom(ctx).Mode; got != "inherit" {
		t.Fatalf("坏模式应归一成 inherit，实得 %q", got)
	}
}

// 存储层转换：empty = inherit；skills:[] = allow 空名单；exclude_skills = exclude。
// 与 ToolPolicy 复用同一套 JSON 校验，坏值在写入口就被拒。
func TestSkillPolicyJSON(t *testing.T) {
	if mode := parseSkillPolicy("").Mode; mode != "inherit" {
		t.Fatalf("空串应解析成 inherit，实得 %q", mode)
	}
	if mode := parseSkillPolicy(`{"mode":"bogus","names":["x"]}`).Mode; mode != "inherit" {
		t.Fatalf("坏 mode 应回落 inherit，实得 %q", mode)
	}
	if _, err := skillPolicyJSON(SkillPolicy{Mode: "allow", Names: nil}); err != nil {
		t.Fatalf("allow + 空名单应合法（= 一个都不要）: %v", err)
	}
	if _, err := skillPolicyJSON(SkillPolicy{Mode: "inherit", Names: []string{"x"}}); err == nil {
		t.Fatal("inherit 带名字应被拒（与 policyJSON 同一条校验）")
	}

	empty := []string{}
	allowEmpty := skillPolicyFromSpec(AgentSpec{Skills: &empty})
	if allowEmpty.Mode != "allow" || len(allowEmpty.Names) != 0 {
		t.Fatalf("skills: [] 应收成 allow 空名单: %+v", allowEmpty)
	}
	names := []string{"demo"}
	ex := skillPolicyFromSpec(AgentSpec{ExcludeSkills: &names})
	if ex.Mode != "exclude" || ex.Names[0] != "demo" {
		t.Fatalf("exclude_skills 应收成 exclude: %+v", ex)
	}
	if inv := skillPolicyFromSpec(AgentSpec{}); inv.Mode != "inherit" {
		t.Fatalf("不写应收成 inherit: %+v", inv)
	}
	if skillScopeDeclared("") || !skillScopeDeclared(`{"mode":"allow","names":[]}`) {
		t.Fatal("skillScopeDeclared 应只认 allow/exclude")
	}
}

// 更新路径必须把 skill_policy 写回库。PUT 的响应是内存里那一行改过的值——只改内存的话，
// 页面显示「已改成 exclude」，下一场 run 读到的还是库里的旧作用域（E1 活体验收抓到的洞：
// use_skill 在 exclude 下仍然读到了正文，查库才发现列没动）。
func TestUpdateAgentPersistsSkillPolicy(t *testing.T) {
	store := newTestStore(t, 30*time.Second, 5*time.Minute)
	ctx := context.Background()
	row, err := store.CreateAgent(ctx, TenantAgent{
		TenantID: "t_scope", Name: "scope-run", Kind: "root", Spec: "你是主助手。",
		ToolPolicy: `{"mode":"inherit","names":[]}`, SkillPolicy: `{"mode":"allow","names":["demo"]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	row.SkillPolicy = `{"mode":"exclude","names":["dep-demo"]}`
	if err := store.UpdateAgent(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := store.TenantAgentByName(ctx, "t_scope", "scope-run")
	if err != nil {
		t.Fatal(err)
	}
	if got.SkillPolicy != row.SkillPolicy {
		t.Fatalf("UpdateAgent 没把 skill_policy 写回库：库里 %q，内存 %q", got.SkillPolicy, row.SkillPolicy)
	}
}

// 解析时的作用域闸：范围外的名字与「表里没有」回同一个 116001 信封、同一个字节，
// 且不读盘（路径不存在也不影响判定顺序）。范围外的技能根也不该被恢复（skillrestore 那段单测在
// skillrestore_test.go 的既有用例里；这里钉解析这一半）。
func TestSkillScopeGatesResolution(t *testing.T) {
	store := newTestStore(t, 30*time.Second, 5*time.Minute)
	ctx := withScope(context.Background(), dataScope{TenantID: "t_scope", UserID: "u"})
	dir := t.TempDir()
	path := filepath.Join(dir, "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: demo\ndescription: 演示\n---\n正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	row := SkillRow{ID: "sk_scope_demo", Scope: "tenant", TenantID: "t_scope", Name: "demo",
		Path: path, Description: "演示", Requires: "[]", Valid: true}
	if err := store.db.WithContext(ctx).Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	box := &skillToolbox{store: store}

	outOfScope := withSkillScope(ctx, SkillPolicy{Mode: "allow", Names: []string{"other"}})
	res, err := box.loadByName(outOfScope, "demo", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("作用域外的技能不该读到正文")
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
	}
	if err := json.Unmarshal([]byte(res.Body), &env); err != nil || env.Code != codeSkillMissing {
		t.Fatalf("作用域外应回 116001，实得 %s", res.Body)
	}
	// 与查一个不存在的名字逐字节相同：不泄漏「它其实存在，只是不在你的范围里」。
	missing, err := box.loadByName(withSkillScope(ctx, SkillPolicy{Mode: "allow", Names: []string{"other"}}), "no-such", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(missing.Body, "no-such", "demo") != res.Body {
		t.Fatalf("作用域外与不存在应同字节:\n%s\n%s", res.Body, missing.Body)
	}
	inScope := withSkillScope(ctx, SkillPolicy{Mode: "allow", Names: []string{"demo"}})
	res, err = box.loadByName(inScope, "demo", false)
	if err != nil || !res.OK || !strings.Contains(res.Body, "正文") {
		t.Fatalf("作用域内应正常读到正文: %v %q", err, res.Body)
	}
}
