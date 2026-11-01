// 本文件是 fileOps 机械路径保留的提取、分类与块往返（C5-A4 提取器 + C5-A5 钉块；
// 设计真源 = 19v6-07 §5「fileOps 式机械路径保留」）。块本体（标记+两组条目的渲染、
// 解析、合并）在本文件末节；钉块与三档收口检查的接线在 compact.go 的 foldInput。
//
// 动机（WeKnora 战例）：散文最先丢路径——模型丢掉「我写过 /workspace/output/deck.html」的
// 记忆后，会去重建磁盘上已存在的文件（大文件分块写 + 重启 = 永不终止的循环）。治法 = 从
// tool_calls 机械提取读写过的路径（全链路 0% LLM：LLM 做取舍、程序做保管），钉进摘要专用块跨压缩继承。
//
// 判定三分支（对每条工具调用依次问、先答上的为准）：
//
//	① 注册表声明位：第一方工具在注册元数据里声明 file_effect = none|read|write|edit。
//	   在注册表且字段非空 → 按声明入桶；不写 = 空串 = 按 none 处理（用户 2026-10-11 裁定）：
//	   无记录、不进 ②——想让提取搭理它就得显式声明；不在注册表（外来工具）→ ②。
//	② 外来前缀表（只服务没有声明的工具）：名字转小写后前缀命中写侧/读侧清单 → 对应桶。
//	③ shell 写意图表（v1）：参数 command 字段的写模式扫描——只抓写（防重建的强信号在写侧，
//	   纯读命令不产生记录）。
//
// 路径与桶的关系：一次调用的路径来源两处（参数 JSON 的 path 字段 + command 字段的 shell 扫描），
// 全部路径进同一个桶；桶由 ①② 判定，两侧都不命中（歧义）而确有路径 → 一律归「改过」——
// 误判改过 = 一行无害条目（上限自然淘汰），误判读过 = 防再生保险丝断在正主上。
//
// 有界性边界（诚实登记，设计同源）：解释器内部写盘（python heredoc 等）对模式规则不可见，
// 不追完备——组织面收口靠工具注册规约（声明位就是那个杠杆），shell 规则只兜底；
// 分类是调用时刻的形状判断，不做事后磁盘校验。
package runengine

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// fileOpEntry 是一条路径记录（三张清单一律用它）：路径 + 来源工具 + 来源 run。
// 来源注记必须随条目走——块里每行要写「（edit_file，r_XXX）」，裸字符串撑不住审计回指。
type fileOpEntry struct {
	path  string
	tool  string
	runID RunID
}

// fileOps 是一次压缩事件进行中的收集容器：三张清单各管一类效果。
// edited 独立于 written 收集只为保住来源注记（write_file 与 edit_file 碰过的分得开）；
// resolve 时两者合并进「改过」组渲染。
type fileOps struct {
	read    []fileOpEntry
	written []fileOpEntry
	edited  []fileOpEntry
}

// add 往指定清单追一条：同一张清单内按 path 去重（保留先进的一条）。
// 去重的「先进」= 收集时序；跨代继承时旧块条目先进池、新提取的排后——这个先后顺序就是淘汰顺序。
func (o *fileOps) add(bucket string, e fileOpEntry) {
	var list *[]fileOpEntry
	switch bucket {
	case "read":
		list = &o.read
	case "written":
		list = &o.written
	case "edited":
		list = &o.edited
	default:
		return
	}
	for _, x := range *list {
		if x.path == e.path {
			return
		}
	}
	*list = append(*list, e)
}

// file_effect 的取值域（注册表声明位；空串 = 不写 = 按 none 处理：无记录、不进启发式）。
const (
	fileEffectNone  = "none"
	fileEffectRead  = "read"
	fileEffectWrite = "write"
	fileEffectEdit  = "edit"
)

