// skilltools.go 是技能面「被看见」的结构（doc/52 B1–B4）：
//   - <available_skills> 目录：只拼 manager 的 Instruction（doc/52 §2.2），程序生成的派生视图；
//   - 三个技能工具（read_skill / use_skill / read_skill_file）进 manager 工具池——
//     B2 起 read_skill 是真身（查表 → 读盘 → 去 frontmatter → 转义渲染），
//     B3 起 use_skill 同源返回正文并把技能根登记进 run 盒子（skillbox.go），
//     B4 起 read_skill_file 是真身（资源根内读取、目录列条目、与正文同一条上限）；
//   - 普通子代理结构性看不见它们：agentSpecHandler 拷入口快照时把四个保留名剔掉（§2.8）。
//
// skill_install（C3）在 B1 就登记进剔除名单：它晚两拍才注册进池，但「子代理不该有写盘工具」
// 这条边界要跟工具注册同拍落下——晚一拍就有一拍是泄漏。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// skillToolNames 是「只进 manager 池」的保留工具名（doc/52 §2.8）。
var skillToolNames = []string{"read_skill", "use_skill", "read_skill_file", "skill_install"}

// 正文上限（doc/52 D26）：≤ 16,384 token（cl100k）且 ≤ 64KB 字节，取更严的。
// 两条都判、都不截断——截断的正文会让执行者拿着半截步骤干活，比拒绝更糟。
const (
	skillBodyMaxTokens = 16384
	skillBodyMaxBytes  = 64 * 1024
)

// dropSkillTools 从子代理入口快照里剔掉四个技能工具名。
//
// 为什么剔在 base 而不是 resolveToolPool：base 是继承档、exclude_tools 档、tools 白名单档
// 三档的共同来源——剔在这里，继承档拿不到，白名单档写了名字会按「未知名」拒载，
// 一份过滤管三条路，不散成三处判断。
func dropSkillTools(ctx context.Context, tools []tool.BaseTool) []tool.BaseTool {
	out := make([]tool.BaseTool, 0, len(tools))
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			out = append(out, t) // 拿不到名的原样保留：过滤不该顺手改装配结果
			continue
		}
		reserved := false
		for _, name := range skillToolNames {
			if info.Name == name {
				reserved = true
				break
			}
		}
		if !reserved {
			out = append(out, t)
		}
	}
	return out
}

// availableSkillsPrompt 渲染 <available_skills> 目录（doc/52 §2.2 的形状）。
//
// 只放 valid 且本租户未禁用的行（调用方传进来的 rows 已是 ListSkills 的生效集）；
// 描述**原样全拼**——选技能的判据是作者写的描述，渲染时不再切字（doc/52 §2.2 末段）。
// 只做 XML 转义与空白折叠：描述里的换行/缩进折成单个空格，`<`/`&` 转义成实体——
// 否则描述里写一段 `<skill>` 示例就能把目录块的边界顶开。
// 空集回空串：没有技能时 Instruction 一个字节都不多。
func availableSkillsPrompt(rows []SkillRow, disabled map[string]bool) string {
	var b strings.Builder
	for _, row := range rows {
		if !row.Valid || disabled[row.Name] {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("<available_skills>\n")
		}
		description := escapeSkillXML(strings.Join(strings.Fields(row.Description), " "))
		fmt.Fprintf(&b, "  <skill>\n    <name>%s</name>\n    <description>%s</description>\n    <location>%s</location>\n  </skill>\n", escapeSkillXML(row.Name), description, escapeSkillXML(row.Path))
	}
	if b.Len() == 0 {
		return ""
	}
	b.WriteString("</available_skills>")
	return b.String()
}

// escapeSkillXML 转义正文/描述里的 `&` 与 `<`（doc/52 §2.10 第 3 条）。
//
// 只转这两个：它们能顶开 XML 结构；`>` 单独出现不改变结构，转了只会让模型的
// 「反转义后与盘上一致」多一步。先 `&` 后 `<`，避免把刚生成的 `&lt;` 再转一次。
// 顺带买到一条性质：正文里不可能出现真的 `</skill>`，所以 fold/trim 按内容标记
// 认块是安全的（D30）。
func escapeSkillXML(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	return strings.ReplaceAll(text, "<", "&lt;")
}

