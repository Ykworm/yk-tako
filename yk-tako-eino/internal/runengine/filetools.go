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
	"errors"
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
	if err := rejectSymlinkEscape(root, full); err != nil {
		return "", fmt.Errorf("路径越出 workspace（%s）", path)
	}
	return full, nil
}

// rejectSymlinkEscape 跟着符号链接看最终落点。父目录还不存在时允许创建。
// 比较前两边都解析符号链接：macOS 上 /var 指向 /private/var，不解析会把合法临时目录误判成越界。
func rejectSymlinkEscape(root, full string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	target := full
	if _, err := os.Lstat(full); err != nil {
		target = filepath.Dir(full)
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil
	}
	if target != full {
		rest, _ := filepath.Rel(target, full)
		if rest != "." {
			real = filepath.Join(real, rest)
		}
	}
	if real == realRoot || strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
		return nil
	}
	return fmt.Errorf("symlink")
}

// reserveStorage 在租户设了存储上限时先占住这次要写的字节。上限 0 表示不限制。
// 调用方必须在写入结束（成功或失败）后调用 release，否则预留会一直算在额度里。
func reserveStorage(ctx context.Context, adding int) (func(), error) {
	noop := func() {}
	ref, ok := workspaceFrom(ctx)
	if !ok || ref.MaxBytes <= 0 || adding <= 0 {
		return noop, nil
	}
	if ref.Reserve != nil {
		release, err := ref.Reserve(adding)
		if errors.Is(err, errStorageQuota) {
			return nil, fmt.Errorf("这个工作区的存储额度用完了")
		}
		if err != nil {
			return nil, err
		}
		if release == nil {
			return noop, nil
		}
		return release, nil
	}
	root := ref.Root
	if ref.TenantRoot != "" {
		root = ref.TenantRoot
	}
	if directoryBytes(root)+int64(adding) > ref.MaxBytes {
		return nil, fmt.Errorf("这个工作区的存储额度用完了")
	}
	return noop, nil
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
			"父目录不存在会自动创建；文件已存在则覆盖、返回文本会标注「（覆盖）」。返回格式：已写入 <path>（<n> 字节）。"+
			"【覆盖已有文件前必须先用 read_file 读它】——没读过会被拒（避免凭记忆整份回写、抹掉别人刚落盘的改动）；"+
			"读过但文件在此期间被别人改过，也会被拒：请重新 read_file，或改用 edit_file 做局部替换"+
			"（edit_file 按内容精确替换，并会保留别人已落盘的改动）。",
		func(ctx context.Context, in writeFileInput) (string, error) {
			full, err := resolveInWorkspace(activeRoot(ctx, root), in.Path)
			if err != nil {
				return "", err
			}
			// 取写锁（粒度=文件）。**比对必须在锁内做**——否则「检查—写」之间有缝，
			// 另一个写者能在缝里落盘，锁就白加了（filefp.go 的三层分工）。
			unlock, err := lockPaths(lockDirFor(ctx, root), []string{full})
			if err != nil {
				if errors.Is(err, errLockTimeout) {
					return "", fmt.Errorf("这个文件正被另一个 agent 写，稍后重试")
				}
				return "", err
			}
			defer unlock()
			// 写之前必须知道「我读到的是哪一版」：基准表里没有这一行 = 本 run 从没读过它。
			// write_file 是整份覆盖语义 → 必须整体是最新版，否则会把读之后的改动抹回旧版。
			base := fileBaselineFrom(ctx)
			agent := agentNameOf(ctx)
			known, hasBase := base.get(agent, full)
			_, statErr := os.Stat(full)
			existed := statErr == nil
			switch {
			case existed && !hasBase:
				return "", fmt.Errorf("你还没用 read_file 读它——先 read_file 读一遍再写（%s）", in.Path)
			case existed && hasBase:
				cur, ferr := fingerprintOf(full)
				if ferr != nil {
					return "", fmt.Errorf("读取 %s 的现状失败：%v", in.Path, ferr)
				}
				if !cur.equal(known) {
					if cur.weak() || known.weak() {
						return "", fmt.Errorf("它跟你读到的不一样了（%s）——先 read_file 重新读取；要做局部修改请用 edit_file（它按内容精确替换，会带上别人已落盘的改动）。（该文件用弱判据：mtime+size）", in.Path)
					}
					return "", fmt.Errorf("它跟你读到的不一样了（%s）——先 read_file 重新读取；要做局部修改请用 edit_file（它按内容精确替换，会带上别人已落盘的改动）", in.Path)
				}
			case !existed && hasBase:
				return "", fmt.Errorf("它刚被删或被移走，别重建——先确认现在的状态（%s）", in.Path)
			}
			release, err := reserveStorage(ctx, len(in.Content))
			if err != nil {
				return "", err
			}
			defer release()
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", fmt.Errorf("创建父目录失败：%v", err)
			}
			if err := os.WriteFile(full, []byte(in.Content), 0o644); err != nil {
				return "", fmt.Errorf("写入失败：%v", err)
			}
			// 写后刷新基准：同一 run 内连续改同一个文件不误报。
			if fp, ferr := fingerprintOf(full); ferr == nil {
				base.set(agent, full, fp)
			}
			if existed {
				return fmt.Sprintf("已写入 %s（%d 字节）（覆盖）", in.Path, len(in.Content)), nil
			}
			return fmt.Sprintf("已写入 %s（%d 字节）", in.Path, len(in.Content)), nil
		})
}