// firstPartyFileEffects 是第一方工具的 file_effect 声明表（注册表声明位的 v1 载体）：
// 工具名 → none/read/write/edit。F 系列逐拍加行——每落一个文件工具，这里与工具池
// （worker.go 装配点）同步登记，两个登记点必须成对（漏这行 = lint 启动失败）。
// get_time 无文件效应、名字不像文件工具，不登记（不在注册表 → 走 ②，前缀不命中、参数无 path → 自然无记录）。
// 转正点 = fileOps 提取接入生产链路（C5-A5）那拍：届时声明字段长进工具的注册元数据，本表随动。
var firstPartyFileEffects = map[string]string{
	"write_file":       fileEffectWrite,
	"read_text_file":   fileEffectRead,
	"read_file":        fileEffectRead,
	"edit_file":        fileEffectEdit,
	"move_file":        fileEffectEdit,
	"create_directory": fileEffectWrite,
	"list_directory":   fileEffectRead,
	"directory_tree":   fileEffectRead,
	"get_file_info":    fileEffectRead,
	"search_files":     fileEffectRead,
	"read_media_file":  fileEffectRead,
	// read_pdf_text（doc/32 A8-1）：读 PDF 文本层，路径仍在 workspace 圈内，声明 read。
	// 不进 fileEffectToolNames：那张表是原来的文件系统工具名，lint 只拦表内漏声明的名字。
	"read_pdf_text": fileEffectRead,
	// memory（M1）：笔记落 PG notes 表、不碰 workspace 路径——显式表态 none（planning §1-6，
	// 作者没声明 = 不碰文件的同一表态方式）。
	"memory": fileEffectNone,
	// search_history（H1、H3，doc/21 §3）：读 runlog_entries 只读不写；截断发生时把完整结果
	// 写进 workspace 的 .tako/search/（H3「截断落盘」）——声明 write。提取程序只扫调用参数、
	// 落盘路径程序生成不在参数里，不钉摘要块（接受现状，planning §5）。
	"search_history": fileEffectWrite,
	// bash（doc/17 §3）：command 字段由 ③ shell 写意图表扫写路径——登记 write 这条链才通
	// （登记 none/留空在 classifyToolCall 提前返回，command 不被扫，折叠丢写路径）。
	"bash": fileEffectWrite,
	// web_search（doc/22 §3，2026-10-18 切新 API 二稿）：每次搜索把结果全文写 workspace 的
	// .tako/search/（程序命名新文件、一次一写不覆盖，沿 search_history 的 spill 机制）——
	// 声明 write。提取程序只扫调用参数、参数里只有 query 没有 path，不会去猜 query 字符串。
	"web_search": fileEffectWrite,
	// web_fetch（doc/57 §2）：每次抓取把整页写进 workspace 的 .tako/fetch/（程序命名新文件、
	// 一次一写不覆盖）——声明 write。参数里只有 url 没有 path，提取程序不会去猜 URL。
	"web_fetch": fileEffectWrite,
}

// fileEffectToolNames 是启动 lint 的目录名单（v1 常量表，逐名带出处；精确命中用，不转小写）：
//
//	read_text_file  read_media_file  write_file  edit_file  move_file
//	create_directory  list_directory  directory_tree  get_file_info  search_files
//	    ← 前 10 个 = MCP filesystem 服务器标准工具（出处：modelcontextprotocol/servers）
//	read_file
//	    ← OpenHands / Open Interpreter 惯用名（write_file、edit_file 与上表重复，去重后净增这一个）
var fileEffectToolNames = map[string]bool{
	"read_text_file":   true,
	"read_media_file":  true,
	"write_file":       true,
	"edit_file":        true,
	"move_file":        true,
	"create_directory": true,
	"list_directory":   true,
	"directory_tree":   true,
	"get_file_info":    true,
	"search_files":     true,
	"read_file":        true,
	"bash":             true,
}