// stripSkillFrontmatter 取出 frontmatter 之后的正文（doc/52 §2.1）：整篇入口文件
// 去掉 YAML 头与紧随的空行，块本身负责换行。
//
// 与 parseSkillFrontmatter 认同一套边界（BOM、`---` 开头、`\n---` 收口），
// 但它的输出是正文而不是字段——读正文（本文件）与扫盘（skills_scan.go）共用同一份
// 「frontmatter 长什么样」的判定，否则「扫盘认了、读正文认不出」会变成静默的空正文。
func stripSkillFrontmatter(md string) (string, error) {
	trimmed := strings.TrimLeft(md, " \t\r\n")
	trimmed = strings.TrimPrefix(trimmed, "\ufeff")
	if !strings.HasPrefix(trimmed, "---") {
		return "", errors.New("missing frontmatter")
	}
	rest := strings.TrimLeft(trimmed[3:], "\r\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", errors.New("frontmatter 未闭合")
	}
	body := rest[end+len("\n---"):]
	// 关闭行自己那一行的剩余部分（行尾空格、\r）丢掉；正文从下一行开始。
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		return "", nil
	}
	return strings.Trim(body, "\r\n"), nil
}

// renderSkillBody 把一次「读正文」的结果渲染成工具结果（doc/52 §2.10/§8.2-2）：
// 成功走 XML 原文，失败走 JSON 信封。
//
// 上限判在**转义前**的正文上：转义是展示形态，作者的字节才是「要进上下文多少东西」
// 的口径（转义只会让字节变多，按转义后判会把本来合法的正文拒掉）。
func renderSkillBody(row SkillRow, body string, counter TokenCounter) string {
	return renderSkillTag(row, body, counter, "")
}

// renderUsedSkillBody 是 use_skill 的成功形态（doc/52 D38）：标签多两个属性——
// requires 是声明原文（空格连接），missing 是**此刻**按过滤后的 PATH 现查的结果。
// 没声明 requires 就两个属性都不写（不写 `requires="" missing=""`：空声明不是事实）。
//
// 正文与 read_skill **逐字节相同**，差的只有这两个属性（§2.3「读」与「用」靠工具名区分）。
// 缺依赖不阻断：正文可能只用不需要它的那部分（§9-13）；missing 是给模型的提前量，不是闸。
func renderUsedSkillBody(row SkillRow, body string, counter TokenCounter, tenantRoot string) string {
	requires := parseSkillRequires(row.Requires)
	if len(requires) == 0 {
		return renderSkillBody(row, body, counter)
	}
	missing := missingSkillDependencies(requires, tenantRoot)
	extra := fmt.Sprintf(` requires="%s" missing="%s"`,
		escapeSkillXML(strings.Join(requires, " ")), escapeSkillXML(strings.Join(missing, " ")))
	return renderSkillTag(row, body, counter, extra)
}

// renderSkillTag 拼 `<skill …>` 块；extra 是 use_skill 才有的属性（带前导空格的空串表示没有）。
func renderSkillTag(row SkillRow, body string, counter TokenCounter, extra string) string {
	if counter.Count(body) > skillBodyMaxTokens || len(body) > skillBodyMaxBytes {
		return skillFailEnvelope(codeSkillBodyTooLarge, "技能正文超过上下文预算（16,384 token 或 64KB），没有读取", "")
	}
	return fmt.Sprintf("<skill name=\"%s\" source=\"%s\" bytes=\"%d\"%s>\n%s\n</skill>",
		escapeSkillXML(row.Name), escapeSkillXML(row.Scope), row.Bytes, extra, escapeSkillXML(body))
}

