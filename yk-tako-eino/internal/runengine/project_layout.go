// 本文件是项目容器的路径底座（方案真源 = doc/76 §5.1/§6.1）。
//
// 病灶：今天「项目目录」同时是用户内容根、媒体落点、bash/fetch 输出落点、项目记忆落点
// 与编排组工作区落点。删项目要删对整棵容器，用户文件清单又要藏掉运行时内容；两件事
// 靠一张黑名单（.media/.tako）维持，任何新写口忘记登记就会污染用户视野。
//
// 改后一个项目 = 一个容器，里面两棵树：
//
//	<workspace>/tenants/<tid>/workspaces/<wid>/
//	├── layout.json      容器格式版本（缺它 = 存量未迁移项目，入口一律拒绝）
//	├── content/         用户内容根：文件工具、普通聊天、发布目标
//	└── runtime/         运行资源：组工作区、临时输出、媒体、项目记忆
//
// layout.json 只在两种时刻写下：
//   - 新项目第一次需要目录时（容器不存在或为空——没有旧字节可搬）；
//   - 显式迁移命令完成「复制 → 哈希校验 → 原子切换」之后。
//
// 存量项目容器里已有 layout.json 之外的条目 = 未迁移，入口回 409 117116。不做双读：
// 双读会让同一项目在两个根上各有一份「看起来正常」的字节，写错一边就是永久分叉。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// projectLayoutFormatVersion 是容器布局的格式版本；布局形状变了才递增。
	projectLayoutFormatVersion = 1
	projectLayoutFileName      = "layout.json"
	projectContentDirName      = "content"
	projectRuntimeDirName      = "runtime"
	// projectMediaDirName 是 runtime 下的媒体目录。逻辑占位路径仍是
	// workspaces/<wid>/.media/<file>（历史消息逐字不变），物理字节落 runtime/media。
	projectMediaDirName = "media"
)

// errProjectLayoutUnmigrated 是存量项目容器还没有 layout.json，且里面已有旧字节。
// HTTP 层统一收成 409 117116；迁移前不提供任何读写路径。
var errProjectLayoutUnmigrated = errors.New("project layout needs migration")

