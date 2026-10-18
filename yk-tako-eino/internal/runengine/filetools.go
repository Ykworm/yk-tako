// 本文件是 F 系列文件工具族（设计真源 = doc/19-v6/19v6-08；施工台账 = tmp/19-v6/filetools/）。
// F1 落「根基拍」：-workspace 旗标（main 侧）指定的作业根由本文件消费——resolveInWorkspace
// 是全部文件工具共用的路径闸，write_file 是首个真文件工具（同拍把 fileOps 提取器的声明位
// 与启动 lint 从空转带入转正，登记行见 fileops.go）。F2–F11 按台账逐拍加工具，全部入此文件。
//
// 路径约束（19v6-08 §2）不是体验决定，是事故防线：没有它，模型被诱导写 ../../.env 时工具
// 照办、run 还 complete；有它 = 工具返回错误文本（经 tools.go 的 wrapToolErrors 回注模型）、
// 模型换路子或放弃，run 不失败。约束形态：入参 path 一律相对 workspace 根（POSIX 风格）、
// 不接受绝对路径；join(根, path) → Clean 后必须仍以「根 + 分隔符」开头（或等于根——目录工具
// 的合法目标），否则报错。已登记盲区：symlink 逃逸（随沙箱 loop）、真沙箱（部署面）。
package runengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// resolveInWorkspace 把「相对根」的工具入参 path 解析成根内的绝对路径，越界即报错。
// 前提：root 已绝对化（main 启动期 Abs + MkdirAll 保证；单测用 t.TempDir()）。
// 前缀判定带分隔符（「根 + /」）防 /ws 与 /ws-other 这类前缀误判；等于根本身放行——
// 目录工具（list_directory 查 "."）要它，文件工具落上去会在 os 调用处得到 EISDIR 类错误。
func resolveInWorkspace(root, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("不接受绝对路径（path 须相对 workspace 根）：%s", path)
	}
	full := filepath.Join(root, path) // Join = 拼接 + Clean：../ 与冗余分隔符在这一步现形
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越出 workspace（%s）", path)
	}
	return full, nil
}

// writeFileInput 是 write_file 的入参 schema（InferTool 反射生成）；两字段都必填
// （json tag 不带 omitempty = required，同 tools.go 的 getTimeInput 注释）。
type writeFileInput struct {
	Path    string `json:"path" jsonschema:"description=相对 workspace 根的路径（POSIX 风格，如 notes/todo.txt）；不接受绝对路径"`
	Content string `json:"content" jsonschema:"description=要写入的完整文件内容（UTF-8 文本，原样写入）"`
}

// newWriteFileTool 造 write_file：整文件覆盖写、父目录自动建、已存在标注「（覆盖）」。
// 返回行「已写入 <path>（<n> 字节）」是活体验收的字节锚（纯文本直出，可被 curl 一眼验证，
// 同 get_time 约定）。错误路径全部返回 Go error，由装配点的 wrapToolErrors 转成回注模型的结果。
func newWriteFileTool(root string) (tool.BaseTool, error) {
	return utils.InferTool("write_file",
		"在 workspace 内创建或覆盖写入一个文件（整文件覆盖）。path 是相对 workspace 根的路径（如 notes/todo.txt）；"+
			"父目录不存在会自动创建；文件已存在则覆盖、返回文本会标注「（覆盖）」。返回格式：已写入 <path>（<n> 字节）。",
		func(_ context.Context, in writeFileInput) (string, error) {
			full, err := resolveInWorkspace(root, in.Path)
			if err != nil {
				return "", err
			}
			_, statErr := os.Stat(full)
			existed := statErr == nil
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", fmt.Errorf("创建父目录失败：%v", err)
			}
			if err := os.WriteFile(full, []byte(in.Content), 0o644); err != nil {
				return "", fmt.Errorf("写入失败：%v", err)
			}
			if existed {
				return fmt.Sprintf("已写入 %s（%d 字节）（覆盖）", in.Path, len(in.Content)), nil
			}
			return fmt.Sprintf("已写入 %s（%d 字节）", in.Path, len(in.Content)), nil
		})
}