// lintFileEffects 是装配期的启动 lint（NewWorker 调一次，不在提取热路径）：注册表中工具名
// 精确命中目录名单、却未声明 file_effect（空串）⇒ 报错（启动失败）。显式 none = 表态豁免。
// 名单用精确匹配而 ② 用前缀：两张表的误报代价不对称——② 误报 = 清单多一行（宁多抓），
// lint 误报 = 阻断启动（宁漏不误杀）；名字像但不精确命中的（read_sensor）不受 lint 管辖。
func lintFileEffects(registry map[string]string) error {
	for name, effect := range registry {
		if effect == "" && fileEffectToolNames[name] {
			return fmt.Errorf("工具 %q 的名字在文件工具目录名单里，但未声明 file_effect——显式声明 none/read/write/edit 后才能启动", name)
		}
	}
	return nil
}

// extractFileOps 从一批模型侧消息的 tool_calls 列提取路径（纯函数，A5 在 fold 定案时喂被压段的行）。
// registry = 注册表声明位（工具名 → file_effect）；坏 arguments JSON 跳过 + log（不拖垮提取）。
func extractFileOps(msgs []ModelMessage, registry map[string]string) fileOps {
	var ops fileOps
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			collectToolCall(&ops, tc, m.RunID, registry)
		}
	}
	return ops
}

// collectToolCall 判定单条调用并落桶：无桶或无路径 = 不产生记录。
func collectToolCall(ops *fileOps, tc schema.ToolCall, runID RunID, registry map[string]string) {
	bucket, paths := classifyToolCall(tc.Function.Name, tc.Function.Arguments, registry)
	for _, p := range paths {
		ops.add(bucket, fileOpEntry{path: p, tool: tc.Function.Name, runID: runID})
	}
}

// classifyToolCall 判定一条调用的桶与路径（判定三分支 + 统一路径提取）。
// 返回 bucket ∈ ""|read|written|edited（空 = 无记录）与它名下的路径表。
func classifyToolCall(name, argsJSON string, registry map[string]string) (string, []string) {
	bucket := ""
	effect, inRegistry := registry[name]
	if inRegistry {
		switch effect {
		case "", fileEffectNone:
			// 不写 = 空串 = 按 none 处理（用户 2026-10-11 裁定）：作者没声明 = 不碰文件——
			// 无记录，也不进 ② 启发式；想让提取搭理它，显式声明 read/write/edit。
			return "", nil
		case fileEffectRead:
			bucket = "read"
		case fileEffectWrite:
			bucket = "written"
		case fileEffectEdit:
			bucket = "edited"
		default:
			return "", nil // 未知取值按 none 兜底（宁漏不误抓）
		}
	} else {
		// 不在注册表 = 外来工具 → ② 前缀表（小写比较）
		n := strings.ToLower(name)
		switch {
		case hasAnyPrefix(n, fileWritePrefixes):
			bucket = "written"
		case hasAnyPrefix(n, fileReadPrefixes):
			bucket = "read"
		}
	}
	paths := pathsFromArgs(argsJSON, name)
	if len(paths) == 0 {
		return "", nil
	}
	if bucket == "" {
		bucket = "written" // 歧义一律归「改过」（唯一触发点：无法判定的按强信号记）
	}
	return bucket, paths
}

// 外来工具前缀表（v1，只服务没有声明的工具；名字转小写后做前缀比较）。
// 写侧命中 → written 桶，读侧命中 → read 桶；两侧不命中且有路径 → 歧义档归 written。
var (
	fileWritePrefixes = []string{"write", "edit", "create", "save", "append", "touch"}
	fileReadPrefixes  = []string{"read", "cat", "ls", "grep", "glob", "find", "head", "tail", "stat"}
)

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// pathsFromArgs 从参数 JSON 提取全部路径：path 字段（非空）+ command 字段的 shell 写意图扫描。
// 坏 JSON：log 一行跳过——模型给了 garbage 不炸提取（也不猜）。JSON 不是对象（数字/数组）同路。
func pathsFromArgs(argsJSON, toolName string) []string {
	if strings.TrimSpace(argsJSON) == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		log.Printf("fileops: 工具 %s 的 arguments 非法 JSON，跳过: %v", toolName, err)
		return nil
	}
	var paths []string
	if p, ok := m["path"].(string); ok && p != "" {
		paths = append(paths, p)
	}
	if c, ok := m["command"].(string); ok && c != "" {
		paths = append(paths, shellWritePaths(c)...)
	}
	return paths
}

