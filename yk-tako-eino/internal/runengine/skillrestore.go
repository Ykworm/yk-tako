// skillrestore.go 是 D35「执行权跟着看板走」的装配期一段（doc/52 §9-26）。
//
// 盒子本身是 run 内状态、每场从空开始（skillbox.go）。新 run 里 manager 从看板读到「第 3 步：跑
// scripts/build.py」之后，bash 若没有那个技能根，命令会撞 Operation not permitted——「记得」不等于
// 「跑得动」。所以装配时按看板里已有的 skill_name 把根补回去：
//
//   - 续跑：查本 run 的 run_tasks（中断前的行一直在 PG 里）；
//   - 新 run：查「上一场」（与 TaskList 的 D34 段同一个选 run 规则）。
//
// **判定用「有没有建过任务」，不用「上一场用过」**：建过任务才是「确实按它办过事」的确定性证据，
// 与 D33 同一信号；只 use_skill 没建任务的技能不补——那说明还没开始办事。
//
// 补之前逐项重判当前状态（valid、未禁用、盘上还在）：禁用绝不靠恢复绕过 116107；任何一条不成立
// 只跳过这一条，**不让装配失败**——盘上少一个技能不该让整场 run 打不开。恢复只给执行权，
// 正文仍要 read_skill（D34）。
package runengine

import (
	"context"
	"log"
	"os"
	"sort"
)

// restoreSkillRoots 把看板里已有技能任务的根补进本场盒子。没有租户/盒子时什么都不做
// （单测与异常路径）；查询失败只记日志，不拦 run。
func (w *Worker) restoreSkillRoots(ctx context.Context, sessionID SessionID, runID RunID, resumed bool) {
	box := skillBoxFrom(ctx)
	tenantID := scopeFrom(ctx).TenantID
	if box == nil || tenantID == "" {
		return
	}
	names := map[string]bool{}
	collect := func(rid RunID) {
		list, err := w.store.TaskSkillNames(ctx, tenantID, rid)
		if err != nil {
			log.Printf("skill restore %s/%s: %v", sessionID, rid, err)
			return
		}
		for _, name := range list {
			names[name] = true
		}
	}
	collect(runID) // 续跑：本 run 已重放的行
	if !resumed {
		// 新 run：上一场没做完的那些技能的活（续跑不并上一场——本 run 的看板才是它的活）。
		if prev, err := w.store.PreviousBoardRun(ctx, tenantID, sessionID, runID); err != nil {
			log.Printf("skill restore %s: 上一场查询失败: %v", sessionID, err)
		} else if prev != "" {
			collect(prev)
		}
	}
	if len(names) == 0 {
		return
	}
	scope := skillScopeFrom(ctx)
	disabled, err := w.store.ListSkillDisables(ctx, tenantID)
	if err != nil {
		log.Printf("skill restore %s: 禁用集读库失败: %v", sessionID, err)
		return
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		if disabled[name] {
			continue // 禁用绝不靠恢复绕过 116107
		}
		if !scope.allows(name) {
			continue // 作用域外的技能不开根：目录看不见它，bash 也不该读得到（E1）
		}
		row, ok, err := w.store.GetSkill(ctx, tenantID, name)
		if err != nil {
			log.Printf("skill restore %s: 查技能 %q 失败: %v", sessionID, name, err)
			continue
		}
		if !ok {
			continue // 行不在/已失效 → 不补；之后 use_skill 自然回 116001
		}
		root, ok := skillResourceRoot(row)
		if !ok {
			continue // 平铺形态没有资源目录，不为它开整层技能根
		}
		if _, err := os.Stat(root); err != nil {
			continue // 目录不在了 → 不补；use_skill 自然回 116001
		}
		box.add(name, root)
	}
}