// skillFailEnvelope 是技能工具失败时的统一回执（doc/52 §8.2-2）：工具结果的 JSON 信封，
// 不是 XML——模型能读到 code 与 message，UI 拿 toolCallId 找到对应调用展示 message。
// name 为空时 data 落 null（116116 不带名字：问题在体积，不在定位哪个技能）。
func skillFailEnvelope(code int, message, name string) string {
	type skillNameData struct {
		Name string `json:"name"`
	}
	payload := struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    *skillNameData `json:"data"`
	}{Code: code, Message: message}
	if name != "" {
		payload.Data = &skillNameData{Name: name}
	}
	raw, err := json.Marshal(payload)
	if err != nil { // 三个字段都是字符串/数字，编码不可能失败；真失败只可能是结构被改坏
		return `{"code":111901,"message":"服务暂时不可用，请再试一次","data":null}`
	}
	return string(raw)
}

// 失败文案（doc/52 §8.2-2 给了 116001 这句原文；116107／116114／116115／116116 只钉了码与形态）。
// 116001 同时用于「表里没有」与「行在盘无」——对调用方是同一件事：这个技能现在读不到。
const (
	skillMissingMsg      = "找不到这个技能，可能已被删除或改名"
	skillDisabledMsg     = "这个技能已被禁用，请先在管理页恢复"
	skillPathBadMsg      = "这个路径在技能资源里读不到；入口正文请用 read_skill 或 use_skill"
	skillFileTooLargeMsg = "资源文件超过单次读取上限（16,384 token 或 64KB），没有读取"
)

type readSkillInput struct {
	Name string `json:"name" jsonschema:"description=技能名字（与目录里的 name 一致）"`
}

type readSkillFileInput struct {
	Name string `json:"name" jsonschema:"description=技能名字（与目录里的 name 一致）"`
	Path string `json:"path" jsonschema:"description=相对该技能根的路径，如 references/fields.md"`
}

// runSkillLoad 是 GET /api/runs/:id/skills 的一行：本场 run 对技能的一次动作（doc/52 §8.2-3）。
// 从 runlog 的 read_skill/use_skill/read_skill_file 行派生（loadsFromEntries）。
// path 只在 read_skill_file 行出现——记录里区分读法靠 tool 名，不靠字段空不空（§2.3）。
// 键序是契约：toolCallId → name → tool → path → agent → seq → ok → code。
type runSkillLoad struct {
	ToolCallID string `json:"toolCallId"`
	Name       string `json:"name"`
	Tool       string `json:"tool"`
	Path       string `json:"path,omitempty"`
	Agent      string `json:"agent"`
	Seq        int64  `json:"seq"`
	OK         bool   `json:"ok"`
	Code       int    `json:"code,omitempty"`
}

type runSkillsData struct {
	RunID RunID          `json:"runId"`
	Loads []runSkillLoad `json:"loads"`
}

// isSkillLoadTool 是「进加载列表」的三个工具。skill_install 不在里面：
// 它不读正文也不登记盒子，不是一次加载。
func isSkillLoadTool(name string) bool {
	return name == "read_skill" || name == "use_skill" || name == "read_skill_file"
}

// loadsFromEntries 从 run 的工具扩展行派生加载列表（doc/52 §8.2-3）。
//
// 配对键是 toolCallId：call 行给 name/path（args 里的调用参数），result 行给结局。
// 结局两种形态——正文工具成功是 XML（`<skill …>` 开头），失败与 read_skill_file 都是
// JSON 信封（`code==0` 即成功）。配对不上的 call（run 中断、result 没落）记 ok=false、
// 不带 code：事实就是「调了，没见到结果」。
// 顺序按 seq（runlog 的自增序号），不按 created_at（EndRun 才写，全是同一时刻）。
func loadsFromEntries(entries []RunlogEntry) []runSkillLoad {
	out := make([]runSkillLoad, 0)
	results := make(map[string]toolResultPayload, len(entries))
	for _, e := range entries {
		if e.Role != "tool_result" || e.Payload == "" {
			continue
		}
		var p toolResultPayload
		if json.Unmarshal([]byte(e.Payload), &p) != nil || p.ToolCallID == "" {
			continue
		}
		results[p.ToolCallID] = p
	}
	for _, e := range entries {
		if e.Role != "tool_call" || e.Payload == "" {
			continue
		}
		var call toolCallPayload
		if json.Unmarshal([]byte(e.Payload), &call) != nil || !isSkillLoadTool(call.Name) {
			continue
		}
		var args readSkillFileInput
		_ = json.Unmarshal([]byte(call.Args), &args)
		load := runSkillLoad{
			ToolCallID: call.ToolCallID,
			Name:       args.Name,
			Tool:       call.Name,
			Agent:      e.Agent,
			Seq:        e.Seq,
		}
		if call.Name == "read_skill_file" {
			load.Path = args.Path
		}
		if res, ok := results[call.ToolCallID]; ok && call.ToolCallID != "" {
			load.OK, load.Code = skillOutcome(res.Output)
		}
		out = append(out, load)
	}
	return out
}