// readTextFileInput 是 read_text_file 的入参 schema：path 必填，offset/limit 非必填
// （omitempty）。offset 是 1-based 起始行、limit 是本次最多几行（doc/58 §3）。
type readTextFileInput struct {
	Path   string `json:"path" jsonschema:"description=相对 workspace 根的路径（POSIX 风格，如 notes/todo.txt）"`
	Offset *int   `json:"offset,omitempty" jsonschema:"description=从第几行开始读（1-based，默认 1）"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"description=本次最多读多少行（默认 2000、上限 2000）"`
}

// newReadTextFileTool / newReadFileTool（F4）是同一实现的两位出口：read_text_file 是 MCP
// filesystem 标准名，read_file 是 OpenHands 系惯用名——别名按真名进声明面（模型喊谁就是谁，
// 两把工具同时在池，同上）而不是给一把工具挂双名参数，声明面看到的始终是模型将调用的名字。
// 实现：流式读，按行交付（每行最多 2000 字符）；一次调用最多给 2000 行 / 50KB，
// 末尾附一行页脚（总行数 + 下一个 offset）——大文件靠 offset/limit 分页读（19v6-08 §3）。
func newReadTextFileTool(root string) (tool.BaseTool, error) {
	return buildReadTool("read_text_file",
		"读取 workspace 内的文本文件。path 是相对 workspace 根的路径（如 notes/todo.txt）。"+
			"offset 从第几行开始读（1-based，默认 1）、limit 本次最多读多少行（默认 2000、上限 2000）——"+
			"读到的内容后面附一行页脚，写明总行数与下一个 offset（读大文件靠它续读）。", root)
}

// newReadFileTool 造 read_file：惯用名别名，参数与行为等同 read_text_file（19v6-08 §3 名单第 11 名）。
func newReadFileTool(root string) (tool.BaseTool, error) {
	return buildReadTool("read_file",
		"读取 workspace 内的文本文件（惯用名，等同 read_text_file：同参同行为）。path 是相对 workspace 根的路径。"+
			"offset 从第几行开始读（1-based，默认 1）、limit 本次最多读多少行（默认 2000、上限 2000）——"+
			"读到的内容后面附一行页脚，写明总行数与下一个 offset（读大文件靠它续读）。", root)
}