// readTextFileInput 是 read_text_file 的入参 schema：head/tail 用指针区分「没给」与「给了 0」，
// 两者都非必填（omitempty）；同给 = 报错（19v6-08 §3「head/tail 同时给出 → 错误」）。
type readTextFileInput struct {
	Path string `json:"path" jsonschema:"description=相对 workspace 根的路径（POSIX 风格，如 notes/todo.txt）"`
	Head *int   `json:"head,omitempty" jsonschema:"description=只读前 N 行（与 tail 二选一，同给报错）"`
	Tail *int   `json:"tail,omitempty" jsonschema:"description=只读后 N 行（与 head 二选一，同给报错）"`
}

// newReadTextFileTool / newReadFileTool（F4）是同一实现的两位出口：read_text_file 是 MCP
// filesystem 标准名，read_file 是 OpenHands 系惯用名——别名按真名进声明面（模型喊谁就是谁，
// 两把工具同时在池，同上）而不是给一把工具挂双名参数，声明面看到的始终是模型将调用的名字。
// 实现：纯文本直出原文；>200KB 且无截读参数 = 错误提示（无界读 = 一次工具调用灌爆上下文；
// 200KB 是减灾档不是安全边界，19v6-08 §3）。head/tail 按行截读；截读分支先剪掉文件尾的
// 那一个换行再分行——文件以 \n 收尾是常态，照 Split 直切会把空串当最后一行，tail N 会少返回一行。
func newReadTextFileTool(root string) (tool.BaseTool, error) {
	return buildReadTool("read_text_file",
		"读取 workspace 内的文本文件，内容原样直出。path 是相对 workspace 根的路径（如 notes/todo.txt）。"+
			"文件超过 200KB 时请改用 head（只读前 N 行）或 tail（只读后 N 行）截读——两者二选一，同给会报错。", root)
}

// newReadFileTool 造 read_file：惯用名别名，参数与行为等同 read_text_file（19v6-08 §3 名单第 11 名）。
func newReadFileTool(root string) (tool.BaseTool, error) {
	return buildReadTool("read_file",
		"读取 workspace 内的文本文件（惯用名，等同 read_text_file：同参同行为）。path 是相对 workspace 根的路径。"+
			"文件超过 200KB 时请改用 head（只读前 N 行）或 tail（只读后 N 行）截读——两者二选一，同给会报错。", root)
}

// buildReadTool 是读工具的两个名字共用的装配体（名字/描述随调用方，行为一份）。
func buildReadTool(name, desc, root string) (tool.BaseTool, error) {
	return utils.InferTool(name, desc,
		func(_ context.Context, in readTextFileInput) (string, error) {
			full, err := resolveInWorkspace(root, in.Path)
			if err != nil {
				return "", err
			}
			if in.Head != nil && in.Tail != nil {
				return "", fmt.Errorf("head 与 tail 不能同时给（截读二选一）")
			}
			if in.Head != nil && *in.Head <= 0 {
				return "", fmt.Errorf("head 须为正整数（给的是 %d）", *in.Head)
			}
			if in.Tail != nil && *in.Tail <= 0 {
				return "", fmt.Errorf("tail 须为正整数（给的是 %d）", *in.Tail)
			}
			fi, err := os.Stat(full)
			if err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("文件不存在：%s", in.Path)
				}
				return "", fmt.Errorf("无法访问 %s：%v", in.Path, err)
			}
			if fi.IsDir() {
				return "", fmt.Errorf("%s 是目录不是文件", in.Path)
			}
			const maxBytes = 200 * 1024
			if in.Head == nil && in.Tail == nil && fi.Size() > maxBytes {
				return "", fmt.Errorf("文件 %s 有 %d 字节、超过 200KB 上限，无界读会灌爆上下文——请用 head（前 N 行）或 tail（后 N 行）截读", in.Path, fi.Size())
			}
			raw, err := os.ReadFile(full)
			if err != nil {
				return "", fmt.Errorf("读取失败：%v", err)
			}
			text := string(raw)
			if in.Head == nil && in.Tail == nil {
				return text, nil
			}
			lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
			if in.Head != nil {
				if n := *in.Head; n < len(lines) {
					lines = lines[:n]
				}
			} else if n := *in.Tail; n < len(lines) {
				lines = lines[len(lines)-n:]
			}
			return strings.Join(lines, "\n"), nil
		})
}