// skillOutcome 判定一次技能工具结果是否成功，返回 (ok, code)。
// 正文工具成功回 XML，失败回 JSON 信封；read_skill_file 两种结局都是 JSON 信封。
// 认不出的形态按失败算——「不知道它成功」不能记成成功。
func skillOutcome(output string) (bool, int) {
	if strings.HasPrefix(strings.TrimSpace(output), "<skill") {
		return true, 0
	}
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal([]byte(output), &env); err != nil {
		return false, 0
	}
	return env.Code == 0, env.Code
}

// skillToolbox 是技能工具的实现体：一个 Store（按租户视界查行）+ 一把自己构造的计数器。
//
// 计数器**懒构造**（doc/52 §10.1-10）：不能复用 `-compact-window>0` 时 main 造的那把——
// 关掉压缩时它是 nil，而正文上限（D26）与压缩无关、必须照判；也不在装配期立刻造：
// `-compact-window=0` 是调试逃生口，不该因为词表拉不到而起不来。第一次真读正文时才拉词表，
// 拉不到就让**这次调用**失败（错误文本进模型）——静默跳过判据是 §2.3 明令禁止的。
type skillToolbox struct {
	store   *Store
	once    sync.Once
	counter TokenCounter
	err     error
}

func (t *skillToolbox) tokenCounter() (TokenCounter, error) {
	t.once.Do(func() { t.counter, t.err = NewCl100kCounter() })
	return t.counter, t.err
}

// skillResult 是一次「按名字取正文」的结局：OK 为真时 Body 是渲染好的 XML、Row 是表行；
// OK 为假时 Body 是失败信封、Row 为零值。
type skillResult struct {
	Row  SkillRow
	Body string
	OK   bool
}

// loadByName 是 read_skill 与 use_skill 共用的读取核（doc/52 §2.3）：按名字取本租户视界内的行 →
// 禁用闸 → 读盘 → 剥 frontmatter → 转义渲染。只读，不碰看板、不写盘、不改任何 run 状态。
//
// forUse 只决定标签形态：use_skill 多带 requires/missing（D38，现查不缓存），
// read_skill 是纯正文。两条路的正文逐字节相同。
//
// 名字按**租户视界**解析（§4.3）：别的租户的同名技能会走「表里没有」这一条，
// 返回的字节与查一个不存在的名字完全一样——存在性不跨租户泄漏。
func (t *skillToolbox) loadByName(ctx context.Context, name string, forUse bool) (skillResult, error) {
	sc := scopeFrom(ctx)
	row, ok, err := t.store.GetSkill(ctx, sc.TenantID, name)
	if err != nil {
		return skillResult{}, err
	}
	if !ok {
		return skillResult{Body: skillFailEnvelope(codeSkillMissing, skillMissingMsg, name)}, nil
	}
	// 作用域外的名字与「表里没有」回同一个字节（E1）：这是这只助手看得到哪些的闸，
	// 不是权限错误的提示——模型能做的动作与查一个不存在的名字相同。
	if !skillScopeFrom(ctx).allows(name) {
		return skillResult{Body: skillFailEnvelope(codeSkillMissing, skillMissingMsg, name)}, nil
	}
	disabled, err := t.store.ListSkillDisables(ctx, sc.TenantID)
	if err != nil {
		return skillResult{}, err
	}
	if disabled[name] {
		return skillResult{Body: skillFailEnvelope(codeSkillDisabled, skillDisabledMsg, name)}, nil
	}
	body, err := readSkillEntryBody(row)
	if err != nil {
		// 行在盘无（手工删目录未重扫）与 frontmatter 读不懂都按「找不到」回：
		// 调用方能做的动作相同（改道或让管理员重扫），具体原因进日志不进工具结果。
		log.Printf("技能 %q: %v", name, err)
		return skillResult{Body: skillFailEnvelope(codeSkillMissing, skillMissingMsg, name)}, nil
	}
	counter, err := t.tokenCounter()
	if err != nil {
		return skillResult{}, fmt.Errorf("技能正文计数器不可用: %w", err)
	}
	if forUse {
		return skillResult{Row: row, Body: renderUsedSkillBody(row, body, counter, tenantRootFor(ctx, runWorkspaceRoot(ctx))), OK: true}, nil
	}
	return skillResult{Row: row, Body: renderSkillBody(row, body, counter), OK: true}, nil
}