// shellWritePaths 从一条 shell command 扫描写意图路径（③，v1 模式规则）：
// 引号感知切词 → 按命令分隔词切段 → 段内逐词套写命令表。
// 只抓写：无重定向的纯读命令（cat / grep / ls）不产生记录。
func shellWritePaths(command string) []string {
	var paths []string
	for _, seg := range shellSegments(shellWords(command)) {
		paths = append(paths, segWritePaths(seg)...)
	}
	return paths
}

// shellWords 把命令切成词：空白分界，引号内空白不断词（词保留引号字符本身，剥壳在收路径时做）。
// v1 不处理转义与嵌套引号——未闭合引号按到串尾收词。
func shellWords(s string) []string {
	var words []string
	var b strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			b.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
			b.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if b.Len() > 0 {
				words = append(words, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		words = append(words, b.String())
	}
	return words
}

// shellSegments 按命令分隔词把词列切成子命令段（一段 = 一条命令的词列，写命令的参数范围到段尾为止）。
// v1 只认独立分隔 token；粘在词上的（"a;b"）不切。
func shellSegments(words []string) [][]string {
	var segs [][]string
	var cur []string
	for _, w := range words {
		switch w {
		case "&&", "||", ";", "|", "&", "|&":
			if len(cur) > 0 {
				segs = append(segs, cur)
				cur = nil
			}
		default:
			cur = append(cur, w)
		}
	}
	if len(cur) > 0 {
		segs = append(segs, cur)
	}
	return segs
}

// segWritePaths 对一个命令段套写命令表（v1，19v6-07 §5 钉死的清单）：
//
//	重定向：>  >>  2>  2>>  → 取下一个词
//	cp / mv：最后一个词
//	tee / touch / mkdir / install：命令之后所有不以 - 开头的词
//	sed / perl：段内存在独立 -i 时取最后一个词（v1 只认独立 token，-ie / -i.bak 不碰）
//	dd：所有 of= 前缀的词，取 = 后的值
//
// 明确不碰：变量展开（> $OUT）、命令替换（$(...)）——收路径时按 $、` 字符跳过；
// heredoc / 解释器内写盘对模式规则不可见（文件头「有界性边界」）。
// 命令名只在段首（i==0）参与匹配：段 = 一条命令（shellSegments 已按 &&/;/| 切过），
// 参数位置出现同名词是 echo/printf 的 payload（echo touch d1），不是命令；重定向是
// 操作符不是命令名，语法上就该出现在参数中间（echo hi > out），任意位置都算。
func segWritePaths(seg []string) []string {
	var paths []string
	add := func(s string) {
		p := stripQuotes(s)
		if p == "" || strings.ContainsAny(p, "$`") {
			return
		}
		paths = append(paths, p)
	}
	for i, w := range seg {
		switch w {
		case ">", ">>", "2>", "2>>":
			if i+1 < len(seg) {
				add(seg[i+1])
			}
		case "cp", "mv":
			// 段首才算命令；代价：sudo cp / time cp 这类包装命令漏抓（v1 只兜底，方向可接受）
			if i == 0 && i+1 < len(seg) {
				add(seg[len(seg)-1])
			}
		case "tee", "touch", "mkdir", "install":
			if i == 0 {
				for _, a := range seg[i+1:] {
					if !strings.HasPrefix(a, "-") {
						add(a)
					}
				}
			}
		case "sed", "perl":
			if i == 0 && hasToken(seg, "-i") && i+1 < len(seg) {
				add(seg[len(seg)-1])
			}
		case "dd":
			if i == 0 {
				for _, a := range seg {
					if v, ok := strings.CutPrefix(a, "of="); ok {
						add(v)
					}
				}
			}
		}
	}
	return paths
}

// stripQuotes 剥掉词首尾的一对同类引号（'…' 或 "…"）；不成对/未闭合原样返回。
func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func hasToken(seg []string, tok string) bool {
	for _, w := range seg {
		if w == tok {
			return true
		}
	}
	return false
}

// fileOpsTableCap 是减灾档的单表上限（19v6-07 §5 三档降级链，2026-10-11 用户裁定）：
// 常态不封顶；钉块时含块收口检查不过 → 两张表各砍到此数（读过 50、改过 50，公平对待、
// 无优先级），重渲染再验；仍超 → 折叠失败走干净 A。
const fileOpsTableCap = 50

// resolvedOps 是归并后的两张渲染表（A5 钉块时按此渲染）。
type resolvedOps struct {
	changed []fileOpEntry // 改过 = written ∪ edited
	read    []fileOpEntry // 读过 = read − 改过（resolve 保证两表无交集）
}

// resolveOps 把三张清单归并成两表（常态不封顶——三档降级链第 1 档）：
//   - 改过 = written 与 edited 合并（同 path 保留先进；written 条目先于 edited 入池）；
//   - 读过 = read 剔除改过表里的 path（写过的不会出现在读过里，与收集顺序无关）；
//   - 封顶是减灾档动作（第 2 档）：capOps 由 A5 在含块收口检查不过时调用，不在此处常驻。
func resolveOps(ops fileOps) resolvedOps {
	changed := make([]fileOpEntry, 0, len(ops.written)+len(ops.edited))
	taken := make(map[string]bool, len(ops.written)+len(ops.edited))
	for _, list := range [][]fileOpEntry{ops.written, ops.edited} {
		for _, e := range list {
			if taken[e.path] {
				continue
			}
			taken[e.path] = true
			changed = append(changed, e)
		}
	}
	read := make([]fileOpEntry, 0, len(ops.read))
	for _, e := range ops.read {
		if taken[e.path] {
			continue
		}
		read = append(read, e)
	}
	return resolvedOps{changed: changed, read: read}
}

// capOps 淘汰最老：超出上限时从头部砍（进池先后 = 淘汰顺序，先进的先淘汰）。
// 身份 = 减灾档工具（A5 在含块收口检查不过时调用）；常态不调用——resolveOps 不封顶。
func capOps(list []fileOpEntry) []fileOpEntry {
	if len(list) <= fileOpsTableCap {
		return list
	}
	return list[len(list)-fileOpsTableCap:]
}

// renderFileOpLine 渲染一条路径行（A5 块的条目行格式）：
//
//   - /workspace/output/deck.html（write_file，r_XXXX）
func renderFileOpLine(e fileOpEntry) string {
	return fmt.Sprintf("- %s（%s，%s）", e.path, e.tool, e.runID)
}

// ---- 块往返（C5-A5）：格式常量 + 渲染/解析/合并。字节契约 = 19v6-07 §5 样本 ----

// fileOpsBlockOpen / fileOpsBlockClose 是块的稳定标记（含版本位 v1——程序靠它认块；
// 没有标记的旧散文摘要自然无从继承）。fileOpsBlockSep 是钉块时摘要正文与块之间的分隔。
const (
	fileOpsBlockOpen  = "<!-- tako:file-ops v1 -->"
	fileOpsBlockClose = "<!-- /tako:file-ops -->"
	fileOpsBlockSep   = "\n\n"
)

// renderFileOpsBlock 渲染块（改过组在前、读过组在后，组序恒定；空组整段省略——空槽省略的
// 同款语义）。两组都空 → 空串 = 无块（钉块点据此跳过检查，无 file 工具的会话零行为变化的落点）。
func renderFileOpsBlock(r resolvedOps) string {
	if len(r.changed) == 0 && len(r.read) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(fileOpsBlockOpen)
	b.WriteString("\n")
	if len(r.changed) > 0 {
		b.WriteString("改过:\n")
		for _, e := range r.changed {
			b.WriteString(renderFileOpLine(e))
			b.WriteString("\n")
		}
	}
	if len(r.read) > 0 {
		b.WriteString("读过:\n")
		for _, e := range r.read {
			b.WriteString(renderFileOpLine(e))
			b.WriteString("\n")
		}
	}
	b.WriteString(fileOpsBlockClose)
	return b.String()
}

// parseFileOpsBlock 解析一段摘要文本里的块（继承半边；失败 = 按空处理 + log，不拒收——
// 验货不扩面的同一原则）。取**最后一个**开标记：程序钉的块永远在摘要末尾，模型若把见过的
// 旧块 echo 进正文，靠后的才是权威那份——echo 冒充不了继承源。
func parseFileOpsBlock(text string) fileOps {
	i := strings.LastIndex(text, fileOpsBlockOpen)
	if i < 0 {
		return fileOps{}
	}
	rest := text[i+len(fileOpsBlockOpen):]
	j := strings.Index(rest, fileOpsBlockClose)
	if j < 0 {
		log.Printf("fileops: 摘要里的 file-ops 块缺闭标记，按空处理")
		return fileOps{}
	}
	var ops fileOps
	bucket := ""
	for _, raw := range strings.Split(rest[:j], "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
		case line == "改过:":
			bucket = "written"
		case line == "读过:":
			bucket = "read"
		case bucket == "" || !strings.HasPrefix(line, "- "):
			// 单行坏点跳过（逐行容错）：坏一行丢一行，不丢整块——保管方向宁多留
			log.Printf("fileops: file-ops 块里跳过无法解析的行: %q", line)
		default:
			e, ok := parseFileOpLine(strings.TrimPrefix(line, "- "))
			if !ok {
				log.Printf("fileops: file-ops 块条目行形状不对，跳过: %q", line)
				continue
			}
			ops.add(bucket, e)
		}
	}
	return ops
}