// editFileInput 是 edit_file 的入参 schema：replace_all 默认 false（omitempty）——
// 「不提全部就要求唯一」是防误伤：多命中时程序不动手，把选择权退回模型。
type editFileInput struct {
	Path       string `json:"path" jsonschema:"description=相对 workspace 根的路径"`
	OldText    string `json:"old_text" jsonschema:"description=要被替换的原文（须与文件中某段逐字符一致）"`
	NewText    string `json:"new_text" jsonschema:"description=替换后的新文本"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"description=替换全部命中；默认 false = 只替换恰好一处命中（0 处或多处都报错）"`
}

// newEditFileTool 造 edit_file：精确串替换，全链路无模糊匹配——匹配不上就报错、把「改哪处」
// 留给模型补上下文，而不是程序猜。默认恰一处命中才动（0 处 = 没有这段；>1 处 = 报错带次数）；
// replace_all=true 才整文件替换全部命中。返回行「已编辑 <path>（替换 n 处）」是字节锚。
func newEditFileTool(root string) (tool.BaseTool, error) {
	return utils.InferTool("edit_file",
		"在 workspace 内编辑文本文件：把 old_text 精确替换成 new_text。path 是相对 workspace 根的路径；"+
			"old_text 必须与文件中某段逐字符一致。默认只替换恰好一处命中——0 处或多处命中都会报错；多处时请补足前后文让它唯一，或显式 replace_all:true 全部替换。"+
			"返回格式：已编辑 <path>（替换 n 处）。",
		func(_ context.Context, in editFileInput) (string, error) {
			full, err := resolveInWorkspace(root, in.Path)
			if err != nil {
				return "", err
			}
			if in.OldText == "" {
				return "", fmt.Errorf("old_text 不能为空")
			}
			fi, err := os.Stat(full)
			if err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("文件不存在：%s", in.Path)
				}
				return "", fmt.Errorf("无法访问 %s：%v", in.Path, err)
			}
			if fi.IsDir() {
				return "", fmt.Errorf("%s 是目录不是文件", in.Path)
			}
			raw, err := os.ReadFile(full)
			if err != nil {
				return "", fmt.Errorf("读取失败：%v", err)
			}
			text := string(raw)
			count := strings.Count(text, in.OldText)
			if count == 0 {
				return "", fmt.Errorf("原文中没有这段内容（old_text 在 %s 中不存在）——请先读文件确认 old_text 逐字符一致", in.Path)
			}
			if count > 1 && !in.ReplaceAll {
				return "", fmt.Errorf("old_text 在 %s 中出现 %d 次、不唯一——补足前后文让它恰好一处，或显式 replace_all:true 全部替换", in.Path, count)
			}
			out := strings.ReplaceAll(text, in.OldText, in.NewText)
			if err := os.WriteFile(full, []byte(out), fi.Mode().Perm()); err != nil {
				return "", fmt.Errorf("写入失败：%v", err)
			}
			return fmt.Sprintf("已编辑 %s（替换 %d 处）", in.Path, count), nil
		})
}

// moveFileInput 是 move_file 的入参 schema。
type moveFileInput struct {
	Path        string `json:"path" jsonschema:"description=源路径（相对 workspace 根）"`
	Destination string `json:"destination" jsonschema:"description=目标路径（相对 workspace 根）；目标已存在时不覆盖、直接报错"`
}

