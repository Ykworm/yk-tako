// skilldeps.go 是技能依赖的检查面（doc/52 §7 / D1）：声明进表，检查现算。
//
// 三处用同一把尺：安装响应、`GET /api/skills/:name/check`、`use_skill` 的标签——
// 都调 skillDependencies，而它只走 subprocessPath（D39 过滤后的 PATH）。检查与执行
// 若用两条 PATH，就会出现「检查说找到、bash 说 command not found」的自相矛盾。
//
// missing 不缓存（§4.7 末段）：那是每台主机、每一刻的事实；存进表就是让副本 B 展示
// 副本 A 的结论。缺依赖不拦任何动作，只报（§7 第 2 条）。
package runengine

import (
	"context"
	"strings"
)

// skillDependency 是一条依赖的检查结果（§6.1 的 dependencies[] 条目，形状由 D1 定）。
//
// 键序是契约：name → found → path（path 只在 found=true 时写）。
// path 是**本副本**上解析到的真实路径；它不承诺「沙箱里跑得起来」（架构、动态库、
// 解释器自己的 stdlib 都可能不成立，§7 末段）。
type skillDependency struct {
	Name  string `json:"name"`
	Found bool   `json:"found"`
	Path  string `json:"path,omitempty"`
}

// skillDependencies 现查一组依赖。tenantRoot 是 `<workspace>/tenants/<tid>`——
// 工具根（`<tenantRoot>/tools/*/bin`）排在宿主 PATH 前面，所以管理员上传的 `ffmpeg`
// 能被找到；其余围栏读不到的宿主段仍是丢掉（D39）。检查与 bash 是同一把尺。
func skillDependencies(names []string, tenantRoot string) []skillDependency {
	return dependenciesInPath(names, subprocessPath(tenantRoot, toolsDirFor(tenantRoot)))
}

// dependenciesInPath 是纯函数核：单测喂固定的 PATH 串，不去读宿主环境。
// 空名字跳过；结果保序、与声明同序（前端按声明顺序展示）。
func dependenciesInPath(names []string, pathEnv string) []skillDependency {
	out := make([]skillDependency, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		dep := skillDependency{Name: name}
		if p, ok := lookPathIn(pathEnv, name); ok {
			dep.Found = true
			dep.Path = p
		}
		out = append(out, dep)
	}
	return out
}

// missingDependencies 从检查结果里挑出没找到的名字（`use_skill` 的 missing 属性、
// 列表的 missing[] 都用它）。**空切片不是 nil**：JSON 里要写 `[]` 不写 `null`。
func missingDependencies(deps []skillDependency) []string {
	out := make([]string, 0)
	for _, d := range deps {
		if !d.Found {
			out = append(out, d.Name)
		}
	}
	return out
}

// missingSkillDependencies 是「声明 → 缺什么」的直达形状。没声明就一个字节都不多算。
func missingSkillDependencies(names []string, tenantRoot string) []string {
	if len(names) == 0 {
		return []string{}
	}
	return missingDependencies(skillDependencies(names, tenantRoot))
}

// runWorkspaceRoot 取本场 run 的围栏根（项目目录）；没有 workspace 引用（单测、
// 装配早期）时回空串——过滤只按系统目录走，不会因此放宽任何东西。
func runWorkspaceRoot(ctx context.Context) string {
	if ref, ok := workspaceFrom(ctx); ok {
		return ref.Root
	}
	return ""
}