// projectLayout 是 layout.json 的形状。只记格式版本与迁移身份，不记任何 Provider 路径。
type projectLayout struct {
	FormatVersion int       `json:"formatVersion"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// projectContainerDir 算项目容器目录，不创建它。
func projectContainerDir(root, tenantID, workspaceID string) (string, error) {
	return workspaceDir(root, tenantID, workspaceID)
}

// projectContentDir 算用户内容根，不创建它。
func projectContentDir(root, tenantID, workspaceID string) (string, error) {
	container, err := projectContainerDir(root, tenantID, workspaceID)
	if err != nil {
		return "", err
	}
	return filepath.Join(container, projectContentDirName), nil
}

// projectRuntimeDir 算运行资源根，不创建它。
func projectRuntimeDir(root, tenantID, workspaceID string) (string, error) {
	container, err := projectContainerDir(root, tenantID, workspaceID)
	if err != nil {
		return "", err
	}
	return filepath.Join(container, projectRuntimeDirName), nil
}

// projectMemoryDir 算项目记忆文件目录（runtime/memory），不创建它。
func projectMemoryDir(root, tenantID, workspaceID string) (string, error) {
	runtimeDir, err := projectRuntimeDir(root, tenantID, workspaceID)
	if err != nil {
		return "", err
	}
	return filepath.Join(runtimeDir, "memory"), nil
}

// projectLayoutPath 算 layout.json 路径。
func projectLayoutPath(container string) string {
	return filepath.Join(container, projectLayoutFileName)
}

// readProjectLayout 读 layout.json；不存在 = (nil, nil)，损坏 = 错误。
func readProjectLayout(container string) (*projectLayout, error) {
	b, err := os.ReadFile(projectLayoutPath(container))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l projectLayout
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("项目布局文件损坏：%w", err)
	}
	return &l, nil
}

// containerLegacyEntries 数容器里「不是原子写临时文件」的条目；容器不存在 = 0。
// layout.json 的写入是同目录临时文件 + rename：并发读者可能在 rename 前看见 `.tako-tmp-*`，
// 它不是存量字节，不能拿来判「这个项目已有旧目录内容」。
func containerLegacyEntries(container string) (int, error) {
	entries, err := os.ReadDir(container)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tako-tmp-") {
			continue
		}
		n++
	}
	return n, nil
}

// ensureProjectLayout 返回项目的内容根与运行资源根，必要时建立新容器。
//
// 判定顺序：
//  1. layout.json 存在 → 版本必须一致；确保 content/runtime 存在。
//  2. layout.json 不存在且容器不存在或为空 → 新项目，写 layout 并建两棵树。
//  3. layout.json 不存在但容器已有条目 → errProjectLayoutUnmigrated。
//
// 第 2 条对空容器就地转正是安全的：没有旧字节可搬，迁移清单为空。已有任何条目的存量
// 项目（哪怕只是一个 .media 文件）一律走显式迁移命令，避免「文件还在旧根、用户却看到
// 空 content」的静默分叉。
func ensureProjectLayout(root, tenantID, workspaceID string) (contentDir, runtimeDir string, err error) {
	container, err := projectContainerDir(root, tenantID, workspaceID)
	if err != nil {
		return "", "", err
	}
	contentDir = filepath.Join(container, projectContentDirName)
	runtimeDir = filepath.Join(container, projectRuntimeDirName)
	layout, err := readProjectLayout(container)
	if err != nil {
		return "", "", err
	}
	if layout != nil {
		if layout.FormatVersion != projectLayoutFormatVersion {
			return "", "", fmt.Errorf("项目布局版本 %d 不受支持（当前 %d）",
				layout.FormatVersion, projectLayoutFormatVersion)
		}
		if err := os.MkdirAll(contentDir, 0o755); err != nil {
			return "", "", err
		}
		if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
			return "", "", err
		}
		return contentDir, runtimeDir, nil
	}
	entries, rerr := containerLegacyEntries(container)
	if rerr != nil {
		return "", "", rerr
	}
	if entries > 0 {
		// 与新项目初始化并发：初始化先写 layout 再建两棵树，所以「看见条目却还没读到 layout」
		// 可能只是上一次读取早于初始化写入。再看一眼；仍没有才是真正的存量容器。
		layout, err = readProjectLayout(container)
		if err != nil {
			return "", "", err
		}
		if layout == nil {
			return "", "", errProjectLayoutUnmigrated
		}
		if layout.FormatVersion != projectLayoutFormatVersion {
			return "", "", fmt.Errorf("项目布局版本 %d 不受支持（当前 %d）",
				layout.FormatVersion, projectLayoutFormatVersion)
		}
		if err := os.MkdirAll(contentDir, 0o755); err != nil {
			return "", "", err
		}
		if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
			return "", "", err
		}
		return contentDir, runtimeDir, nil
	}
	// 先写 layout 再建两棵树：反序会让并发读者看到「有目录、没 layout」的窗口，把刚建的新项目
	// 判成存量（409 117116）。崩溃在两步之间只会留下空容器或只有 layout 的容器，下次调用补树。
	now := time.Now().UTC()
	b, err := json.Marshal(projectLayout{
		FormatVersion: projectLayoutFormatVersion,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(container, 0o755); err != nil {
		return "", "", err
	}
	if err := atomicWriteFile(projectLayoutPath(container), b, 0o644); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", "", err
	}
	return contentDir, runtimeDir, nil
}

// projectLayoutReady 只读判断项目容器是否已迁移，不建目录。
// 入口在「先拒绝、后动手」的位置用它：未迁移项目不该因为一次读请求被建出空 content。
func projectLayoutReady(root, tenantID, workspaceID string) (bool, error) {
	container, err := projectContainerDir(root, tenantID, workspaceID)
	if err != nil {
		return false, err
	}
	layout, err := readProjectLayout(container)
	if err != nil {
		return false, err
	}
	return layout != nil, nil
}

// projectLayoutNeedsMigration 判断存量项目是否必须先跑迁移命令：
// 没有 layout.json 且容器里已有条目。容器不存在或为空 = 新项目，不需要迁移
// （第一次使用时原地初始化，与 ensureProjectLayout 第 2 条同一判据）。
func projectLayoutNeedsMigration(root, tenantID, workspaceID string) (bool, error) {
	container, err := projectContainerDir(root, tenantID, workspaceID)
	if err != nil {
		return false, err
	}
	layout, err := readProjectLayout(container)
	if err != nil {
		return false, err
	}
	if layout != nil {
		return false, nil
	}
	entries, err := containerLegacyEntries(container)
	if err != nil {
		return false, err
	}
	if entries == 0 {
		return false, nil
	}
	// 与新项目初始化并发时，上面的 layout 读取可能早于初始化写入，而 ReadDir 已看见目录。
	// 初始化先写 layout 再建树，所以「条目在、layout 也在」在这里重读一次就能收口。
	layout, err = readProjectLayout(container)
	if err != nil {
		return false, err
	}
	return layout == nil, nil
}

// runtimeOutputDir 返回内部输出（bash 全文、抓取整页、搜索全文）的物理目录与逻辑前缀。
//
// 有运行资源根（项目容器内）→ 物理 <runtime>/tmp/<kind>，逻辑 runtime/tmp/<kind>；
// 纯会话（没有项目容器）→ 物理 <会话根>/.tako/<kind>，逻辑 .tako/<kind>（旧布局不迁移）。
// 逻辑路径要进工具回执并落进永久历史，所以必须与物理布局解耦：历史消息里的旧路径由
// resolveRuntimeReadPath 映射回新位置。
func runtimeOutputDir(ctx context.Context, fallbackRoot, kind string) (physDir, relDir string) {
	if ref, ok := workspaceFrom(ctx); ok && ref.RuntimeRoot != "" {
		return filepath.Join(ref.RuntimeRoot, "tmp", kind), "runtime/tmp/" + kind
	}
	return filepath.Join(fallbackRoot, ".tako", kind), ".tako/" + kind
}

// resolveRuntimeReadPath 解析两类逻辑路径：
//
//	runtime/...            → 运行资源根内的相对路径（新工具回执）
//	.tako/{bash,fetch,search}/... → runtime/tmp/{...}（历史消息里的旧回执）
//
// 第二个返回值 false = 不是这两类，调用方按普通内容路径解析。
// 没有运行资源根（纯会话）时旧 .tako 路径仍按内容根解析，由调用方原样落回旧布局。
func resolveRuntimeReadPath(ctx context.Context, ref WorkspaceRef, path string) (string, bool, error) {
	if ref.RuntimeRoot == "" {
		return "", false, nil
	}
	slash := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if slash == "runtime" || strings.HasPrefix(slash, "runtime/") {
		rel := strings.TrimPrefix(strings.TrimPrefix(slash, "runtime"), "/")
		full, err := resolveInWorkspace(ref.RuntimeRoot, rel)
		return full, true, err
	}
	for _, kind := range []string{"bash", "fetch", "search"} {
		prefix := ".tako/" + kind
		if slash == prefix || strings.HasPrefix(slash, prefix+"/") {
			rest := strings.TrimPrefix(strings.TrimPrefix(slash, prefix), "/")
			full, err := resolveInWorkspace(filepath.Join(ref.RuntimeRoot, "tmp", kind), rest)
			return full, true, err
		}
	}
	return "", false, nil
}

// writeProjectLayoutAfterMigration 由迁移命令在 staging 校验通过、原子切换完成后调用：
// 容器里已经是新的 content/runtime 两棵树，只补 layout.json。
func writeProjectLayoutAfterMigration(container string, createdAt time.Time) error {
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	b, err := json.Marshal(projectLayout{
		FormatVersion: projectLayoutFormatVersion,
		CreatedAt:     createdAt,
		UpdatedAt:     time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return atomicWriteFile(projectLayoutPath(container), b, 0o644)
}