// newMoveFileTool 造 move_file：移动/重命名文件或目录。不覆盖既有目标（覆盖是静默丢内容的
// 入口，退回模型抉择）；目标父目录自动建。返回「已移动 <path> → <destination>」。
func newMoveFileTool(root string) (tool.BaseTool, error) {
	return utils.InferTool("move_file",
		"在 workspace 内移动或重命名文件/目录。path 是源、destination 是目标（都相对 workspace 根）；"+
			"目标已存在时不覆盖、直接报错；目标的父目录不存在会自动创建。返回格式：已移动 <path> → <destination>。",
		func(_ context.Context, in moveFileInput) (string, error) {
			src, err := resolveInWorkspace(root, in.Path)
			if err != nil {
				return "", err
			}
			dst, err := resolveInWorkspace(root, in.Destination)
			if err != nil {
				return "", err
			}
			if _, err := os.Stat(src); err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("源不存在：%s", in.Path)
				}
				return "", fmt.Errorf("无法访问 %s：%v", in.Path, err)
			}
			if _, err := os.Stat(dst); err == nil {
				return "", fmt.Errorf("目标已存在：%s（move_file 不覆盖）", in.Destination)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return "", fmt.Errorf("创建父目录失败：%v", err)
			}
			if err := os.Rename(src, dst); err != nil {
				return "", fmt.Errorf("移动失败：%v", err)
			}
			return fmt.Sprintf("已移动 %s → %s", in.Path, in.Destination), nil
		})
}

// createDirectoryInput 是 create_directory 的入参 schema。
type createDirectoryInput struct {
	Path string `json:"path" jsonschema:"description=要创建的目录（相对 workspace 根，可多级如 notes/2026）"`
}

// newCreateDirectoryTool 造 create_directory：递归建；已存在（且是目录）= 幂等成功。
func newCreateDirectoryTool(root string) (tool.BaseTool, error) {
	return utils.InferTool("create_directory",
		"在 workspace 内创建目录（多级路径会递归创建）。path 是相对 workspace 根的路径；目录已存在时幂等成功。返回格式：已建目录 <path>。",
		func(_ context.Context, in createDirectoryInput) (string, error) {
			full, err := resolveInWorkspace(root, in.Path)
			if err != nil {
				return "", err
			}
			fi, statErr := os.Stat(full)
			switch {
			case statErr == nil && !fi.IsDir():
				return "", fmt.Errorf("%s 已存在且不是目录", in.Path)
			case statErr == nil:
				return fmt.Sprintf("已建目录 %s（已存在）", in.Path), nil
			}
			if err := os.MkdirAll(full, 0o755); err != nil {
				return "", fmt.Errorf("创建目录失败：%v", err)
			}
			return fmt.Sprintf("已建目录 %s", in.Path), nil
		})
}

// listDirectoryInput 是 list_directory 的入参 schema。
type listDirectoryInput struct {
	Path string `json:"path" jsonschema:"description=要列出的目录（相对 workspace 根，如 src；根目录用 .）"`
}

// newListDirectoryTool 造 list_directory：每行一个条目、目录带 / 后缀、按文件名升序
// （os.ReadDir 自带排序）、不输出 ./..；空目录直出「（空目录）」。纯名不带大小/时间——
// 这是给模型看的省 token 形态，元信息另有 get_file_info。
func newListDirectoryTool(root string) (tool.BaseTool, error) {
	return utils.InferTool("list_directory",
		"列出 workspace 内一个目录的直接子项，每行一个名字（目录以 / 结尾，按名排序）。path 是相对 workspace 根的路径；空目录返回（空目录）。",
		func(_ context.Context, in listDirectoryInput) (string, error) {
			full, err := resolveInWorkspace(root, in.Path)
			if err != nil {
				return "", err
			}
			fi, err := os.Stat(full)
			if err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("目录不存在：%s", in.Path)
				}
				return "", fmt.Errorf("无法访问 %s：%v", in.Path, err)
			}
			if !fi.IsDir() {
				return "", fmt.Errorf("%s 不是目录", in.Path)
			}
			entries, err := os.ReadDir(full)
			if err != nil {
				return "", fmt.Errorf("读取目录失败：%v", err)
			}
			if len(entries) == 0 {
				return "（空目录）", nil
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				names = append(names, name)
			}
			return strings.Join(names, "\n"), nil
		})
}

