// skillscope.go 是技能作用域（doc/52 E1）：manager 规格里的 `skills:`／`exclude_skills:`。
//
// 它与禁用（skill_disables）是两把不同的闸，管的事也不同：
//   - 作用域是**这只助手看哪些**：目录条目不进 prompt，三个技能工具按名字解析时报「找不到」；
//   - 禁用是**这个租户用不用**：目录不出现、工具回 116107。
//
// 作用域的判定在调用那一刻做，不靠目录——目录只是发现用的提示（§2.7），禁用/盘上变化会让
// 目录与解析结果不一致，闸只认解析时读到的行。作用域同理：policy 在 ctx 里（run 级），
// 不随目录文本走，模型把目录里的名字念歪了也不会多读到一个技能。
//
// 值从哪来：assembleRun 读 manager 那一行的 skill_policy，放进 RunProfile，worker.Run 开局
// 塞进 ctx；子代理拿不到技能工具（dropSkillTools），所以它们即使沿用同一个 ctx 也没有调用面。
package runengine

import "context"

type skillScopeKey struct{}

// normalizeSkillPolicy 把坏值/零值收成 inherit：作用域坏掉时「全放」比「全禁」安全——
// 少一个技能不会让 run 起不来，多一个技能至少是盘上与禁用两道闸之后的可见集（§2.7）。
func normalizeSkillPolicy(p SkillPolicy) SkillPolicy {
	if p.Mode != "allow" && p.Mode != "exclude" {
		return SkillPolicy{Mode: "inherit", Names: []string{}}
	}
	if p.Names == nil {
		p.Names = []string{}
	}
	return p
}

func withSkillScope(ctx context.Context, p SkillPolicy) context.Context {
	return context.WithValue(ctx, skillScopeKey{}, normalizeSkillPolicy(p))
}

// skillScopeFrom 取本场 run 的作用域；没写过（单测、启动自检）= inherit。
func skillScopeFrom(ctx context.Context) SkillPolicy {
	p, _ := ctx.Value(skillScopeKey{}).(SkillPolicy)
	return normalizeSkillPolicy(p)
}

// allows 判定一个名字是否在这条作用域里：inherit 全放、allow 只看名单、exclude 看名单外。
// 这里只按名字判，不碰库——行的存在性、valid、禁用由调用方先判。
func (p SkillPolicy) allows(name string) bool {
	p = normalizeSkillPolicy(p)
	switch p.Mode {
	case "allow":
		for _, n := range p.Names {
			if n == name {
				return true
			}
		}
		return false
	case "exclude":
		for _, n := range p.Names {
			if n == name {
				return false
			}
		}
	}
	return true
}

// scopeSkillRows 按作用域过滤目录行。inherit 原样返回（不复制：调用方只读），
// 其余模式回新切片——目录是派生视图，过滤不改行本身。
func scopeSkillRows(rows []SkillRow, p SkillPolicy) []SkillRow {
	if normalizeSkillPolicy(p).Mode == "inherit" {
		return rows
	}
	out := make([]SkillRow, 0, len(rows))
	for _, row := range rows {
		if p.allows(row.Name) {
			out = append(out, row)
		}
	}
	return out
}