// readSkill 是 read_skill 的真身：只取正文，不登记盒子。
func (t *skillToolbox) readSkill(ctx context.Context, in readSkillInput) (string, error) {
	res, err := t.loadByName(ctx, strings.TrimSpace(in.Name), false)
	return res.Body, err
}

// useSkill 是 use_skill 的真身：正文与 read_skill 逐字节相同（§2.3「读」与「用」靠工具名区分），
// 成功时再把这个技能根登记进 run 盒子——之后 bash 的围栏才对它放只读（D25）。
// 失败不登记：模型没读到正文就不该获得这个目录的读权。
func (t *skillToolbox) useSkill(ctx context.Context, in readSkillInput) (string, error) {
	name := strings.TrimSpace(in.Name)
	res, err := t.loadByName(ctx, name, true)
	if err != nil || !res.OK {
		return res.Body, err
	}
	recordUsedSkill(ctx, res.Row)
	return res.Body, nil
}

// recordUsedSkill 把一次成功的 use_skill 记进 run 盒子。拆出来是为了让单测不碰库；
// 没有盒子时只记日志——工具结果照常返回，围栏不会对这个根放行。
func recordUsedSkill(ctx context.Context, row SkillRow) {
	b := skillBoxFrom(ctx)
	if b == nil {
		log.Printf("use_skill %q: 本场没有技能盒子，围栏不会打开这个技能根", row.Name)
		return
	}
	root, ok := skillResourceRoot(row)
	if !ok {
		// 平铺形态（<name>.md）没有资源目录：不能拿它所在目录当根，否则整层技能目录都放行了。
		return
	}
	b.add(row.Name, root)
} // readSkillFile 是 read_skill_file 的真身（doc/52 §2.3/§8.2-2）：按名字取行 → 禁用闸 →
// 在**这个技能的资源根**内解析 path → 文件回 JSON（content/bytes）、目录回不递归的条目列表。
// 资源边界全靠 `path` 先拼到根下再 `EvalSymlinks` 后检查包含关系——不依赖沙箱，
// 也不依赖谁在调；读不到隔壁技能、读不到宿主机任意文件。
func (t *skillToolbox) readSkillFile(ctx context.Context, in readSkillFileInput) (string, error) {
	name := strings.TrimSpace(in.Name)
	sc := scopeFrom(ctx)
	row, ok, err := t.store.GetSkill(ctx, sc.TenantID, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return skillFailEnvelope(codeSkillMissing, skillMissingMsg, name), nil
	}
	if !skillScopeFrom(ctx).allows(name) {
		return skillFailEnvelope(codeSkillMissing, skillMissingMsg, name), nil
	}
	disabled, err := t.store.ListSkillDisables(ctx, sc.TenantID)
	if err != nil {
		return "", err
	}
	if disabled[name] {
		return skillFailEnvelope(codeSkillDisabled, skillDisabledMsg, name), nil
	}
	counter, err := t.tokenCounter()
	if err != nil {
		return "", fmt.Errorf("技能资源计数器不可用: %w", err)
	}
	return readSkillResource(row, in.Path, counter), nil
}