// parseFileOpLine 解析一条条目行（renderFileOpLine 的逆）：path（tool，runID）——
// path 取最后一个「（」之前（路径本身可能含括号）；工具名 / run id 里不会出现「，」。
func parseFileOpLine(s string) (fileOpEntry, bool) {
	open := strings.LastIndex(s, "（")
	if open <= 0 || !strings.HasSuffix(s, "）") {
		return fileOpEntry{}, false
	}
	parts := strings.Split(s[open+len("（"):len(s)-len("）")], "，")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fileOpEntry{}, false
	}
	return fileOpEntry{path: s[:open], tool: parts[0], runID: RunID(parts[1])}, true
}

// inheritFileOps 解析全部旧摘要文本里的块并合并（正常一代一块；多块按文本序，早的先进池）。
func inheritFileOps(oldTexts []string) fileOps {
	var ops fileOps
	for _, t := range oldTexts {
		ops = mergeFileOps(ops, parseFileOpsBlock(t))
	}
	return ops
}

// mergeFileOps 两批并一批：旧条目在前、新提取排后——这个先后就是淘汰顺序
// （capOps 砍头 = 最老先走）。跨批按 path 去重交给 resolveOps 的 taken 表。
func mergeFileOps(old, next fileOps) fileOps {
	return fileOps{
		read:    concatFileOpEntries(old.read, next.read),
		written: concatFileOpEntries(old.written, next.written),
		edited:  concatFileOpEntries(old.edited, next.edited),
	}
}

func concatFileOpEntries(a, b []fileOpEntry) []fileOpEntry {
	out := make([]fileOpEntry, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}