// buildReadTool 是读工具的两个名字共用的装配体（名字/描述随调用方，行为一份）。
func buildReadTool(name, desc, root string) (tool.BaseTool, error) {
	return utils.InferTool(name, desc,
		func(ctx context.Context, in readTextFileInput) (string, error) {
			full, err := resolveInWorkspace(activeRoot(ctx, root), in.Path)
			if err != nil {
				return "", err
			}
			if err := checkReadArgs(in); err != nil {
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
			// 流式读（doc/58 §3.3）：一趟扫过去同时算整文件 sha256、点总行数、只收要的那一页。
			// 整份文件不进内存——大文件今天只能 head/tail，中间那段永远读不到。
			offset, limit := 1, readWindowMaxLines
			if in.Offset != nil {
				offset = *in.Offset
			}
			if in.Limit != nil {
				limit = *in.Limit
			}
			var (
				page   []string
				total  int
				capped bool
				used   int
			)
			fp, err := scanTextFile(full, func(no int, text string) {
				total = no
				if capped || no < offset || len(page) >= limit {
					return
				}
				n := len(text)
				if len(page) > 0 {
					n++ // 行间的换行也占字节（dsh 的 lineByteSize）
				}
				if used+n > readWindowMaxBytes {
					capped = true
					return
				}
				used += n
				page = append(page, text)
			})
			if err != nil {
				return "", fmt.Errorf("读取失败：%v", err)
			}
			// 记基准：「我读到的是哪一版」——write_file 的写前判据靠它（filefp.go）。
			// 记的是**整份文件**的指纹（流式算的 sha256，读到的只是其中一页），因为
			// write_file 是整份覆盖语义 → 尾部的改动同样会被它抹掉，同样算冲突。
			// bash 读不记（它不走 read_file）→ bash 改过的文件，write_file 会拒并要求重读。
			base := fileBaselineFrom(ctx)
			base.set(agentNameOf(ctx), full, fp)
			// 越界报错（空文件读第 1 行不算越界——dsh 的同款豁免）。
			if offset > total && !(total == 0 && offset == 1) {
				return "", fmt.Errorf("offset %d 超出文件末尾（共 %d 行）", offset, total)
			}
			return readBody(page, readFooter(offset, total, page, capped)), nil
		})
}

// checkReadArgs 判读工具的入参：offset 与 limit 各有取值范围。报错文案是契约（doc/58 §3.1）。
func checkReadArgs(in readTextFileInput) error {
	if in.Offset != nil && *in.Offset < 1 {
		return fmt.Errorf("offset 须为正整数（给的是 %d）", *in.Offset)
	}
	if in.Limit != nil {
		if *in.Limit < 1 {
			return fmt.Errorf("limit 须为正整数（给的是 %d）", *in.Limit)
		}
		if *in.Limit > readWindowMaxLines {
			return fmt.Errorf("limit 不得大于 %d（给的是 %d）", readWindowMaxLines, *in.Limit)
		}
	}
	return nil
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
			"【做局部修改请用本工具，而不是 write_file】——它在编辑那一刻重新读文件，"+
			"所以别人在这期间落盘的其它改动会被原样保留（write_file 是整份覆盖，会把它们抹掉）。"+
			"返回格式：已编辑 <path>（替换 n 处）。",
		func(ctx context.Context, in editFileInput) (string, error) {
			full, err := resolveInWorkspace(activeRoot(ctx, root), in.Path)
			if err != nil {
				return "", err
			}
			if in.OldText == "" {
				return "", fmt.Errorf("old_text 不能为空")
			}
			// 取写锁（粒度=文件）。edit_file 不查基准表做拒绝判断——OldText 锚点本身就是
			// 「我读到的那一段」，它是 edit 的基准；但锁仍然要取：它保证下面的「现读」读到的是
			// 完整的一版（不会读到别人写了一半的文件），并让「读—改—写」成为原子的一段。
			unlock, err := lockPaths(lockDirFor(ctx, root), []string{full})
			if err != nil {
				if errors.Is(err, errLockTimeout) {
					return "", fmt.Errorf("这个文件正被另一个 agent 写，稍后重试")
				}
				return "", err
			}
			defer unlock()
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
			// 写后刷新基准（若有这一行）：让表跟磁盘保持一致，否则下一次 write_file 会因为
			// 基准过期而误拒（我们刚自己改过它，内容变了但那是合法的）。
			base := fileBaselineFrom(ctx)
			agent := agentNameOf(ctx)
			if fp, ferr := fingerprintOf(full); ferr == nil {
				base.set(agent, full, fp)
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
		func(ctx context.Context, in moveFileInput) (string, error) {
			src, err := resolveInWorkspace(activeRoot(ctx, root), in.Path)
			if err != nil {
				return "", err
			}
			dst, err := resolveInWorkspace(activeRoot(ctx, root), in.Destination)
			if err != nil {
				return "", err
			}
			// 取写锁：move 动的是两个路径（源与目标）→ 两把锁。lockPaths 内部去重 + 按字典序取
			// （防 AB-BA 死锁）；源与目标同路径时只取一把（取两次同一把锁 = 自己等自己）。
			unlock, err := lockPaths(lockDirFor(ctx, root), []string{src, dst})
			if err != nil {
				if errors.Is(err, errLockTimeout) {
					return "", fmt.Errorf("这个文件正被另一个 agent 写，稍后重试")
				}
				return "", err
			}
			defer unlock()
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
		func(ctx context.Context, in createDirectoryInput) (string, error) {
			full, err := resolveInWorkspace(activeRoot(ctx, root), in.Path)
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
		func(ctx context.Context, in listDirectoryInput) (string, error) {
			full, err := resolveInWorkspace(activeRoot(ctx, root), in.Path)
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
		func(ctx context.Context, in directoryTreeInput) (string, error) {
			p := in.Path
			if p == "" {
				p = "."
			}
			full, err := resolveInWorkspace(activeRoot(ctx, root), p)
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
		func(ctx context.Context, in getFileInfoInput) (string, error) {
			full, err := resolveInWorkspace(activeRoot(ctx, root), in.Path)
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
		func(ctx context.Context, in searchFilesInput) (string, error) {
			full, err := resolveInWorkspace(activeRoot(ctx, root), in.Path)
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

// resolveReadMediaPath 决定 read_media_file 读哪一个文件。
// 占位 path 是 workspaces/<wid>/... 时，先按清完的 wid 做 VisibleWorkspace，
// 再从租户根下的这个工作区读。范围不是这场会话的作业根。
// sessions/<sid>/... 不再解析。其余相对路径仍落在作业根里。
func resolveReadMediaPath(ctx context.Context, store *Store, fallbackRoot, path string) (string, error) {
	if wid, rel, ok := parseSessionMediaPath(path); ok {
		who, hasWho := principalFromCtx(ctx)
		ref, hasRef := workspaceFrom(ctx)
		if store == nil || !hasWho || !hasRef || ref.TenantRoot == "" {
			return "", fmt.Errorf("文件不存在：%s", path)
		}
		if _, err := store.VisibleWorkspace(ctx, wid, who.TenantID, who.UserID); err != nil {
			return "", fmt.Errorf("文件不存在：%s", path)
		}
		return resolveInWorkspace(filepath.Join(ref.TenantRoot, "workspaces", wid), rel)
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.ToSlash(strings.TrimSpace(path))))
	if cleaned == "sessions" || strings.HasPrefix(cleaned, "sessions/") {
		return "", fmt.Errorf("文件不存在：%s", path)
	}
	return resolveInWorkspace(activeRoot(ctx, fallbackRoot), path)
}

// readMediaFileInput 是 read_media_file 的入参 schema。
type readMediaFileInput struct {
	Path string `json:"path" jsonschema:"description=占位行里的 path 原文，形如 workspaces/<id>/.media/<file>。作业根内的相对路径仍可用。不要改写 path。"`
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
// >5MB 拒读：读进来就是往上下文灌大字节（减灾档），上限与入口图同值（mediaMaxBytes）。
func newReadMediaFileTool(store *Store, root string) (tool.BaseTool, error) {
	return utils.InferTool("read_media_file",
		"读取媒体文件的元信息（path/mime/size/sha 占位行；媒体字节不会写入对话记录）。"+
			"path 用对话里占位行的 path 原文（workspaces/<id>/.media/<file>），不要改写。文件超过 5MB 会拒绝。",
		func(ctx context.Context, in readMediaFileInput) (string, error) {
			full, err := resolveReadMediaPath(ctx, store, root, in.Path)
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
	return fmt.Sprintf("[media path=%s mime=%s size=%dB sha=%s；已按路径读取。媒体字节不会写入对话记录，请勿重复调用]",
		relPath, mime, size, shaHex)
}