// treeNodeCap 是 directory_tree 的节点减灾档：500 个条目封顶，超出以「（已截断）」收尾——
// 深树/宽树一眼看结构，又不给一次工具调用灌爆上下文的机会。
const treeNodeCap = 500

// directoryTreeInput 是 directory_tree 的入参 schema：path 不写 = 根 "."；depth 上限 5。
type directoryTreeInput struct {
	Path  string `json:"path,omitempty" jsonschema:"description=起始目录（相对 workspace 根）；不写 = 工作区根 ."`
	Depth *int   `json:"depth,omitempty" jsonschema:"description=最大展示层级，默认 3、上限 5（超出按 5 计）"`
}

// newDirectoryTreeTool 造 directory_tree：两空格一级缩进、目录带 / 后缀；超过 depth 的目录
// 以「…」标记不再展开（只在真有下一层时出现）。depth 超上限是**收窄**不是错误（减灾档语义，
// 同 200KB 之于读：方向都是「少给」，给不出错的机会）。
func newDirectoryTreeTool(root string) (tool.BaseTool, error) {
	return utils.InferTool("directory_tree",
		"以缩进树列出 workspace 内目录结构（两空格一级，目录以 / 结尾）。path 是起始目录（不写 = 根）；"+
			"depth 默认 3、上限 5。超过 depth 的目录以 … 标记不再展开；条目超 500 会截断。",
		func(_ context.Context, in directoryTreeInput) (string, error) {
			p := in.Path
			if p == "" {
				p = "."
			}
			full, err := resolveInWorkspace(root, p)
			if err != nil {
				return "", err
			}
			fi, err := os.Stat(full)
			if err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("目录不存在：%s", p)
				}
				return "", fmt.Errorf("无法访问 %s：%v", p, err)
			}
			if !fi.IsDir() {
				return "", fmt.Errorf("%s 不是目录", p)
			}
			depth := 3
			if in.Depth != nil {
				if *in.Depth <= 0 {
					return "", fmt.Errorf("depth 须为正整数（给的是 %d）", *in.Depth)
				}
				depth = *in.Depth
				if depth > 5 {
					depth = 5
				}
			}
			display := strings.TrimSuffix(p, "/")
			if display == "" {
				display = "."
			}
			var lines []string
			nodes := 0
			truncated := false
			var walk func(dir string, level int)
			walk = func(dir string, level int) {
				if truncated {
					return
				}
				entries, err := os.ReadDir(dir)
				if err != nil {
					return // 读不了的子树静默跳过：树是导航视图，不因一枝失败全废
				}
				for _, e := range entries {
					if nodes >= treeNodeCap {
						truncated = true
						return
					}
					name := e.Name()
					if e.IsDir() {
						name += "/"
					}
					lines = append(lines, strings.Repeat("  ", level)+name)
					nodes++
					if !e.IsDir() {
						continue
					}
					child := filepath.Join(dir, e.Name())
					if level+1 <= depth {
						walk(child, level+1)
					} else if dirNonEmpty(child) {
						lines = append(lines, strings.Repeat("  ", level+1)+"…")
					}
				}
			}
			walk(full, 1)
			out := append([]string{display + "/"}, lines...)
			if truncated {
				out = append(out, "（已截断）")
			}
			return strings.Join(out, "\n"), nil
		})
}

// dirNonEmpty 判断目录是否有条目（… 标记只在真有下一层时出现）；读失败按空处理。
func dirNonEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// getFileInfoInput 是 get_file_info 的入参 schema。
type getFileInfoInput struct {
	Path string `json:"path" jsonschema:"description=相对 workspace 根的路径（文件或目录，如 hello.txt 或 .）"`
}