// skillResourceRoot 给出一个技能的资源根：目录形态（入口是 `<name>/SKILL.md`）是入口文件所在目录；
// 平铺形态（`<name>.md`）自己就是根、没有资源目录——第二个返回 false，调用方回 116114（§2.3）。
// 不把平铺形态的所在目录当根：那会让 `path:"别的技能.md"` 读到隔壁技能。
func skillResourceRoot(row SkillRow) (string, bool) {
	if filepath.Base(row.Path) != "SKILL.md" {
		return "", false
	}
	return filepath.Dir(row.Path), true
}

// cleanSkillRel 校验并规整调用方给的相对路径（doc/52 §2.3）：
// 空、绝对路径、任何一段 `..` 一律拒——**不**先拼再靠 Clean 折叠（`a/../b` 能折回根内，
// 但「含 `..` 就拒」是契约原文，先拒后拼才不会被符号链接与折叠规则绕出两套语义）。
// `./x` 这类可折叠的前缀放行：它不改变落点，模型也常这么写。
func cleanSkillRel(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || filepath.IsAbs(p) {
		return "", false
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return "", false
		}
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == ".." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return clean, true
}

// pathWithin 判定 target 是否落在 root 内（两侧都应是 EvalSymlinks 之后的真实路径）。
// rel=="." 也算在内（目标就是根自己）；`..` 前缀才是出界。
func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// skillFileData / skillDirData 是 read_skill_file 成功时的 data（doc/52 §8.2-2）。
// 模式由显式的 `type` 字段承载：file 带 content/bytes，dir 带 entries，两者不混用；
// path 回显调用方传的相对路径（入口文件的绝对路径不外给）。
// 键序是契约：name → path → type → content/entries → bytes。
type skillFileData struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Bytes   int64  `json:"bytes"`
}

type skillDirEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Bytes *int64 `json:"bytes,omitempty"`
}

type skillDirData struct {
	Name    string          `json:"name"`
	Path    string          `json:"path"`
	Type    string          `json:"type"`
	Entries []skillDirEntry `json:"entries"`
}

// skillOKEnvelope 是成功回执的信封（与失败信封同一套 code/message/data，doc/52 §8.2-2）。
func skillOKEnvelope(data any) string {
	payload := struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data"`
	}{Code: codeOK, Data: data}
	raw, err := json.Marshal(payload)
	if err != nil { // data 全是字符串/数字/切片，编码不可能失败；真失败只可能是结构被改坏
		return skillFailEnvelope(codeInternal, "服务暂时不可用，请再试一次", "")
	}
	return string(raw)
}

// readSkillResource 在技能根内读一个相对路径（doc/52 §2.3）。纯函数（不碰库），便于单测。
//
// 判定顺序：形态（平铺没有资源）→ 路径字面 → 根/目标的真实路径 → 包含关系 → 入口文件
// → 目录列条目 → 普通文件读取 → 上限。**越界与缺失回同一个 116114、同一句文案**：
// 调用方能做的动作相同，且不透露根外有没有那个东西。
// 上限与正文同一条判据（16,384 token 或 64KB，D26/§9-31）：超了回 116115，一个字节都不带。
func readSkillResource(row SkillRow, rawPath string, counter TokenCounter) string {
	root, ok := skillResourceRoot(row)
	if !ok {
		return skillFailEnvelope(codeSkillPathBad, skillPathBadMsg, row.Name)
	}
	rel, ok := cleanSkillRel(rawPath)
	if !ok {
		return skillFailEnvelope(codeSkillPathBad, skillPathBadMsg, row.Name)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return skillFailEnvelope(codeSkillPathBad, skillPathBadMsg, row.Name)
	}
	target, err := filepath.EvalSymlinks(filepath.Join(rootReal, rel))
	if err != nil {
		return skillFailEnvelope(codeSkillPathBad, skillPathBadMsg, row.Name)
	}
	if !pathWithin(rootReal, target) {
		return skillFailEnvelope(codeSkillPathBad, skillPathBadMsg, row.Name)
	}
	info, err := os.Stat(target)
	if err != nil {
		return skillFailEnvelope(codeSkillPathBad, skillPathBadMsg, row.Name)
	}
	if entry, err := os.Stat(filepath.Join(rootReal, "SKILL.md")); err == nil && os.SameFile(info, entry) {
		return skillFailEnvelope(codeSkillPathBad, skillPathBadMsg, row.Name)
	}
	display := strings.TrimSpace(rawPath)
	if info.IsDir() {
		return skillOKEnvelope(skillDirData{
			Name: row.Name, Path: display, Type: "dir",
			Entries: skillDirEntries(target),
		})
	}
	if !info.Mode().IsRegular() {
		return skillFailEnvelope(codeSkillPathBad, skillPathBadMsg, row.Name)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return skillFailEnvelope(codeSkillPathBad, skillPathBadMsg, row.Name)
	}
	if len(raw) > skillBodyMaxBytes || counter.Count(string(raw)) > skillBodyMaxTokens {
		return skillFailEnvelope(codeSkillFileTooLarge, skillFileTooLargeMsg, row.Name)
	}
	return skillOKEnvelope(skillFileData{
		Name: row.Name, Path: display, Type: "file",
		Content: string(raw), Bytes: int64(len(raw)),
	})
}

