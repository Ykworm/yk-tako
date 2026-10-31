// skillbox.go 是本场 run「用过哪些技能」的登记簿（doc/52 D25）：use_skill 成功时把技能根记进来，
// bash 的围栏从这里取只读根（D37）。一场 run 一个盒子，挂 ctx；同一场里工具并行调用，所以带锁。
//
// 它只影响**读得到哪**，不影响写：围栏对技能根只放 file-read*，写仍然只有工作区根。
// 子代理拿不到这个盒子（agents.go 的 runSubagentGuarded 换空盒）——爹用过的技能根不跟着 bash 进子代理。
package runengine

import (
	"context"
	"sync"
)

// skillRoot 是一次 use_skill 的落点：Name 给人看（B5 任务归属要用），Root 是围栏要的目录
// （SKILL.md 所在目录，不是文件本身）。
type skillRoot struct {
	Name string
	Root string
}

// skillBox 是 run 级可变盒子。没有它时 use_skill 仍返回正文，只是围栏打不开那个根（调用方记日志）。
type skillBox struct {
	mu    sync.RWMutex
	roots []skillRoot
}

func newSkillBox() *skillBox { return &skillBox{} }

// add 记下一个技能根。同根重复 use 只保留一份——profile 里重复 subpath 不报错，
// 但围栏文件没有理由随调用次数线性变长。重复 use 会把它挪到末尾：末尾 = 「最近一次」，
// TaskCreate 盖戳（usedSkillName）与围栏都不需要另存一个时间戳。
func (b *skillBox) add(name, root string) {
	if root == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, r := range b.roots {
		if r.Root == root {
			b.roots = append(append(b.roots[:i], b.roots[i+1:]...), skillRoot{Name: name, Root: root})
			return
		}
	}
	b.roots = append(b.roots, skillRoot{Name: name, Root: root})
}

// list 返回快照：拼围栏的调用方拿到的切片不会再被别的工具调用改掉。
func (b *skillBox) list() []skillRoot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]skillRoot(nil), b.roots...)
}

type skillBoxKey struct{}

func withSkillBox(ctx context.Context, b *skillBox) context.Context {
	return context.WithValue(ctx, skillBoxKey{}, b)
}

func skillBoxFrom(ctx context.Context) *skillBox {
	b, _ := ctx.Value(skillBoxKey{}).(*skillBox)
	return b
}

// usedSkillName 是本 run 最近一次成功 use_skill 的技能名（D32）：TaskCreate 拿它盖 `skill_name`。
// 盒子里按「最近一次」排（add 对重复项挪到末尾），所以取末尾那条。
// read_skill 不写盒子 → 只读过正文的任务不带技能名；子代理拿空盒 → 它建的任务也不带。
func usedSkillName(ctx context.Context) string {
	b := skillBoxFrom(ctx)
	if b == nil {
		return ""
	}
	roots := b.list()
	if len(roots) == 0 {
		return ""
	}
	return roots[len(roots)-1].Name
}

// skillReadRoots 是 startBash 拼围栏要的那份只读根：没有盒子（或盒子为空）= 没有额外读权。
func skillReadRoots(ctx context.Context) []string {
	b := skillBoxFrom(ctx)
	if b == nil {
		return nil
	}
	roots := b.list()
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, r.Root)
	}
	return out
}