// newGetFileInfoTool 造 get_file_info：固定四行元信息（size/modified/mode/type）——
// 模型改文件前后自查「多大、改过没」的常规动作。size 对目录是目录项本身的字节数（OS 语义，不递归）。
// mode 是符号权限串（-rw-r--r-- 形，Perm().String() 直接产出）——19v6-08 §3 钉的字形，勿改回八进制。
func newGetFileInfoTool(root string) (tool.BaseTool, error) {
	return utils.InferTool("get_file_info",
		"查看 workspace 内一个文件或目录的元信息，固定四行：size（字节）/ modified（RFC3339）/ mode（权限串如 -rw-r--r--）/ type（文件或目录）。path 相对 workspace 根。",
		func(_ context.Context, in getFileInfoInput) (string, error) {
			full, err := resolveInWorkspace(root, in.Path)
			if err != nil {
				return "", err
			}
			fi, err := os.Stat(full)
			if err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("路径不存在：%s", in.Path)
				}
				return "", fmt.Errorf("无法访问 %s：%v", in.Path, err)
			}
			kind := "文件"
			if fi.IsDir() {
				kind = "目录"
			}
			return fmt.Sprintf("size: %d 字节\nmodified: %s\nmode: %s\ntype: %s",
				fi.Size(), fi.ModTime().Format(time.RFC3339), fi.Mode().Perm().String(), kind), nil
		})
}

// searchHitsCap / searchFileSizeCap 是 search_files 的两个减灾档：50 条命中封顶、
// 单文件 >1MB 跳过——防一次搜索把上下文或耗时拉爆。
const (
	searchHitsCap     = 50
	searchFileSizeCap = 1 << 20
)

// searchFilesInput 是 search_files 的入参 schema。
type searchFilesInput struct {
	Path     string `json:"path" jsonschema:"description=起始目录（相对 workspace 根，如 src 或 .）"`
	Query    string `json:"query" jsonschema:"description=要搜索的固定子串（纯文本，不是正则）"`
	FileGlob string `json:"file_glob,omitempty" jsonschema:"description=可选：只搜文件名匹配该 glob 的文件（如 *.go）"`
}

// newSearchFilesTool 造 search_files：递归固定子串搜索（v1 不做正则——ReDoS 面不自找，19v6-08 §1）。
// 命中格式 relpath:行号:行内容（relpath 相对 workspace 根）；WalkDir 字典序遍历所以行序稳定。
func newSearchFilesTool(root string) (tool.BaseTool, error) {
	return utils.InferTool("search_files",
		"在 workspace 内递归搜索固定子串（纯文本，不是正则），返回命中的 相对路径:行号:行内容，每行一条。"+
			"path 是起始目录（相对 workspace 根）；file_glob 可选（如 *.go 只搜 Go 文件）。最多返回 50 条。",
		func(_ context.Context, in searchFilesInput) (string, error) {
			full, err := resolveInWorkspace(root, in.Path)
			if err != nil {
				return "", err
			}
			if in.Query == "" {
				return "", fmt.Errorf("query 不能为空")
			}
			fi, err := os.Stat(full)
			if err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("目录不存在：%s", in.Path)
				}
				return "", fmt.Errorf("无法访问 %s：%v", in.Path, err)
			}
			if !fi.IsDir() {
				return "", fmt.Errorf("%s 不是目录", in.Path)
			}
			var hits []string
			overflow := false
			walkErr := filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil // 读不了的条目跳过：搜索是尽力而为的导航
				}
				if d.IsDir() {
					return nil
				}
				if in.FileGlob != "" {
					if ok, _ := filepath.Match(in.FileGlob, d.Name()); !ok {
						return nil
					}
				}
				info, err := d.Info()
				if err != nil || info.Size() > searchFileSizeCap {
					return nil
				}
				raw, err := os.ReadFile(p)
				if err != nil {
					return nil
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return nil
				}
				rel = filepath.ToSlash(rel)
				for i, line := range strings.Split(string(raw), "\n") {
					if !strings.Contains(line, in.Query) {
						continue
					}
					if len(hits) >= searchHitsCap {
						overflow = true
						return filepath.SkipAll
					}
					hits = append(hits, fmt.Sprintf("%s:%d:%s", rel, i+1, line))
				}
				return nil
			})
			if walkErr != nil && walkErr != filepath.SkipAll {
				return "", fmt.Errorf("搜索中断：%v", walkErr)
			}
			if len(hits) == 0 {
				return "（无命中）", nil
			}
			out := strings.Join(hits, "\n")
			if overflow {
				out += "\n（更多命中已省略）"
			}
			return out, nil
		})
}