// skillDirEntries 列一个目录的**直接**条目（不递归，doc/52 §8.2-2）：名字、类型、文件大小。
// 目录不带 bytes（不汇总、不递归）；符号链接按解析后的目标定类型，悬空链接跳过——
// 列一个读不了的条目只会让模型再失败一次。ReadDir 自带按名字排序。
func skillDirEntries(dir string) []skillDirEntry {
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return []skillDirEntry{}
	}
	out := make([]skillDirEntry, 0, len(dirents))
	for _, d := range dirents {
		info, err := os.Stat(filepath.Join(dir, d.Name()))
		if err != nil {
			continue
		}
		entry := skillDirEntry{Name: d.Name(), Type: "file"}
		if info.IsDir() {
			entry.Type = "dir"
		} else {
			size := info.Size()
			entry.Bytes = &size
		}
		out = append(out, entry)
	}
	return out
}

// readSkillEntryBody 读一行的入口文件并取出正文（frontmatter 之后的部分）。
func readSkillEntryBody(row SkillRow) (string, error) {
	raw, err := os.ReadFile(row.Path)
	if err != nil {
		return "", fmt.Errorf("读盘失败 %s: %w", row.Path, err)
	}
	body, err := stripSkillFrontmatter(string(raw))
	if err != nil {
		return "", fmt.Errorf("frontmatter 解析失败 %s: %w", row.Path, err)
	}
	return body, nil
}

// newSkillTools 构造四个技能工具（doc/52 §2.3 / §2.8）。read/use/file 的参数全必填，
// 没有可选字段——「读」与「用」靠工具名区分，不靠某个字段空不空（§2.3 的理由段）。
// skill_install（C3）吃来源串/命令串，自己只暂存；落盘由人批准后的端点执行。
func newSkillTools(store *Store, mediaRoot string) ([]tool.BaseTool, error) {
	box := &skillToolbox{store: store}
	readSkill, err := utils.InferTool("read_skill",
		"读一个技能的正文（SKILL.md 去掉 frontmatter 后整篇）。只读，没有副作用，不建看板任务。",
		box.readSkill)
	if err != nil {
		return nil, err
	}
	useSkill, err := utils.InferTool("use_skill",
		"读一个技能的正文并声明本场按它执行；之后建看板任务会带上这个技能名。要办事用它，只想看看用 read_skill。",
		box.useSkill)
	if err != nil {
		return nil, err
	}
	readSkillFileTool, err := utils.InferTool("read_skill_file",
		"读一个技能目录里的资源文件（相对技能根的 path）。path 指向目录时列出直接条目。入口文件请用 read_skill。",
		box.readSkillFile)
	if err != nil {
		return nil, err
	}
	installTool, err := newSkillInstallTool(store, mediaRoot)
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{readSkill, useSkill, readSkillFileTool, installTool}, nil
}