// readMediaFileInput 是 read_media_file 的入参 schema。
type readMediaFileInput struct {
	Path string `json:"path" jsonschema:"description=相对 workspace 根的路径（如 assets/pic.png）"`
}

// mediaMimeByExt 是 read_media_file 的扩展名→MIME 表（小写扩展名匹配）；表外 = application/octet-stream。
var mediaMimeByExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".svg": "image/svg+xml",
	".pdf": "application/pdf", ".mp3": "audio/mpeg", ".wav": "audio/wav", ".mp4": "video/mp4",
}

// newReadMediaFileTool 造 read_media_file：回执是占位行（doc/32 A3）——path/mime/size/sha
// 一行带全，**不含媒体字节**。旧版回 data URI 单行：模型每调一次，几 MB base64 就永久抄进
// runlog/model_history/帧三处落库面（doc/32 §1 病灶），而文本管道下模型本来就看不见图。
// 句尾措辞是「请勿重复调用」——回执的读者就是刚调完的模型，指回自己 = 死循环（与入口占位
// mediaPlaceholder 的措辞分工见 media.go）。多模态消费随 translator 升级（19v6-08 §3 挂账）。
// >5MB 拒读：读进来就是往上下文灌大字节（减灾档），帽与入口图同值（mediaMaxBytes）。
func newReadMediaFileTool(root string) (tool.BaseTool, error) {
	return utils.InferTool("read_media_file",
		"读取 workspace 内的媒体文件的元信息（path/mime/size/sha 占位行；当前版本不回传媒体字节）。"+
			"path 是相对 workspace 根的路径；文件超过 5MB 会拒绝（请先裁剪或换小文件）。",
		func(_ context.Context, in readMediaFileInput) (string, error) {
			full, err := resolveInWorkspace(root, in.Path)
			if err != nil {
				return "", err
			}
			fi, err := os.Stat(full)
			if err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("文件不存在：%s", in.Path)
				}
				return "", fmt.Errorf("无法访问 %s：%v", in.Path, err)
			}
			if fi.IsDir() {
				return "", fmt.Errorf("%s 是目录不是文件", in.Path)
			}
			if fi.Size() > mediaMaxBytes {
				return "", fmt.Errorf("文件 %s 有 %d 字节、超过 5MB 上限——base64 会再膨胀约 1/3，请先裁剪或换用更小的文件", in.Path, fi.Size())
			}
			raw, err := os.ReadFile(full)
			if err != nil {
				return "", fmt.Errorf("读取失败：%v", err)
			}
			mime, ok := mediaMimeByExt[strings.ToLower(filepath.Ext(in.Path))]
			if !ok {
				mime = "application/octet-stream"
			}
			sum := sha256.Sum256(raw)
			return mediaReceiptPlaceholder(in.Path, mime, len(raw), hex.EncodeToString(sum[:])), nil
		})
}

// mediaReceiptPlaceholder 是工具回执占位（doc/32 A3）：与入口占位 mediaPlaceholder 同
// 字段面、不同句尾——不带「再调 read_media_file」自指。
func mediaReceiptPlaceholder(relPath, mime string, size int, shaHex string) string {
	return fmt.Sprintf("[media path=%s mime=%s size=%dB sha=%s；已按路径读取。当前版本不回传媒体字节，请勿重复调用]",
		relPath, mime, size, shaHex)
}
